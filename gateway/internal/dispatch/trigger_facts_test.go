package dispatch_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/runs"
	"github.com/jfillman/autopilot/gateway/internal/session"
)

// These tests hold the rule for a trigger's takeover: nothing is decided from the clock or from the absence of a field; every fact is
// recorded durably when it happens and read back as it was recorded.

func factsRig(t *testing.T) *peopleRig {
	p := newPeopleRig(t)
	img := "ghcr.io/jfillman/autopilot-agent-runtime@sha256:" + strings.Repeat("a", 64)
	p.g.Defs["triage-agent"].Image = &img
	p.api.GoneWhenAbsent = true
	return p
}

func openTrig(t *testing.T, p *peopleRig) string {
	t.Helper()
	res, err := p.g.OpenTriggered(ctx, "triage-agent", "glidepath:e1", "t-aaaaaaaaaaaa")
	if err != nil || res == nil || res.Session == nil {
		t.Fatalf("open: %+v %v", res, err)
	}
	return *res.Session
}

// Item 1: a person's end is a recorded fact (who, when, which control), still read as that after the deadline has passed.
func TestAPersonsEndIsRecordedAndStillReadAfterTheDeadline(t *testing.T) {
	p := factsRig(t)
	sid := openTrig(t, p)
	if _, err := p.g.EndSession(ctx, p.person(adminID), sid, "stop", ""); err != nil {
		t.Fatal(err)
	}
	row, _ := p.persist.row(sid)
	if row.CloseReason == nil || row.CloseReason.Control != session.CloseEnd || row.CloseReason.By != adminID || row.CloseReason.At.IsZero() {
		t.Fatalf("the end was not recorded with who, when and the control: %+v", row.CloseReason)
	}
	p.g.Now = func() time.Time { return t0.Add(2 * time.Hour) } // past its deadline
	if st := p.g.TriggerSessionState(sid); st.CloseControl != session.CloseEnd {
		t.Fatalf("a person's end read as %q after the deadline", st.CloseControl)
	}
	// a session the sweep closed at its deadline says so; one closed with no reason says nothing
	p2 := factsRig(t)
	s2 := openTrig(t, p2)
	p2.g.Now = func() time.Time { return t0.Add(2 * time.Hour) }
	if st := p2.g.TriggerSessionState(s2); st.CloseControl != session.CloseDeadline {
		t.Fatalf("an open session past its deadline read as %q", st.CloseControl)
	}
	s := p2.store.Get(s2)
	s.Lock()
	s.Closed = true
	s.Unlock()
	if st := p2.g.TriggerSessionState(s2); st.CloseControl != "" || !st.Known {
		t.Fatalf("a close with no recorded reason must read as none: %+v", st)
	}
}

// Item 2: when the backend cannot be asked, the resume is resumable, and nothing is closed, deleted or given up.
func TestACouldNotTellOnResumeIsResumableAndKeepsTheRun(t *testing.T) {
	p := factsRig(t)
	sid := openTrig(t, p)
	if out, err := p.g.LaunchTriggered(ctx, "triage-agent", sid, map[string]any{"prompt": "x"}); err != nil || !out.OK {
		t.Fatalf("launch: %+v %v", out, err)
	}
	p.api.Err = errors.New("API server unreachable")
	_, err := p.g.LaunchTriggered(ctx, "triage-agent", sid, map[string]any{"prompt": "x"})
	if err == nil || !dispatch.IsResumable(err) {
		t.Fatalf("an unknown must be resumable, got %v", err)
	}
	if _, rerr := p.g.RunExists(ctx, sid); rerr == nil || !dispatch.IsResumable(rerr) {
		t.Fatalf("RunExists on an unknown must be resumable, got %v", rerr)
	}
	p.api.Err = nil
	if s := p.store.Get(sid); s.Closed {
		t.Fatal("the session of a launched run was closed on an unknown")
	}
	if len(p.api.Manifests) != 1 {
		t.Fatalf("the run was lost: %d", len(p.api.Manifests))
	}
	if ok, rerr := p.g.RunExists(ctx, sid); rerr != nil || !ok {
		t.Fatalf("with the backend back the run exists: %v %v", ok, rerr)
	}
}

// Item 3: the reservation is durable before the run is created, and "created" is its own write right after.
func TestTheRunIsReservedBeforeItIsCreatedAndCreatedIsItsOwnWrite(t *testing.T) {
	p := factsRig(t)
	sid := openTrig(t, p)
	var seenAtCreate session.Record
	p.g.SetRuns(&runs.Backend{Store: &gatedStore{FakeStore: p.api, onCreate: func(string) { seenAtCreate, _ = p.persist.row(sid) }}})
	var afterCreate []bool
	p.persist.onSave = func() { row, _ := p.persist.row(sid); afterCreate = append(afterCreate, row.LaunchedAt != nil) }
	if out, err := p.g.LaunchTriggered(ctx, "triage-agent", sid, map[string]any{"prompt": "x"}); err != nil || !out.OK {
		t.Fatalf("launch: %+v %v", out, err)
	}
	if seenAtCreate.Run == nil || *seenAtCreate.Run == "" || seenAtCreate.LaunchedAt != nil {
		t.Fatalf("at createRun the durable row must name the run and not yet say created: %+v", seenAtCreate)
	}
	if row, _ := p.persist.row(sid); row.LaunchedAt == nil {
		t.Fatal("created was never made durable")
	}
	if len(afterCreate) == 0 {
		t.Fatal("no save observed")
	}
}

