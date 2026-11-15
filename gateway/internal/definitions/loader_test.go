package definitions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/clearance/client"
)

// What the Go loader does that Python's does not, and the properties the publish must have whatever Python does. Each test names the
// failure it is about; the equivalence with Python is the other file's.

const (
	repo   = "acme/gate-api"
	commit = "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b"
	owner  = "group:default/team-gate"
	image  = "ghcr.io/acme/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func defDoc(name string, over map[string]any) map[string]any {
	d := map[string]any{
		"name": name, "description": "d", "owner": owner, "kind": "task",
		"identity": map[string]any{"type": "workload", "serviceAccount": "autopilot-runs/" + name},
		"profile": map[string]any{"tierCeiling": "T1", "tools": []any{"repo.file.get"}, "repos": map[string]any{"allow": []any{"acme/gate-api"}},
			"limits": map[string]any{"ttlMinutes": 20, "toolCalls": 1, "githubCalls": 1, "openPrs": 0, "modelTokens": 1, "children": 0}},
		"runtime": map[string]any{"image": image},
	}
	for k, v := range over {
		d[k] = v
	}
	return d
}

func xrOf(name, ns string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{"owner": owner, "source": map[string]any{"repo": repo, "path": "agents/" + name + ".yaml", "commit": commit}}}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// rig is a poller over fakes, with a publisher that records and can be told to fail or to block.
type rig struct {
	t      *testing.T
	set    *definitions.Set
	mgr    *definitions.Manager
	p      *definitions.Poller
	lister *fakeLister
	files  *fakeFiles
	pub    *fakePublisher
	store  *memStore
	defs   map[string]*definitions.Definition
	now    time.Time
	logs   *bytes.Buffer
}

type fakePublisher struct {
	mu      sync.Mutex
	pins    [][]client.PinnedDefinition
	rej     [][]client.RejectedPin
	bundled [][]client.ProvidedDefinition
	fail    bool
	gate    chan struct{} // when set, a publish waits for it
	entered chan struct{}
}

func (f *fakePublisher) PublishDefinitions(_ context.Context, pins []client.PinnedDefinition, rej []client.RejectedPin, bundled []client.ProvidedDefinition) (*client.DefinitionsResult, bool) {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pins, f.rej, f.bundled = append(f.pins, pins), append(f.rej, rej), append(f.bundled, bundled)
	if f.fail {
		return nil, false
	}
	return &client.DefinitionsResult{Cluster: len(pins), Provided: len(bundled)}, true
}

func (f *fakePublisher) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.pins) }

func newRig(t *testing.T, namespaces ...string) *rig {
	t.Helper()
	if len(namespaces) == 0 {
		namespaces = []string{"app-gate-api-cicd"}
	}
	r := &rig{t: t, lister: &fakeLister{}, files: &fakeFiles{files: map[string]map[string]any{}}, pub: &fakePublisher{}, store: &memStore{}, logs: &bytes.Buffer{}}
	b := bundledSet(t, map[string]any{"researcher": defDoc("researcher", map[string]any{"owner": "group:default/team-platform"})})
	r.set = definitions.NewSet(b)
	r.defs = r.set.Snapshot()
	log := slog.New(slog.NewTextHandler(r.logs, nil))
	r.mgr = definitions.NewManager(r.set, r.store, r.pub, func(m map[string]*definitions.Definition) { r.defs = m }, log)
	r.p = definitions.NewPoller()
	r.p.Namespaces, r.p.Lister, r.p.Files, r.p.Sink, r.p.Set, r.p.Baseline = namespaces, r.lister, r.files, r.mgr, r.set, tools.Default()
	r.p.Policy = definitions.PinPolicy{Tools: tools.Default(), Detailed: true}
	r.now = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	r.p.Now = func() time.Time { return r.now }
	r.p.Log = log
	return r
}

func (r *rig) pin(name, ns string, doc map[string]any) {
	r.lister.items = append(r.lister.items, xrOf(name, ns))
	r.files.files[repo+"|agents/"+name+".yaml|"+commit] = map[string]any{"doc": doc}
}

