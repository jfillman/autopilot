package definitions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/jfillman/clearance/client"
)

// MaxPublishBytes is the largest set the core takes in one PUT /v1/definitions (4 MiB): a larger one is not sent, because the core
// would refuse it and the refusal would hide which part was too big.
const MaxPublishBytes = 4 << 20

// Publisher is the policy core, as far as publishing the set goes (core.Core.PublishDefinitions).
type Publisher interface {
	PublishDefinitions(ctx context.Context, pins []client.PinnedDefinition, rejected []client.RejectedPin, bundled []client.ProvidedDefinition) (*client.DefinitionsResult, bool)
}

// RowStore is where the cluster set is kept between restarts (gateway_definitions): replaced whole in one transaction.
type RowStore interface {
	ReplaceDefinitions(ctx context.Context, rows []Row) error
}

// Manager is the gateway's side of the definition set (Gateway.apply_cluster_definitions and _publish_definitions): it applies a
// loader's result to the live set, keeps it, hands the gateway the new snapshot, and publishes the whole set to the core.
//
// The publish is the authority the core uses, so it is built to be safe to repeat and never partial: the request is made from one
// consistent snapshot of the bundled definitions, the pins and the rejections (and the version they belong to); one publish runs at a
// time; a publish the core did not take is made again whole on the next call, because the set is state and there is nothing to
// queue; and a set that changed while a publish was running is published again, not marked as published.
type Manager struct {
	Set   *Set
	Store RowStore  // nil: the set is not kept across restarts
	Core  Publisher // nil: nothing is published
	// SetDefs hands the gateway the new snapshot of every definition by name; a running session resolves its definition by name on
	// every call, so a re-pin to a tighter definition applies to runs in flight.
	SetDefs func(map[string]*Definition)
	Log     *slog.Logger
	// MaxBytes is the publish budget (MaxPublishBytes unless a test lowers it); Namespaces how many namespaces the loader reads (it
	// divides the budget); NamespaceShare, when set, is one namespace's share outright.
	MaxBytes       int
	Namespaces     int
	NamespaceShare int

	mu          sync.Mutex // the set and the unpublished flag
	pubMu       sync.Mutex // one publish at a time
	unpublished bool
	last        Published
}

// Published is what the last successful publish told the core.
type Published struct {
	Pins, Rejected, Bundled int
	Digest                  string // SHA-256 of the request's content (pins, rejections, bundled definitions)
	Bytes                   int
}

// NewManager is a manager of set. The core has not been given the set yet: the first publish happens whatever the loader finds (the
// core keeps its set across a gateway restart, and a set that emptied while the gateway was down must still reach it).
func NewManager(set *Set, store RowStore, c Publisher, setDefs func(map[string]*Definition), log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{Set: set, Store: store, Core: c, SetDefs: setDefs, Log: log, unpublished: true}
}

// ApplyCluster makes loaded + rejected the cluster set, keeps it in the store, gives the gateway the new snapshot and publishes the
// whole set. It returns what changed.
func (m *Manager) ApplyCluster(ctx context.Context, loaded []*Loaded, rejected []Rejected) Changes {
	m.mu.Lock()
	loaded, rejected = m.fit(loaded, rejected)
	ch := m.Set.ReplaceCluster(loaded, rejected)
	if ch.Any() {
		if m.SetDefs != nil {
			m.SetDefs(m.Set.Snapshot())
		}
		if m.Store != nil {
			if err := m.Store.ReplaceDefinitions(ctx, m.Set.Records()); err != nil {
				// The live set is what runs; the store is for a restart, which would start from the last set it holds.
				m.Log.Error("the definition set could not be kept in the gateway database: a restart will start from the last set that was kept", "err", err.Error())
			}
		}
		m.unpublished = true
	}
	m.mu.Unlock()
	m.Publish(ctx)
	return ch
}

