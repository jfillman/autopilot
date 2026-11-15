package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/runs"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/autopilot/gateway/internal/triggers"
)

// The crash harness. It drives the real code (the bridge's fire and takeover path, LaunchTriggered, launch, deleteRuns,
// AbandonTriggered, the person controls) over the real session Store, with a fault injector in the place of every durable thing
// the gateway has: the session database, the trigger ledger and the run backend. The run backend keeps real state (the runs that
// exist, by name; every create that was ever committed), and can answer a call with a fault.
//
// A "crash" is the Nth durable write or backend call of the first process: that call completes (its effect is in the store or the
// backend) and from then on every call of that process fails without effect, which is what stopping the process there leaves. A fresh
// gateway, store and bridge are then built over what the database and the backend hold, and take the trigger over.
//
// The scenarios are enumerated, not written: a clean run counts its writes and calls, and every one of them is crashed at, for
// the plain launch, for a hold, a person's end and a freeze placed before each of them (and at the start of the takeover), and for
// each class of backend answer at each backend call. After the takeover, and after the deadline's sweep, the invariants are
// asserted against the backend's state, not against a flag.

var errDead = errors.New("the process is dead")

// couldNotTell is an answer that proves nothing about what the backend did (a transport error, a timeout, a 5xx).
type couldNotTell struct{ msg string }

func (e *couldNotTell) Error() string      { return e.msg }
func (e *couldNotTell) CouldNotTell() bool { return true }
func (e *couldNotTell) Kind() string       { return "KubeRunError" }
func (e *couldNotTell) Evidence() string   { return e.msg }

// alreadyExists is the API server's 409 for a name that is taken.
type alreadyExists struct{ name string }

func (e *alreadyExists) Error() string       { return "AgentRun " + e.name + " already exists" }
func (e *alreadyExists) AlreadyExists() bool { return true }
func (e *alreadyExists) Kind() string        { return "KubeRunError" }
func (e *alreadyExists) Evidence() string    { return e.Error() }

type chaos struct {
	mu      sync.Mutex
	n, base int
	crashAt int
	dead    bool
	inHook  bool
	hookAt  int
	hook    func(kind string)
	log     []string
}

// begin counts one durable call; the hook (a person's action) runs first when this is the call it was set before.
func (c *chaos) begin(kind string) (int, error) {
	c.mu.Lock()
	if c.dead {
		c.mu.Unlock()
		return 0, errDead
	}
	if !c.inHook {
		c.base++
		// A person's action cannot run inside a save (it would be the same save): when the call it was set before is a save, it runs
		// before the next call that is not one, which is the same point (the saves of a session are ordered).
		if c.hook != nil && c.hookAt <= c.base && kind != "save" {
			h := c.hook
			c.hook, c.inHook = nil, true
			c.mu.Unlock()
			h(kind)
			c.mu.Lock()
			c.inHook = false
			if c.dead {
				c.mu.Unlock()
				return 0, errDead
			}
		}
	}
	c.n++
	cur := c.n
	c.log = append(c.log, kind)
	c.mu.Unlock()
	return cur, nil
}

// leftover runs the action that was set before a call the process never made (it ended first).
func (c *chaos) leftover() {
	c.mu.Lock()
	h := c.hook
	if h == nil || c.dead {
		c.mu.Unlock()
		return
	}
	c.hook, c.inHook = nil, true
	c.mu.Unlock()
	h("end-of-process")
	c.mu.Lock()
	c.inHook = false
	c.mu.Unlock()
}

// done makes the process dead when this was the call it crashes at.
func (c *chaos) done(cur int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.crashAt != 0 && cur == c.crashAt {
		c.dead = true
		return errDead
	}
	return nil
}

type chaosPersist struct {
	c *chaos
	p *memPersist
}

func (cp *chaosPersist) Load(ctx context.Context) ([]session.Record, error) { return cp.p.Load(ctx) }
func (cp *chaosPersist) Save(ctx context.Context, recs []session.Record) error {
	cur, err := cp.c.begin("save")
	if err != nil {
		return err
	}
	if err := cp.p.Save(ctx, recs); err != nil {
		return err
	}
	return cp.c.done(cur)
}

type chaosLedger struct {
	triggers.Ledger
	c *chaos
}