func (r *rig) poll() definitions.Changes {
	r.now = r.now.Add(time.Minute)
	return r.p.Poll(context.Background())
}

// A namespace that is not listed is never read: an Agent XR there, whoever made it, defines no agent and is not even listed.
func TestAnAgentXRInANamespaceThatWasNotNamedIsNeverRead(t *testing.T) {
	r := newRig(t, "app-gate-api-cicd")
	r.pin("good", "app-gate-api-cicd", defDoc("good", nil))
	r.pin("sneaky", "agent-r-0a1b2c", defDoc("sneaky", nil))
	r.pin("other", "app-other-cicd", defDoc("other", nil))
	ch := r.poll()
	if len(ch.Added) != 1 || ch.Added[0] != "good" || r.defs["sneaky"] != nil || r.defs["other"] != nil {
		t.Fatalf("only the listed namespace's XR may define an agent: %+v", ch)
	}
	for _, f := range r.files.calls {
		if strings.Contains(f, "sneaky") || strings.Contains(f, "other") {
			t.Fatalf("a file was fetched for an XR of a namespace that was not named: %v", r.files.calls)
		}
	}
	if got := r.lister.listed(); len(got) != 1 || got[0] != "app-gate-api-cicd" {
		t.Fatalf("exactly the named namespaces are listed, one namespaced list each: %v", got)
	}
}

// An item whose own namespace is not the one that was listed (a lister that was fooled, a proxy that merged lists) is dropped.
func TestAnItemThatIsNotInTheNamespaceThatWasListedIsDropped(t *testing.T) {
	r := newRig(t, "app-gate-api-cicd")
	r.lister.fixed = map[string][]map[string]any{"app-gate-api-cicd": {xrOf("liar", "agent-r-0a1b2c"), xrOf("fine", "app-gate-api-cicd")}}
	r.files.files[repo+"|agents/liar.yaml|"+commit] = map[string]any{"doc": defDoc("liar", nil)}
	r.files.files[repo+"|agents/fine.yaml|"+commit] = map[string]any{"doc": defDoc("fine", nil)}
	ch := r.poll()
	if len(ch.Added) != 1 || ch.Added[0] != "fine" {
		t.Fatalf("%+v", ch)
	}
	if !strings.Contains(r.logs.String(), "not in the namespace that was listed") {
		t.Fatal("the dropped item must be said")
	}
}

// A list that fails in any namespace is the whole list failing: nothing is applied, nothing is unloaded, the last good set stays, and
// it is said so; an answer of zero items is a different answer.
func TestAFailedListKeepsTheLastGoodSetAndAnEmptyOneIsNotTheSame(t *testing.T) {
	r := newRig(t, "app-gate-api-cicd", "app-other-cicd")
	r.pin("good", "app-gate-api-cicd", defDoc("good", nil))
	r.poll()
	if r.defs["good"] == nil || r.pub.count() != 1 {
		t.Fatal("the pin loads and the set is published")
	}
	before := r.defs
	for _, fail := range []string{"airframe", "denied"} {
		r.lister.fail = fail
		r.logs.Reset()
		if ch := r.poll(); ch.Any() {
			t.Fatalf("%s: a failed list changed the set: %+v", fail, ch)
		}
		if len(r.defs) != len(before) || r.defs["good"] == nil {
			t.Fatalf("%s: a failed list unloaded an agent", fail)
		}
		if !strings.Contains(r.logs.String(), "keeping the current set") {
			t.Fatalf("%s: a failed list must be said, loudly: %s", fail, r.logs.String())
		}
	}
	if r.pub.count() != 1 {
		t.Fatalf("a failed list published (%d)", r.pub.count())
	}
	r.lister.fail = ""
	// zero items, once: not believed; twice in a row: the pins were deleted
	r.lister.items = nil
	if ch := r.poll(); ch.Any() || r.defs["good"] == nil {
		t.Fatalf("one empty answer unloaded an agent: %+v", ch)
	}
	if ch := r.poll(); len(ch.Removed) != 1 || r.defs["good"] != nil {
		t.Fatalf("two empty answers in a row are the answer: %+v", ch)
	}
	// and an answer that was not empty in between starts the count again
	r.pin("good", "app-gate-api-cicd", defDoc("good", nil))
	r.poll()
	r.lister.items = nil
	r.poll()
	r.pin("good", "app-gate-api-cicd", defDoc("good", nil))
	r.poll()
	r.lister.items = nil
	if ch := r.poll(); ch.Any() || r.defs["good"] == nil {
		t.Fatalf("empty, full, empty is not two empties in a row: %+v", ch)
	}
}