// fit is the set that goes in: loaded and rejected as they are when every namespace is within its own share of the publish budget; a
// namespace over its share loses its own rows (its pins, and its rejections, which are replaced by one row that says so) and nothing
// else changes. The share is fixed by configuration (the budget less the bundled definitions, over the namespaces the loader is
// told to read), not by what the others do, so nothing a namespace writes can make another namespace's pins go, or the bundled
// set stay unpublished. It runs BEFORE the live set is replaced, so the gateway never runs a set the core cannot be given.
func (m *Manager) fit(loaded []*Loaded, rejected []Rejected) ([]*Loaded, []Rejected) {
	budget := m.MaxBytes
	if budget <= 0 {
		budget = MaxPublishBytes
	}
	size := func(v any) int { b, _ := json.Marshal(v); return len(b) }
	share := m.NamespaceShare
	if share <= 0 {
		n := m.Namespaces
		if n <= 0 {
			n = 20
		}
		if share = (budget - size(m.Set.BundledSet()) - 4096) / n; share < 1024 {
			share = 1024
		}
	}
	per := map[string]int{}
	for _, l := range loaded {
		per[strOr(l.Source.Namespace, "")] += size(l.Doc) + size(l.Source.View()) + len(l.Owner) + 64
	}
	for _, r := range rejected {
		per[strOr(r.Source.Namespace, "")] += size(r) + 64
	}
	over := map[string]bool{}
	for ns, n := range per {
		if n > share {
			over[ns] = true
			m.Log.Error("the rows of one namespace do not fit its share of the publish budget: none of its pins is loaded", "namespace", ns, "bytes", n, "share", share)
		}
	}
	if len(over) == 0 {
		return loaded, rejected
	}
	var keep []*Loaded
	var rej []Rejected
	for _, l := range loaded {
		if !over[strOr(l.Source.Namespace, "")] {
			keep = append(keep, l)
		}
	}
	for _, r := range rejected {
		if !over[strOr(r.Source.Namespace, "")] {
			rej = append(rej, r)
		}
	}
	for ns := range over {
		nsp := ns
		rej = append(rej, Rejected{Name: "?", Source: Source{Kind: "cluster", Namespace: &nsp},
			Reason: "DF-BUDGET: this namespace's pins and rejections together do not fit its share of what the core takes in one set; none of its pins is loaded"})
	}
	return keep, rej
}

// Publish publishes the current set if the core does not have it yet, and reports whether the core now has it. One publish at a
// time, of the set as it is when the publish starts, so a later set is never overwritten by an earlier one.
func (m *Manager) Publish(ctx context.Context) bool {
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	m.mu.Lock()
	if !m.unpublished {
		m.mu.Unlock()
		return true
	}
	loaded, rejected, version := m.Set.ClusterSet()
	bundled := m.Set.BundledSet()
	m.mu.Unlock()
	if m.Core == nil {
		return false
	}
	pins := make([]client.PinnedDefinition, 0, len(loaded))
	for _, l := range loaded {
		pins = append(pins, client.PinnedDefinition{Source: pinSource(l.Source), Owner: l.Owner, Doc: l.Doc})
	}
	var rej []client.RejectedPin
	for _, r := range rejected {
		rej = append(rej, client.RejectedPin{Name: r.Name, Source: pinSource(r.Source), Reason: r.Reason})
	}
	provided := make([]client.ProvidedDefinition, 0, len(bundled))
	for _, b := range bundled {
		provided = append(provided, client.ProvidedDefinition{Name: b.Name, Doc: b.Doc})
	}
	body, err := json.Marshal(struct {
		Pins     []client.PinnedDefinition
		Rejected []client.RejectedPin
		Bundled  []client.ProvidedDefinition
	}{pins, rej, provided})
	if err != nil {
		m.Log.Error("the definition set cannot be encoded: not published", "err", err.Error())
		return false
	}
	if len(body) > MaxPublishBytes {
		m.Log.Error("the definition set is larger than the core takes in one publish: not published; the core keeps the set it has",
			"bytes", len(body), "limit", MaxPublishBytes, "pins", len(pins))
		return false
	}
	sum := sha256.Sum256(body)
	res, ok := m.Core.PublishDefinitions(ctx, pins, rej, provided)
	if !ok {
		return false
	}
	m.mu.Lock()
	if m.Set.Version() == version {
		m.unpublished = false
	}
	m.last = Published{Pins: len(pins), Rejected: len(rej), Bundled: len(provided), Digest: hex.EncodeToString(sum[:]), Bytes: len(body)}
	m.mu.Unlock()
	attrs := []any{"pins", len(pins), "rejected", len(rej), "bundled", len(provided), "digest", hex.EncodeToString(sum[:]), "bytes", len(body)}
	if res != nil {
		attrs = append(attrs, "added", len(res.Added), "changed", len(res.Changed), "removed", len(res.Removed),
			"core_rejected", len(res.Rejected), "core_cleared", len(res.Cleared), "core_pinned", res.Cluster, "core_bundled", res.Provided)
	}
	m.Log.Info("definitions published", attrs...)
	return true
}

// LastPublished is what the last successful publish carried (the zero value before the first).
func (m *Manager) LastPublished() Published {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

// Unpublished reports whether the core is still to be given the current set.
func (m *Manager) Unpublished() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unpublished
}

func pinSource(s Source) client.PinSource {
	return client.PinSource{Repo: s.Repo, Path: s.Path, Commit: s.Commit, Namespace: s.Namespace, XR: s.XR, LoadedAt: s.LoadedAt}
}
