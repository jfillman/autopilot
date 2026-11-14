package dispatch_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/runs"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/clearance/client"
)

// ---- the sweep and a spawn ----------------------------------------------------------------------------------------

// A child's run is being made (slow), the sweep fires: the real API server answers 404 ("Gone") for a run that does not
// exist yet, and the sweep must not settle the child as if its run had ended.
func TestTheSweepCannotSettleAChildWhoseRunIsBeingCreated(t *testing.T) {
	r, _ := treeRig(t, nil)
	f := runs.NewFake() // answers "Gone" for a run that does not exist, like the API server's 404
	var inCreate atomic.Bool
	f.OnCreate = func() { inCreate.Store(true); time.Sleep(400 * time.Millisecond) }
	r.g.SetRuns(f)
	var res *dispatch.Result
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		res = r.call(r.person("user:alice"), "s-0000a1", "run.spawn", spawnArgs("researcher", map[string]any{"tool_calls": 10}))
	}()
	for !inCreate.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	r.g.ExpireAt(context.Background(), t0.Add(time.Minute)) // the sweep fires during the slow create
	wg.Wait()
	if !res.OK {
		t.Fatalf("%v %v", res.Decision, *res.Error)
	}
	kid := r.store.Get(res.Data.(map[string]any)["session"].(string))
	kid.Lock()
	closed, outcome := kid.Closed, kid.Outcome
	kid.Unlock()
	p := r.store.Get("s-0000a1")
	if closed || outcome != nil || p.Reserved.ToolCalls != 10 || len(f.Live) != 1 {
		t.Fatalf("the sweep settled a child that was being made: closed=%v outcome=%v reserved=%+v", closed, outcome, p.Reserved)
	}
}

// The call's time runs out while the child is saved after its run was made: the agent gets the success it earned or the
// child is undone (run deleted, reservation back, one outcome), never the mix.
func TestASpawnWhoseTimeRunsOutDuringTheFinalSaveIsNeverAMix(t *testing.T) {
	r, _ := treeRig(t, nil)
	f := runs.NewFake()
	var created atomic.Bool
	f.OnCreate = func() { created.Store(true) }
	f.ReturnName = "r-00beef" // a name other than the one saved first, so the child is saved again after the run exists
	r.g.SetRuns(f)
	r.g.CallTimeout = 100 * time.Millisecond
	r.persist.onSave = func() {
		if created.Load() {
			time.Sleep(300 * time.Millisecond) // a slow database after the run exists
		}
	}
	res := r.call(r.person("user:alice"), "s-0000a1", "run.spawn", spawnArgs("researcher", map[string]any{"tool_calls": 10}))
	var kid *session.Session
	for _, s := range r.store.All() {
		if s.ID != "s-0000a1" {
			kid = s
		}
	}
	p := r.store.Get("s-0000a1")
	kid.Lock()
	closed, run := kid.Closed, kid.Run
	kid.Unlock()
	if len(r.outcomes()) != 1 {
		t.Fatalf("one outcome: %d", len(r.outcomes()))
	}
	if res.OK {
		if closed || run == nil || len(f.Live) != 1 {
			t.Fatalf("told it succeeded but the child is not alive")
		}
		return
	}
	if !closed || run != nil || len(f.Live) != 0 || p.Reserved != (client.Budgets{}) {
		t.Fatalf("told it failed (%q) but the child lives: closed=%v run=%v live=%v reserved=%+v", r.outcomes()[0].Body.Result, closed, run, f.Live, p.Reserved)
	}
}

// A grant whose parent is another live session of the same principal and task must not attach the child to it.
func TestAGrantsParentMustBeTheSessionThatAsked(t *testing.T) {
	r, oc := treeRig(t, nil)
	same := r.store.Get("s-0000a1").Limits // as wide as the real parent, so only the binding can refuse it
	other := sessionRow("s-0000b2", "user:alice", "orchestrator", "", func(rec *session.Record) { rec.TaskID = "t-0000a1"; rec.Limits = same })
	_ = r.persist.Save(context.Background(), []session.Record{other})
	st2, _ := session.NewStore(context.Background(), r.persist)
	r.g.Store, r.store = st2, st2
	oc.Grant = func(g *client.Grant, _ *client.Spend) { p := "s-0000b2"; g.Parent = &p; g.Depth = 1 }
	res := r.call(r.person("user:alice"), "s-0000a1", "run.spawn", spawnArgs("researcher", map[string]any{"tool_calls": 10}))
	o := r.store.Get("s-0000b2")
	if res.OK || res.Decision["rule"] != "R007" || o.Reserved != (client.Budgets{}) || len(o.Children) != 0 || len(r.store.All()) != 2 {
		t.Fatalf("%v %v %+v", res.OK, res.Decision, o.Reserved)
	}
	if out := r.outcomes(); len(out) != 1 || out[0].Body.Refusal == nil || out[0].Body.Refusal.Rule != "R007" {
		t.Fatalf("%+v", out)
	}
}

