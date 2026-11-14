package triggers_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/triggers"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// fakeGW is the gateway as the triggers see it: it models the session half of a trigger (a live session of the same task is a
// dedupe), and can be told to fail, hold or panic.
type fakeGW struct {
	mu       sync.Mutex
	defs     map[string]*definitions.Definition
	opens    []string // agent/task per open
	launches []string // session per launch
	sessions map[string]string
	hasRun   map[string]bool
	n        int
	holdRule map[string]string // agent -> rule an open is refused with
	openErr  error
	panicOn  map[string]bool
	launchOK bool // false: a launch is refused (launch-failed)
	events   []map[string]any
	outcomes [][]map[string]any
	onOpen   func(ctx context.Context)
}

func newGW(defs ...*definitions.Definition) *fakeGW {
	g := &fakeGW{defs: map[string]*definitions.Definition{}, sessions: map[string]string{}, hasRun: map[string]bool{}, holdRule: map[string]string{},
		panicOn: map[string]bool{}, launchOK: true}
	for _, d := range defs {
		g.defs[d.Name] = d
	}
	return g
}

func (g *fakeGW) Definitions() map[string]*definitions.Definition { return g.defs }

func (g *fakeGW) OpenTriggered(ctx context.Context, agent, trigger, task string) (*dispatch.Result, error) {
	g.mu.Lock()
	onOpen := g.onOpen
	g.mu.Unlock()
	if onOpen != nil {
		onOpen(ctx)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.panicOn[agent] {
		panic("boom")
	}
	if g.openErr != nil {
		return nil, g.openErr
	}
	g.opens = append(g.opens, agent+"/"+task)
	if r := g.holdRule[agent]; r != "" {
		return &dispatch.Result{Decision: map[string]any{"allow": false, "rule": r}}, nil
	}
	if sid, ok := g.sessions[agent+"/"+task]; ok {
		return &dispatch.Result{OK: true, Decision: map[string]any{"allow": true}, Data: map[string]any{"session": sid, "deduped": true}, Session: &sid}, nil
	}
	g.n++
	sid := fmt.Sprintf("s-%06d", g.n)
	g.sessions[agent+"/"+task] = sid
	return &dispatch.Result{OK: true, Decision: map[string]any{"allow": true}, Data: map[string]any{"session": sid, "deduped": false}, Session: &sid}, nil
}

func (g *fakeGW) Launch(_ context.Context, sid string, _ map[string]any) (*dispatch.Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.launches = append(g.launches, sid)
	if !g.launchOK {
		msg := "no run backend"
		return &dispatch.Result{Decision: map[string]any{"allow": false, "rule": "R001"}, Error: &msg}, nil
	}
	g.hasRun[sid] = true
	return &dispatch.Result{OK: true, Data: map[string]any{"run": "r-" + sid[2:], "session": sid}}, nil
}

func (g *fakeGW) SessionHasRun(sid string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hasRun[sid]
}

func (g *fakeGW) RecordEvent(_ context.Context, summary map[string]any, outcomes []map[string]any) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = append(g.events, summary)
	g.outcomes = append(g.outcomes, outcomes)
	return true, nil
}

func (g *fakeGW) nOpens() int  { g.mu.Lock(); defer g.mu.Unlock(); return len(g.opens) }
func (g *fakeGW) nLaunch() int { g.mu.Lock(); defer g.mu.Unlock(); return len(g.launches) }

func triage(match map[string]string, perHour int) *definitions.Definition {
	instr := "triage it"
	return &definitions.Definition{Name: "triage-agent", Kind: "event", IdentityType: "workload", Instructions: &instr,
		Triggers: []definitions.Trigger{{Type: "event", BindingKey: "gate-api.pipelinerun.finished", Match: match, MaxPerHour: perHour}}}
}

func nightly() *definitions.Definition {
	return &definitions.Definition{Name: "nightly-reviewer", Kind: "scheduled", IdentityType: "workload",
		Triggers: []definitions.Trigger{{Type: "interval", Every: "24h", Interval: 24 * time.Hour}}}
}

// eventID is a glidepath-api event id: 16 digits of receive time in microseconds, a dash, a hash.
func eventID(t time.Time, hash string) string { return fmt.Sprintf("%016d-%s", t.UnixMicro(), hash) }

func finished(eid, pr, outcome string) map[string]any {
	return map[string]any{"id": eid, "app": "gate-api", "routingKey": "gate-api.pipelinerun.finished", "receivedAt": "2026-11-14T11:59:00Z",
		"event": map[string]any{"context": map[string]any{"type": "dev.cdevents.pipelinerun.finished.0.3.0"},
			"subject": map[string]any{"id": pr, "content": map[string]any{"outcome": outcome, "pipelineName": "build"}}}}
}

type rig struct {
	t      *testing.T
	gw     *fakeGW
	ledger *triggers.MemoryLedger
	m      *triggers.Recorder
	now    time.Time
	b      *triggers.Bridge
	tok    string
}