// A first poll that finds nothing still publishes (the core must learn the bundled set, and an emptied set must reach it).
func TestTheFirstPollPublishesEvenWhenThereIsNothingPinned(t *testing.T) {
	r := newRig(t)
	if ch := r.poll(); ch.Any() {
		t.Fatalf("%+v", ch)
	}
	if r.pub.count() != 1 || len(r.pub.bundled[0]) != 1 || len(r.pub.pins[0]) != 0 {
		t.Fatalf("one publish of the bundled set and no pins: %d, %v", r.pub.count(), r.pub.pins)
	}
	r.poll()
	if r.pub.count() != 1 {
		t.Fatal("a poll that finds the same set publishes nothing")
	}
}

// More Agent XRs than the core takes in one set is a list failure, not a set that is cut: no partial set reaches the core.
func TestMoreXRsThanTheCoreTakesIsRefusedWhole(t *testing.T) {
	r := newRig(t)
	r.pin("good", "app-gate-api-cicd", defDoc("good", nil))
	r.poll()
	for i := 0; i <= definitions.MaxPins; i++ {
		r.lister.items = append(r.lister.items, xrOf(fmt.Sprintf("a%d", i), "app-gate-api-cicd"))
	}
	r.logs.Reset()
	if ch := r.poll(); ch.Any() || r.defs["good"] == nil || r.pub.count() != 1 {
		t.Fatalf("an over-long list must change and publish nothing: %+v", ch)
	}
	if !strings.Contains(r.logs.String(), "keeping the current set") {
		t.Fatal(r.logs.String())
	}
}

// The publish is made whole from one snapshot, repeated until the core has it, and never marks a set as published that changed while it
// was on its way.
func TestAPublishTheCoreDidNotTakeIsMadeAgainWholeAndASetThatChangedMidPublishIsNotMarkedPublished(t *testing.T) {
	r := newRig(t)
	r.pub.fail = true
	r.pin("one", "app-gate-api-cicd", defDoc("one", nil))
	r.poll()
	if !r.mgr.Unpublished() || r.defs["one"] == nil {
		t.Fatal("a publish the core did not take leaves the set applied locally and unpublished")
	}
	r.pub.fail = false
	r.poll() // no change: published again, whole
	if r.mgr.Unpublished() || r.pub.count() != 2 || len(r.pub.pins[1]) != 1 || len(r.pub.bundled[1]) != 1 {
		t.Fatalf("the whole set is published again: unpublished=%v publishes=%d", r.mgr.Unpublished(), r.pub.count())
	}
	if r.poll(); r.pub.count() != 2 {
		t.Fatal("published and unchanged: nothing sent")
	}
	// a change that lands while a publish is in flight
	r.pub.gate, r.pub.entered = make(chan struct{}), make(chan struct{}, 4)
	r.pin("two", "app-gate-api-cicd", defDoc("two", nil))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.poll() }()
	<-r.pub.entered // the publish of {one, two} is in flight
	v0 := r.set.Version()
	wg.Add(1)
	go func() { defer wg.Done(); r.mgr.ApplyCluster(context.Background(), nil, nil) }() // the set empties meanwhile
	for i := 0; r.set.Version() == v0; i++ {
		if i > 400 {
			t.Fatal("the second apply did not land")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.pub.gate <- struct{}{} // the first publish finishes: it was of a set that is no longer current
	select {
	case <-r.pub.entered:
		r.pub.gate <- struct{}{}
	case <-time.After(2 * time.Second):
		t.Fatal("the set that changed during the publish was not published again")
	}
	wg.Wait()
	if r.mgr.Unpublished() {
		t.Fatal("the latest set is published")
	}
	last := r.pub.pins[len(r.pub.pins)-1]
	if len(last) != 0 {
		t.Fatalf("the last publish is the latest set (no pins), not the earlier one: %d pins", len(last))
	}
}

// A publish is never a partial set: pins, rejections and bundled definitions come from one snapshot, whatever else is applying.
func TestConcurrentAppliesAndPublishesAreRaceFreeAndEveryPublishIsOneWholeSet(t *testing.T) {
	r := newRig(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				name := fmt.Sprintf("a%d", i)
				doc := defDoc(name, nil)
				l, rej := definitions.FromPin(definitions.FromPlain(doc).(*definitions.OMap), pinSource(name), owner, r.set.Bundled(), tools.Default(), definitions.PinPolicy{})
				if rej != nil {
					t.Error(rej.Reason)
					return
				}
				r.mgr.ApplyCluster(context.Background(), []*definitions.Loaded{l}, []definitions.Rejected{{Name: "x", Source: pinSource("x"), Reason: "r"}})
				_ = r.set.Snapshot()
			}
		}(i)
	}
	wg.Wait()
	for i, pins := range r.pub.pins {
		if len(pins) > 1 || len(r.pub.bundled[i]) != 1 || len(r.pub.rej[i]) != 1 {
			t.Fatalf("publish %d is not a whole set: %d pins, %d bundled, %d rejected", i, len(pins), len(r.pub.bundled[i]), len(r.pub.rej[i]))
		}
	}
}

