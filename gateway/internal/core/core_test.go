package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/fakecore"
	"github.com/jfillman/clearance/client"
)

// The vector was made by Python: json.dumps(v, sort_keys=True, separators=(",", ":"), ensure_ascii=False).
func TestCanonicalIsPythonsCanonicalJSON(t *testing.T) {
	in := `{"b":[1,2.5,"é\n\t\"\\/<>&\u0001\u007f"],"a":{"x":null,"y":true,"z":1.0,"big":12345678901234567890},"é":"日本"}`
	d := json.NewDecoder(bytes.NewReader([]byte(in)))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	want := "{\"a\":{\"big\":12345678901234567890,\"x\":null,\"y\":true,\"z\":1.0},\"b\":[1,2.5,\"é\\n\\t\\\"\\\\/<>&\\u0001\x7f\"],\"é\":\"日本\"}"
	if got := string(core.Canonical(v)); got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	if got := core.ArgsDigest(v); got != "43c88ca3363c369070d10940a4720be62b0982e1649bfedce5a59423e6815439" {
		t.Fatalf("digest %s", got)
	}
}

func TestIsoIsTheRecordsTimestampFormat(t *testing.T) {
	ts := time.Date(2026, 11, 9, 12, 0, 1, 999, time.FixedZone("x", 3600))
	if got := core.Iso(ts); got != "2026-11-09T11:00:01Z" {
		t.Fatal(got)
	}
}

func newCore(t *testing.T, fc *fakecore.Core) (*core.Core, *core.MemoryOutbox) {
	ob := core.NewMemoryOutbox()
	return core.New(fc.Client(200*time.Millisecond), ob, false), ob
}

func TestAnUnreachableCoreIsNoDecisionAndA4xxIsRefusedNotAnAllow(t *testing.T) {
	fc := fakecore.New()
	defer fc.Close()
	c, _ := newCore(t, fc)
	fc.Down.Store(true)
	_, err := c.Evaluate(context.Background(), &client.EvaluationRequest{})
	var un *core.Unreachable
	if !errors.As(err, &un) || un.Refused || un.Rule() != core.OutageRule {
		t.Fatalf("%v", err)
	}
	if v := core.OutageVerdict(un, nil); v.Allow || v.Rule != core.OutageRule || v.Name != "core-unreachable" {
		t.Fatalf("%+v", v)
	}
	ref := &core.Unreachable{Msg: "bad request", Refused: true}
	if core.OutageVerdict(ref, nil).Rule != core.RefusedRule {
		t.Fatal("a refused request is CORE-REFUSED")
	}
}