func newRig(t *testing.T, gw *fakeGW, base string) *rig {
	t.Helper()
	r := &rig{t: t, gw: gw, ledger: &triggers.MemoryLedger{}, m: &triggers.Recorder{}, now: t0}
	r.tok = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(r.tok, []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.b = r.bridge(base)
	return r
}

func (r *rig) bridge(base string, mod ...func(*triggers.BridgeConfig)) *triggers.Bridge {
	cfg := triggers.BridgeConfig{BaseURL: base, TokenFile: r.tok, Now: func() time.Time { return r.now }, BackoffMin: time.Millisecond, BackoffMax: 4 * time.Millisecond}
	for _, m := range mod {
		m(&cfg)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://glidepath.invalid"
	}
	b, err := triggers.NewBridge(r.gw, r.ledger, cfg, r.m, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

// ---- once --------------------------------------------------------------------------------------------------------------------

func TestAnEventLaunchesTheMatchingAgentOnce(t *testing.T) {
	gw := newGW(triage(map[string]string{"subject.content.outcome": "failure"}, 0), nightly())
	r := newRig(t, gw, "")
	got, err := r.b.Handle(context.Background(), finished("e1", "pr-1", "failure"), "stream")
	if err != nil || len(got) != 1 || got[0].Result != "launched" || !got[0].New || got[0].Session == "" || got[0].Run == "" {
		t.Fatalf("%+v %v", got, err)
	}
	// the same event again (a replay on the same process, and then a restart with a new bridge on the same ledger)
	for i, b := range []*triggers.Bridge{r.b, r.bridge("", func(c *triggers.BridgeConfig) { c.Now = func() time.Time { return t0.Add(time.Hour) } })} {
		got, err = b.Handle(context.Background(), finished("e1", "pr-1", "failure"), "stream")
		if err != nil || (i == 0 && len(got) != 0) || (i == 1 && (len(got) != 1 || got[0].Result != "deduped" || got[0].New || !got[0].Replay || got[0].Session == "")) {
			t.Fatalf("%d: %+v %v", i, got, err)
		}
	}
	// the same pipelinerun under another event id (the sweep after the stream): the key is the same, the ledger says it is done
	got, _ = r.bridge("").Handle(context.Background(), finished("reconcile:gate-api/pr-1", "pr-1", "failure"), "reconcile")
	if len(got) != 1 || got[0].Result != "deduped" || got[0].New || got[0].Replay {
		t.Fatalf("%+v", got)
	}
	if gw.nOpens() != 1 || gw.nLaunch() != 1 {
		t.Fatalf("%d opens, %d launches", gw.nOpens(), gw.nLaunch())
	}
	// an event that does not match: not launched, but kept as history
	got, _ = r.b.Handle(context.Background(), finished("e2", "pr-2", "success"), "stream")
	if len(got) != 0 || gw.nLaunch() != 1 || len(gw.events) != 2 {
		t.Fatalf("%+v launches %d events %d", got, gw.nLaunch(), len(gw.events))
	}
	// the run got a summary of the event and the definition's instructions, never the raw event
	if r.m.Count("outcome/triage-agent/stream/launched") != 1 || r.m.Count("outcome/triage-agent/stream/deduped") != 1 || r.m.Count("outcome/triage-agent/reconcile/deduped") != 1 {
		t.Fatalf("%v", r.m.C)
	}
}

func TestTheStormBrakeAndTheHold(t *testing.T) {
	gw := newGW(triage(nil, 2))
	r := newRig(t, gw, "")
	var results []string
	for i := 0; i < 4; i++ {
		got, _ := r.b.Handle(context.Background(), finished(fmt.Sprint("e", i), fmt.Sprint("pr-", i), "failure"), "stream")
		results = append(results, got[0].Result)
	}
	if strings.Join(results, ",") != "launched,launched,braked,braked" || gw.nOpens() != 2 {
		t.Fatalf("%v %d", results, gw.nOpens())
	}
	// a braked event is decided: no later path launches it either
	got, _ := r.b.Handle(context.Background(), finished("reconcile:gate-api/pr-3", "pr-3", "failure"), "reconcile")
	if got[0].Result != "deduped" || gw.nOpens() != 2 {
		t.Fatalf("%+v", got)
	}
	// an hour later the window is open again
	r.now = t0.Add(61 * time.Minute)
	got, _ = r.b.Handle(context.Background(), finished("e9", "pr-9", "failure"), "stream")
	if got[0].Result != "launched" {
		t.Fatalf("%+v", got)
	}
	// R024 is held; any other refusal is refused; both are decided and kept (a fresh agent: the brake above is spent)
	gw = newGW(triage(nil, 100))
	r = newRig(t, gw, "")
	gw.holdRule["triage-agent"] = "R024"
	got, _ = r.b.Handle(context.Background(), finished("e10", "pr-10", "failure"), "stream")
	if got[0].Result != "held" || got[0].Rule != "R024" {
		t.Fatalf("%+v", got)
	}
	gw.holdRule["triage-agent"] = "R001"
	got, _ = r.b.Handle(context.Background(), finished("e11", "pr-11", "failure"), "stream")
	if got[0].Result != "refused" {
		t.Fatalf("%+v", got)
	}
	row, _ := r.ledger.Get(context.Background(), "triage-agent", "gate-api.pipelinerun.finished/pr-10")
	if row == nil || row.State != "held" {
		t.Fatalf("%+v", row)
	}
}

func TestAFailureForOneAgentDoesNotSkipTheOthers(t *testing.T) {
	a, b := triage(nil, 0), triage(nil, 0)
	b.Name = "another-agent"
	gw := newGW(a, b)
	gw.panicOn["another-agent"] = true
	r := newRig(t, gw, "")
	got, err := r.b.Handle(context.Background(), finished("e1", "pr-1", "failure"), "stream")
	if err != nil || len(got) != 2 || got[0].Agent != "another-agent" || got[0].Result != "launch-failed" || got[1].Agent != "triage-agent" || got[1].Result != "launched" {
		t.Fatalf("%+v %v", got, err)
	}
	gw.panicOn["another-agent"] = false
	gw.openErr = errors.New("core down")
	got, _ = r.b.Handle(context.Background(), finished("e2", "pr-2", "failure"), "stream")
	if got[0].Result != "launch-failed" || got[1].Result != "launch-failed" {
		t.Fatalf("%+v", got)
	}
	gw.openErr = nil
	gw.launchOK = false
	got, _ = r.b.Handle(context.Background(), finished("e3", "pr-3", "failure"), "stream")
	if got[0].Result != "launch-failed" || got[0].Session == "" {
		t.Fatalf("%+v", got)
	}
}

// ---- what a crash leaves, and the repair --------------------------------------------------------------------------------------

// The trigger crash-point table (docs/admin/go-gateway.md). Each case builds what the database holds when the process died at that
// point, starts a second process, and shows the trigger fires once.
func TestACrashAtEachPointLeavesATriggerThatFiresOnce(t *testing.T) {
	ctx := context.Background()
	env := finished("e1", "pr-1", "failure")
	key := "gate-api.pipelinerun.finished/pr-1"
	task := triggers.EventTaskID("triage-agent", key)
	claim := func(r *rig) {
		if ok, err := r.ledger.Claim(ctx, "triage-agent", key, "stream", "e1", t0, triggers.StaleBefore(t0, t0)); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	restart := func(r *rig) *triggers.Bridge {
		// the new process starts later than the dead one's claim
		r.now = t0.Add(10 * time.Second)
		return r.bridge("")
	}
	cases := []struct {
		name  string
		build func(r *rig)
		want  string // what the second process decides
		opens int    // opens in all
		runs  int    // launches in all
	}{
		{"dies after the claim, before the session is opened", func(r *rig) { claim(r) }, "launched", 1, 1},
		{"dies after the session is opened (and saved), before it is launched", func(r *rig) {
			claim(r)
			r.gw.sessions["triage-agent/"+task] = "s-orphan"
		}, "launched", 1, 1}, // the repair: the live session of this key has no run, so it is launched, not deduped away
		{"dies after the run is launched, before the decision is written", func(r *rig) {
			claim(r)
			r.gw.sessions["triage-agent/"+task] = "s-orphan"
			r.gw.hasRun["s-orphan"] = true
		}, "deduped", 1, 0}, // a live run of this task: one run, and the row says so
		{"dies after the decision, before the cursor moves", func(r *rig) {
			claim(r)
			_ = r.ledger.Decide(ctx, "triage-agent", key, "launched", "s-1", "r-1")
		}, "", 0, 0}, // decided: the replay is a dedupe and opens nothing
	}
	for _, c := range cases {
		gw := newGW(triage(nil, 0))
		r := newRig(t, gw, "")
		c.build(r)
		got, err := restart(r).Handle(ctx, env, "recent")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		switch {
		case c.want == "" && (len(got) != 1 || got[0].Result != "deduped" || got[0].New):
			t.Errorf("%s: %+v", c.name, got)
		case c.want != "" && (len(got) != 1 || got[0].Result != c.want || !got[0].New):
			t.Errorf("%s: %+v", c.name, got)
		}
		if gw.nOpens() != c.opens || gw.nLaunch() != c.runs {
			t.Errorf("%s: %d opens, %d launches", c.name, gw.nOpens(), gw.nLaunch())
		}
		// a third process, and the same event again: nothing more happens
		r.now = t0.Add(time.Hour)
		again, _ := r.bridge("").Handle(ctx, env, "stream")
		if len(again) != 1 || again[0].New || gw.nLaunch() > 1 {
			t.Errorf("%s: a replay fired again: %+v %d", c.name, again, gw.nLaunch())
		}
	}
	// a claim of this very process, not yet decided, is not an orphan: nobody else takes it
	gw := newGW(triage(nil, 0))
	r := newRig(t, gw, "")
	if ok, _ := r.ledger.Claim(ctx, "triage-agent", key, "stream", "e1", t0.Add(time.Second), triggers.StaleBefore(t0, t0.Add(time.Second))); !ok {
		t.Fatal("not claimed")
	}
	r.now = t0.Add(2 * time.Second)
	got, _ := r.b.Handle(ctx, env, "stream")
	if len(got) != 1 || got[0].New || got[0].Result != "deduped" || gw.nOpens() != 0 {
		t.Fatalf("%+v", got)
	}
	// ... until it is older than the stale limit
	r.now = t0.Add(triggers.Stale + 2*time.Second)
	got, _ = r.b.Handle(ctx, finished("e1b", "pr-1", "failure"), "stream")
	if len(got) != 1 || !got[0].New || got[0].Result != "launched" {
		t.Fatalf("%+v", got)
	}
}

// The ledger failing: nothing is fired for an event whose claim failed, the event is not marked seen (so it is handled when the
// ledger is back), and the error tells the caller not to move the cursor. A decision that cannot be written is the same.
func TestALedgerFailureIsNotAnEventHandled(t *testing.T) {
	ctx := context.Background()
	gw := newGW(triage(nil, 0))
	r := newRig(t, gw, "")
	r.ledger.Fail("claim", errors.New("connection lost"))
	if got, err := r.b.Handle(ctx, finished("e1", "pr-1", "failure"), "stream"); err == nil || len(got) != 0 || gw.nOpens() != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	got, err := r.b.Handle(ctx, finished("e1", "pr-1", "failure"), "stream")
	if err != nil || got[0].Result != "launched" {
		t.Fatalf("not retried: %+v %v", got, err)
	}
	r.ledger.Fail("decide", errors.New("connection lost"))
	if _, err := r.b.Handle(ctx, finished("e2", "pr-2", "failure"), "stream"); err == nil {
		t.Fatal("a decision that could not be written was reported as handled")
	}
	// the run exists and the row is `claimed`: after a restart the claim is an orphan, answered by the live run, which is not launched twice
	n := gw.nLaunch()
	r.now = t0.Add(time.Minute)
	got, _ = r.bridge("").Handle(ctx, finished("e2", "pr-2", "failure"), "stream")
	if got[0].Result != "deduped" || gw.nLaunch() != n {
		t.Fatalf("%+v launches %d, was %d", got, gw.nLaunch(), n)
	}
}

// ---- what an event may be --------------------------------------------------------------------------------------------------------

func TestMalformedAndOversizedEventsAreDroppedAndCounted(t *testing.T) {
	gw := newGW(triage(nil, 0))
	r := newRig(t, gw, "")
	long := strings.Repeat("x", 300)
	bad := map[string]map[string]any{
		"no id":                          {"app": "gate-api", "routingKey": "gate-api.pipelinerun.finished"},
		"an id of the wrong type":        {"id": 5},
		"an id with a space":             {"id": "a b", "routingKey": "gate-api.pipelinerun.finished"},
		"an id with a newline":           {"id": "a\nb"},
		"an id too long":                 {"id": strings.Repeat("a", 201)},
		"a routing key too long":         {"id": "e1", "routingKey": long},
		"an app of the wrong type":       {"id": "e1", "app": []any{"x"}},
		"an event that is not an object": {"id": "e1", "routingKey": "gate-api.pipelinerun.finished", "event": "oops"},
		"a subject id too long":          {"id": "e1", "app": "gate-api", "routingKey": "gate-api.pipelinerun.finished", "event": map[string]any{"subject": map[string]any{"id": long}}},
		"a received time too long":       {"id": "e1", "routingKey": "x", "receivedAt": long},
	}
	for name, env := range bad {
		got, err := r.b.Handle(context.Background(), env, "stream")
		if err != nil || len(got) != 0 {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	if gw.nOpens() != 0 || len(gw.events) != 0 {
		t.Fatalf("a dropped event reached the gateway: %d opens, %d events", gw.nOpens(), len(gw.events))
	}
	drops := 0
	for _, k := range []string{"bad-id", "oversize", "malformed", "bad-key"} {
		drops += r.m.Count("dropped/" + k)
	}
	if drops != len(bad) {
		t.Fatalf("%d dropped, %d counted: %v", len(bad), drops, r.m.C)
	}
	// values the event carries are not trusted to be what the summary expects: odd shapes do not panic
	odd := finished("e-odd", "pr-odd", "failure")
	odd["event"].(map[string]any)["subject"] = map[string]any{"id": "pr-odd", "content": map[string]any{"outcome": "failure", "n": 1.5, "b": true, "nested": map[string]any{"a": 1}, "big": strings.Repeat("z", 400)}}
	got, err := r.b.Handle(context.Background(), odd, "stream")
	if err != nil || len(got) != 1 || got[0].Result != "launched" {
		t.Fatalf("%+v %v", got, err)
	}
	sum := triggers.Summarize(odd)
	content := sum["subject"].(map[string]any)["content"].(map[string]any)
	if _, ok := content["nested"]; ok || content["big"] != nil || content["outcome"] != "failure" {
		t.Fatalf("%v", content)
	}
	// the summary is bounded: many large values leave the subject within its limit
	many := map[string]any{}
	for i := 0; i < 40; i++ {
		many[fmt.Sprint("k", i)] = strings.Repeat("v", 290)
	}
	odd["event"].(map[string]any)["subject"] = map[string]any{"id": "pr", "content": many}
	raw := fmt.Sprint(triggers.Summarize(odd)["subject"])
	if len(raw) > 2500 {
		t.Fatalf("the subject is %d bytes", len(raw))
	}
}

func TestTheSeenSetIsBounded(t *testing.T) {
	gw := newGW(triage(map[string]string{"subject.content.outcome": "never"}, 0))
	r := newRig(t, gw, "")
	for i := 0; i < triggers.SeenKept+50; i++ {
		_, _ = r.b.Handle(context.Background(), finished(fmt.Sprint("e", i), "pr", "success"), "stream")
	}
	// the oldest has been forgotten: seen again, it is handled again (and, matching nothing, only recorded)
	n := len(gw.events)
	_, _ = r.b.Handle(context.Background(), finished("e0", "pr", "success"), "stream")
	if len(gw.events) != n+1 {
		t.Fatal("the oldest id is still remembered")
	}
}

// ---- matching ----------------------------------------------------------------------------------------------------------------------

func TestTopicMatchingIsLinear(t *testing.T) {
	cases := []struct {
		pat, key string
		want     bool
	}{
		{"gate-api.pipelinerun.finished", "gate-api.pipelinerun.finished", true},
		{"*.pipelinerun.finished", "gate-api.pipelinerun.finished", true},
		{"#.finished", "gate-api.pipelinerun.finished", true},
		{"#", "", true},
		{"#", "a.b.c", true},
		{"a.#", "a", true},
		{"a.#.b", "a.b", true},
		{"a.#.b", "a.x.y.b", true},
		{"a.*", "a", false},
		{"a.*", "a.b.c", false},
		{"*", "", true}, // one word, and it is empty (Python: "".split(".") is [""])
		{"", "", true},
		{"a.b", "a.b.c", false},
		{"a.*.c", "a.b.c", true},
		{"#.#.#", "a", true},
	}
	for _, c := range cases {
		if got := triggers.AMQPTopicMatch(c.pat, c.key); got != c.want {
			t.Errorf("%q vs %q: %v", c.pat, c.key, got)
		}
	}
	// Python's recursion is exponential in the number of '#': this pattern and key finish at once here
	start := time.Now()
	pat := strings.Repeat("#.", 40) + "x"
	key := strings.Repeat("w.", 300) + "y"
	if triggers.AMQPTopicMatch(pat, key) || time.Since(start) > time.Second {
		t.Fatalf("matched, or took %v", time.Since(start))
	}
}

func TestTheLedgerKey(t *testing.T) {
	env := finished("e1", "pr-1", "failure")
	if k := triggers.TriggerKey(env); k != "gate-api.pipelinerun.finished/pr-1" {
		t.Fatal(k)
	}
	env["app"] = "other"
	if k := triggers.TriggerKey(env); k != "event:e1" {
		t.Fatalf("an event whose routing key is not its app's pipelinerun is keyed by its id: %s", k)
	}
	delete(env, "id")
	if k := triggers.TriggerKey(env); k != "" {
		t.Fatalf("an event with no id has no key: %s", k)
	}
	big := finished("e1", "pr-1", "failure")
	big["id"] = strings.Repeat("a", 200)
	big["routingKey"] = "gate-api.pipelinerun.finished." + strings.Repeat("b", 250)
	big["event"].(map[string]any)["subject"].(map[string]any)["id"] = strings.Repeat("c", 250)
	if k := triggers.TriggerKey(big); k != "" {
		t.Fatalf("a key over the bound: %d", len(k))
	}
}

func TestSlotsAndTaskIDsAreStable(t *testing.T) {
	if triggers.SlotKey("24h", 20000) != "slot:24h:20000" || triggers.EventTaskID("a", "k") != triggers.EventTaskID("a", "k") ||
		triggers.EventTaskID("a", "k") == triggers.EventTaskID("b", "k") || !strings.HasPrefix(triggers.SlotTaskID("a", 1), "t-") || len(triggers.SlotTaskID("a", 1)) != 14 {
		t.Fatal("task ids")
	}
	n, start := triggers.SlotOf(time.Date(2026, 11, 14, 5, 30, 0, 0, time.UTC), 6*time.Hour)
	if start != time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC) || start.Unix()/(6*3600) != n {
		t.Fatal(n, start)
	}
}

// ---- glidepath-api -----------------------------------------------------------------------------------------------------------------

// api is a glidepath-api: recent events, a stream, run records; it counts what it is asked and what it is sent.
type api struct {
	mu      sync.Mutex
	srv     *httptest.Server
	recent  []string // JSON of each event
	stream  func(w http.ResponseWriter, r *http.Request)
	runs    string
	auth    []string
	lastIDs []string
	since   []string
	calls   map[string]int
}

func newAPI(t *testing.T) *api {
	a := &api{calls: map[string]int{}}
	mux := http.NewServeMux()
	note := func(r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.calls[r.URL.Path]++
		a.auth = append(a.auth, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Glidepath-Authorization"))
		a.lastIDs = append(a.lastIDs, r.Header.Get("Last-Event-ID"))
		a.since = append(a.since, r.URL.Query().Get("since"))
	}
	mux.HandleFunc("/v1/events/recent", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		a.mu.Lock()
		body := `{"events":[` + strings.Join(a.recent, ",") + `]}`
		a.mu.Unlock()
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		a.mu.Lock()
		f := a.stream
		a.mu.Unlock()
		if f == nil {
			http.Error(w, "no", 500)
			return
		}
		f(w, r)
	})
	mux.HandleFunc("/v1/runs", func(w http.ResponseWriter, r *http.Request) {
		note(r)
		a.mu.Lock()
		body := a.runs
		a.mu.Unlock()
		_, _ = io.WriteString(w, body)
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

func sse(w http.ResponseWriter, evs ...string) {
	w.Header().Set("content-type", "text/event-stream")
	for _, e := range evs {
		_, _ = io.WriteString(w, e)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func ev(id string, e map[string]any) string {
	raw, _ := jsonMarshal(e)
	return "id: " + id + "\ndata: " + string(raw) + "\n\n"
}

func TestTheStreamResumesFromTheCursorAndHandlesWhatItCarries(t *testing.T) {
	gw := newGW(triage(map[string]string{"subject.content.outcome": "failure"}, 0))
	a := newAPI(t)
	r := newRig(t, gw, a.srv.URL)
	id1, id2, id3, id4 := eventID(t0.Add(-4*time.Minute), "aa"), eventID(t0.Add(-3*time.Minute), "bb"), eventID(t0.Add(-2*time.Minute), "cc"), eventID(t0.Add(-time.Minute), "dd")
	// the saved cursor is id1; glidepath-api holds id2 as "recent"; the stream then carries id3 and id4
	if _, err := r.ledger.Cursor(context.Background(), triggers.Source, t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	_ = r.ledger.Advance(context.Background(), triggers.Source, id1)
	raw2, _ := jsonMarshal(finished(id2, "pr-2", "failure"))
	a.recent = []string{string(raw2)}
	a.stream = func(w http.ResponseWriter, _ *http.Request) {
		sse(w, ": ping\n\n", ev(id3, finished(id3, "pr-3", "failure")), "event: other\ndata: ignored\n\n", ev(id4, finished(id4, "pr-4", "success")))
	}
	n, err := r.b.RunOnce(context.Background())
	if err != nil || n != 2 { // two events; the data-only block that is not JSON is dropped
		t.Fatalf("%d %v", n, err)
	}
	if gw.nLaunch() != 2 {
		t.Fatalf("%d launches", gw.nLaunch())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.since[0] != id1 || a.lastIDs[1] != id2 {
		t.Fatalf("catch-up since %q, stream Last-Event-ID %q: want %s then %s", a.since[0], a.lastIDs[1], id1, id2)
	}
	for _, h := range a.auth {
		if h != "Bearer secret-token|Bearer secret-token" {
			t.Fatalf("the gateway's own token goes in both headers: %q", h)
		}
	}
	cur, _ := r.ledger.Cursor(context.Background(), triggers.Source, t0)
	if cur.LastID == nil || *cur.LastID != id4 {
		t.Fatalf("the cursor is %v", cur.LastID)
	}
	if r.m.Count("resume/caught-up") != 1 || r.m.Count("events_received/recent") != 1 || r.m.Count("events_received/stream") != 2 {
		t.Fatalf("%v", r.m.C)
	}
}

func TestTheCursorOnlyFollowsIDsItCanTrust(t *testing.T) {
	gw := newGW(triage(nil, 0))
	a := newAPI(t)
	r := newRig(t, gw, a.srv.URL)
	good := eventID(t0.Add(-time.Minute), "aa")
	future := eventID(t0.Add(time.Hour), "ff")
	a.stream = func(w http.ResponseWriter, _ *http.Request) {
		sse(w, ev("zzzzzzzzzz", finished("zzzzzzzzzz", "pr-1", "failure")), // handled, but "zzz" is no glidepath-api id: the cursor stays
			ev(future, finished(future, "pr-2", "failure")), // from the future: the cursor stays
			ev(good, finished(good, "pr-3", "failure")))
	}
	if _, err := r.b.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	cur, _ := r.ledger.Cursor(context.Background(), triggers.Source, t0)
	if cur.LastID == nil || *cur.LastID != good {
		t.Fatalf("the cursor is %v, want %s: a source cannot push it past what is to come", cur.LastID, good)
	}
	if gw.nLaunch() != 3 {
		t.Fatalf("the events were still handled: %d", gw.nLaunch())
	}
	// and never backward
	older := eventID(t0.Add(-time.Hour), "00")
	a.stream = func(w http.ResponseWriter, _ *http.Request) { sse(w, ev(older, finished(older, "pr-4", "failure"))) }
	_, _ = r.b.RunOnce(context.Background())
	if cur, _ = r.ledger.Cursor(context.Background(), triggers.Source, t0); *cur.LastID != good {
		t.Fatalf("%v", *cur.LastID)
	}
}

// A ledger failure ends the stream and leaves the cursor at the last event fully handled, so the resume sees the failed event again.
func TestALedgerFailureEndsTheStreamAndKeepsTheCursor(t *testing.T) {
	gw := newGW(triage(nil, 0))
	a := newAPI(t)
	r := newRig(t, gw, a.srv.URL)
	id1, id2, id3 := eventID(t0.Add(-3*time.Minute), "aa"), eventID(t0.Add(-2*time.Minute), "bb"), eventID(t0.Add(-time.Minute), "cc")
	a.stream = func(w http.ResponseWriter, _ *http.Request) {
		sse(w, ev(id1, finished(id1, "pr-1", "failure")))
		for i := 0; gw.nLaunch() < 1 && i < 2000; i++ { // until the first event is handled (not a fixed wait)
			time.Sleep(time.Millisecond)
		}
		r.ledger.Fail("claim", errors.New("connection lost"))
		sse(w, ev(id2, finished(id2, "pr-2", "failure")), ev(id3, finished(id3, "pr-3", "failure")))
	}
	n, err := r.b.RunOnce(context.Background())
	if err == nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	cur, _ := r.ledger.Cursor(context.Background(), triggers.Source, t0)
	if *cur.LastID != id1 || gw.nLaunch() != 1 {
		t.Fatalf("cursor %s, %d launches", *cur.LastID, gw.nLaunch())
	}
}

func TestOversizeLinesEventsAndGarbageAreDroppedAndTheStreamGoesOn(t *testing.T) {
	gw := newGW(triage(nil, 0))
	a := newAPI(t)
	r := newRig(t, gw, a.srv.URL)
	good := eventID(t0.Add(-time.Minute), "aa")
	hugeLine := "data: " + strings.Repeat("x", triggers.MaxLine+10) + "\n\n"
	manyLines := ""
	for i := 0; i < 20; i++ {
		manyLines += "data: " + strings.Repeat("y", 20000) + "\n"
	}
	a.stream = func(w http.ResponseWriter, _ *http.Request) {
		sse(w, hugeLine, manyLines+"\n", "data: {not json\n\n", "data: [1,2]\n\n", "id: "+strings.Repeat("i", 300)+"\ndata: {}\n\n",
			"data: null\n\n", "\n\n\n", ev(good, finished(good, "pr-1", "failure")), "id: half\ndata: {\"id\":\"half\"")
	}
	n, err := r.b.RunOnce(context.Background())
	if err != nil || gw.nLaunch() != 1 {
		t.Fatalf("%d events, %v, %d launches", n, err, gw.nLaunch())
	}
	if r.m.Count("dropped/oversize") != 3 || r.m.Count("dropped/malformed") < 3 {
		t.Fatalf("%v", r.m.C)
	}
}

func TestTheBridgeNeverFollowsARedirect(t *testing.T) {
	var hits atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"events":[]}`)
	}))
	defer other.Close()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer front.Close()
	gw := newGW(triage(nil, 0))
	r := newRig(t, gw, front.URL)
	if _, err := r.b.GetJSON(context.Background(), "/v1/runs", nil); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("%v", err)
	}
	if _, err := r.b.RunOnce(context.Background()); err == nil {
		t.Fatal("a redirected stream was followed")
	}
	if hits.Load() != 0 {
		t.Fatalf("another host was sent %d requests, with the gateway's token", hits.Load())
	}
}

func TestTheBridgeCapsAnswersAndNamesNoSecret(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"runs":[`+strings.Repeat(`{"a":1},`, triggers.MaxJSON/8+10)+`{}]}`)
	}))
	defer big.Close()
	gw := newGW(triage(nil, 0))
	r := newRig(t, gw, big.URL)
	if _, err := r.b.GetJSON(context.Background(), "/v1/runs", nil); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("%v", err)
	}
	// an unreadable or empty token file is an error that names neither the token nor the file's contents
	for _, content := range []string{"", "  \n"} {
		_ = os.WriteFile(r.tok, []byte(content), 0o600)
		if _, err := r.b.GetJSON(context.Background(), "/v1/runs", nil); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("%v", err)
		}
	}
	_ = os.WriteFile(r.tok, []byte("bad\nvalue"), 0o600)
	if _, err := r.b.GetJSON(context.Background(), "/v1/runs", nil); err == nil || strings.Contains(err.Error(), "bad") {
		t.Fatalf("a token that cannot be a header value: %v", err)
	}
	for _, base := range []string{"ftp://x", "http://", "http://u:p@host", "http://host?x=1", "nonsense", "http://host#f", ""} {
		if _, err := triggers.NewBridge(gw, r.ledger, triggers.BridgeConfig{BaseURL: base, TokenFile: r.tok}, nil, nil); err == nil {
			t.Errorf("%q was accepted as glidepath-api's URL", base)
		}
	}
}

// The reconnect delay is bounded and jittered: 1, 2, 4, 4, 4 ... (scaled down here), starting over after a stream that delivered.
func TestTheReconnectBackoffIsBounded(t *testing.T) {
	gw := newGW(triage(nil, 0))
	a := newAPI(t) // the stream answers 500
	r := newRig(t, gw, a.srv.URL)
	var waits []time.Duration
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	b := r.bridge(a.srv.URL, func(c *triggers.BridgeConfig) {
		c.BackoffMin, c.BackoffMax = 100*time.Millisecond, 400*time.Millisecond
		c.Wait = func(ctx context.Context, d time.Duration) bool {
			mu.Lock()
			defer mu.Unlock()
			waits = append(waits, d)
			if len(waits) == 6 {
				// a stream that delivers an event starts the backoff over
				good := eventID(t0.Add(-time.Minute), "aa")
				a.mu.Lock()
				a.stream = func(w http.ResponseWriter, _ *http.Request) { sse(w, ev(good, finished(good, "pr-1", "failure"))) }
				a.mu.Unlock()
			}
			if len(waits) == 9 {
				cancel()
				return false
			}
			return ctx.Err() == nil
		}
	})
	b.Run(ctx)
	within := func(d, base time.Duration) bool { return d >= base && d <= base+base/5+1 }
	want := []time.Duration{100, 200, 400, 400, 400}
	for i, w := range want {
		if !within(waits[i], w*time.Millisecond) {
			t.Fatalf("wait %d is %v, want about %v: %v", i, waits[i], w*time.Millisecond, waits)
		}
	}
	if !within(waits[7], 100*time.Millisecond) {
		t.Fatalf("a stream that delivered did not start the backoff over: %v", waits)
	}
}

// A silent stream is dropped after the idle time and the bridge reconnects; a slow gateway call cannot hold the stream past its
// context.
func TestASilentStreamIsDroppedAndAStalledGatewayDoesNotHoldTheBridge(t *testing.T) {
	gw := newGW(triage(nil, 0))
	a := newAPI(t)
	r := newRig(t, gw, a.srv.URL)
	a.stream = func(w http.ResponseWriter, req *http.Request) {
		sse(w, ": hello\n\n")
		<-req.Context().Done()
	}
	b := r.bridge(a.srv.URL, func(c *triggers.BridgeConfig) { c.Idle = 80 * time.Millisecond })
	start := time.Now()
	if _, err := b.RunOnce(context.Background()); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("%v after %v: a silent stream ends with an error so the loop reconnects", err, time.Since(start))
	}
	// a gateway call that never returns on its own ends with the context
	good := eventID(t0.Add(-time.Minute), "aa")
	a.stream = func(w http.ResponseWriter, _ *http.Request) { sse(w, ev(good, finished(good, "pr-1", "failure"))) }
	gw.onOpen = func(ctx context.Context) { <-ctx.Done() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _, _ = r.bridge(a.srv.URL).RunOnce(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a stalled gateway call held the bridge past its context")
	}
}

// ---- reconcile -----------------------------------------------------------------------------------------------------------------------

func runsJSON(rows ...string) string { return `{"runs":[` + strings.Join(rows, ",") + `]}` }

func run(name, app, status string, end time.Time) string {
	return fmt.Sprintf(`{"name":%q,"app":%q,"namespace":"ns","status":%q,"pipeline":"build","endTime":%q}`, name, app, status, end.Format("2006-01-02T15:04:05.123456789Z"))
}

func TestTheSweepLaunchesWhatNoEventDeliveredOnceAndOnlyWithinItsBounds(t *testing.T) {
	ctx := context.Background()
	gw := newGW(triage(map[string]string{"subject.content.outcome": "failure"}, 0))
	a := newAPI(t)
	r := newRig(t, gw, a.srv.URL)
	// the ledger first ran an hour ago
	cur, _ := r.ledger.Cursor(ctx, triggers.Source, t0.Add(-time.Hour))
	a.runs = runsJSON(
		run("pr-old", "gate-api", "failed", t0.Add(-2*time.Hour)),                                 // finished before the floor: never reconciled
		run("pr-new", "gate-api", "failed", t0.Add(-10*time.Minute)),                              // missed
		run("pr-seen", "gate-api", "failed", t0.Add(-20*time.Minute)),                             // its event arrived
		run("pr-fresh", "gate-api", "failed", t0.Add(-30*time.Second)),                            // too fresh: left to its event
		run("pr-ok", "gate-api", "succeeded", t0.Add(-11*time.Minute)),                            // does not match the trigger
		`{"name":"Bad Name","app":"gate-api","status":"failed","endTime":"2026-11-14T11:50:00Z"}`, // not a name
		`{"app":"gate-api","status":"failed"}`, `"junk"`, `{"name":"x","app":"gate-api","endTime":"not a time"}`)
	if _, err := r.b.Handle(ctx, finished("e-seen", "pr-seen", "failure"), "stream"); err != nil {
		t.Fatal(err)
	}
	rc := triggers.NewReconciler(r.b, time.Minute, 24)
	fresh, err := rc.Sweep(ctx)
	if err != nil || len(fresh) != 1 || fresh[0].Result != "launched" || gw.nLaunch() != 2 {
		t.Fatalf("%+v %v launches %d floor %v", fresh, err, gw.nLaunch(), cur.Floor)
	}
	if r.m.Count("missed/triage-agent/launched") != 1 || r.m.Count("sweep/ok") != 1 || r.m.Gauge("sweep_ok") == 0 {
		t.Fatalf("%v", r.m.C)
	}
	// the history names the rebuilt event
	last := gw.events[len(gw.events)-1]
	if last["id"] != "reconcile:gate-api/pr-new" {
		t.Fatalf("%v", last)
	}
	// a second sweep launches nothing more
	if fresh, _ = rc.Sweep(ctx); len(fresh) != 0 || gw.nLaunch() != 2 {
		t.Fatalf("%+v", fresh)
	}
	// the sweep asked for the apps a trigger can match, the window and the most it may have
	a.mu.Lock()
	n := a.calls["/v1/runs"]
	a.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d reads", n)
	}
	// glidepath-api failing is a failed sweep, not a launch
	a.mu.Lock()
	a.runs = "not json"
	a.mu.Unlock()
	if _, err := rc.Sweep(ctx); err == nil || r.m.Count("sweep/failed") != 1 {
		t.Fatalf("%v %v", err, r.m.C)
	}
	// no trigger can match a finished pipelinerun: the sweep reads nothing
	gw.defs = map[string]*definitions.Definition{"x": {Name: "x", Triggers: []definitions.Trigger{{Type: "event", BindingKey: "gate-api.pipelinerun.started"}}}}
	if got, apps := triggers.WatchedApps(gw.defs); got || len(apps) != 0 {
		t.Fatal(got, apps)
	}
}

func TestWatchedApps(t *testing.T) {
	d := map[string]*definitions.Definition{
		"a": {Triggers: []definitions.Trigger{{Type: "event", BindingKey: "gate-api.pipelinerun.finished"}}},
		"b": {Triggers: []definitions.Trigger{{Type: "event", BindingKey: "other.pipelinerun.*"}, {Type: "interval", Every: "1h", Interval: time.Hour}}},
		"c": {Triggers: []definitions.Trigger{{Type: "event", BindingKey: "x.deployment.finished"}}},
	}
	any_, apps := triggers.WatchedApps(d)
	if !any_ || len(apps) != 2 || !apps["gate-api"] || !apps["other"] {
		t.Fatal(any_, apps)
	}
	d["w"] = &definitions.Definition{Triggers: []definitions.Trigger{{Type: "event", BindingKey: "#.finished"}}}
	if any_, apps = triggers.WatchedApps(d); !any_ || len(apps) != 0 {
		t.Fatal("a wildcard app reads the whole fleet", apps)
	}
	if env, ok := triggers.EnvelopeFromRun(map[string]any{"name": "pr-1", "app": "gate-api", "status": "weird"}); !ok || env["event"].(map[string]any)["subject"].(map[string]any)["content"].(map[string]any)["outcome"] != "error" {
		t.Fatal(env)
	}
}

// ---- the scheduler -----------------------------------------------------------------------------------------------------------------------

func TestTheSchedulerFiresASlotOnceAcrossRestartsAndRecordsWhatWasMissed(t *testing.T) {
	ctx := context.Background()
	gw := newGW(nightly())
	ledger := &triggers.MemoryLedger{}
	m := &triggers.Recorder{}
	now := time.Date(2026, 11, 14, 0, 2, 0, 0, time.UTC) // two minutes into the slot that began at 00:00
	clock := func() time.Time { return now }
	// the ledger first saw this agent a few days ago: the slots since then were missed while the gateway was down
	_, _ = ledger.Cursor(ctx, "schedule:nightly-reviewer", time.Date(2026, 11, 10, 6, 0, 0, 0, time.UTC))
	s := triggers.NewScheduler(gw, ledger, clock, m, nil)
	done := s.Tick(ctx)
	var fired, missed int
	for _, d := range done {
		if d.Result == "launched" {
			fired++
		}
		if d.Result == "missed" {
			missed++
		}
	}
	if fired != 1 || missed != 3 || gw.nLaunch() != 1 || m.Count("slot_missed/nightly-reviewer") != 3 || m.Gauge("slot_lag/nightly-reviewer") != 120 {
		t.Fatalf("%+v launches %d: %v %v", done, gw.nLaunch(), m.C, m.G)
	}
	// the same process, the next tick: nothing
	now = now.Add(30 * time.Second)
	if done = s.Tick(ctx); len(done) != 0 {
		t.Fatalf("%+v", done)
	}
	// a restart inside the grace window: the slot is in the ledger, nothing fires
	now = now.Add(time.Minute)
	s2 := triggers.NewScheduler(gw, ledger, clock, m, nil)
	if done = s2.Tick(ctx); len(done) != 0 || gw.nLaunch() != 1 {
		t.Fatalf("%+v launches %d", done, gw.nLaunch())
	}
	// after the grace window the slot is not fired late, and an earlier miss is not recorded twice
	now = time.Date(2026, 11, 14, 0, 11, 0, 0, time.UTC)
	if done = s2.Tick(ctx); len(done) != 0 {
		t.Fatalf("%+v", done)
	}
	// the next slot, a day on, fires; the same slot's task id (a hash) is the same for the same agent and slot
	now = time.Date(2026, 11, 15, 0, 1, 0, 0, time.UTC)
	if done = s2.Tick(ctx); len(done) != 1 || done[0].Result != "launched" || gw.nLaunch() != 2 {
		t.Fatalf("%+v", done)
	}
	firstSlot, _ := triggers.SlotOf(time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), 24*time.Hour)
	found := false
	for _, o := range gw.opens {
		found = found || o == "nightly-reviewer/"+triggers.SlotTaskID("nightly-reviewer", firstSlot)
	}
	if !found {
		t.Fatalf("%v", gw.opens)
	}
}

func TestTheSchedulerHoldsRefusesAndSurvivesAFailingAgent(t *testing.T) {
	ctx := context.Background()
	a, b, c := nightly(), nightly(), nightly()
	b.Name, c.Name = "b-agent", "c-agent"
	gw := newGW(a, b, c)
	gw.holdRule["nightly-reviewer"] = "R024"
	gw.panicOn["b-agent"] = true
	ledger := &triggers.MemoryLedger{}
	now := time.Date(2026, 11, 14, 0, 1, 0, 0, time.UTC)
	s := triggers.NewScheduler(gw, ledger, func() time.Time { return now }, nil, nil)
	done := s.Tick(ctx)
	res := map[string]string{}
	for _, d := range done {
		res[d.Agent] = d.Result
	}
	if res["nightly-reviewer"] != "held" || res["b-agent"] != "launch-failed" || res["c-agent"] != "launched" {
		t.Fatalf("%v", res)
	}
	// a ledger that fails is tried again on the next tick, within the grace window
	ledger2 := &triggers.MemoryLedger{}
	gw2 := newGW(nightly())
	ledger2.Fail("claim", errors.New("connection lost"))
	s = triggers.NewScheduler(gw2, ledger2, func() time.Time { return now }, nil, nil)
	if done = s.Tick(ctx); len(done) != 0 || gw2.nLaunch() != 0 {
		t.Fatalf("%+v", done)
	}
	if done = s.Tick(ctx); len(done) != 1 || done[0].Result != "launched" {
		t.Fatalf("%+v", done)
	}
	// only scheduled workload agents are on the clock
	other := nightly()
	other.Name, other.Kind = "e", "event"
	if got := triggers.NewScheduler(newGW(other), &triggers.MemoryLedger{}, func() time.Time { return now }, nil, nil).Tick(ctx); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

// A slot whose launch crashed after the open (a live session without a run): the repair launches it, once.
func TestTheSchedulerRepairsAnOpenedSlotThatWasNeverLaunched(t *testing.T) {
	ctx := context.Background()
	gw := newGW(nightly())
	ledger := &triggers.MemoryLedger{}
	now := time.Date(2026, 11, 14, 0, 1, 0, 0, time.UTC)
	slot, _ := triggers.SlotOf(now, 24*time.Hour)
	gw.sessions["nightly-reviewer/"+triggers.SlotTaskID("nightly-reviewer", slot)] = "s-orphan"
	// the dead process's claim, from before this one started
	_, _ = ledger.Claim(ctx, "nightly-reviewer", triggers.SlotKey("24h", slot), "schedule", "", now.Add(-time.Minute), now)
	s := triggers.NewScheduler(gw, ledger, func() time.Time { return now }, nil, nil)
	if done := s.Tick(ctx); len(done) != 1 || done[0].Result != "launched" || done[0].Session != "s-orphan" || gw.nLaunch() != 1 {
		t.Fatalf("%+v %d", done, gw.nLaunch())
	}
	if done := triggers.NewScheduler(gw, ledger, func() time.Time { return now.Add(time.Minute) }, nil, nil).Tick(ctx); len(done) != 0 || gw.nLaunch() != 1 {
		t.Fatalf("%+v", done)
	}
}
