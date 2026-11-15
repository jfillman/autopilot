package definitions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// AgentsGroupVersion is where Agent XRs live (airframe xrds/agent.yaml).
const AgentsGroupVersion = "catalog.hangar.io/v1alpha1"

// DefaultInterval is how often the loader polls (Python: AGENT_XRS_INTERVAL, 60 s).
const DefaultInterval = 60 * time.Second

// XRLister lists the Agent XRs of one namespace. An error means "I could not tell": the loader keeps the current set. A namespace
// where the kind is not served, or that is not there, is an error too (it is not "no XRs").
type XRLister interface {
	ListAgents(ctx context.Context, namespace string) ([]map[string]any, error)
}

// FileReader reads one file of a repository at a commit through the agents' GitHub App (repo.file.get's reader): its result is
// {path, ref, found | type, content, note, ...}. A *core.Denied is the token broker's rule (not an onboarded app's own repository,
// a malformed name): final for this pin. Any other error is a failure that may pass (GitHub or the broker unreachable): the pin
// is skipped this poll and what was loaded for it stays.
type FileReader interface {
	ReadFile(ctx context.Context, repo, path, ref string) (map[string]any, error)
}

// Sink takes what a poll found: the manager that keeps the set and publishes it.
type Sink interface {
	ApplyCluster(ctx context.Context, loaded []*Loaded, rejected []Rejected) Changes
}

// Poller is the Agent XR loader (agent_xrs.AgentXRs). Every Interval it lists the XRs, fetches each pinned file once per commit,
// decides each pin (FromPin) and hands the set to the Sink.
//
// Where it reads: only the namespaces it is configured with, one namespaced list each. Python lists every namespace of the cluster
// and trusts whatever it finds; here an XR in a namespace that is not listed is never read, and an XR whose own namespace is not the
// one that was listed is dropped. Who may create an Agent XR in those namespaces is the cluster's RBAC (the XRD's tenants flow:
// a reviewed pull request on the tenants repository); the namespaces a run or an app team controls are not put in the list.
//
// What it does when something fails, so that nothing unloads an agent by accident: a list that fails (the API server, RBAC, a kind
// that is not served) keeps the whole current set; a pin whose file cannot be fetched for a reason that may pass is skipped and keeps
// whatever was loaded for it; when every pin was skipped nothing is applied. Only a pin that cannot become a definition for a reason
// that will not change (no such file at that commit, not YAML, the broker's refusal, the schema, the rules) is rejected, with the reason.
// And an empty answer after a non-empty one is not believed until it is given twice in a row (the one difference from Python in
// what a poll does).
type Poller struct {
	Namespaces []string
	Lister     XRLister
	Files      FileReader
	Sink       Sink
	Set        *Set
	Baseline   *tools.Registry
	Policy     PinPolicy
	Interval   time.Duration
	Now        func() time.Time
	Log        *slog.Logger

	mu      sync.Mutex
	cache   map[pinKey]*cached
	fetches int
	empties int
	// listFailures counts lists that failed in a row.
	listFailures int
}

type pinKey struct{ repo, path, commit string }

var fullCommit = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type cached struct {
	doc    *OMap
	reason string // set when the file cannot be a definition (published)
	detail string // the same, whole, for the log
	at     string
	// expires is set for an answer that is about the repository's state and not the commit's (the broker's refusal, no such file):
	// it is asked again after NegativeCacheLifetime, so a pin whose app is onboarded later, or whose file is pushed later, is read.
	expires time.Time
}

// NewPoller is a poller; its Namespaces, Lister, Files, Sink, Set and Baseline are set by the caller.
func NewPoller() *Poller {
	return &Poller{Interval: DefaultInterval, Now: func() time.Time { return time.Now().UTC() }, Log: slog.Default(), cache: map[pinKey]*cached{}}
}

// Fetches is how many files the loader has fetched (a pin is fetched once per commit).
func (p *Poller) Fetches() int { p.mu.Lock(); defer p.mu.Unlock(); return p.fetches }

func (p *Poller) iso() string { return p.Now().UTC().Format("2006-01-02T15:04:05Z") }

