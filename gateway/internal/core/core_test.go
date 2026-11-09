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