func (l *chaosLedger) Claim(ctx context.Context, agent, key, via, ev string, now, stale time.Time) (bool, error) {
	cur, err := l.c.begin("claim")
	if err != nil {
		return false, err
	}
	ok, e := l.Ledger.Claim(ctx, agent, key, via, ev, now, stale)
	if d := l.c.done(cur); d != nil {
		return false, d
	}
	return ok, e
}
func (l *chaosLedger) Opened(ctx context.Context, agent, key, sid string) error {
	cur, err := l.c.begin("opened")
	if err != nil {
		return err
	}
	e := l.Ledger.Opened(ctx, agent, key, sid)
	if d := l.c.done(cur); d != nil {
		return d
	}
	return e
}
func (l *chaosLedger) Decide(ctx context.Context, agent, key, state, sid, run string) error {
	cur, err := l.c.begin("decide")
	if err != nil {
		return err
	}
	e := l.Ledger.Decide(ctx, agent, key, state, sid, run)
	if d := l.c.done(cur); d != nil {
		return d
	}
	return e
}

func (l *chaosLedger) Advance(ctx context.Context, source, lastID string) error {
	cur, err := l.c.begin("advance")
	if err != nil {
		return err
	}
	e := l.Ledger.Advance(ctx, source, lastID)
	if d := l.c.done(cur); d != nil {
		return d
	}
	return e
}

// fakeRuns is the run backend's state: the runs that exist, and every create that was ever committed.
type fakeRuns struct {
	mu      sync.Mutex
	live    map[string]bool
	created []string
	late    []string
	landed  map[string]bool // runs that landed after their create's answer was lost: the sweep's, not a decision's
	down    bool
}

func newFakeRuns() *fakeRuns { return &fakeRuns{live: map[string]bool{}, landed: map[string]bool{}} }

func (f *fakeRuns) settle() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.landLocked(nil)
}

func (f *fakeRuns) landLocked(except []string) {
	skip := map[string]int{}
	for _, n := range except {
		skip[n]++
	}
	var keep []string
	for _, n := range f.late {
		if skip[n] > 0 {
			skip[n]--
			keep = append(keep, n)
			continue
		}
		f.live[n] = true
		f.landed[n] = true
		f.created = append(f.created, n)
	}
	f.late = keep
}

func (f *fakeRuns) liveNames() []string { return f.live2(false) }

// liveNotLate is the live runs that did not land late.
func (f *fakeRuns) liveNotLate() []string { return f.live2(true) }

