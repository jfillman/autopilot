package triggers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/triggers"
)

func init() { triggers.RetryWaits = []time.Duration{time.Millisecond, time.Millisecond} }

const key1 = "gate-api.pipelinerun.finished/pr-1"

// The reviewer's interleaving: the claim is won, the run is launched, the decision cannot be written (the ledger stays away past the
// retries) and the stream ends; the replay's claim loses and must complete the decision, not report a plain dedupe; the run finishes;
// later the sweep re-delivers the key and must find it decided. One run.
func TestADecisionThatCannotBeWrittenIsCompletedByTheReplayAndNeverLaunchesTwice(t *testing.T) {
	ctx := context.Background()
	gw := newGW(triage(nil, 0))
	r := newRig(t, gw, "")
	r.now = t0.Add(time.Second) // after the bridge started: the claim is this process's, and fresh
	r.ledger.DecideFails(errors.New("connection lost"), 100)
	if _, err := r.b.Handle(ctx, finished("e1", "pr-1", "failure"), "stream"); err == nil {
		t.Fatal("a decision that could not be written was reported as handled")
	}
	row, _ := r.ledger.Get(ctx, "triage-agent", key1)
	if row == nil || row.State != "claimed" || row.Session == nil {
		t.Fatalf("the row must say a session was opened for it: %+v", row)
	}
	sid := *row.Session
	r.ledger.DecideFails(nil, 0) // the database is back; the replay of the same event, whose claim now loses
	r.now = t0.Add(2 * time.Second)
	got, err := r.b.Handle(ctx, finished("e1", "pr-1", "failure"), "stream")
	if err != nil || len(got) != 1 || got[0].Result != "launched" || !got[0].New || got[0].Session != sid || got[0].Run == "" {
		t.Fatalf("the replay must complete the decision: %+v %v", got, err)
	}
	if row, _ = r.ledger.Get(ctx, "triage-agent", key1); row.State != "launched" || *row.Session != sid {
		t.Fatalf("not decided: %+v", row)
	}
	// the run finishes (its session closes), and five minutes later the sweep re-delivers the key
	gw.mu.Lock()
	gw.closed[sid] = true
	gw.mu.Unlock()
	r.now = t0.Add(10 * time.Minute)
	got, _ = r.bridge("").Handle(ctx, finished("reconcile:gate-api/pr-1", "pr-1", "failure"), "reconcile")
	if len(got) != 1 || got[0].New || gw.nOpens() != 1 || gw.nLaunch() != 1 {
		t.Fatalf("%+v: %d opens, %d launches", got, gw.nOpens(), gw.nLaunch())
	}
}

