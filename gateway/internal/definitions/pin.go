package definitions

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/pyrepr"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// RunIdentityNamespace is the namespace a pinned agent's workload identity lives in (engine/definitions.py).
const RunIdentityNamespace = "autopilot-runs"

// Source is where a definition came from. Kind is "bundled" (the image's agents/ directory) or "cluster" (an Agent XR's pin).
// A field that is nil is absent (Python's None), which is not the same as an empty string: the published request carries an
// empty repo but not an absent one.
type Source struct {
	Kind      string
	Repo      *string
	Path      *string
	Commit    *string
	Namespace *string
	XR        *string
	LoadedAt  *string
}

// S is a pointer to a copy of s.
func S(s string) *string { return &s }

func strOr(p *string, d string) string {
	if p == nil {
		return d
	}
	return *p
}

// Pin is one line: repo@short path, or `bundled`.
func (s Source) Pin() string {
	if s.Kind != "cluster" {
		return "bundled"
	}
	c := strOr(s.Commit, "")
	if len(c) > 7 {
		c = c[:7]
	}
	show := func(p *string) string {
		if p == nil {
			return "None"
		}
		return *p
	}
	return fmt.Sprintf("%s@%s %s", show(s.Repo), c, show(s.Path))
}

// View is Source.view(): the fields that are set, and the pin.
func (s Source) View() map[string]any {
	out := map[string]any{"kind": s.Kind}
	for k, v := range map[string]*string{"repo": s.Repo, "path": s.Path, "commit": s.Commit, "namespace": s.Namespace, "xr": s.XR, "loaded_at": s.LoadedAt} {
		if v != nil {
			out[k] = *v
		}
	}
	out["pin"] = s.Pin()
	return out
}

// SourceFromView is Source.from_view: a stored row's source.
func SourceFromView(v map[string]any) Source {
	str := func(k string) *string {
		if s, ok := v[k].(string); ok {
			return &s
		}
		return nil
	}
	kind, _ := v["kind"].(string)
	if kind == "" {
		kind = "cluster"
	}
	return Source{Kind: kind, Repo: str("repo"), Path: str("path"), Commit: str("commit"), Namespace: str("namespace"), XR: str("xr"), LoadedAt: str("loaded_at")}
}

func (s Source) key() string {
	f := func(p *string) string {
		if p == nil {
			return "\x00"
		}
		return "\x01" + *p
	}
	return strings.Join([]string{s.Kind, f(s.Repo), f(s.Path), f(s.Commit), f(s.Namespace), f(s.XR), f(s.LoadedAt)}, "\x02")
}

// Loaded is a pin that became a definition: the definition, its source, the XR's owner and the document it was read from.
type Loaded struct {
	Def    *Definition
	Source Source
	Owner  string
	Doc    map[string]any
}

// Name is the agent's name.
func (l *Loaded) Name() string { return l.Def.Name }

// Rejected is a pin that did not become a definition, and why. Reason is what is published (to the core and the read API): for a pin
// it is a rule id and a path, never a value out of the file (see PinPolicy.Detailed); Detail is the whole story, for the gateway's log.
type Rejected struct {
	Name   string
	Source Source
	Reason string
	Detail string
}

func (r Rejected) key() string { return r.Name + "\x03" + r.Source.key() + "\x03" + r.Reason }

var lineOf = regexp.MustCompile(`line (\d+)`)

// ReadPinnedFile reads one pinned file's text (agent_xrs._decide). It returns the document, or the reason it is not one. With detailed
// the reason is Python's ("<path> is not valid YAML: <the parser's first line>", "<path> is not a YAML mapping (an agent definition)")
// plus, for a file that reads but is not written strictly (value.go), "<path> is not acceptable: <what>"; without it (what a pin is
// published with) the reason is a rule id and, for a syntax error, a line: "<path>: DF-YAML at line 3", "<path>: DF-NOTMAP",
// "<path>: DF-STRICT". The second result is the full text in either case, for the log.
func ReadPinnedFile(path, text string, detailed bool) (doc *OMap, reason, detail string) {
	doc, err := ParseYAML([]byte(text))
	var se *StrictError
	var ye *YAMLSyntaxError
	switch {
	case err == nil:
		return doc, "", ""
	case errors.Is(err, ErrNotAMapping):
		detail = path + " is not a YAML mapping (an agent definition)"
		reason = path + ": DF-NOTMAP"
	case errors.As(err, &ye):
		first := ye.First
		if r := []rune(first); len(r) > 160 {
			first = string(r[:160])
		}
		detail = path + " is not valid YAML: " + first
		reason = path + ": DF-YAML"
		if m := lineOf.FindStringSubmatch(first); m != nil {
			reason += " at line " + m[1]
		}
	case errors.As(err, &se):
		detail = path + " is not acceptable: " + se.Msg
		reason = path + ": DF-STRICT"
	default:
		detail = path + " is not valid YAML: " + err.Error()
		reason = path + ": DF-YAML"
	}
	if detailed {
		reason = detail
	}
	return nil, reason, detail
}