func (f *fakeRuns) live2(skipLate bool) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n, v := range f.live {
		if v && !(skipLate && f.landed[n]) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// fault is the class of answer the first process's idx-th backend call gets.
type fault struct {
	idx   int
	class string
}

// chaosRuns is the AgentRun store the run backend talks to, as the Kubernetes API would answer it.
type chaosRuns struct {
	c     *chaos
	f     *fakeRuns
	plan  *fault
	bidx  int
	calls *[]string // the kinds of the backend calls in this process, in order
	mu    sync.Mutex
}

func (r *chaosRuns) call(kind, name string, do func(class string) error) error {
	cur, err := r.c.begin("backend:" + kind)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.bidx++
	idx := r.bidx
	if r.calls != nil {
		*r.calls = append(*r.calls, kind)
	}
	r.mu.Unlock()
	class := ""
	if r.plan != nil && r.plan.idx == idx {
		class = r.plan.class
	}
	r.f.mu.Lock()
	pending := append([]string(nil), r.f.late...)
	r.f.mu.Unlock()
	e := do(class)
	r.f.mu.Lock()
	r.f.landLocked(r.f.late[len(pending):]) // a create that was late lands after the next call, not before it
	r.f.mu.Unlock()
	if d := r.c.done(cur); d != nil {
		return d
	}
	return e
}

func (r *chaosRuns) Create(_ context.Context, m map[string]any) error {
	name := m["metadata"].(map[string]any)["name"].(string)
	return r.call("create", name, func(class string) error {
		r.f.mu.Lock()
		defer r.f.mu.Unlock()
		if r.f.down {
			return &couldNotTell{"API server unreachable"}
		}
		switch class {
		case "create-before-commit":
			return &couldNotTell{"timeout"}
		case "create-after-commit":
			r.f.live[name] = true
			r.f.created = append(r.f.created, name)
			return &couldNotTell{"timeout"}
		case "create-late":
			r.f.late = append(r.f.late, name)
			return &couldNotTell{"timeout"}
		case "create-refused":
			return &runs.Error{Msg: "API server 422 creating AgentRun " + name}
		}
		if r.f.live[name] {
			return &alreadyExists{name}
		}
		r.f.live[name] = true
		r.f.created = append(r.f.created, name)
		return nil
	})
}

func (r *chaosRuns) Delete(_ context.Context, name string) error {
	return r.call("delete", name, func(class string) error {
		r.f.mu.Lock()
		defer r.f.mu.Unlock()
		if r.f.down {
			return &couldNotTell{"API server unreachable"}
		}
		switch class {
		case "delete-before-commit":
			return &couldNotTell{"timeout"}
		case "delete-after-commit":
			delete(r.f.live, name)
			return &couldNotTell{"timeout"}
		}
		delete(r.f.live, name) // a 404 is a success
		return nil
	})
}

func (r *chaosRuns) Outcome(_ context.Context, name string) (string, error) {
	var out string
	err := r.call("get", name, func(class string) error {
		r.f.mu.Lock()
		defer r.f.mu.Unlock()
		if r.f.down || class == "get-timeout" {
			return &couldNotTell{"timeout"}
		}
		if !r.f.live[name] {
			out = "Gone"
		}
		return nil
	})
	return out, err
}

// world is everything that survives a crash: the database, the ledger, the holds' table and the backend.
type world struct {
	p       *peopleRig
	persist *memPersist
	ledger  *triggers.MemoryLedger
	holds   *dispatch.MemoryHolds
	runs    *fakeRuns
	clock   time.Time
	resume  bool // the event arrives through the bridge's catch-up (Resume), and the cursor is part of what is checked
}

type proc struct {
	g   *dispatch.Gateway
	b   *triggers.Bridge
	c   *chaos
	led *chaosLedger
	cr  *chaosRuns
}

func (w *world) newProc(t *testing.T, c *chaos, plan *fault) *proc {
	t.Helper()
	cp := &chaosPersist{c: c, p: w.persist}
	st, err := session.NewStore(ctx, cp)
	if err != nil {
		t.Fatal(err)
	}
	co := core.New(w.p.fc.Client(2*time.Second), core.NewMemoryOutbox(), false)
	g, err := dispatch.New(loadDefs(t), st, co, w.p.tokens, tools.Default(), map[string]dispatch.Handler{})
	if err != nil {
		t.Fatal(err)
	}
	g.Now = func() time.Time { return w.clock }
	g.OnPersistError = func(error) {}
	cr := &chaosRuns{c: c, f: w.runs, plan: plan, calls: &[]string{}}
	g.SetRuns(&runs.Backend{Store: cr})
	g.Results = &dispatch.MemoryResults{Rows: map[string]map[string]any{}}
	g.SetInbox(w.p.inbox, w.p.dir, []string{"hangar-prod/tower/*"})
	g.Admins = []string{adminID}
	g.Holds = w.holds
	g.Fingerprint = dispatch.Fingerprint
	img := "ghcr.io/jfillman/autopilot-agent-runtime@sha256:" + strings.Repeat("a", 64)
	g.Defs["triage-agent"].Image = &img
	if err := g.ReloadHolds(ctx); err != nil {
		t.Fatal(err)
	}
	led := &chaosLedger{Ledger: w.ledger, c: c}
	cfg := triggers.BridgeConfig{BaseURL: "http://glidepath.invalid", AllowHTTP: true, TokenFile: "/dev/null", Now: func() time.Time { return w.clock }}
	if w.resume {
		cfg.BaseURL, cfg.TokenFile = harnessURL, harnessToken
	}
	b, err := triggers.NewBridge(g, led, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &proc{g: g, b: b, c: c, led: led, cr: cr}
}

// The event of the catch-up runs: an id as glidepath-api's are (receive time in microseconds, a dash, a hash), after the cursor's.
var (
	harnessEventID  = fmt.Sprintf("%016d-%s", t0.Add(-time.Second).UnixMicro(), "abc123")
	harnessCursorID = fmt.Sprintf("%016d-%s", t0.Add(-time.Minute).UnixMicro(), "aaa000")
	harnessURL      string
	harnessToken    string
)

func harnessEnvelope(id string) map[string]any {
	e := harnessEvent()
	e["id"] = id
	return e
}

// startHarnessAPI serves the catch-up page glidepath-api would: the one event, after the cursor.
func startHarnessAPI(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events/recent" {
			http.Error(w, "no", 404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []any{harnessEnvelope(harnessEventID)}})
	}))
	t.Cleanup(srv.Close)
	harnessURL = srv.URL
	harnessToken = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(harnessToken, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func harnessEvent() map[string]any {
	return map[string]any{"id": "e1", "app": "gate-api", "routingKey": "gate-api.pipelinerun.finished",
		"event": map[string]any{"context": map[string]any{"type": "t"}, "subject": map[string]any{"id": "pr-1", "content": map[string]any{"outcome": "failure"}}}}
}

const harnessKey = "gate-api.pipelinerun.finished/pr-1"

// A scenario: where the first process crashes (0: it does not), the action a person takes and where (before the actionAt-th call of
// the first process; -1: at the start of the takeover), the backend fault, and whether the first takeover meets a backend that is down.
type scenario struct {
	crashAt  int
	action   string // "", hold, end, freeze
	actionAt int
	fault    *fault
	down     bool
	resume   bool
}

func (s scenario) String() string {
	out := fmt.Sprintf("crash@%d", s.crashAt)
	if s.action != "" {
		out += fmt.Sprintf(" %s@%d", s.action, s.actionAt)
	}
	if s.fault != nil {
		out += fmt.Sprintf(" %s@backend-call-%d", s.fault.class, s.fault.idx)
	}
	if s.down {
		out += " takeover-with-backend-down"
	}
	return out
}

type result struct {
	ops, base  int
	calls      []string
	log        []string // every durable call of the first process, in order
	violations []string
}

// runScenario runs one scenario to the end and checks the invariants.
func runScenario(t *testing.T, p *peopleRig, sc scenario) (res result) {
	t.Helper()
	viol := func(format string, a ...any) { res.violations = append(res.violations, fmt.Sprintf(format, a...)) }
	w := &world{p: p, persist: newPersist(), ledger: &triggers.MemoryLedger{}, holds: &dispatch.MemoryHolds{}, runs: newFakeRuns(), clock: t0, resume: sc.resume}
	if sc.resume {
		_, _ = w.ledger.Cursor(ctx, triggers.Source, t0.Add(-time.Hour))
		_ = w.ledger.Advance(ctx, triggers.Source, harnessCursorID)
	}
	deliver := func(pr *proc, via string) error {
		if sc.resume {
			_, err := pr.b.Resume(ctx)
			return err
		}
		_, err := pr.b.Handle(ctx, harnessEvent(), via)
		return err
	}
	// the cursor never moves past an event whose trigger is not decided
	cursorOK := func(when string) {
		if !sc.resume {
			return
		}
		cur, _ := w.ledger.Cursor(ctx, triggers.Source, t0)
		r, _ := w.ledger.Get(ctx, "triage-agent", harnessKey)
		if cur.LastID != nil && *cur.LastID >= harnessEventID && (r == nil || r.State == "claimed") {
			viol("%s: the cursor moved past the event while its trigger is undecided: %+v", when, r)
		}
	}
	applied, decidedAtApply, createdAtApply := false, false, 0

	apply := func(pr *proc, kind string) {
		var sid string
		for _, s := range pr.g.Store.All() {
			s.Lock()
			if s.Agent == "triage-agent" {
				sid = s.ID
			}
			s.Unlock()
		}
		switch sc.action {
		case "hold":
			if _, err := pr.g.HoldTriggers(ctx, p.person(adminID), "triage-agent", "storm", 1, ""); err != nil {
				return
			}
		case "end":
			if sid == "" {
				return
			}
			if _, err := pr.g.EndSession(ctx, p.person(adminID), sid, "stop", ""); err != nil {
				return
			}
		case "freeze":
			if sid == "" {
				return
			}
			if _, err := pr.g.FreezeSession(ctx, p.person(adminID), sid, "stop", ""); err != nil {
				return
			}
		}
		applied = true
		// A hold or an end that lands while the decision is being written, or after it, does not undo a launch that is past its last
		// check (the run is then the sweep's to end); and a create that is already in flight when the hold lands is undone by the
		// check after it, not prevented.
		if row, _ := w.ledger.Get(ctx, "triage-agent", harnessKey); (row != nil && row.State != "claimed") || kind == "decide" {
			decidedAtApply = true
		}
		w.runs.mu.Lock()
		createdAtApply = len(w.runs.created)
		if kind == "backend:create" {
			createdAtApply++
		}
		w.runs.mu.Unlock()
	}

	// the first process
	c1 := &chaos{crashAt: sc.crashAt}
	p1 := w.newProc(t, c1, sc.fault)
	if sc.action != "" && sc.actionAt > 0 {
		c1.hookAt, c1.hook = sc.actionAt, func(kind string) { apply(p1, kind) }
	}
	runBounded(t, sc, func() { _ = deliver(p1, "stream"); c1.leftover() })
	cursorOK("the first process")
	res.ops, res.base, res.calls = c1.n, c1.base, *p1.cr.calls
	res.log = append([]string(nil), c1.log...)
	w.runs.settle()

	// the takeover: a new process, later than the dead one's claim
	w.clock = t0.Add(10 * time.Minute)
	takeover := func(down bool) (outs []triggers.Outcome, err error) {
		w.runs.mu.Lock()
		w.runs.down = down
		w.runs.mu.Unlock()
		c2 := &chaos{}
		p2 := w.newProc(t, c2, nil)
		if sc.action != "" && sc.actionAt == -1 && !applied {
			c2.hookAt, c2.hook = 1, func(kind string) { apply(p2, kind) }
		}
		runBounded(t, sc, func() { err = deliver(p2, "recent"); c2.leftover() })
		cursorOK("a takeover")
		w.runs.settle()
		return
	}
	row := func() *triggers.Row { r, _ := w.ledger.Get(ctx, "triage-agent", harnessKey); return r }
	sessionRows := func() []session.Record {
		recs, _ := w.persist.Load(ctx)
		var out []session.Record
		for _, r := range recs {
			if r.Agent == "triage-agent" {
				out = append(out, r)
			}
		}
		return out
	}
	// every decision other than `launched` says no run was made for the key: no run may be live under it
	noRunUnderDecision := func(when string) {
		r := row()
		if r != nil && r.State != "claimed" && r.State != "launched" && len(w.runs.liveNotLate()) > 0 {
			viol("%s: the row was decided %q while the run %v is live", when, r.State, w.runs.liveNotLate())
		}
	}
	if sc.down {
		_, _ = takeover(true)
		noRunUnderDecision("takeover with the backend down")
		if r := row(); r != nil && r.State == "launched" && len(w.runs.created) == 0 {
			viol("a launched row with no run ever created (backend down)")
		}
	}
	_, err := takeover(false)
	if err != nil {
		viol("the clean takeover did not decide: %v", err)
	}
	if sc.resume {
		if cur, _ := w.ledger.Cursor(ctx, triggers.Source, t0); cur.LastID == nil || *cur.LastID != harnessEventID {
			viol("the cursor did not reach the event after a clean takeover: %v", cur.LastID)
		}
	}
	r := row()
	if r == nil || r.State == "claimed" {
		viol("the key is not decided after a clean takeover: %+v", r)
	}
	noRunUnderDecision("takeover")

	// at most one run ever
	if n := len(w.runs.created); n > 1 {
		viol("%d runs were created for one key: %v", n, w.runs.created)
	}
	// a launched row has a run that was made, and a live session has it live
	if r != nil && r.State == "launched" && r.Session != nil {
		name := runs.RunName(*r.Session)
		made := false
		for _, n := range w.runs.created {
			made = made || n == name
		}
		if !made {
			viol("a launched row (%s) with no run ever created", name)
		}
		for _, sr := range sessionRows() {
			if sr.ID == *r.Session && !sr.Closed && !w.runs.live[name] {
				viol("a launched row whose live session has no run live")
			}
		}
	}
	// a person's action
	if applied {
		w.runs.mu.Lock()
		after := len(w.runs.created) - createdAtApply
		if len(w.runs.landed) > 0 {
			after = 0 // a create issued before the action whose answer was lost and which landed later: the sweep's (checked below)
		}
		w.runs.mu.Unlock()
		if after > 0 {
			viol("%d run(s) were created after the %s was placed", after, sc.action)
		}
		strict := (sc.action == "end" || sc.action == "hold") && !decidedAtApply
		if strict && len(w.runs.liveNotLate()) > 0 {
			viol("a run is live after the %s: %v", sc.action, w.runs.liveNotLate())
		}
	}
	// the deadline's sweep: nothing may be left live that no session names, or that a closed session leaves behind
	w.clock = t0.Add(3 * time.Hour)
	c3 := &chaos{}
	p3 := w.newProc(t, c3, nil)
	runBounded(t, sc, func() { p3.g.ExpireAt(ctx, w.clock) })
	w.runs.settle()
	if l := w.runs.liveNames(); len(l) > 0 {
		viol("after the sweep the run(s) %v are still live (nothing names them)", l)
	}
	return res
}

// runBounded runs fn and fails the scenario when it does not come back (a deadlock).
func runBounded(t *testing.T, sc scenario, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }() // a panic in the code under test is the process dying
		fn()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s: the code under test did not return (a deadlock)", sc)
	}
}