// The restart variants: the row is `claimed` with its session and a new process takes the claim. What the session is decides what
// happens, and no other session is opened while the recorded one is usable.
func TestAClaimWithASessionRecordedIsResumedAccordingToTheSession(t *testing.T) {
	ctx := context.Background()
	type sess struct {
		hasRun, launched, closed, frozen, ended, unknown bool
		ctl                                              string // how it was closed when not by a person
	}
	for name, c := range map[string]struct {
		s        sess
		want     string
		rule     string
		opens    int
		launches int
	}{
		"live with a run":                        {sess{hasRun: true, launched: true}, "launched", "", 0, 0},
		"live with no run":                       {sess{}, "launched", "", 0, 1},
		"live, frozen before the launch":         {sess{frozen: true}, "refused", "CT-FROZEN", 0, 0},
		"finished (closed after launching)":      {sess{hasRun: true, launched: true, closed: true}, "launched", "", 0, 0},
		"closed by a person before launching":    {sess{closed: true, ended: true}, "refused", "CT-ENDED", 0, 0},
		"closed and frozen before launching":     {sess{closed: true, frozen: true}, "refused", "CT-FROZEN", 0, 0},
		"closed by its deadline, never launched": {sess{closed: true, ctl: "deadline"}, "launched", "", 1, 1}, // the only reason a fresh session may follow
		"closed with no reason recorded":         {sess{closed: true}, "refused", "CT-CLOSED", 0, 0},
		"closed by the gateway (abandoned)":      {sess{closed: true, ctl: "abandon"}, "refused", "CT-CLOSED", 0, 0},
		"unknown to this gateway":                {sess{unknown: true}, "deduped", "", 0, 0},
	} {
		gw := newGW(triage(nil, 0))
		r := newRig(t, gw, "")
		task := triggers.EventTaskID("triage-agent", key1)
		if !c.s.unknown {
			gw.sessions["triage-agent/"+task] = "s-old"
		}
		gw.hasRun["s-old"], gw.launched["s-old"], gw.closed["s-old"], gw.frozen["s-old"] = c.s.hasRun, c.s.launched, c.s.closed, c.s.frozen
		switch {
		case c.s.ended:
			gw.closeCtl["s-old"] = "end"
		case c.s.closed:
			gw.closeCtl["s-old"] = c.s.ctl
		}
		if ok, _ := r.ledger.Claim(ctx, "triage-agent", key1, "stream", "e1", t0, t0); !ok {
			t.Fatal("claim")
		}
		_ = r.ledger.Opened(ctx, "triage-agent", key1, "s-old")
		r.now = t0.Add(10 * time.Minute) // a new process, later than the dead one's claim
		got, err := r.bridge("").Handle(ctx, finished("e2", "pr-1", "failure"), "recent")
		if err != nil || len(got) != 1 || got[0].Result != c.want || got[0].Rule != c.rule || !got[0].New {
			t.Errorf("%s: %+v %v", name, got, err)
		}
		if gw.nOpens() != c.opens || gw.nLaunch() != c.launches {
			t.Errorf("%s: %d opens, %d launches", name, gw.nOpens(), gw.nLaunch())
		}
		row, _ := r.ledger.Get(ctx, "triage-agent", key1)
		if row.State == "claimed" {
			t.Errorf("%s: not decided", name)
		}
	}
	// a session that was closed by its deadline is opened afresh only while the trigger is still valid: an event from two days ago is `missed`
	gw := newGW(triage(nil, 0))
	r := newRig(t, gw, "")
	gw.sessions["triage-agent/"+triggers.EventTaskID("triage-agent", key1)] = "s-old"
	gw.closed["s-old"], gw.closeCtl["s-old"] = true, "deadline"
	_, _ = r.ledger.Claim(ctx, "triage-agent", key1, "stream", "e1", t0, t0)
	_ = r.ledger.Opened(ctx, "triage-agent", key1, "s-old")
	r.now = t0.Add(72 * time.Hour)
	got, _ := r.bridge("").Handle(ctx, finished("e2", "pr-1", "failure"), "recent")
	if len(got) != 1 || got[0].Result != "missed" || gw.nOpens() != 0 {
		t.Fatalf("%+v", got)
	}
}

// The reviewer's other interleaving: the session cannot be written to the row (every retry fails), so it is closed and Handle errors;
// the row stays `claimed` with no session; the replay, in the same process, must run the trigger from the start (not answer deduped,
// which lost it): exactly one run. A restart variant of the same.
func TestASessionThatCannotBeRecordedIsRetriedByTheReplayAndLaunchesOnce(t *testing.T) {
	ctx := context.Background()
	for _, restart := range []bool{false, true} {
		gw := newGW(triage(nil, 0))
		r := newRig(t, gw, "")
		r.now = t0.Add(time.Second)
		failing := &failingOpened{MemoryLedger: r.ledger}
		b, _ := triggers.NewBridge(gw, failing, triggers.BridgeConfig{BaseURL: "http://glidepath.invalid", AllowHTTP: true, TokenFile: r.tok, Now: func() time.Time { return r.now }}, nil, nil)
		if _, err := b.Handle(ctx, finished("e1", "pr-1", "failure"), "stream"); err == nil || gw.nLaunch() != 0 {
			t.Fatalf("%v %d", err, gw.nLaunch())
		}
		if row, _ := r.ledger.Get(ctx, "triage-agent", key1); row == nil || row.State != "claimed" || row.Session != nil {
			t.Fatalf("%+v", row)
		}
		r.now = t0.Add(2 * time.Second)
		next := r.b
		if restart {
			r.now = t0.Add(10 * time.Minute)
			next = r.bridge("")
		}
		got, err := next.Handle(ctx, finished("e1", "pr-1", "failure"), "stream")
		if err != nil || len(got) != 1 || got[0].Result != "launched" || !got[0].New || gw.nLaunch() != 1 {
			t.Fatalf("restart=%v: %+v %v launches %d: a trigger was dropped or fired twice", restart, got, err, gw.nLaunch())
		}
	}
}