func TestACallersOwnDeadlineIsNotTheCoresFault(t *testing.T) {
	fc := fakecore.New()
	defer fc.Close()
	fc.Latency = 300 * time.Millisecond
	cl, _ := client.New(client.Options{BaseURL: fc.URL, Token: client.StaticToken("x"), Timeout: 5 * time.Second, BreakerThreshold: -1})
	c := core.New(cl, nil, false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.Evaluate(ctx, &client.EvaluationRequest{})
	var un *core.Unreachable
	if !errors.As(err, &un) {
		t.Fatalf("%v", err)
	}
	if c.BreakerState() == "open" {
		t.Fatal("the caller's deadline moved the breaker")
	}
}

func TestAnOutcomeTheCoreCannotTakeWaitsAndKeepsOrder(t *testing.T) {
	fc := fakecore.New()
	defer fc.Close()
	c, ob := newCore(t, fc)
	fc.Down.Store(true)
	for _, id := range []string{"d-1", "d-2"} {
		if seq := c.Outcome(context.Background(), id, core.OutcomeArgs{Result: "ok"}); seq != nil {
			t.Fatal("posted while down")
		}
	}
	fc.Down.Store(false)
	// a new one must not overtake what waits
	if seq := c.Outcome(context.Background(), "d-3", core.OutcomeArgs{Result: "ok"}); seq == nil {
		t.Fatal("the new outcome should have drained the box first and then been posted")
	}
	var got []string
	for _, o := range fc.Outcomes {
		got = append(got, o.DecisionID)
	}
	if len(got) != 3 || got[0] != "d-1" || got[1] != "d-2" || got[2] != "d-3" || ob.Len("done") != 2 {
		t.Fatalf("%v done=%d", got, ob.Len("done"))
	}
}

func TestARequiredRecordFailsClosedInsteadOfQueueing(t *testing.T) {
	fc := fakecore.New()
	defer fc.Close()
	c, ob := newCore(t, fc)
	fc.Down.Store(true)
	_, err := c.Record(context.Background(), "run.launch", map[string]any{"session": "s-1", "decision": "allow", "result": "created"}, true)
	var un *core.Unreachable
	if !errors.As(err, &un) || ob.Len("pending") != 0 {
		t.Fatalf("%v pending=%d", err, ob.Len("pending"))
	}
	if _, err := c.Record(context.Background(), "run.launch", map[string]any{"session": "s-1"}, false); err != nil || ob.Len("pending") != 1 {
		t.Fatalf("%v pending=%d", err, ob.Len("pending"))
	}
}

func TestARefusalRuleOnlyGoesOnTheOutcomeWhenTheCoreWouldTakeIt(t *testing.T) {
	fc := fakecore.New()
	defer fc.Close()
	c, _ := newCore(t, fc)
	c.Outcome(context.Background(), "d-1", core.OutcomeArgs{Result: "refused", RefusalRule: "HR-FLOOD", RefusalMsg: "five"})
	c.Outcome(context.Background(), "d-2", core.OutcomeArgs{Result: "refused", RefusalRule: "not a rule!", RefusalMsg: "x", Effect: &client.Effect{Repo: "o/r"}})
	if r := fc.Outcomes[0].Body.Refusal; r == nil || r.Rule != "HR-FLOOD" {
		t.Fatalf("%+v", fc.Outcomes[0].Body)
	}
	if fc.Outcomes[1].Body.Refusal != nil || fc.Outcomes[1].Body.Effect == nil {
		t.Fatalf("%+v", fc.Outcomes[1].Body)
	}
}

// A decision that takes longer than the Clearance client's own 250 ms (a ledger append took 306 ms live on 2026-11-12) is
// decided, not denied, under the decision path's 1 s; and a timeout is retried once, so one slow attempt is no failure.
func TestASlowDecisionIsDecidedNotDenied(t *testing.T) {
	fc := fakecore.New()
	defer fc.Close()
	fc.Latency = 400 * time.Millisecond
	opt := core.ClientOptions(fc.URL, client.StaticToken("autopilot-system/autopilot-gateway"), 0)
	if opt.Timeout != time.Second || opt.Retries != 1 || opt.BreakerThreshold != 5 || core.DecisionTimeout <= client.DefaultTimeout {
		t.Fatalf("%+v", opt)
	}
	cl, err := client.New(opt)
	if err != nil {
		t.Fatal(err)
	}
	c := core.New(cl, core.NewMemoryOutbox(), false)
	v, err := c.Evaluate(context.Background(), &client.EvaluationRequest{})
	if err != nil || !v.Allow {
		t.Fatalf("%+v %v", v, err)
	}
	if fc.Evals.Load() != 1 {
		t.Fatalf("a 400 ms answer was asked for %d times", fc.Evals.Load())
	}
	// the client's own 250 ms would have timed out, been retried and failed
	old, _ := client.New(core.ClientOptions(fc.URL, client.StaticToken("x"), 250*time.Millisecond))
	if _, err := core.New(old, core.NewMemoryOutbox(), false).Evaluate(context.Background(), &client.EvaluationRequest{}); err == nil {
		t.Fatal("the old timeout should not have decided a 400 ms answer")
	}
	if o := core.ClientOptions(fc.URL, nil, 2500*time.Millisecond); o.Timeout != 2500*time.Millisecond {
		t.Fatal("CLEARANCE_CORE_TIMEOUT is not used")
	}
}

// ctxOutbox is a memory outbox whose Add fails when its context has ended (as Postgres's does).
type ctxOutbox struct{ *core.MemoryOutbox }

func (o ctxOutbox) Add(ctx context.Context, e core.Entry, reason, errText string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return o.MemoryOutbox.Add(ctx, e, reason, errText)
}

func (o ctxOutbox) HasPending(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return o.MemoryOutbox.HasPending(ctx)
}

// The caller's context is nearly spent by the time an outcome that cannot be posted is queued (the pending check and the drain used it): the
// entry must still be queued, under a context of its own.
func TestAnOutcomeIsQueuedEvenWhenTheCallersContextIsSpent(t *testing.T) {
	fc := fakecore.New()
	defer fc.Close()
	mem := core.NewMemoryOutbox()
	c := core.New(fc.Client(200*time.Millisecond), ctxOutbox{mem}, false)
	fc.Down.Store(true)
	if seq := c.Outcome(context.Background(), "d-1", core.OutcomeArgs{Result: "ok"}); seq != nil || mem.Len("pending") != 1 {
		t.Fatalf("the first outcome was not queued")
	}
	spent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second)) // what is left of the caller's time is nothing
	defer cancel()
	// something already waits, so this goes down the pending branch (drain fails, then queue)
	if seq := c.Outcome(spent, "d-2", core.OutcomeArgs{Result: "ok"}); seq != nil {
		t.Fatal("posted while down")
	}
	if mem.Len("pending") != 2 {
		t.Fatalf("the outcome was lost: %d queued", mem.Len("pending"))
	}
	// an outage read is queued the same way
	c.OutageRead(spent, map[string]any{"tool": "catalog.read"})
	if mem.Len("pending") != 3 {
		t.Fatalf("the outage read was lost: %d queued", mem.Len("pending"))
	}
}

