package triggers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/triggers"
)

// Every durable write of a trigger's life and the window after it is a row here; the test builds what that window leaves behind, lets a
// new process take the claim over (the first one failing to tell, where the window says so), and checks the invariants of the doc's
// crash-point table: at most one run; never zero runs under a `launched` row; never a launch after a person's end or a freeze, or
// under a hold; and the row is decided in the end.
func TestEveryCrashWindowIsTakenOverToAtMostOneRunAndNeverALostOne(t *testing.T) {
	ctx := context.Background()
	key := key1
	task := triggers.EventTaskID("triage-agent", key)
	type win struct {
		name     string
		build    func(gw *fakeGW) // what the dead process left (the session s-old, tied to the row)
		noSess   bool             // the row holds no session
		decided  string           // the row was already decided as this
		tell     bool             // the first takeover cannot tell (the backend); the second can
		want     string           // the row's decision at the end
		runs     int              // runs that exist at the end
		launches int              // launches the takeovers made
		noLaunch bool             // nothing may launch (a person's end, a freeze, a hold)
	}
	wins := []win{
		{name: "W0 after the claim, before the session", noSess: true, want: "launched", runs: 1, launches: 1},
		{name: "W1 session opened and recorded, run not reserved", build: func(g *fakeGW) {}, want: "launched", runs: 1, launches: 1},
		{name: "W2 run name reserved (written), run not created", build: func(g *fakeGW) { g.reserved["s-old"] = true }, want: "launched", runs: 1, launches: 1},
		{name: "W3 run created and recorded, row not decided", build: func(g *fakeGW) { g.hasRun["s-old"], g.launched["s-old"] = true, true }, want: "launched", runs: 1},
		{name: "W4 run created, session since closed (finished)", build: func(g *fakeGW) {
			g.hasRun["s-old"], g.launched["s-old"], g.closed["s-old"], g.closeCtl["s-old"] = true, true, true, "finish"
		}, want: "launched", runs: 1},
		{name: "W5 reserved, session closed by its deadline, backend cannot tell", build: func(g *fakeGW) {
			g.reserved["s-old"], g.closed["s-old"], g.closeCtl["s-old"] = true, true, "deadline"
			g.existsErr = errors.New("api server unreachable")
		}, tell: true, want: "launched", runs: 1, launches: 1},
		{name: "W6 reserved, closed, and the run exists", build: func(g *fakeGW) {
			g.hasRun["s-old"], g.closed["s-old"], g.closeCtl["s-old"] = true, true, "deadline"
		}, want: "launched", runs: 1},
		{name: "W7 run made, a hold took it away (recorded), not decided", build: func(g *fakeGW) {
			g.launched["s-old"], g.removed["s-old"] = true, "hold"
			g.closed["s-old"], g.closeCtl["s-old"] = true, "abandon"
		}, want: "held", noLaunch: true},
		{name: "W8 run made, removed because the session closed (recorded)", build: func(g *fakeGW) {
			g.launched["s-old"], g.removed["s-old"] = true, "closed"
			g.closed["s-old"], g.closeCtl["s-old"] = true, "end"
		}, want: "refused", noLaunch: true},
		{name: "W9 run made, removed because it could not be recorded", build: func(g *fakeGW) {
			g.launched["s-old"], g.removed["s-old"] = true, "record-failed"
			g.closed["s-old"], g.closeCtl["s-old"] = true, "abandon"
		}, want: "launch-failed", noLaunch: true},
		{name: "W10 a person ended the session before the launch", build: func(g *fakeGW) { g.closed["s-old"], g.closeCtl["s-old"] = true, "end" }, want: "refused", noLaunch: true},
		{name: "W11 the person's end is read after the deadline too", build: func(g *fakeGW) { g.closed["s-old"], g.closeCtl["s-old"] = true, "end" }, want: "refused", noLaunch: true},
		{name: "W12 the session froze before the launch", build: func(g *fakeGW) { g.frozen["s-old"] = true }, want: "refused", noLaunch: true},
		{name: "W13 closed by the deadline, never launched", build: func(g *fakeGW) { g.closed["s-old"], g.closeCtl["s-old"] = true, "deadline" }, want: "launched", runs: 1, launches: 1},
		{name: "W14 closed with no reason recorded", build: func(g *fakeGW) { g.closed["s-old"] = true }, want: "refused", noLaunch: true},
		{name: "W15 a hold is in force when the live session is resumed", build: func(g *fakeGW) { g.holdRule["triage-agent"] = "R024" }, want: "held", noLaunch: true},
		{name: "W16 the decision was written (the dedupe)", build: func(g *fakeGW) { g.hasRun["s-old"], g.launched["s-old"] = true, true }, decided: "launched", want: "launched", runs: 1},
		{name: "W17 the session is unknown to this gateway", build: func(g *fakeGW) { delete(g.sessions, "triage-agent/"+task) }, want: "deduped"},
	}
	for _, w := range wins {
		gw := newGW(triage(nil, 0))
		gw.reserved = map[string]bool{}
		r := newRig(t, gw, "")
		if !w.noSess {
			gw.sessions["triage-agent/"+task] = "s-old"
		}
		if w.build != nil {
			w.build(gw)
		}
		if ok, err := r.ledger.Claim(ctx, "triage-agent", key, "stream", "e1", t0, t0); !ok || err != nil {
			t.Fatal(w.name, ok, err)
		}
		if !w.noSess {
			_ = r.ledger.Opened(ctx, "triage-agent", key, "s-old")
		}
		if w.decided != "" {
			_ = r.ledger.Decide(ctx, "triage-agent", key, w.decided, "s-old", "r-old")
		}
		r.now = t0.Add(10 * time.Minute)
		got, err := r.bridge("").Handle(ctx, finished("e2", "pr-1", "failure"), "recent")
		if w.tell {
			if err == nil {
				t.Errorf("%s: a window that cannot be told must stay resumable, got %+v", w.name, got)
				continue
			}
			if row, _ := r.ledger.Get(ctx, "triage-agent", key); row.State != "claimed" || gw.nLaunch() != 0 || !gw.hasRun["s-old"] && !gw.reserved["s-old"] {
				t.Errorf("%s: an unknown must leave the claim and the session as they were: %+v launches=%d", w.name, row, gw.nLaunch())
			}
			gw.existsErr = nil
			r.now = t0.Add(20 * time.Minute)
			got, err = r.bridge("").Handle(ctx, finished("e3", "pr-1", "failure"), "recent")
		}
		answer := w.want
		if w.decided != "" {
			answer = "deduped" // the replay of a decided key is a dedupe; the row stays as it was decided
		}
		if err != nil || len(got) != 1 || got[0].Result != answer {
			t.Errorf("%s: %+v %v", w.name, got, err)
			continue
		}
		row, _ := r.ledger.Get(ctx, "triage-agent", key)
		if row.State != w.want {
			t.Errorf("%s: the row says %q", w.name, row.State)
		}
		runs := 0
		for _, v := range gw.hasRun {
			if v {
				runs++
			}
		}
		if runs > 1 || runs != w.runs {
			t.Errorf("%s: %d runs exist, want %d", w.name, runs, w.runs)
		}
		if gw.nLaunch() != w.launches {
			t.Errorf("%s: %d launches, want %d", w.name, gw.nLaunch(), w.launches)
		}
		if w.noLaunch && (gw.nLaunch() != 0 || gw.nOpens() != 0) {
			t.Errorf("%s: something launched or opened where nothing may", w.name)
		}
		if row.State == "launched" && row.Session != nil {
			sid := *row.Session
			if !gw.hasRun[sid] && !gw.launched[sid] || gw.removed[sid] != "" {
				t.Errorf("%s: a `launched` row with no run behind it (session %s)", w.name, sid)
			}
		}
		// the same event again, later: nothing more happens
		r.now = t0.Add(time.Hour)
		before := gw.nLaunch()
		_, _ = r.bridge("").Handle(ctx, finished("e4", "pr-1", "failure"), "stream")
		if gw.nLaunch() != before || gw.nOpens() > 1 {
			t.Errorf("%s: a replay fired again", w.name)
		}
	}
}