// A decision that cannot be written after an outcome that was determined without a session (held, braked, refused): the replay writes
// THAT outcome, even though the hold was released in between; a restart variant evaluates afresh (the outcome is only in memory).
func TestADeterminedOutcomeIsWrittenAsItWasEvenIfTheHoldIsReleased(t *testing.T) {
	ctx := context.Background()
	for _, outcome := range []string{"held", "braked", "refused"} {
		gw := newGW(triage(nil, 1))
		r := newRig(t, gw, "")
		r.now = t0.Add(time.Second)
		switch outcome {
		case "held":
			gw.holdRule["triage-agent"] = "R024"
		case "refused":
			gw.holdRule["triage-agent"] = "R001"
		case "braked":
			if got, _ := r.b.Handle(ctx, finished("e0", "pr-0", "failure"), "stream"); len(got) != 1 {
				t.Fatal(got)
			}
		}
		r.ledger.DecideFails(errors.New("connection lost"), 100)
		if _, err := r.b.Handle(ctx, finished("e1", "pr-1", "failure"), "stream"); err == nil {
			t.Fatalf("%s: handled although the decision was not written", outcome)
		}
		delete(gw.holdRule, "triage-agent") // released meanwhile
		r.ledger.DecideFails(nil, 0)
		r.now = t0.Add(2 * time.Second)
		before := gw.nLaunch()
		got, err := r.b.Handle(ctx, finished("e1", "pr-1", "failure"), "stream")
		if err != nil || len(got) != 1 || got[0].Result != outcome || gw.nLaunch() != before {
			t.Fatalf("%s: %+v %v (launches %d, was %d)", outcome, got, err, gw.nLaunch(), before)
		}
		if row, _ := r.ledger.Get(ctx, "triage-agent", key1); row.State != outcome {
			t.Fatalf("%s: %+v", outcome, row)
		}
	}
}

// The session is written to the row right after the open and before the launch: a launch that never happens (the process died) leaves
// a row that says so; and when the row cannot be written nothing launches and the session is closed again.
func TestTheSessionIsRecordedBeforeTheLaunchAndWithoutItNothingLaunches(t *testing.T) {
	ctx := context.Background()
	gw := newGW(triage(nil, 0))
	r := newRig(t, gw, "")
	gw.onOpen = nil
	sawRow := false
	gw.beforeLaunch = func() {
		row, _ := r.ledger.Get(ctx, "triage-agent", key1)
		sawRow = row != nil && row.Session != nil && row.State == "claimed"
	}
	if _, err := r.b.Handle(ctx, finished("e1", "pr-1", "failure"), "stream"); err != nil || !sawRow {
		t.Fatalf("the launch ran with no session recorded in the row: %v", err)
	}
	gw = newGW(triage(nil, 0))
	r = newRig(t, gw, "")
	// the ledger fails every time
	failing := &failingOpened{MemoryLedger: r.ledger}
	b, _ := triggers.NewBridge(gw, failing, triggers.BridgeConfig{BaseURL: "http://glidepath.invalid", AllowHTTP: true, TokenFile: r.tok, Now: func() time.Time { return r.now }}, nil, nil)
	if _, err := b.Handle(ctx, finished("e1", "pr-1", "failure"), "stream"); err == nil || gw.nLaunch() != 0 || len(gw.abandoned) != 1 {
		t.Fatalf("%v: %d launches, abandoned %v", err, gw.nLaunch(), gw.abandoned)
	}
}

type failingOpened struct{ *triggers.MemoryLedger }

func (f *failingOpened) Opened(context.Context, string, string, string) error {
	return errors.New("connection lost")
}

// A hold that is saved after a trigger's session was opened and before it launches stops the launch: the session is closed and the
// trigger is recorded as held.
func TestAHoldBetweenTheOpenAndTheLaunchStopsTheLaunch(t *testing.T) {
	ctx := context.Background()
	gw := newGW(triage(nil, 0))
	r := newRig(t, gw, "")
	gw.lateHold["triage-agent"], gw.holdRule["triage-agent"] = false, ""
	gw.beforeLaunch = func() { gw.holdRule["triage-agent"] = "R024" } // saved after the open, before the launch
	got, err := r.b.Handle(ctx, finished("e1", "pr-1", "failure"), "stream")
	if err != nil || len(got) != 1 || got[0].Result != "held" || got[0].Rule != "R024" || gw.nLaunch() != 0 {
		t.Fatalf("%+v %v launches %d", got, err, gw.nLaunch())
	}
	if row, _ := r.ledger.Get(ctx, "triage-agent", key1); row.State != "held" {
		t.Fatalf("%+v", row)
	}
}