func pinSource(name string) definitions.Source {
	return definitions.Source{Kind: "cluster", Repo: definitions.S(repo), Path: definitions.S("agents/" + name + ".yaml"), Commit: definitions.S(commit),
		Namespace: definitions.S("app-gate-api-cicd"), XR: definitions.S(name)}
}

// What was published is recorded: how many of each and a digest, so two publishes of the same set can be seen to be the same.
func TestWhatWasPublishedIsRecordedWithACountAndADigest(t *testing.T) {
	r := newRig(t)
	r.pin("one", "app-gate-api-cicd", defDoc("one", nil))
	r.poll()
	p := r.mgr.LastPublished()
	if p.Pins != 1 || p.Bundled != 1 || p.Rejected != 0 || len(p.Digest) != 64 || p.Bytes == 0 {
		t.Fatalf("%+v", p)
	}
	if !strings.Contains(r.logs.String(), "definitions published") || !strings.Contains(r.logs.String(), p.Digest) {
		t.Fatalf("the log line carries the counts and the digest: %s", r.logs.String())
	}
	// the same set again (the core was down, then up) has the same digest
	r.pub.fail = true
	r.mgr.ApplyCluster(context.Background(), []*definitions.Loaded{mustLoad(t, "two")}, nil)
	r.pub.fail = false
	r.mgr.Publish(context.Background())
	d1 := r.mgr.LastPublished().Digest
	r.mgr.ApplyCluster(context.Background(), []*definitions.Loaded{mustLoad(t, "two")}, nil)
	if d1 == p.Digest || r.mgr.LastPublished().Digest != d1 {
		t.Fatalf("digests: %s %s %s", p.Digest, d1, r.mgr.LastPublished().Digest)
	}
}

func mustLoad(t *testing.T, name string) *definitions.Loaded {
	l, rej := definitions.FromPin(definitions.FromPlain(defDoc(name, nil)).(*definitions.OMap), pinSource(name), owner, nil, tools.Default(), definitions.PinPolicy{})
	if rej != nil {
		t.Fatal(rej.Reason)
	}
	return l
}