func TestEveryCrashWindowOfTheRealCodeKeepsTheTriggerOnceAndSafe(t *testing.T) {
	triggers.RetryWaits = []time.Duration{time.Microsecond, time.Microsecond}
	dispatch.HoldReadWaits = []time.Duration{time.Microsecond}
	startHarnessAPI(t)
	p := newPeopleRig(t)
	failures := map[string][]string{}
	total := 0
	// the same enumeration for an event that arrives by Handle (the stream) and one that arrives by the catch-up (Resume), where the
	// cursor is part of what is written and checked
	for _, resume := range []bool{false, true} {
		var scs []scenario
		add := func(s scenario) { s.resume = resume; scs = append(scs, s) }
		run := func(s scenario) result { s.resume = resume; return runScenario(t, p, s) }

		plain := run(scenario{})
		t.Logf("the clean run's durable calls (resume=%v), each a window a crash is enumerated at: %v", resume, plain.log)
		if plain.ops < 6 || len(plain.calls) == 0 {
			t.Fatalf("the clean run is too small to enumerate: %+v", plain)
		}
		for n := 1; n <= plain.ops; n++ {
			add(scenario{crashAt: n})
			add(scenario{crashAt: n, down: true})
		}
		for _, action := range []string{"hold", "end", "freeze"} {
			points := []int{-1}
			for k := 1; k <= plain.base; k++ {
				points = append(points, k)
			}
			for _, at := range points {
				clean := run(scenario{action: action, actionAt: at})
				add(scenario{action: action, actionAt: at})
				for n := 1; n <= clean.ops; n++ {
					add(scenario{crashAt: n, action: action, actionAt: at})
				}
			}
		}
		// every class of backend answer at every backend call of the clean run
		classes := map[string][]string{"create": {"create-before-commit", "create-after-commit", "create-late", "create-refused"},
			"delete": {"delete-before-commit", "delete-after-commit"}, "get": {"get-timeout"}}
		for i, kind := range plain.calls {
			for _, class := range classes[kind] {
				f := &fault{idx: i + 1, class: class}
				faulted := run(scenario{fault: f})
				add(scenario{fault: f})
				for n := 1; n <= faulted.ops; n++ {
					add(scenario{crashAt: n, fault: f})
				}
			}
		}
		// a hold that lands between the create and its record makes the launch delete the run: every class of answer to that delete,
		// and to the create it races with
		for _, class := range classes["delete"] {
			for at := 1; at <= plain.base; at++ {
				for idx := 1; idx <= len(plain.calls)+2; idx++ {
					add(scenario{action: "hold", actionAt: at, fault: &fault{idx: idx, class: class}})
				}
			}
		}
		for _, class := range classes["create"] {
			for at := 1; at <= plain.base; at++ {
				add(scenario{action: "hold", actionAt: at, fault: &fault{idx: 1, class: class}})
			}
		}
		t.Logf("resume=%v: %d scenarios over %d durable calls (%d backend calls) of the clean run", resume, len(scs), plain.ops, len(plain.calls))
		for _, sc := range scs {
			res := runScenario(t, p, sc)
			total++
			for _, v := range res.violations {
				failures[invariantOf(v)] = append(failures[invariantOf(v)], sc.String()+": "+v)
			}
		}
	}
	t.Logf("%d scenarios in all", total)
	var names []string
	for k := range failures {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v := failures[k]
		show := v
		if len(show) > 4 {
			show = show[:4]
		}
		t.Errorf("%s: %d scenarios, e.g.\n   %s", k, len(v), strings.Join(show, "\n   "))
	}
}

func invariantOf(v string) string {
	switch {
	case strings.Contains(v, "decided") && strings.Contains(v, "is live"):
		return "a decision was written while a run is live"
	case strings.Contains(v, "runs were created"):
		return "more than one run for one key"
	case strings.Contains(v, "after the sweep"):
		return "a run nothing names survives the sweep"
	case strings.Contains(v, "created after the"):
		return "a run was created after a person's hold, end or freeze"
	case strings.Contains(v, "is live after the"):
		return "a run is live after a person's hold or end"
	case strings.Contains(v, "launched row"):
		return "a launched row without a run"
	case strings.Contains(v, "cursor"):
		return "the cursor and the row disagree"
	case strings.Contains(v, "not decided"):
		return "a key left undecided after a clean takeover"
	}
	return v
}