// The scheduler has the same shape: a slot is not marked fired until its row is decided, so a slot whose session could not be recorded
// (or whose decision could not be written) is carried out on the next tick, once, while its grace window lasts; a restart likewise.
func TestASlotIsNotFiredUntilItsRowIsDecided(t *testing.T) {
	ctx := context.Background()
	for _, how := range []string{"opened", "decide-held", "decide-launched", "restart"} {
		gw := newGW(nightly())
		ledger := &triggers.MemoryLedger{}
		now := time.Date(2026, 11, 14, 0, 1, 0, 0, time.UTC)
		clock := func() time.Time { return now }
		var l triggers.Ledger = ledger
		if how == "opened" || how == "restart" {
			l = &failingOpened{MemoryLedger: ledger}
		}
		if how == "decide-held" {
			gw.holdRule["nightly-reviewer"] = "R024"
		}
		if how == "decide-held" || how == "decide-launched" {
			ledger.DecideFails(errors.New("connection lost"), 100)
		}
		s := triggers.NewScheduler(gw, l, clock, nil, nil)
		if done := s.Tick(ctx); len(done) != 0 || gw.nLaunch() > 1 {
			t.Fatalf("%s: %+v", how, done)
		}
		ledger.DecideFails(nil, 0)
		delete(gw.holdRule, "nightly-reviewer") // released meanwhile
		now = now.Add(time.Minute)
		next := s
		if how == "restart" {
			now = now.Add(2 * time.Minute)
			next = triggers.NewScheduler(gw, ledger, clock, nil, nil)
		} else if how == "opened" {
			next = triggers.NewScheduler(gw, ledger, clock, nil, nil) // the failing wrapper is dropped: the database is back
		}
		done := next.Tick(ctx)
		want := map[string]string{"opened": "launched", "restart": "launched", "decide-held": "held", "decide-launched": "launched"}[how]
		if len(done) != 1 || done[0].Result != want {
			t.Fatalf("%s: %+v, want %s", how, done, want)
		}
		wantLaunches := map[string]int{"opened": 1, "restart": 1, "decide-held": 0, "decide-launched": 1}[how]
		if gw.nLaunch() != wantLaunches {
			t.Fatalf("%s: %d launches, want %d", how, gw.nLaunch(), wantLaunches)
		}
		if again := triggers.NewScheduler(gw, ledger, clock, nil, nil).Tick(ctx); len(again) != 0 || gw.nLaunch() != wantLaunches {
			t.Fatalf("%s: fired again: %+v", how, again)
		}
	}
}