// ---- the sweep is bounded --------------------------------------------------------------------------------------------

func closedWithRun(id, run string, mod func(*session.Record)) session.Record {
	return sessionRow(id, "user:alice", "researcher", run, func(r *session.Record) {
		r.Closed = true
		r.Outcome = map[string]any{"status": "done", "via": "runtime"}
		if mod != nil {
			mod(r)
		}
	})
}

func countRecords(r *runRig, result string) int {
	n := 0
	for _, rec := range r.records() {
		if rec["kind"] == "run.delete" && strings.HasPrefix(result, "") && strings.HasPrefix(toString(rec["result"]), result) {
			n++
		}
	}
	return n
}

func toString(v any) string { s, _ := v.(string); return s }

// A run whose delete never succeeds is retried on a backoff (1, 2, 4 ... 30 minutes) and its failure is recorded when it
// starts and at most once an hour after, however often the sweep runs; its success is recorded once.
func TestAFailingDeleteIsRetriedOnABackoffAndRecordedAtMostOnceAnHour(t *testing.T) {
	r := newRunRig(t, withPersist(newPersist(closedWithRun("s-0000a1", "r-0000a1", nil))))
	r.api.SetFailDelete("r-0000a1")
	ctx := context.Background()
	for i := 0; i < 24*60; i++ { // a day of sweeps, one a minute
		r.g.ExpireAt(ctx, t0.Add(time.Duration(i)*time.Minute))
	}
	errs, tries := countRecords(r, "error:"), r.api.DeleteCalls["r-0000a1"]
	if errs < 2 || errs > 25 {
		t.Errorf("%d failure records in a day: want the first and then at most one an hour (at most 25)", errs)
	}
	if tries < 10 || tries > 60 {
		t.Errorf("%d delete attempts in a day: backoff to 30 minutes means about 53", tries)
	}
	r.api.SetFailDelete()
	r.g.ExpireAt(ctx, t0.Add(25*time.Hour))
	if countRecords(r, "deleted") != 1 || r.store.Get("s-0000a1").Run != nil {
		t.Fatalf("one success record, run cleared")
	}
	before := len(r.records())
	r.g.ExpireAt(ctx, t0.Add(26*time.Hour))
	if len(r.records()) != before {
		t.Fatal("a finished run is not handled again")
	}
}

// A descendant named by its ancestor and again by its own entry is handled once per sweep.
func TestEachRunIsHandledOncePerSweep(t *testing.T) {
	kid := "s-0000c1"
	parent := closedWithRun("s-0000a1", "r-0000a1", func(r *session.Record) { r.Children = []string{kid} })
	child := closedWithRun(kid, "r-0000c1", func(r *session.Record) {
		p := "s-0000a1"
		r.ParentID, r.Depth, r.Children = &p, 1, []string{"s-0000d1"}
	})
	grand := closedWithRun("s-0000d1", "r-0000d1", func(r *session.Record) { p := kid; r.ParentID, r.Depth = &p, 2 })
	r := newRunRig(t, withPersist(newPersist(parent, child, grand)))
	r.api.SetFailDelete("r-0000a1", "r-0000c1", "r-0000d1")
	r.g.ExpireAt(context.Background(), t0)
	for _, name := range []string{"r-0000a1", "r-0000c1", "r-0000d1"} {
		if n := r.api.DeleteCalls[name]; n != 1 {
			t.Errorf("%s: %d delete attempts in one sweep, want 1", name, n)
		}
	}
	if n := countRecords(r, "error:"); n != 3 {
		t.Errorf("%d failure records for 3 runs", n)
	}
}

