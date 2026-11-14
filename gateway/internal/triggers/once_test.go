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
	if err != nil || len(got) != 1 || got[0].Result != "deduped" || !got[0].New || got[0].Session != sid || got[0].Run == "" {
		t.Fatalf("the replay must complete the decision: %+v %v", got, err)
	}
	if row, _ = r.ledger.Get(ctx, "triage-agent", key1); row.State != "deduped" || *row.Session != sid {
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

// The restart variants of the same: the row is `claimed` with its session and a new process takes the stale claim. Whatever became of
// the session, no other is opened: a live one with a run is a dedupe, a live one with none is launched, a finished one decides.
func TestAStaleClaimWithASessionRecordedOpensNoOtherSession(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		hasRun, closed bool
		want           string
		launches       int
	}{
		"live with a run":    {true, false, "deduped", 0},
		"live with no run":   {false, false, "launched", 1},
		"finished (closed)":  {true, true, "deduped", 0},
		"closed with no run": {false, true, "deduped", 0},
	} {
		gw := newGW(triage(nil, 0))
		r := newRig(t, gw, "")
		task := triggers.EventTaskID("triage-agent", key1)
		gw.sessions["triage-agent/"+task] = "s-old"
		gw.hasRun["s-old"] = c.hasRun
		gw.closed["s-old"] = c.closed
		if ok, _ := r.ledger.Claim(ctx, "triage-agent", key1, "stream", "e1", t0, t0); !ok {
			t.Fatal("claim")
		}
		_ = r.ledger.Opened(ctx, "triage-agent", key1, "s-old")
		r.now = t0.Add(10 * time.Minute) // a new process, later than the dead one's claim
		got, err := r.bridge("").Handle(ctx, finished("e2", "pr-1", "failure"), "recent")
		if err != nil || len(got) != 1 || got[0].Result != c.want || got[0].Session != "s-old" || !got[0].New {
			t.Errorf("%s: %+v %v", name, got, err)
		}
		if gw.nOpens() != 0 || gw.nLaunch() != c.launches {
			t.Errorf("%s: %d opens, %d launches", name, gw.nOpens(), gw.nLaunch())
		}
		if row, _ := r.ledger.Get(ctx, "triage-agent", key1); row.State == "claimed" {
			t.Errorf("%s: not decided", name)
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