// PinPolicy is what the Go gateway asks of a pinned definition beyond the schema and the Python rules. A definition arrives through a
// reviewed pull request on the tenants repository, but its author is an app team, and the app team's namespace is where the XR lives:
// so a pin is held to what an app team's agent may have, whoever the author is. Bundled definitions are the platform's own and are
// not held to it; neither is a candidate `document`.
//
// NewPinPolicy builds the one `serve` uses. The zero value enforces only the rules that need no configuration (lower-case,
// owner-literal repository patterns, non-blanket app patterns, the size and list caps) and publishes redacted reasons; the rules
// that need the platform's configuration are off until their field is set.
type PinPolicy struct {
	// Tools is the registry a pin's tools must be in; AllowTools, when set, is the allowlist of tools a pin may name (the default is
	// DefaultPinTools: the read tools and the propose-only lower tools; nothing that promotes, rolls back, syncs, applies, spawns or
	// opens an upper pull request unless the platform lists it).
	Tools      *tools.Registry
	AllowTools map[string]bool
	// MaxTier is the highest tier ceiling a pin may name: T0, T1 or T2 ("" is T2, which is the schema's own highest).
	MaxTier string
	// Owners are the GitHub owners a pin's repository patterns may name (GITHUB_OWNERS); nil skips the check.
	Owners []string
	// ImageGlobs are the runtime images a pin (and its sidecars) may name: the images the bundled definitions run and
	// RUNTIME_IMAGES_EXTRA, as whole-image globs; nil skips the check.
	ImageGlobs []string
	// BindApp ties a pin to the app its namespace belongs to (`app-<app>-cicd`): its repository, its repository patterns, its app
	// patterns and its event keys may name that app only. A pin in a namespace that is no app's is refused.
	BindApp bool
	// Namespaces are the namespaces the loader reads; with them a name has one owner (NameOwner).
	Namespaces []string
	// Names lists, for the agents whose names do not start with their app, the one namespace each may be pinned from (AGENT_XRS_NAMES).
	Names map[string]string
	// Detailed publishes Python's reasons, which quote the file (a value, a key, a name). Only the equivalence tests set it.
	Detailed bool
}

var tierRank = map[string]int{"T0": 0, "T1": 1, "T2": 2}

var onlyWildcards = regexp.MustCompile(`^\*+$`)

// Caps on a pinned definition (the schema has none on a list, and the core takes 4 MiB for the whole set): one tenant cannot fill the
// budget the others share.
const (
	MaxPinDocBytes        = 12 << 10 // the document as published
	MaxPinsPerNamespace   = 32
	maxList               = 32 // repos.allow, tools, denyPaths, fieldAllow, fieldDeny, models, apis.allow, glidepath/airframe apps, triggers
	maxPinFieldBytes      = 256
	NegativeCacheLifetime = 5 * time.Minute
)

// PolicyError is a pin refused by the policy. Path is where, in schema terms; Msg is the whole reason, for the log.
type PolicyError struct{ Path, Msg string }

func (e *PolicyError) Error() string { return "policy: " + e.Path + ": " + e.Msg }

func pol(path, format string, a ...any) error { return &PolicyError{path, fmt.Sprintf(format, a...)} }