// Item 3: a session that names a run the backend lacks is relaunched; one whose run exists is not; an unknown stays resumable.
func TestANamedRunIsAskedOfTheBackendBeforeAReopen(t *testing.T) {
	p := factsRig(t)
	sid := openTrig(t, p)
	name := runName(sid)
	s := p.store.Get(sid)
	s.Lock()
	s.Run = &name
	s.Unlock()
	if ok, err := p.g.RunExists(ctx, sid); err != nil || ok {
		t.Fatalf("absent: %v %v", ok, err)
	}
	if out, err := p.g.LaunchTriggered(ctx, "triage-agent", sid, map[string]any{"prompt": "x"}); err != nil || !out.OK || len(p.api.Manifests) != 1 {
		t.Fatalf("a reserved name with no run must be made: %+v %v %d", out, err, len(p.api.Manifests))
	}
	if out, err := p.g.LaunchTriggered(ctx, "triage-agent", sid, map[string]any{"prompt": "x"}); err != nil || !out.OK || len(p.api.Manifests) != 1 {
		t.Fatalf("a run that is there must not be made twice: %+v %v %d", out, err, len(p.api.Manifests))
	}
}

// Item 4: a run a hold took away is recorded as removed, with the reason, before the row is decided; the session does not read as a
// launched run without that fact.
func TestARunTakenAwayByAHoldIsRecordedWithTheReason(t *testing.T) {
	p := factsRig(t)
	sid := openTrig(t, p)
	once := false
	p.g.SetRuns(&runs.Backend{Store: &gatedStore{FakeStore: p.api, onCreate: func(string) {
		if !once {
			once = true
			_, _ = p.g.HoldTriggers(ctx, p.person(adminID), "triage-agent", "storm", 1, "")
		}
	}}})
	out, _ := p.g.LaunchTriggered(ctx, "triage-agent", sid, map[string]any{"prompt": "x"})
	if out == nil || out.OK {
		t.Fatalf("the hold must stop the launch: %+v", out)
	}
	st := p.g.TriggerSessionState(sid)
	if st.Removed != session.RemovedHold {
		t.Fatalf("removal not recorded: %+v", st)
	}
	if row, _ := p.persist.row(sid); row.RunRemoved == nil || row.RunRemoved.Reason != session.RemovedHold {
		t.Fatalf("the removal is not durable: %+v", row.RunRemoved)
	}
	if len(p.api.Manifests) != 0 {
		t.Fatalf("the run is still there: %d", len(p.api.Manifests))
	}
}

// Item 5: a hold in the store is not seen until it is loaded; until then triggered opens fail closed, after it they are held.
func TestTheHoldCopyIsLoadedAtStartAndFailsClosedUntilThen(t *testing.T) {
	p := factsRig(t)
	now := t0
	if err := p.holds.Put(ctx, &dispatch.Hold{Agent: "triage-agent", By: adminID, Reason: "storm", Since: now, Until: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	p.g.Holds = p.holds // a restarted gateway: the store has the hold and nothing has read it
	p.g.Holds = p.holds
	if res, err := p.g.OpenTriggered(ctx, "triage-agent", "glidepath:e2", "t-bbbbbbbbbbbb"); err == nil && res != nil && res.OK {
		t.Fatalf("an open before the holds are loaded must not succeed: %+v", res)
	}
	if err := p.g.ReloadHolds(ctx); err != nil {
		t.Fatal(err)
	}
	if res, _ := p.g.OpenTriggered(ctx, "triage-agent", "glidepath:e2", "t-bbbbbbbbbbbb"); res == nil || res.OK {
		t.Fatalf("a loaded hold must hold: %+v", res)
	}
}

// flakyHolds is a HoldStore whose reads and writes can be made to fail; a write that fails still commits (the worst case).
type flakyHolds struct {
	*dispatch.MemoryHolds
	putErr, readErr error
}

func (f *flakyHolds) Put(c context.Context, h *dispatch.Hold) error {
	if f.putErr != nil {
		_ = f.MemoryHolds.Put(c, h) // committed, but the caller was told it failed
		return f.putErr
	}
	return f.MemoryHolds.Put(c, h)
}
func (f *flakyHolds) All(c context.Context) ([]*dispatch.Hold, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.MemoryHolds.All(c)
}
func (f *flakyHolds) Get(c context.Context, a string) (*dispatch.Hold, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.MemoryHolds.Get(c, a)
}

// A hold write that errored may have committed: the copy is distrusted, opens fail closed until the store is read again, and then
// the hold that did commit holds.
func TestAHoldWriteThatErroredInvalidatesTheCopy(t *testing.T) {
	p := factsRig(t)
	f := &flakyHolds{MemoryHolds: p.holds}
	p.g.Holds = f
	if err := p.g.ReloadHolds(ctx); err != nil {
		t.Fatal(err)
	}
	f.putErr = errors.New("connection reset")
	if _, err := p.g.HoldTriggers(ctx, p.person(adminID), "triage-agent", "storm", 1, ""); err == nil {
		t.Fatal("the failed write was reported as made")
	}
	f.putErr, f.readErr = nil, errors.New("database away")
	if res, err := p.g.OpenTriggered(ctx, "triage-agent", "glidepath:e3", "t-cccccccccccc"); err == nil && res != nil && res.OK {
		t.Fatalf("an open with an untrusted hold copy must fail closed: %+v", res)
	}
	f.readErr = nil
	if res, _ := p.g.OpenTriggered(ctx, "triage-agent", "glidepath:e3", "t-cccccccccccc"); res == nil || res.OK {
		t.Fatalf("the hold that committed must hold once the store is read: %+v", res)
	}
}