// A set larger than the core takes is cut before it is applied (and said), so the gateway never runs a set the core was not given.
func TestASetLargerThanTheCoreTakesIsNotSent(t *testing.T) {
	r := newRig(t)
	var loaded []*definitions.Loaded
	for i := 0; i < 200; i++ {
		name := fmt.Sprintf("p%d", i)
		src := pinSource(name)
		d := defDoc(name, nil)
		d["runtime"].(map[string]any)["instructions"] = strings.Repeat("y", 1500)
		l, rej := definitions.FromPin(definitions.FromPlain(d).(*definitions.OMap), src, owner, nil, tools.Default(), definitions.PinPolicy{})
		if rej != nil {
			t.Fatal(rej.Reason)
		}
		// make each document large without breaking the schema: a long tool-free description is capped, so pad the stored doc
		l.Doc["padding"] = strings.Repeat("z", 25000)
		loaded = append(loaded, l)
	}
	r.mgr.ApplyCluster(context.Background(), loaded, nil)
	// the gateway never runs a set the core was not given: the pins of the namespace that does not fit are dropped before the swap
	if r.pub.count() != 1 || len(r.pub.pins[0]) != 0 || len(r.pub.rej[0]) != 200 || r.defs["p0"] != nil || !strings.Contains(r.logs.String(), "do not fit the publish budget") {
		t.Fatalf("a set over the core's limit is cut before it is applied (%d publishes): %s", r.pub.count(), r.logs.String())
	}
}

// The store failing does not stop the set from running: the live set is what runs, the store is for the restart.
func TestAStoreThatFailsDoesNotStopTheSet(t *testing.T) {
	r := newRig(t)
	r.mgr.Store = failingStore{}
	r.pin("one", "app-gate-api-cicd", defDoc("one", nil))
	r.poll()
	if r.defs["one"] == nil || r.pub.count() != 1 || !strings.Contains(r.logs.String(), "could not be kept") {
		t.Fatalf("%v %d %s", r.defs["one"], r.pub.count(), r.logs.String())
	}
}

type failingStore struct{}

func (failingStore) ReplaceDefinitions(context.Context, []definitions.Row) error {
	return errors.New("disk")
}

// One bad pin does not take down the set, nor silently drop the others: the valid ones load, the bad one is rejected with its reason, and
// both are published.
func TestOneBadPinDoesNotTakeDownTheSetNorDropTheOthers(t *testing.T) {
	r := newRig(t)
	r.pin("good", "app-gate-api-cicd", defDoc("good", nil))
	r.pin("bad", "app-gate-api-cicd", defDoc("bad", map[string]any{"kind": "daemon"}))
	r.pin("also-good", "app-gate-api-cicd", defDoc("also-good", nil))
	ch := r.poll()
	if len(ch.Added) != 2 || len(ch.Rejected) != 1 || ch.Rejected[0].Name != "bad" || !strings.Contains(ch.Rejected[0].Reason, "'daemon' is not one of") {
		t.Fatalf("%+v", ch)
	}
	if r.pub.count() != 1 || len(r.pub.pins[0]) != 2 || len(r.pub.rej[0]) != 1 {
		t.Fatalf("both the loaded and the rejected are published together: %d pins, %d rejected", len(r.pub.pins[0]), len(r.pub.rej[0]))
	}
}

