package definitions

import (
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/jfillman/autopilot/gateway/internal/pyrepr"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// Changes is what one ReplaceCluster changed (engine/definitions.py Changes). Empty means the poll found the same set as before.
type Changes struct {
	Added, Changed, Removed []string
	Rejected                []Rejected // rejections that were not there before
	Cleared                 []Rejected // rejections that are gone (fixed, or the pin removed)
}

// Any is bool(changes).
func (c Changes) Any() bool {
	return len(c.Added)+len(c.Changed)+len(c.Removed)+len(c.Rejected)+len(c.Cleared) > 0
}

// Set is the live definition set: the bundled definitions, then the cluster ones, by name. Bundled names come first and can never
// be shadowed. A reader takes Snapshot, a map that is never changed after it is handed out; ReplaceCluster swaps the whole
// cluster set at once under a lock, so a reader never sees a half-applied set.
type Set struct {
	mu       sync.Mutex
	bundled  map[string]*Definition
	cluster  map[string]*Loaded
	rejected []Rejected
	all      map[string]*Definition
	version  int
}

// NewSet is a set of the bundled definitions and no pins.
func NewSet(bundled map[string]*Definition) *Set {
	b := make(map[string]*Definition, len(bundled))
	for k, v := range bundled {
		b[k] = v
	}
	s := &Set{bundled: b, cluster: map[string]*Loaded{}}
	s.all = s.merge()
	return s
}

func (s *Set) merge() map[string]*Definition {
	all := make(map[string]*Definition, len(s.bundled)+len(s.cluster))
	for n, l := range s.cluster {
		all[n] = l.Def
	}
	for n, d := range s.bundled { // bundled names win
		all[n] = d
	}
	return all
}

// Snapshot is the definitions by name as they stand. The map is the caller's to read and never to write.
func (s *Set) Snapshot() map[string]*Definition {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.all
}

// Version is bumped on every change.
func (s *Set) Version() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// Bundled are the platform's definitions (the image's agents/).
func (s *Set) Bundled() map[string]*Definition { return s.bundled }

// Source is where a name came from, nil when there is no such definition.
func (s *Set) Source(name string) *Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.cluster[name]; ok {
		src := l.Source
		return &src
	}
	if _, ok := s.bundled[name]; ok {
		return &Source{Kind: "bundled"}
	}
	return nil
}

// Loaded is the loaded pin of a name.
func (s *Set) Loaded(name string) *Loaded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cluster[name]
}

// ClusterNames are the loaded pins' names.
func (s *Set) ClusterNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.cluster))
	for n := range s.cluster {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ClusterSet is the cluster set as it stands, for the core: every loaded pin by name and every rejection, taken together under
// one lock with the version they belong to.
func (s *Set) ClusterSet() (loaded []*Loaded, rejected []Rejected, version int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.cluster {
		loaded = append(loaded, l)
	}
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].Name() < loaded[j].Name() })
	return loaded, append([]Rejected(nil), s.rejected...), s.version
}

// BundledDoc is one bundled definition as the core is told of it.
type BundledDoc struct {
	Name string
	Doc  map[string]any
}