// A hung API server cannot delay the closing of expired sessions beyond the sweep's own deadline, and cannot stall a live
// call on a session the sweep is reading.
func TestAHungAPIServerCannotDelayExpiryOrBlockACall(t *testing.T) {
	r, _ := treeRig(t, nil)
	_ = r.persist.Save(context.Background(), []session.Record{sessionRow("s-0000e1", "user:alice", "researcher", "r-0000e1", func(rec *session.Record) {
		rec.ExpiresAt = t0.Add(-time.Minute).Format(session.IsoFormat)
	})})
	st2, _ := session.NewStore(context.Background(), r.persist)
	r.g.Store, r.store = st2, st2
	f := runs.NewFake()
	f.Live["s-0000a1"], f.Live["s-0000e1"] = "r-0000a1", "r-0000e1"
	var reading atomic.Bool
	f.OnEnded = func(ctx context.Context) { reading.Store(true); <-ctx.Done() } // the API server never answers
	r.g.SetRuns(f)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	done := make(chan time.Duration)
	go func() {
		start := time.Now()
		r.g.ExpireAt(ctx, t0)
		done <- time.Since(start)
	}()
	for !reading.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	// while the sweep waits on the API server, a live call on the session it is reading is not stalled
	start := time.Now()
	if res := r.call(r.person("user:alice"), "s-0000a1", "argo.app.get", map[string]any{"app": "x"}); !res.OK {
		t.Fatalf("%v", res.Decision)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("a live call took %v while the sweep waited on the API server", d)
	}
	if d := <-done; d > 3*time.Second {
		t.Errorf("the sweep took %v: its deadline was not honoured", d)
	}
	e := r.store.Get("s-0000e1")
	e.Lock()
	defer e.Unlock()
	if !e.Closed || e.Outcome["status"] != "expired" {
		t.Fatalf("expiry was delayed: %+v", e.Outcome)
	}
}

// ---- run_finish ------------------------------------------------------------------------------------------------------

func TestFinishRunAnswersAnUnknownOrClosedSessionAsItAnswersAForeignOne(t *testing.T) {
	r, _ := treeRig(t, nil)
	_ = r.persist.Save(context.Background(), []session.Record{
		sessionRow("s-0000b2", "user:alice", "researcher", "r-0000b2", nil),
		closedWithRun("s-0000c3", "r-0000c3", nil)})
	st2, _ := session.NewStore(context.Background(), r.persist)
	r.g.Store, r.store = st2, st2
	pod := r.pod("r-0000a1")
	ctx := context.Background()
	var answers []string
	for _, sid := range []string{"s-0000b2" /* another's */, "s-nope" /* unknown */, "s-0000c3" /* closed, another's */} {
		_, err := r.g.FinishRun(ctx, pod, sid, map[string]any{"status": "done"})
		ce := asCtl(err)
		if ce == nil {
			t.Fatalf("%s: %v", sid, err)
		}
		answers = append(answers, ce.Rule+"/"+ce.Message)
	}
	if answers[0] != answers[1] || answers[1] != answers[2] || !strings.HasPrefix(answers[0], "R001/") {
		t.Fatalf("an id's existence is told: %v", answers)
	}
	// its own run, once the session ended, is still told so
	if _, err := r.g.FinishRun(ctx, pod, "s-0000a1", map[string]any{"status": "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.g.FinishRun(ctx, pod, "s-0000a1", map[string]any{"status": "done"}); asCtl(err) == nil || asCtl(err).Rule != "R004" {
		t.Fatalf("%v", err)
	}
}

func TestALateFinishCannotOverwriteTheExpiredOutcome(t *testing.T) {
	r, _ := treeRig(t, nil)
	f := runs.NewFake()
	f.Live["s-0000a1"] = "r-0000a1"
	r.g.SetRuns(f)
	pod := r.pod("r-0000a1")
	late := t0.Add(2 * time.Hour) // past the session's deadline
	r.g.Now = func() time.Time { return late }
	if _, err := r.g.FinishRun(context.Background(), pod, "s-0000a1", map[string]any{"status": "done", "final_text": "late"}); asCtl(err) == nil || asCtl(err).Rule != "R004" {
		t.Fatalf("a finish past the deadline: %v", err)
	}
	if len(r.results.Rows) != 0 {
		t.Fatal("a late answer was kept")
	}
	r.g.ExpireAt(context.Background(), late)
	s := r.store.Get("s-0000a1")
	s.Lock()
	status := s.Outcome["status"]
	s.Unlock()
	if status != "expired" {
		t.Fatalf("%v", status)
	}
	r.g.Now = func() time.Time { return t0 } // even with the clock back, the written outcome stands
	// (the sweep also deleted the run, so the pod is no longer the session's run: refused either way)
	if _, err := r.g.FinishRun(context.Background(), pod, "s-0000a1", map[string]any{"status": "done"}); asCtl(err) == nil {
		t.Fatal("a finish after the expired outcome was accepted")
	}
	s.Lock()
	defer s.Unlock()
	if s.Outcome["status"] != "expired" {
		t.Fatalf("overwritten: %v", s.Outcome)
	}
}