// failingOutbox cannot be written (its store is down).
type failingOutbox struct{ *core.MemoryOutbox }

func (failingOutbox) Add(context.Context, core.Entry, string, string) (int64, error) {
	return 0, errors.New("the outbox store is down")
}

// An outcome with paths that can be neither posted nor queued is in neither Clearance nor the outbox: a Preflight score of a run that could
// have made it is refused (CORE-OUTCOME-LOST), and one of a run that began after it is not; an outcome with no paths does not count.
func TestALostOutcomeWithPathsRefusesTheScoresOfRunsThatCouldHaveMadeIt(t *testing.T) {
	fc := fakecore.New()
	defer fc.Close()
	c := core.New(fc.Client(200*time.Millisecond), failingOutbox{core.NewMemoryOutbox()}, false)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour) // a run that began an hour ago
	if _, err := c.BeginEffectCheck(ctx, t0); err != nil {
		t.Fatalf("nothing was lost yet: %v", err)
	}
	fc.Down.Store(true)
	c.Outcome(ctx, "d-1", core.OutcomeArgs{Result: "ok"}) // no effect: lost, but it carried no paths
	if _, err := c.BeginEffectCheck(ctx, t0); err != nil {
		t.Fatalf("an outcome with no paths was counted: %v", err)
	}
	c.Outcome(ctx, "d-2", core.OutcomeArgs{Result: "ok", Effect: &client.Effect{Repo: "o/r", PR: 7, Paths: []string{"secrets/prod.yaml"}}})
	var blocked *core.OutboxBlocked
	if _, err := c.BeginEffectCheck(ctx, t0); !errors.As(err, &blocked) || blocked.Rule != core.OutcomeLostRule {
		t.Fatalf("%v", err)
	}
	if _, err := c.BeginEffectCheck(ctx, time.Now().Add(time.Hour)); err != nil { // a run that began after it
		t.Fatalf("a later run was refused: %v", err)
	}
}