// BundledSet is the bundled definitions (name, document) by name, those that have a document (Definitions.bundled_set).
func (s *Set) BundledSet() []BundledDoc {
	names := make([]string, 0, len(s.bundled))
	for n := range s.bundled {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []BundledDoc
	for _, n := range names {
		if d := s.bundled[n]; len(d.Doc) > 0 {
			out = append(out, BundledDoc{n, d.Doc})
		}
	}
	return out
}

// ReplaceCluster makes loaded + rejected the cluster set. A name that appears twice in loaded (two XRs, two namespaces, one agent
// name) is a rejection of both: Clearance will not guess; a pin of a bundled name is rejected. It returns what changed.
func (s *Set) ReplaceCluster(loaded []*Loaded, rejected []Rejected) Changes {
	s.mu.Lock()
	defer s.mu.Unlock()
	byName := map[string]*Loaded{}
	cands := map[string][]*Loaded{}
	for _, l := range loaded {
		cands[l.Name()] = append(cands[l.Name()], l)
		byName[l.Name()] = l
	}
	rej := append([]Rejected(nil), rejected...)
	for n, ls := range cands {
		if len(ls) < 2 {
			continue
		}
		// An agent name is held by the namespace that has it. A pin of the same name from another namespace is the one that is
		// refused, so nobody can take another team's agent away by creating an XR of its name. A name nobody holds yet that two
		// namespaces ask for at once goes to neither (Python's rule for every duplicate), and the holder's own pin that is gone or
		// broken lets go of the name.
		var holder *Loaded
		if cur, ok := s.cluster[n]; ok {
			for _, l := range ls {
				if samePtr(l.Source.Namespace, cur.Source.Namespace) {
					holder = l
				}
			}
		}
		for _, l := range ls {
			switch {
			case holder == nil:
				rej = append(rej, Rejected{n, l.Source, fmt.Sprintf("two pins name the agent %s; neither is loaded", pyrepr.Str(n)), ""})
			case l != holder:
				rej = append(rej, Rejected{n, l.Source, fmt.Sprintf("DF-NAME: another namespace holds the agent name %s", pyrepr.Str(n)), ""})
			}
		}
		if holder != nil {
			byName[n] = holder
		} else {
			delete(byName, n)
		}
	}
	for n, l := range byName {
		if _, ok := s.bundled[n]; ok {
			rej = append(rej, Rejected{n, l.Source, fmt.Sprintf("%s is a bundled platform definition; a pin cannot replace it", pyrepr.Str(n)), ""})
			delete(byName, n)
		}
	}
	// sorted(dict.fromkeys(rej), key=(name, reason)): the first of equal rejections, in order of name and reason.
	seen := map[string]bool{}
	var rejT []Rejected
	for _, r := range rej {
		if !seen[r.key()] {
			seen[r.key()] = true
			rejT = append(rejT, r)
		}
	}
	sort.SliceStable(rejT, func(i, j int) bool {
		if rejT[i].Name != rejT[j].Name {
			return rejT[i].Name < rejT[j].Name
		}
		return rejT[i].Reason < rejT[j].Reason
	})

	var ch Changes
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		l := byName[n]
		o, ok := s.cluster[n]
		switch {
		case !ok:
			ch.Added = append(ch.Added, n)
		case !samePtr(o.Source.Commit, l.Source.Commit) || !reflect.DeepEqual(o.Doc, l.Doc):
			ch.Changed = append(ch.Changed, n)
		}
	}
	var oldNames []string
	for n := range s.cluster {
		oldNames = append(oldNames, n)
	}
	sort.Strings(oldNames)
	for _, n := range oldNames {
		if _, ok := byName[n]; !ok {
			ch.Removed = append(ch.Removed, n)
		}
	}
	old := map[string]bool{}
	for _, r := range s.rejected {
		old[r.key()] = true
	}
	now := map[string]bool{}
	for _, r := range rejT {
		now[r.key()] = true
		if !old[r.key()] {
			ch.Rejected = append(ch.Rejected, r)
		}
	}
	for _, r := range s.rejected {
		if !now[r.key()] {
			ch.Cleared = append(ch.Cleared, r)
		}
	}
	if ch.Any() {
		s.cluster, s.rejected = byName, rejT
		s.all = s.merge()
		s.version++
	}
	return ch
}

// Row is one row of the store (gateway_definitions): a loaded pin with its document, or a rejected one with its reason.
type Row = map[string]any

// Records is the cluster set as rows for the store: loaded ones with their document, rejected ones with their reason. Bundled
// definitions are not rows; every process has them from its image.
func (s *Set) Records() []Row {
	loaded, rejected, _ := s.ClusterSet()
	out := []Row{}
	for _, l := range loaded {
		out = append(out, Row{"name": l.Name(), "state": "loaded", "source": l.Source.View(), "doc": l.Doc})
	}
	for _, r := range rejected {
		out = append(out, Row{"name": r.Name, "state": "rejected", "source": r.Source.View(), "reason": r.Reason})
	}
	return out
}

// FromRecords rebuilds a cluster set from the store's rows (Definitions.apply_records): a row whose document no longer parses
// (a schema change between images, a row written by something else) is a rejection that says so, not a silent drop. The pin
// policy and the Python pin rules are applied again: the store is state the gateway wrote, not something it trusts blindly.
func FromRecords(rows []Row, reserved map[string]*Definition, baseline *tools.Registry, policy PinPolicy) ([]*Loaded, []Rejected) {
	var loaded []*Loaded
	var rejected []Rejected
	for _, r := range rows {
		name, _ := r["name"].(string)
		sv, _ := r["source"].(map[string]any)
		src := SourceFromView(sv)
		if r["state"] != "loaded" {
			reason, _ := r["reason"].(string)
			if reason == "" {
				reason = "rejected"
			}
			rejected = append(rejected, Rejected{name, src, reason, ""})
			continue
		}
		doc, ok := FromPlain(r["doc"]).(*OMap)
		if !ok {
			rejected = append(rejected, Rejected{name, src, "stored definition no longer parses: the document is not an object", ""})
			continue
		}
		owner, _ := r["owner"].(string)
		if o, ok := doc.M["owner"].(string); ok {
			owner = o
		}
		l, rej := FromPin(doc, src, owner, reserved, baseline, policy)
		if rej != nil {
			rejected = append(rejected, Rejected{name, src, "stored definition no longer parses: " + rej.Reason, rej.Detail})
			continue
		}
		loaded = append(loaded, l)
	}
	return loaded, rejected
}

func samePtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