// The pin policy the Go gateway adds: a ceiling the operator can lower, tools the gateway has, repository and app patterns that name
// what they mean.
func TestThePinPolicy(t *testing.T) {
	reg := tools.Default()
	load := func(over map[string]any, p definitions.PinPolicy) string {
		d := definitions.FromPlain(defDoc("agent", over)).(*definitions.OMap)
		p.Detailed = true
		_, rej := definitions.FromPin(d, pinSource("agent"), owner, nil, reg, p)
		if rej == nil {
			return ""
		}
		return rej.Reason
	}
	prof := func(f func(p map[string]any)) map[string]any {
		p := defDoc("agent", nil)["profile"].(map[string]any)
		f(p)
		return map[string]any{"profile": p}
	}
	if r := load(nil, definitions.PinPolicy{Tools: reg}); r != "" {
		t.Fatal(r)
	}
	if r := load(prof(func(p map[string]any) { p["tierCeiling"] = "T2" }), definitions.PinPolicy{Tools: reg, MaxTier: "T1"}); !strings.HasPrefix(r, "policy: profile/tierCeiling: T2 is above T1") {
		t.Fatalf("a ceiling the operator lowered: %q", r)
	}
	if r := load(prof(func(p map[string]any) { p["tierCeiling"] = "T1" }), definitions.PinPolicy{Tools: reg, MaxTier: "T1"}); r != "" {
		t.Fatalf("at the ceiling is allowed: %q", r)
	}
	if r := load(prof(func(p map[string]any) { p["tierCeiling"] = "T1" }), definitions.PinPolicy{Tools: reg, MaxTier: "T0"}); r == "" {
		t.Fatal("T1 is above a T0 ceiling")
	}
	for _, pat := range []string{"*/*", "acme/*", "*/x", "Acme/x", "ac*/x", "*cme/x", "acme/**"} {
		if r := load(prof(func(p map[string]any) { p["repos"] = map[string]any{"allow": []any{pat}} }), definitions.PinPolicy{}); !strings.HasPrefix(r, "policy: profile/repos/allow") {
			t.Errorf("repos.allow %q: %q", pat, r)
		}
	}
	for _, pat := range []string{"acme/gate-api", "acme/gate-*", "acme/*-api", "acme/g*e-api", "a-b_c/d.e"} {
		if r := load(prof(func(p map[string]any) { p["repos"] = map[string]any{"allow": []any{pat}} }), definitions.PinPolicy{}); r != "" {
			t.Errorf("repos.allow %q is a name: %q", pat, r)
		}
	}
	// the policy is not applied to bundled definitions: the platform's own agents name `jfillman/*`
	b := defDoc("platform", map[string]any{"owner": "group:default/team-platform"})
	b["profile"].(map[string]any)["repos"] = map[string]any{"allow": []any{"acme/*"}}
	if _, err := definitions.ParseDoc(definitions.FromPlain(b).(*definitions.OMap), reg); err != nil {
		t.Fatalf("a bundled definition is parsed without the pin policy: %v", err)
	}
}

// A workload definition always has a service account once it has been parsed: the schema requires it, and the S3 refusal of a
// triggered run of one with none (R008) can therefore only ever see a definition that was built by hand.
func TestAParsedWorkloadDefinitionAlwaysHasAServiceAccount(t *testing.T) {
	reg := tools.Default()
	for _, id := range []map[string]any{{"type": "workload"}, {"type": "workload", "serviceAccount": ""}, {"type": "workload", "serviceAccount": nil}} {
		d := definitions.FromPlain(defDoc("w", map[string]any{"identity": id})).(*definitions.OMap)
		if _, err := definitions.ParseDoc(d, reg); err == nil {
			t.Errorf("%v parsed", id)
		}
	}
	f := loadFile(t)
	n := 0
	for _, c := range f.Cases {
		got := decideGo(t, c)
		if got.Loaded != nil && got.Loaded.Agent != nil {
			a := norm(t, got.Loaded.Agent).(map[string]any)
			if a["identity_type"] == "workload" && (a["service_account"] == nil || a["service_account"] == "") {
				t.Errorf("%s: a loaded workload definition without a service account", c.Name)
			}
			n++
		}
	}
	if n < 50 {
		t.Fatalf("only %d loaded definitions were checked", n)
	}
}

// Stored rows go back through the same decision a pin does.
func TestStoredRowsAreDecidedAgainAndABadOneIsARejectionNotASilentDrop(t *testing.T) {
	reg := tools.Default()
	good := definitions.Row{"name": "good", "state": "loaded", "source": pinSource("good").View(), "doc": defDoc("good", nil)}
	bad := definitions.Row{"name": "bad", "state": "loaded", "source": pinSource("bad").View(), "doc": defDoc("bad", map[string]any{"kind": "nope"})}
	wide := defDoc("wide", nil)
	wide["profile"].(map[string]any)["repos"] = map[string]any{"allow": []any{"*/*"}}
	wideRow := definitions.Row{"name": "wide", "state": "loaded", "source": pinSource("wide").View(), "doc": wide}
	shadow := definitions.Row{"name": "researcher", "state": "loaded", "source": pinSource("researcher").View(), "doc": defDoc("researcher", nil)}
	rej := definitions.Row{"name": "old", "state": "rejected", "source": pinSource("old").View(), "reason": "was bad"}
	loaded, rejected := definitions.FromRecords([]definitions.Row{good, bad, wideRow, shadow, rej}, map[string]*definitions.Definition{"researcher": {Name: "researcher"}}, reg,
		definitions.PinPolicy{Tools: reg})
	if len(loaded) != 1 || loaded[0].Name() != "good" || len(rejected) != 4 {
		t.Fatalf("%d loaded, %d rejected", len(loaded), len(rejected))
	}
	for _, r := range rejected {
		if r.Name != "old" && !strings.HasPrefix(r.Reason, "stored definition no longer parses: ") {
			t.Errorf("%s: %q", r.Name, r.Reason)
		}
	}
}