// Seed warms the cache from the rows the last process stored: the documents it loaded, by their pins, so a GitHub outage at
// restart drops no agent. It returns how many.
func (p *Poller) Seed(rows []Row) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range rows {
		sv, _ := r["source"].(map[string]any)
		s := SourceFromView(sv)
		repo, path, commit := strOr(s.Repo, ""), strOr(s.Path, ""), strOr(s.Commit, "")
		doc, isDoc := r["doc"].(map[string]any)
		if r["state"] == "loaded" && repo != "" && path != "" && commit != "" && isDoc {
			at := strOr(s.LoadedAt, "")
			if at == "" {
				at = p.iso()
			}
			p.cache[pinKey{repo, path, commit}] = &cached{doc: FromPlain(doc).(*OMap), at: at}
			n++
		}
	}
	return n
}

// Run polls until ctx ends. One bad poll (including a panic) never ends the loop.
func (p *Poller) Run(ctx context.Context) {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					p.Log.Error("agent pins: poll failed", "panic", fmt.Sprint(r))
				}
			}()
			p.Poll(ctx)
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.Interval):
		}
	}
}

type skipPin struct{}

func strField(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func mapField(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

func ptrIfString(m map[string]any, k string) *string {
	if s, ok := m[k].(string); ok {
		return &s
	}
	return nil
}

// list reads every namespace; one failure is the whole list failing.
func (p *Poller) list(ctx context.Context) ([]map[string]any, error) {
	var items []map[string]any
	for _, ns := range p.Namespaces {
		got, err := p.Lister.ListAgents(ctx, ns)
		if err != nil {
			return nil, fmt.Errorf("namespace %s: %w", ns, err)
		}
		for _, it := range got {
			if strField(mapField(it, "metadata"), "namespace") != ns {
				p.Log.Warn("agent pins: an Agent XR that is not in the namespace that was listed was ignored", "namespace", ns,
					"name", strField(mapField(it, "metadata"), "name"))
				continue
			}
			items = append(items, it)
		}
		if len(items) > MaxPins {
			return nil, fmt.Errorf("more than %d Agent XRs: the core takes no more in one set", MaxPins)
		}
	}
	return items, nil
}

// Poll is one pass: list, fetch what is new, decide, apply. It returns what changed.
func (p *Poller) Poll(ctx context.Context) Changes {
	items, err := p.list(ctx)
	if err != nil {
		// The set the gateway runs and the set the core holds stay as they are. Said every poll; as an error once it has gone on.
		p.mu.Lock()
		p.empties = 0 // an empty answer on either side of a failed list is not two in a row
		p.listFailures++
		n := p.listFailures
		p.mu.Unlock()
		lvl := slog.LevelWarn
		if n >= 3 {
			lvl = slog.LevelError
		}
		p.Log.Log(ctx, lvl, "agent pins: could not list Agent XRs; keeping the current set", "err", err.Error(), "consecutive_failures", n)
		return Changes{}
	}
	p.mu.Lock()
	p.listFailures = 0
	p.mu.Unlock()
	if len(items) == 0 && len(p.Set.Records()) > 0 {
		p.mu.Lock()
		p.empties++
		n := p.empties
		p.mu.Unlock()
		if n < 2 {
			p.Log.Warn("agent pins: the list of Agent XRs is empty; the set is not emptied on one answer (it is, if the next poll says the same)")
			return Changes{}
		}
	} else if len(items) > 0 {
		p.mu.Lock()
		p.empties = 0
		p.mu.Unlock()
	}
	var loaded []*Loaded
	var rejected []Rejected
	// A tenant's namespace holds at most MaxPinsPerNamespace pins (the first by name): one cannot fill the budget the others share.
	perNS := map[string][]string{}
	for _, xr := range items {
		md := mapField(xr, "metadata")
		perNS[strField(md, "namespace")] = append(perNS[strField(md, "namespace")], strField(md, "name"))
	}
	overQuota := map[string]bool{}
	for ns, names := range perNS {
		sort.Strings(names)
		for i, n := range names {
			if i >= MaxPinsPerNamespace {
				overQuota[ns+"/"+n] = true
			}
		}
	}
	skipped := 0
	skippedNames := map[string]bool{} // the pins whose file could not be read this poll: only these keep what was loaded for them
	current := map[pinKey]bool{}
	reserved := p.Set.Bundled()
	for _, xr := range items {
		meta, spec := mapField(xr, "metadata"), mapField(xr, "spec")
		srcSpec := mapField(spec, "source")
		src := Source{Kind: "cluster", Repo: S(strField(srcSpec, "repo")), Path: S(strField(srcSpec, "path")), Commit: S(strField(srcSpec, "commit")),
			Namespace: ptrIfString(meta, "namespace"), XR: ptrIfString(meta, "name")}
		repo, path, commit, name := strField(srcSpec, "repo"), strField(srcSpec, "path"), strField(srcSpec, "commit"), strField(meta, "name")
		if repo == "" || path == "" || commit == "" || name == "" {
			n := name
			if n == "" {
				n = "?"
			}
			rejected = append(rejected, Rejected{Name: n, Source: src, Reason: "the XR has no complete spec.source (repo, path, commit)"})
			continue
		}
		if overQuota[strField(meta, "namespace")+"/"+name] {
			rejected = append(rejected, Rejected{Name: name, Source: src, Reason: fmt.Sprintf("DF-QUOTA: more than %d Agent XRs in this namespace", MaxPinsPerNamespace)})
			continue
		}
		if err := p.Policy.CheckSource(strField(meta, "namespace"), repo); err != nil {
			// Before anything is fetched: no token is minted for a repository this namespace's app does not own.
			var pe *PolicyError
			errors.As(err, &pe)
			rej := Rejected{Name: name, Source: src, Reason: "DF-POLICY: " + pe.Path, Detail: err.Error()}
			if p.Policy.Detailed {
				rej.Reason = err.Error()
			}
			rejected = append(rejected, rej)
			continue
		}
		if !fullCommit.MatchString(commit) {
			// A pin is immutable only if it names a commit: a branch or a tag moves without the review the pin is for, and a cache that
			// never fetches it again would hold whichever version it saw first.
			rejected = append(rejected, Rejected{Name: name, Source: src, Reason: "spec.source.commit is not a full commit id (40 or 64 hex digits): a branch or a tag can move without the review a pin is for"})
			continue
		}
		key := pinKey{repo, path, commit}
		current[key] = true
		c, err := p.fetch(ctx, src, key)
		if err != nil {
			skipped++
			skippedNames[name] = true
			continue
		}
		src.LoadedAt = S(c.at)
		if c.reason != "" {
			rejected = append(rejected, Rejected{Name: name, Source: src, Reason: c.reason, Detail: c.detail})
			continue
		}
		l, rej := FromPin(c.doc, src, strField(spec, "owner"), reserved, p.Baseline, p.Policy)
		if rej != nil {
			rejected = append(rejected, *rej)
		} else {
			loaded = append(loaded, l)
		}
	}
	if skipped > 0 && len(loaded) == 0 && len(rejected) == 0 && len(items) > 0 {
		// Everything unreachable (GitHub or the broker down): a poll that would empty the set is not applied.
		p.Log.Warn("agent pins: every pin is unreachable; keeping the current set", "unreachable", skipped)
		return Changes{}
	}
	if skipped > 0 {
		// The unreachable pins keep their last state: re-add what is loaded for them so the swap drops nothing. Only them: a pin whose XR is
		// gone is gone, whatever else could not be read (Python re-adds every definition this poll did not see, so a deleted pin lives on
		// for as long as any other pin is unreachable).
		seen := map[string]bool{}
		for _, l := range loaded {
			seen[l.Name()] = true
		}
		for _, r := range rejected {
			seen[r.Name] = true
		}
		p.mu.Lock()
		for _, name := range p.Set.ClusterNames() {
			l := p.Set.Loaded(name)
			if l == nil || seen[name] || !skippedNames[name] {
				continue
			}
			key := pinKey{strOr(l.Source.Repo, ""), strOr(l.Source.Path, ""), strOr(l.Source.Commit, "")}
			if c := p.cache[key]; c != nil && c.doc != nil {
				loaded = append(loaded, l)
			}
		}
		p.mu.Unlock()
	}
	ch := p.Sink.ApplyCluster(ctx, loaded, rejected)
	for _, r := range ch.Rejected {
		// The published reason carries no value out of the file; what the file said is here, in the gateway's log.
		p.Log.Warn("agent pins: a pin was rejected", "agent", r.Name, "pin", r.Source.Pin(), "reason", r.Reason, "detail", r.Detail)
	}
	p.prune(current)
	if ch.Any() {
		p.Log.Info("agent pins: applied", "added", len(ch.Added), "changed", len(ch.Changed), "removed", len(ch.Removed),
			"rejected", len(ch.Rejected), "cleared", len(ch.Cleared), "cluster_definitions", len(p.Set.ClusterNames()))
	}
	return ch
}

// prune forgets cached files that no pin and no loaded definition names any more.
func (p *Poller) prune(current map[pinKey]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, name := range p.Set.ClusterNames() {
		if l := p.Set.Loaded(name); l != nil {
			current[pinKey{strOr(l.Source.Repo, ""), strOr(l.Source.Path, ""), strOr(l.Source.Commit, "")}] = true
		}
	}
	for k := range p.cache {
		if !current[k] {
			delete(p.cache, k)
		}
	}
}

// fetch is the pin's file, once per (repo, path, commit): kept, whatever it said. A *skipPin error means this poll could not tell.
func (p *Poller) fetch(ctx context.Context, s Source, key pinKey) (*cached, error) {
	p.mu.Lock()
	if c, ok := p.cache[key]; ok && (c.expires.IsZero() || p.Now().Before(c.expires)) {
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()
	r, err := p.Files.ReadFile(ctx, key.repo, key.path, key.commit)
	var out *cached
	var denied *core.Denied
	switch {
	case errors.As(err, &denied):
		// The broker's rule (not an onboarded app's own repository) or a malformed name: final for this pin.
		out = &cached{reason: key.path + ": " + denied.Rule, detail: denied.Rule + ": " + denied.Message, at: p.iso(), expires: p.Now().Add(NegativeCacheLifetime)}
		if p.Policy.Detailed {
			out.reason = out.detail
		}
	case err != nil:
		p.Log.Warn("agent pins: a pinned file is unreachable this poll", "repo", key.repo, "commit", short(key.commit), "path", key.path, "err", err.Error())
		return nil, &skipErr{err}
	default:
		out = p.decide(r, key)
	}
	p.mu.Lock()
	p.fetches++
	p.cache[key] = out
	p.mu.Unlock()
	return out, nil
}

type skipErr struct{ error }

func short(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

// decide is agent_xrs._decide: what a fetched file is, as a document or the reason it is not.
func (p *Poller) decide(r map[string]any, k pinKey) *cached {
	at := p.iso()
	where := fmt.Sprintf("%s in %s at %s", k.path, k.repo, short(k.commit))
	if f, ok := r["found"].(bool); ok && !f {
		return &cached{reason: fmt.Sprintf("no file %s in %s at %s", k.path, k.repo, short(k.commit)), at: at, expires: p.Now().Add(NegativeCacheLifetime)}
	}
	if t, _ := r["type"].(string); t != "file" {
		shown := "non-file"
		if v, ok := r["type"]; ok {
			shown = fmt.Sprint(v)
			if v == nil {
				shown = "None"
			}
		}
		return &cached{reason: fmt.Sprintf("%s is a %s, not a file", where, shown), at: at}
	}
	content, isString := r["content"].(string)
	if r["content"] == nil || !isString {
		note, _ := r["note"].(string)
		if note == "" {
			note = "not text"
		}
		return &cached{reason: fmt.Sprintf("%s is %s", where, note), at: at}
	}
	if len(content) > MaxPinDocBytes*2 {
		return &cached{reason: k.path + ": DF-STRICT", detail: fmt.Sprintf("%s is larger than a pinned definition may be", k.path), at: at}
	}
	doc, reason, detail := ReadPinnedFile(k.path, content, p.Policy.Detailed)
	if reason != "" {
		return &cached{reason: reason, detail: detail, at: at}
	}
	return &cached{doc: doc, at: at}
}