// AppOfNamespace is the app a namespace belongs to: `app-<app>-cicd` (the tenants flow's), and nothing else. An app whose name makes the
// name-based mapping of its repositories ambiguous (`gitops-*`, `infra-*`: see RepoApp) has no pins: the gateway has no registry of an
// app's repositories to ask (the broker and the write tools bind an app to its repositories by their names, backends.AppOf, and the
// Backstage directory is S6), so the one way to be sure `gitops-infra-foo` is app foo's and not app infra-foo's is that no app is
// named infra-foo, and that `gitops-x` is app x's gitops repository and not the main repository of an app named gitops-x.
func AppOfNamespace(ns string) (string, bool) {
	m := nsApp.FindStringSubmatch(ns)
	if m == nil || strings.HasPrefix(m[1], "gitops-") || strings.HasPrefix(m[1], "infra-") {
		return "", false
	}
	return m[1], true
}

// AppOfName reports whether a pinned agent's name belongs to the app: it is the app's name or starts with it and a dash. This is
// decided from the XR alone, so two namespaces can never contend for one name and there is no state to hold, to lose or to move: an
// outage cannot hand a name to another tenant, nothing can be squatted, and an empty store at cutover changes nothing. Names that do
// not follow the convention (the agents that were live before it) are listed for their namespace in PinPolicy.Names.
func AppOfName(app, name string) bool { return name == app || strings.HasPrefix(name, app+"-") }

var nsApp = regexp.MustCompile(`^app-([a-z0-9]+(?:-[a-z0-9]+)*)-cicd$`)

// RepoApp is the app a repository belongs to by Glidepath's naming (<app>, gitops-<app>, gitops-infra-<app>), and whether it is a
// gitops repository: backends.AppOf, which the broker and every write tool bind an app to its repositories with, for names in the
// lower case a pin is held to (a test holds the two equal).
func RepoApp(name string) (app string, gitops bool) {
	for _, p := range []string{"gitops-infra-", "gitops-"} {
		if strings.HasPrefix(name, p) {
			return name[len(p):], true
		}
	}
	return name, false
}

// CheckSource is whether a pin of repo in namespace ns may be fetched at all: before any token is minted for it. The repository must
// be one of the namespace's own app (<app>, gitops-<app>, gitops-infra-<app>) under an owner this gateway may use.
func (p PinPolicy) CheckSource(ns, name, repo string) error {
	if !p.BindApp {
		return nil
	}
	app, ok := AppOfNamespace(ns)
	if !ok {
		return pol("namespace", "the namespace is not an app's (app-<app>-cicd)")
	}
	if err := p.checkName(app, ns, name); err != nil {
		return err
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return pol("spec.source.repo", "not owner/name")
	}
	if got, _ := RepoApp(name); got != app {
		return pol("spec.source.repo", "the repository is not this app's")
	}
	if p.Owners != nil && !slices.Contains(p.Owners, owner) {
		return pol("spec.source.repo", "the owner is not one this gateway may use")
	}
	return nil
}

// NameOwner is the one namespace that may load a name, decided from configuration alone: the namespace AGENT_XRS_NAMES lists for it, else the
// configured namespace whose app is the LONGEST app that equals the name or is a dash-terminated prefix of it. Another namespace's XR of
// the name is refused alone, so prefix-related apps (gate, gate-api) cannot contend for a name and take each other's agents away. With no
// namespaces configured (the zero policy) the owner is whichever app the name belongs to, which is only the namespace's own check.
func (p PinPolicy) NameOwner(name string) (string, bool) {
	if ns, ok := p.Names[name]; ok {
		return ns, true
	}
	best, bestApp := "", ""
	for _, ns := range p.Namespaces {
		app, ok := AppOfNamespace(ns)
		if ok && AppOfName(app, name) && len(app) > len(bestApp) {
			best, bestApp = ns, app
		}
	}
	return best, best != ""
}

func (p PinPolicy) checkName(app, ns, name string) error {
	if p.Namespaces == nil {
		if AppOfName(app, name) || p.Names[name] == ns {
			return nil
		}
	} else if owner, ok := p.NameOwner(name); ok && owner == ns {
		return nil
	}
	return pol("metadata.name", "an agent's name belongs to one namespace: the one listed for it, else the app that is the longest match of its start (%s, %s-... when it is this namespace's)", app, app)
}