// The pending check is asked on a context of its own: with the caller's already ended it would fail and the outcome would be dropped before
// anything was queued (a store whose HasPending fails on a done context, as Postgres's does).
func TestTheOutboxIsAskedWhetherAnythingWaitsOnAContextOfItsOwn(t *testing.T) {
	fc := fakecore.New()
	defer fc.Close()
	mem := core.NewMemoryOutbox()
	c := core.New(fc.Client(200*time.Millisecond), ctxOutbox{mem}, false)
	fc.Down.Store(true)
	spent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	// nothing waits yet: the caller's context has ended, the core is down, the outcome is queued
	c.Outcome(spent, "d-1", core.OutcomeArgs{Result: "ok", Effect: &client.Effect{Paths: []string{"a.yaml"}}})
	if mem.Len("pending") != 1 {
		t.Fatalf("the outcome was lost: %d queued", mem.Len("pending"))
	}
	// something waits now (the pending branch asks twice)
	c.Outcome(spent, "d-2", core.OutcomeArgs{Result: "ok", Effect: &client.Effect{Paths: []string{"b.yaml"}}})
	if mem.Len("pending") != 2 {
		t.Fatalf("the outcome was lost: %d queued", mem.Len("pending"))
	}
	// and nothing was counted as lost for the guard
	if _, err := c.BeginEffectCheck(context.Background(), time.Now().Add(-time.Hour)); err == nil {
		t.Fatal("the pending entries should block, not pass")
	} else if b, ok := err.(*core.OutboxBlocked); !ok || b.Rule != core.OutboxPendingRule {
		t.Fatalf("%v", err) // a lost outcome would be OutcomeLostRule
	}
}