// What a poll found never changes what a reader holds: a snapshot is a map that is not written to again.
func TestASnapshotIsNeverWrittenToAgain(t *testing.T) {
	r := newRig(t)
	r.pin("one", "app-gate-api-cicd", defDoc("one", nil))
	before := r.set.Snapshot()
	n := len(before)
	r.poll()
	if len(before) != n || before["one"] != nil || r.set.Snapshot()["one"] == nil {
		t.Fatal("a snapshot taken before a change must not see it, and the new one must")
	}
	if _, ok := r.set.Snapshot()["researcher"]; !ok {
		t.Fatal("the bundled definitions are always there")
	}
}

func TestMarshalOfAPublishedPinIsTheDocumentAsRead(t *testing.T) {
	l := mustLoad(t, "one")
	raw, err := json.Marshal(l.Doc)
	if err != nil || !strings.Contains(string(raw), `"ttlMinutes":20`) {
		t.Fatalf("%s %v", raw, err)
	}
}

// A pin must name a full commit id. A branch or a tag moves without the review the pin is for, and the loader fetches a pin once.
func TestAPinMustNameAFullCommit(t *testing.T) {
	for commit, ok := range map[string]bool{commit: true, strings.Repeat("a", 64): true, "main": false, "v1.0.0": false, "HEAD": false, strings.Repeat("a", 39): false,
		strings.ToUpper(commit): false, commit + "0": false, "": false, "refs/heads/main": false} {
		r := newRig(t)
		x := xrOf("one", "app-gate-api-cicd")
		x["spec"].(map[string]any)["source"].(map[string]any)["commit"] = commit
		r.lister.items = []map[string]any{x}
		r.files.files[repo+"|agents/one.yaml|"+commit] = map[string]any{"doc": defDoc("one", nil)}
		ch := r.poll()
		if ok && len(ch.Added) != 1 {
			t.Errorf("%q: a full commit id must load: %+v", commit, ch)
		}
		if !ok {
			if len(ch.Added) != 0 || len(ch.Rejected) != 1 || len(r.files.calls) != 0 {
				t.Errorf("%q: must be rejected before anything is fetched: %+v, fetched %v", commit, ch, r.files.calls)
			}
		}
	}
}

// A list that keeps failing is said louder each time it goes on.
func TestAListThatKeepsFailingIsSaidAsAnError(t *testing.T) {
	r := newRig(t)
	r.lister.fail = "airframe"
	for i := 0; i < 2; i++ {
		r.poll()
	}
	if strings.Contains(r.logs.String(), "level=ERROR") {
		t.Fatal("two failures are warnings")
	}
	r.poll()
	if !strings.Contains(r.logs.String(), "level=ERROR") || !strings.Contains(r.logs.String(), "consecutive_failures=3") {
		t.Fatalf("the third in a row is an error: %s", r.logs.String())
	}
	r.lister.fail = ""
	r.poll()
	r.lister.fail = "airframe"
	r.logs.Reset()
	r.poll()
	if strings.Contains(r.logs.String(), "level=ERROR") {
		t.Fatal("a success resets the count")
	}
}

// An empty answer on either side of a failed list is not two empty answers in a row.
func TestAnEmptyListAroundAFailedOneIsNotTwoInARow(t *testing.T) {
	r := newRig(t)
	r.pin("one", "app-gate-api-cicd", defDoc("one", nil))
	r.poll()
	r.lister.items = nil
	r.poll()
	r.lister.fail = "airframe"
	r.poll()
	r.lister.fail = ""
	if ch := r.poll(); ch.Any() || r.defs["one"] == nil {
		t.Fatalf("%+v", ch)
	}
}
