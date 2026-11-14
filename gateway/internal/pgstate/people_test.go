package pgstate_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/pgstate"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func open(t *testing.T) (*pgstate.State, context.Context) {
	t.Helper()
	ctx := context.Background()
	st, err := pgstate.Open(ctx, scratch(t), 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close(ctx) })
	return st, ctx
}

func TestTheInboxRoundTripsAndKeepsItsLimits(t *testing.T) {
	st, ctx := open(t)
	in := pgstate.NewInbox(st)
	grp := "group:default/team"
	r := &dispatch.HumanRequest{ID: "h-0000000000aa", TaskID: "t-1", Session: "s-1", Agent: "a", Owner: "user:alice", Group: &grp, Question: "q?", Options: []string{"a", "b"},
		CreatedAt: t0, ExpiresAt: t0.Add(time.Hour), Status: "pending"}
	if err := in.Add(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := in.Get(ctx, r.ID)
	if err != nil || got == nil || got.Question != "q?" || *got.Group != grp || len(got.Options) != 2 || !got.CreatedAt.Equal(t0) {
		t.Fatalf("%+v %v", got, err)
	}
	if none, _ := in.Get(ctx, "h-nope"); none != nil {
		t.Fatal("a request that is not there")
	}
	if n, _ := in.CountPending(ctx, "s-1", t0); n != 1 {
		t.Fatal(n)
	}
	if n, _ := in.CountPending(ctx, "s-1", t0.Add(2*time.Hour)); n != 0 {
		t.Fatal("an expired request counts")
	}
	for i, id := range []string{"h-0000000000ac", "h-0000000000ab"} {
		_ = in.Add(ctx, &dispatch.HumanRequest{ID: id, TaskID: "t-1", Session: "s-1", Agent: "a", Owner: "user:alice", Question: "q", CreatedAt: t0.Add(time.Duration(2-i) * time.Second),
			ExpiresAt: t0.Add(time.Hour), Status: "pending"})
	}
	list, _ := in.Pending(ctx, t0)
	if len(list) != 3 || list[0].ID != "h-0000000000aa" || list[1].ID != "h-0000000000ab" {
		t.Fatalf("oldest first: %v", list)
	}
	ans, by := "yes", "user:alice"
	r.Status, r.Answer, r.AnsweredBy, r.AnsweredAt = "answered", &ans, &by, &t0
	if err := in.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	if list, _ = in.Pending(ctx, t0); len(list) != 2 {
		t.Fatalf("an answered request is pending: %d", len(list))
	}
	got, _ = in.Get(ctx, r.ID)
	if got.Status != "answered" || *got.Answer != "yes" || *got.AnsweredBy != "user:alice" || got.AnsweredVia != nil {
		t.Fatalf("%+v", got)
	}
	if n, err := in.Prune(ctx, t0.Add(3*time.Hour)); err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
}

func TestHoldsRoundTrip(t *testing.T) {
	st, ctx := open(t)
	h := pgstate.NewHolds(st)
	via := "hangar-prod/tower/backend"
	if err := h.Put(ctx, &dispatch.Hold{Agent: "triage-agent", By: "user:root", Via: &via, Reason: "storm", Since: t0, Until: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err := h.Get(ctx, "triage-agent")
	if err != nil || got == nil || got.By != "user:root" || *got.Via != via || !got.Until.Equal(t0.Add(time.Hour)) {
		t.Fatalf("%+v %v", got, err)
	}
	_ = h.Put(ctx, &dispatch.Hold{Agent: "triage-agent", By: "user:bob", Reason: "again", Since: t0, Until: t0.Add(2 * time.Hour)})
	if got, _ = h.Get(ctx, "triage-agent"); got.By != "user:bob" || got.Via != nil {
		t.Fatalf("a renewal replaces the hold: %+v", got)
	}
	if all, _ := h.All(ctx); len(all) != 1 {
		t.Fatal(all)
	}
	_ = h.Delete(ctx, "triage-agent")
	if got, _ = h.Get(ctx, "triage-agent"); got != nil {
		t.Fatal("not deleted")
	}
}

func TestEventHistoryIsOneRowPerEvent(t *testing.T) {
	st, ctx := open(t)
	ev := pgstate.NewEvents(st)
	app, rk := "gate-api", "gate-api.pipelinerun.finished"
	rec := &dispatch.EventRecord{ID: "e1", ReceivedAt: t0, Source: "glidepath", App: &app, RoutingKey: &rk, Subject: map[string]any{"id": "pr-1", "content": map[string]any{}},
		Outcomes: []map[string]any{{"agent": "triage-agent", "result": "launched"}}}
	for i := 0; i < 2; i++ { // a replay replaces the row
		if err := ev.Record(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM gateway_events WHERE id = 'e1' AND app = 'gate-api' AND record->>'routing_key' = $1", rk).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
}

// The claim is one statement and the primary key decides: of many racing for one trigger, exactly one wins. A claim left `claimed`
// is an orphan only once it is older than the cut-off; a decided one is never claimed again.
func TestTheLedgerClaimIsWonOnce(t *testing.T) {
	st, ctx := open(t)
	l := pgstate.NewTriggers(st)
	var won atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := l.Claim(ctx, "triage-agent", "k1", "stream", "e1", t0, t0.Add(-time.Minute)); err != nil {
				t.Error(err)
			} else if ok {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d claims won", won.Load())
	}
	row, _ := l.Get(ctx, "triage-agent", "k1")
	if row == nil || row.State != "claimed" || row.Via != "stream" || *row.EventID != "e1" {
		t.Fatalf("%+v", row)
	}
	if ok, _ := l.Claim(ctx, "triage-agent", "k1", "recent", "e2", t0.Add(time.Minute), t0.Add(-time.Second)); ok {
		t.Fatal("a live claim was taken")
	}
	if ok, _ := l.Claim(ctx, "triage-agent", "k1", "recent", "e2", t0.Add(time.Hour), t0.Add(time.Minute)); !ok {
		t.Fatal("an orphan was not taken")
	}
	_ = l.Decide(ctx, "triage-agent", "k1", "launched", "s-1", "r-1")
	if ok, _ := l.Claim(ctx, "triage-agent", "k1", "recent", "e3", t0.Add(48*time.Hour), t0.Add(47*time.Hour)); ok {
		t.Fatal("a decided trigger was claimed again")
	}
	row, _ = l.Get(ctx, "triage-agent", "k1")
	if row.State != "launched" || *row.Session != "s-1" || *row.Run != "r-1" {
		t.Fatalf("%+v", row)
	}
	if err := l.Decide(ctx, "triage-agent", "k1", "not-a-state", "", ""); err == nil {
		t.Fatal("a state outside the set was written")
	}
	if _, err := l.Claim(ctx, "triage-agent", "k2", "carrier-pigeon", "", t0, t0); err == nil {
		t.Fatal("a path outside the set was written")
	}
	if n, _ := l.Prune(ctx, t0.Add(31*24*time.Hour)); n != 1 {
		t.Fatalf("%d pruned", n)
	}
}

func TestTheCursorOnlyMovesForwardAndKeepsItsFloor(t *testing.T) {
	st, ctx := open(t)
	l := pgstate.NewTriggers(st)
	c, err := l.Cursor(ctx, "glidepath", t0)
	if err != nil || c.LastID != nil || !c.Floor.Equal(t0) {
		t.Fatalf("%+v %v", c, err)
	}
	if c, _ = l.Cursor(ctx, "glidepath", t0.Add(time.Hour)); !c.Floor.Equal(t0) {
		t.Fatal("the floor moved")
	}
	for _, id := range []string{"1794000000000000-bb", "1794000000000000-aa", "1793000000000000-zz", "1794000000000001-00"} {
		if err := l.Advance(ctx, "glidepath", id); err != nil {
			t.Fatal(err)
		}
	}
	if c, _ = l.Cursor(ctx, "glidepath", t0); *c.LastID != "1794000000000001-00" {
		t.Fatalf("%s", *c.LastID)
	}
	// bytewise, not the locale's: upper case sorts before lower case
	_ = l.Advance(ctx, "glidepath", "1794000000000001-0A")
	if c, _ = l.Cursor(ctx, "glidepath", t0); *c.LastID != "1794000000000001-0A" {
		t.Fatalf("%s", *c.LastID)
	}
	_ = l.Advance(ctx, "glidepath", "1794000000000001-0a")
	_ = l.Advance(ctx, "glidepath", "1794000000000001-0B")
	if c, _ = l.Cursor(ctx, "glidepath", t0); *c.LastID != "1794000000000001-0a" {
		t.Fatalf("%s: compared by the locale", *c.LastID)
	}
}