// Check is nil for a definition a pin of this source may be, else the reason it may not.
func (p PinPolicy) Check(d *Definition, src Source) error {
	max := p.MaxTier
	if max == "" {
		max = "T2"
	}
	app := ""
	if p.BindApp {
		var ok bool
		if app, ok = AppOfNamespace(strOr(src.Namespace, "")); !ok {
			return pol("namespace", "the namespace is not an app's (app-<app>-cicd)")
		}
	}
	if p.BindApp {
		if err := p.checkName(app, strOr(src.Namespace, ""), d.Name); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(d.Doc)
	if len(raw) > MaxPinDocBytes {
		return pol("<root>", "the definition is larger than %d bytes", MaxPinDocBytes)
	}
	prof, _ := d.Doc["profile"].(map[string]any)
	repos, _ := prof["repos"].(map[string]any)
	apis, _ := prof["apis"].(map[string]any)
	rt, _ := d.Doc["runtime"].(map[string]any)
	lists := map[string][]any{}
	for path, v := range map[string]any{"profile/tools": prof["tools"], "profile/models": prof["models"], "profile/repos/allow": repos["allow"],
		"profile/repos/denyPaths": repos["denyPaths"], "profile/repos/fieldAllow": repos["fieldAllow"], "profile/repos/fieldDeny": repos["fieldDeny"],
		"profile/apis/allow": apis["allow"], "triggers": d.Doc["triggers"]} {
		l, _ := v.([]any)
		lists[path] = l
	}
	for path, l := range lists {
		if len(l) > maxList {
			return pol(path, "more than %d entries", maxList)
		}
		for _, e := range l {
			if s, ok := e.(string); ok && len(s) > maxPinFieldBytes {
				return pol(path, "an entry longer than %d bytes", maxPinFieldBytes)
			}
		}
	}
	if tierRank[d.Limits.TierCeiling] > tierRank[max] {
		return pol("profile/tierCeiling", "%s is above %s, the highest ceiling a pinned agent may have", d.Limits.TierCeiling, max)
	}
	if p.BindApp {
		if err := checkFields(d, prof, apis, app); err != nil {
			return err
		}
	}
	if d.IdentityType == "delegated" && d.ServiceAccount != nil {
		return pol("identity/serviceAccount", "a delegated pin acts as the person who opens it and names no service account")
	}
	upper := false
	for _, t := range toolList(d) {
		if p.Tools != nil {
			if _, ok := p.Tools.Get(t); !ok {
				return pol("profile/tools", "%s is not a tool this gateway has", pyrepr.Str(t))
			}
		}
		if p.AllowTools != nil && !p.AllowTools[t] {
			return pol("profile/tools", "%s is not a tool a pinned agent may have", pyrepr.Str(t))
		}
		upper = upper || t == "repo.pr.open_upper"
	}
	for _, pat := range d.ReposAllow {
		owner, repo, _ := strings.Cut(pat, "/")
		switch {
		case pat != strings.ToLower(pat):
			return pol("profile/repos/allow", "%s: write the pattern in lower case (repository names match without regard to case)", pyrepr.Str(pat))
		case strings.Contains(owner, "*"):
			return pol("profile/repos/allow", "%s: the owner must be written out, not matched with a wildcard", pyrepr.Str(pat))
		case onlyWildcards.MatchString(repo):
			return pol("profile/repos/allow", "%s: the repository part may not be only wildcards (name the repositories, or a prefix such as \"gate-*\")", pyrepr.Str(pat))
		}
		if !p.BindApp {
			continue
		}
		if p.Owners != nil && !slices.Contains(p.Owners, owner) {
			return pol("profile/repos/allow", "%s: the owner is not one this gateway may use", pyrepr.Str(pat))
		}
		got, gitops := RepoApp(repo)
		if strings.Contains(repo, "*") || got != app {
			return pol("profile/repos/allow", "%s: a pin may name its own app's repositories only, written out (%s, gitops-%s)", pyrepr.Str(pat), app, app)
		}
		if gitops && !upper {
			return pol("profile/repos/allow", "%s: a gitops repository needs the upper pull request tool, which this pin does not have", pyrepr.Str(pat))
		}
	}
	for _, section := range []string{"glidepath", "airframe"} {
		sec, _ := prof[section].(map[string]any)
		apps, _ := sec["apps"].([]any)
		if len(apps) > maxList {
			return pol("profile/"+section+"/apps", "more than %d entries", maxList)
		}
		for _, a := range apps {
			s, _ := a.(string)
			if onlyWildcards.MatchString(s) {
				return pol("profile/"+section+"/apps", "%s: an app pattern that is only wildcards names every app", pyrepr.Str(s))
			}
			if p.BindApp && s != app {
				return pol("profile/"+section+"/apps", "%s: a pin may name its own app only", pyrepr.Str(s))
			}
		}
	}
	if p.BindApp {
		ts, _ := d.Doc["triggers"].([]any)
		for _, e := range ts {
			t, _ := e.(map[string]any)
			if t["type"] != "event" && t["type"] != "alert" {
				continue
			}
			key, _ := t["bindingKey"].(string)
			if t["type"] != "event" || t["source"] != "glidepath" || !strings.HasPrefix(key, app+".") {
				return pol("triggers", "an event trigger of a pin is a Glidepath event whose key starts with its own app (%s.)", app)
			}
		}
	}
	if p.ImageGlobs != nil {
		imgs := []string{}
		if s, ok := rt["image"].(string); ok {
			imgs = append(imgs, s)
		}
		sc, _ := rt["sidecars"].([]any)
		for _, e := range sc {
			m, _ := e.(map[string]any)
			if s, ok := m["image"].(string); ok {
				imgs = append(imgs, s)
			}
		}
		for _, img := range imgs {
			ok := false
			for _, g := range p.ImageGlobs {
				ok = ok || globMatch(g, img)
			}
			if !ok {
				return pol("runtime/image", "%s is not an image this deployment lists for pinned agents", pyrepr.Str(img))
			}
		}
	}
	return nil
}

// What a pin may set that the schema leaves open, and the platform's maxima for it. A pin comes from an app team: the values are
// bounded here, not left to whoever writes the file. (A workload pin's service account, autopilot-runs/<its own name>, is safe only
// because a pinned agent's name belongs to its app, AppOfName: another app cannot name this app's identity.)
const (
	maxTTLMinutes  = 120
	maxToolCalls   = 500
	maxGithubCalls = 200
	maxOpenPRs     = 5
	maxModelTokens = 2_000_000
	maxChildren    = 5
)

func checkFields(d *Definition, prof, apis map[string]any, app string) error {
	l := d.Limits
	for _, c := range []struct {
		name     string
		v, limit int64
	}{{"ttlMinutes", l.TTLMinutes, maxTTLMinutes}, {"toolCalls", l.ToolCalls, maxToolCalls}, {"githubCalls", l.GithubCalls, maxGithubCalls},
		{"openPrs", l.OpenPRs, maxOpenPRs}, {"modelTokens", l.ModelTokens, maxModelTokens}, {"children", l.Children, maxChildren}} {
		if c.v > c.limit {
			return pol("profile/limits/"+c.name, "%d is above %d, the most a pinned agent may have", c.v, c.limit)
		}
	}
	if l.Network == "allowlist" {
		return pol("profile/network", "a pinned agent may not have an egress allowlist")
	}
	if l.Compute != "small" && l.Compute != "medium" {
		return pol("profile/compute", "a pinned agent runs on small or medium compute")
	}
	if air, _ := prof["airframe"].(map[string]any); air["proposeFlight"] == true {
		return pol("profile/airframe/proposeFlight", "a pinned agent may not open the Flight half of an AppSpec")
	}
	for _, a := range stringList(apis["allow"]) {
		if a != app {
			return pol("profile/apis/allow", "%s: a pin may name its own app's API only", pyrepr.Str(a))
		}
	}
	return nil
}

func stringList(v any) []string {
	l, _ := v.([]any)
	var out []string
	for _, e := range l {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// globMatch is a whole-string match where `*` stands for any run of characters (a glob with no wildcard is an exact image).
func globMatch(glob, s string) bool {
	parts := strings.Split(glob, "*")
	if len(parts) == 1 {
		return glob == s
	}
	if !strings.HasPrefix(s, parts[0]) || !strings.HasSuffix(s, parts[len(parts)-1]) || len(s) < len(parts[0])+len(parts[len(parts)-1]) {
		return false
	}
	s = s[len(parts[0]) : len(s)-len(parts[len(parts)-1])]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return true
}

// DefaultPinTools are the tools a pin may name unless the platform says otherwise: every read tool (tier 0) and the propose-only ones
// that open a pull request on a lower environment, ask a person, or keep an artifact. Not in it: promote, rollback, sync, apply, spawn,
// close, re-run, cancel, xr.request and the upper pull request tool.
func DefaultPinTools(reg *tools.Registry) map[string]bool {
	out := map[string]bool{"repo.pr.open": true, "glidepath.cicd.propose": true, "human.request": true, "artifact.put": true}
	for _, n := range reg.Names() {
		if sp, ok := reg.Get(n); ok && int(sp.Tier) == 0 {
			out[n] = true
		}
	}
	return out
}

func toolList(d *Definition) []string {
	prof, _ := d.Doc["profile"].(map[string]any)
	ts, _ := prof["tools"].([]any)
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		if s, ok := t.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// FromPin decides one pin (engine/definitions.py from_pin): src.XR is the XR's name, owner its spec.owner, reserved the bundled
// definitions' names. A bad document is a Rejected with a reason, never a panic or an error. With policy.Detailed the reason is
// Python's, which quotes the file; otherwise it is a rule id and a path (DF-SCHEMA profile/limits/ttlMinutes), and the whole text is
// in Detail, for the log.
func FromPin(doc *OMap, src Source, owner string, reserved map[string]*Definition, baseline *tools.Registry, policy PinPolicy) (*Loaded, *Rejected) {
	name := strOr(src.XR, "")
	if name == "" {
		name = "?"
	}
	rej := func(generic, format string, a ...any) (*Loaded, *Rejected) {
		detail := fmt.Sprintf(format, a...)
		reason := generic
		if policy.Detailed {
			reason = detail
		}
		return nil, &Rejected{Name: name, Source: src, Reason: reason, Detail: detail}
	}
	d, err := ParseDoc(doc, baseline)
	if err != nil {
		path := "<root>"
		var de *DefinitionError
		if errors.As(err, &de) && de.Path != "" {
			path = de.Path
		}
		return rej("DF-SCHEMA: "+path, "schema: %s", err.Error())
	}
	if d.Name != name {
		return rej("DF-NAME: the file's name is not the XR's name", "the file's name is %s; a pin named %s must pin a definition named %s", pyrepr.Str(d.Name), pyrepr.Str(name), pyrepr.Str(name))
	}
	if d.Owner == nil || *d.Owner != owner {
		shown := "None"
		if d.Owner != nil {
			shown = pyrepr.Str(*d.Owner)
		}
		return rej("DF-OWNER: the file's owner is not the XR's owner", "the file's owner is %s; the pin's owner is %s: they must agree", shown, pyrepr.Str(owner))
	}
	if d.IdentityType == "workload" && (d.ServiceAccount == nil || *d.ServiceAccount != RunIdentityNamespace+"/"+d.Name) {
		shown := "None"
		if d.ServiceAccount != nil {
			shown = pyrepr.Str(*d.ServiceAccount)
		}
		return rej("DF-IDENTITY: the workload identity is not "+RunIdentityNamespace+"/"+d.Name, "a pinned agent's workload identity is %s/%s, not %s", RunIdentityNamespace, d.Name, shown)
	}
	if _, bundled := reserved[name]; bundled {
		return rej("DF-BUNDLED: "+pyrepr.Str(name)+" is a bundled platform definition; a pin cannot replace it", "%s is a bundled platform definition; a pin cannot replace it", pyrepr.Str(name))
	}
	if err := policy.Check(d, src); err != nil {
		path := "<root>"
		var pe *PolicyError
		if errors.As(err, &pe) {
			path = pe.Path
		}
		return rej(policyReason(path), "%s", err.Error())
	}
	return &Loaded{Def: d, Source: src, Owner: *d.Owner, Doc: d.Doc}, nil
}

// policyReason is the published reason of a policy refusal: a rule id and a place; DF-NAME for a name that is another namespace's.
func policyReason(path string) string {
	if path == "metadata.name" {
		return "DF-NAME: the agent's name belongs to another namespace"
	}
	return "DF-POLICY: " + path
}