// An event that panics on every delivery does not hold the cursor forever: after PoisonAttempts counted failures, each within
// PoisonQuiet of the one before and the first and last PoisonAfter apart (counted on the ledger row, so a restart does not start again),
// its trigger is decided launch-failed (CT-POISON), after any run it may have made is resolved through the backend. A "could not tell"
// (an outage of a dependency) is not counted, however long it lasts. A session whose run is recorded launched is decided launched.
func TestAnEventThatKeepsPanickingIsDecidedAfterABoundAndAnOutageIsNeverCounted(t *testing.T) {
	ctx := context.Background()
	oldA, oldT, oldQ := triggers.PoisonAttempts, triggers.PoisonAfter, triggers.PoisonQuiet
	triggers.PoisonAttempts, triggers.PoisonAfter, triggers.PoisonQuiet = 3, 3*time.Minute, time.Hour
	defer func() { triggers.PoisonAttempts, triggers.PoisonAfter, triggers.PoisonQuiet = oldA, oldT, oldQ }()
	deliver := func(r *rig, at time.Duration) ([]triggers.Outcome, error) {
		r.now = t0.Add(at)
		return r.bridge("").Handle(ctx, finished("e1", "pr-1", "failure"), "stream") // every delivery is a new process: the count lives on the row
	}
	// 1. a panic: not decided under the bound, decided at it
	{
		gw := newGW(triage(nil, 0))
		r := newRig(t, gw, "")
		gw.panicOn["triage-agent"] = true
		for n := 1; n <= 3; n++ {
			if got, err := deliver(r, time.Duration(n)*time.Minute); err == nil {
				t.Fatalf("#%d: a stuck event was decided early: %+v", n, got)
			}
		}
		if row, _ := r.ledger.Get(ctx, "triage-agent", key1); row.State != "claimed" || row.Failures != 3 {
			t.Fatalf("%+v", row)
		}
		got, err := deliver(r, 4*time.Minute)
		if err != nil || len(got) != 1 || got[0].Result != "launch-failed" || got[0].Rule != "CT-POISON" || !got[0].New {
			t.Fatalf("%+v %v", got, err)
		}
		if r.m.C["poisoned/triage-agent"] != 1 {
			t.Fatalf("no distinct metric: %v", r.m.C)
		}
		if again, _ := deliver(r, 5*time.Minute); len(again) != 1 || again[0].New {
			t.Fatalf("%+v", again)
		}
	}
	// 2. the rule's window: a failure from hours ago is not part of the count (a quiet gap starts it again); 3 failures within the
	// quiet window and 3 minutes apart decide it
	{
		gw := newGW(triage(nil, 0))
		r := newRig(t, gw, "")
		gw.panicOn["triage-agent"] = true
		for _, at := range []time.Duration{time.Minute, 2 * time.Minute} { // two failures, then silence
			_, _ = deliver(r, at)
		}
		got, err := deliver(r, 5*time.Hour) // the third is hours later: the count starts again at 1
		if err == nil {
			t.Fatalf("a failure hours after the others made it poison: %+v", got)
		}
		if row, _ := r.ledger.Get(ctx, "triage-agent", key1); row.Failures != 1 || !row.FirstFailed.Equal(t0.Add(5*time.Hour)) {
			t.Fatalf("the count was not started again: %+v", row)
		}
		_, _ = deliver(r, 5*time.Hour+2*time.Minute)
		if got, err = deliver(r, 5*time.Hour+4*time.Minute); err != nil || len(got) != 1 || got[0].Rule != "CT-POISON" {
			t.Fatalf("%+v %v", got, err)
		}
	}
	// 3. an outage ("could not tell", a hold store down, a backend down) is never counted: hours of it decide nothing and reap nothing
	{
		gw := newGW(triage(nil, 0))
		r := newRig(t, gw, "")
		task := triggers.EventTaskID("triage-agent", key1)
		gw.sessions["triage-agent/"+task] = "s-1"
		gw.hasRun["s-1"], gw.launched["s-1"] = true, true
		gw.closed["s-1"], gw.closeCtl["s-1"] = true, "finish" // a closed session names a run: the backend is asked, and it is down
		gw.existsErr = errors.New("api server unreachable")
		for n := 1; n <= 20; n++ {
			if got, err := deliver(r, time.Duration(n)*10*time.Minute); err == nil {
				t.Fatalf("#%d: decided during an outage: %+v", n, got)
			}
		}
		if row, _ := r.ledger.Get(ctx, "triage-agent", key1); row.State != "claimed" || row.Failures != 0 || len(gw.reaped) != 0 || !gw.hasRun["s-1"] {
			t.Fatalf("an outage was counted or its run reaped: %+v reaped=%v", row, gw.reaped)
		}
	}
	// 4. poison after a launch: decided from the record (launched), the run left to its session
	{
		gw := newGW(triage(nil, 0))
		r := newRig(t, gw, "")
		task := triggers.EventTaskID("triage-agent", key1)
		gw.sessions["triage-agent/"+task] = "s-1"
		gw.hasRun["s-1"], gw.launched["s-1"] = true, true
		if ok, _ := r.ledger.Claim(ctx, "triage-agent", key1, "stream", "e1", t0, t0); !ok {
			t.Fatal("claim")
		}
		_ = r.ledger.Opened(ctx, "triage-agent", key1, "s-1")
		gw.beforeLaunch = func() { panic("boom") } // the launch of the recorded session panics on every delivery
		var got []triggers.Outcome
		var err error
		for n := 1; n <= 4; n++ {
			got, err = deliver(r, time.Duration(n)*time.Minute)
		}
		if err != nil || len(got) != 1 || got[0].Result != "launched" || got[0].Rule != "CT-POISON" || got[0].Session != "s-1" {
			t.Fatalf("%+v %v", got, err)
		}
		if len(gw.reaped) != 0 || !gw.hasRun["s-1"] {
			t.Fatalf("a launched run was deleted because the event kept failing: %v", gw.reaped)
		}
	}
}