// An outcome lost during the read of Clearance (after the guard began) is caught by Finish, which repeats the lost-outcome check.
func TestEffectGuardFinishBlocksAnOutcomeLostDuringTheRead(t *testing.T) {
	fc := fakecore.New()
	defer fc.Close()
	c := core.New(fc.Client(200*time.Millisecond), failingOutbox{core.NewMemoryOutbox()}, false)
	ctx := context.Background()
	since := time.Now().Add(-time.Hour)
	g, err := c.BeginEffectCheck(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Finish(ctx); err != nil {
		t.Fatalf("nothing was lost: %v", err)
	}
	fc.Down.Store(true) // mid-read: a write's outcome can be neither posted nor queued
	c.Outcome(ctx, "d-1", core.OutcomeArgs{Result: "ok", Effect: &client.Effect{Paths: []string{"secrets/prod.yaml"}}})
	var blocked *core.OutboxBlocked
	if err := g.Finish(ctx); !errors.As(err, &blocked) || blocked.Rule != core.OutcomeLostRule {
		t.Fatalf("%v", err)
	}
}

// lossInEntries is an outbox that cannot be written and, once armed, runs fire (which loses an outcome) from inside its next Entries query.
type lossInEntries struct {
	*core.MemoryOutbox
	armed bool
	fire  func()
}

func (o *lossInEntries) Add(context.Context, core.Entry, string, string) (int64, error) {
	return 0, errors.New("the outbox store is down")
}

func (o *lossInEntries) Entries(ctx context.Context, q core.OutboxQuery) ([]core.OutboxRow, error) {
	if o.armed {
		o.armed = false
		o.fire()
	}
	return o.MemoryOutbox.Entries(ctx, q)
}

// An outcome lost while Finish's own outbox query runs is seen: the lost-outcome check comes after that query.
func TestEffectGuardFinishBlocksAnOutcomeLostWhileItsOutboxQueryRuns(t *testing.T) {
	fc := fakecore.New()
	defer fc.Close()
	ob := &lossInEntries{MemoryOutbox: core.NewMemoryOutbox()}
	c := core.New(fc.Client(200*time.Millisecond), ob, false)
	ctx := context.Background()
	g, err := c.BeginEffectCheck(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	fc.Down.Store(true)
	ob.fire = func() {
		c.Outcome(ctx, "d-1", core.OutcomeArgs{Result: "ok", Effect: &client.Effect{Paths: []string{"secrets/prod.yaml"}}})
	}
	ob.armed = true
	var blocked *core.OutboxBlocked
	if err := g.Finish(ctx); !errors.As(err, &blocked) || blocked.Rule != core.OutcomeLostRule {
		t.Fatalf("%v", err)
	}
}

// memLost is a LostStore that outlives a Core, as a database does.
type memLost struct {
	at   time.Time
	fail bool
}

func (m *memLost) Mark(_ context.Context, at time.Time) error {
	if m.fail {
		return errors.New("the marker store is down")
	}
	if at.After(m.at) {
		m.at = at
	}
	return nil
}
func (m *memLost) Load(context.Context) (time.Time, error) { return m.at, nil }
func (m *memLost) Clear(context.Context) error             { m.at = time.Time{}; return nil }

// The lost-outcome marker is set when an outcome with paths is lost, is read back by a new core over the same store (a restart), and is
// gone once cleared.
func TestTheLostOutcomeMarkerSurvivesARestartAndIsCleared(t *testing.T) {
	fc := fakecore.New()
	defer fc.Close()
	ctx := context.Background()
	store := &memLost{}
	c := core.New(fc.Client(200*time.Millisecond), failingOutbox{core.NewMemoryOutbox()}, false)
	if err := c.UseLostStore(ctx, store); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour)
	fc.Down.Store(true)
	c.Outcome(ctx, "d-1", core.OutcomeArgs{Result: "ok", Effect: &client.Effect{Paths: []string{"a.yaml"}}})
	if store.at.IsZero() { // set
		t.Fatal("the marker was not stored")
	}
	// restart: a new core over the same store, with a healthy outbox, still refuses
	c2 := core.New(fc.Client(200*time.Millisecond), core.NewMemoryOutbox(), false)
	if _, err := c2.BeginEffectCheck(ctx, t0); err != nil {
		t.Fatalf("a fresh core with no store knew of a loss: %v", err)
	}
	if err := c2.UseLostStore(ctx, store); err != nil {
		t.Fatal(err)
	}
	var blocked *core.OutboxBlocked
	if _, err := c2.BeginEffectCheck(ctx, t0); !errors.As(err, &blocked) || blocked.Rule != core.OutcomeLostRule {
		t.Fatalf("the marker did not survive the restart: %v", err)
	}
	if _, err := c2.BeginEffectCheck(ctx, time.Now().Add(time.Hour)); err != nil { // a run that began after it
		t.Fatalf("a later run was refused: %v", err)
	}
	// clear
	if err := c2.ClearLost(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.BeginEffectCheck(ctx, t0); err != nil {
		t.Fatalf("still refused after the clear: %v", err)
	}
	c3 := core.New(fc.Client(200*time.Millisecond), core.NewMemoryOutbox(), false)
	if err := c3.UseLostStore(ctx, store); err != nil {
		t.Fatal(err)
	}
	if _, err := c3.BeginEffectCheck(ctx, t0); err != nil {
		t.Fatalf("the clear did not reach the store: %v", err)
	}
}

// A marker store that cannot be written still leaves the marker in force in this process.
func TestALostOutcomeIsKeptInMemoryWhenItsStoreIsDown(t *testing.T) {
	fc := fakecore.New()
	defer fc.Close()
	ctx := context.Background()
	c := core.New(fc.Client(200*time.Millisecond), failingOutbox{core.NewMemoryOutbox()}, false)
	if err := c.UseLostStore(ctx, &memLost{fail: true}); err != nil {
		t.Fatal(err)
	}
	fc.Down.Store(true)
	c.Outcome(ctx, "d-1", core.OutcomeArgs{Result: "ok", Effect: &client.Effect{Paths: []string{"a.yaml"}}})
	var blocked *core.OutboxBlocked
	if _, err := c.BeginEffectCheck(ctx, time.Now().Add(-time.Hour)); !errors.As(err, &blocked) || blocked.Rule != core.OutcomeLostRule {
		t.Fatalf("%v", err)
	}
}
