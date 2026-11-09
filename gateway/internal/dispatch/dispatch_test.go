package dispatch_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/auth"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/fakecore"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/clearance/client"
)

var alice = &dispatch.Principal{ID: "user:alice", Kind: "user", TierCeiling: 1}

type rig struct {
	fc    *fakecore.Core
	g     *dispatch.Gateway
	store *session.Store
	s     *session.Session
	ob    *core.MemoryOutbox
}

func newRig(t *testing.T, handlers map[string]dispatch.Handler, allowReads bool) *rig {
	t.Helper()
	fc := fakecore.New()
	t.Cleanup(fc.Close)
	st, _ := session.NewStore(context.Background(), nil)
	ob := core.NewMemoryOutbox()
	c := core.New(fc.Client(300*time.Millisecond), ob, allowReads)
	reg := tools.Default()
	defs := map[string]*definitions.Definition{"coding-agent": {Name: "coding-agent", Kind: "task", DenyPaths: []string{".github/**"}}}
	g, err := dispatch.New(defs, st, c, auth.Static{"t": alice}, reg, handlers)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s := st.Adopt(&client.Grant{Session: "s-1", TaskID: "t-1", Principal: "user:alice", Agent: "coding-agent",
		Limits:    client.Limits{TierCeiling: "T1", TTLMinutes: 30, ToolCalls: 100, GithubCalls: 100, OpenPRs: 3, Tools: []string{"catalog.read", "metrics.query", "repo.pr.open"}, Models: []string{}, Network: "clearance", Compute: "small"},
		ExpiresAt: now.Add(30 * time.Minute)}, now, nil, 0)
	return &rig{fc, g, st, s, ob}
}

func ok(data any) dispatch.Handler {
	return func(context.Context, *session.Session, map[string]any) (any, error) { return data, nil }
}

func TestAllowedCallRunsTheBackendChargesAndReportsTheOutcome(t *testing.T) {
	r := newRig(t, map[string]dispatch.Handler{"catalog.read": ok(map[string]any{"n": 1})}, false)
	res, err := r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": "x"})
	if err != nil || !res.OK || res.Decision["allow"] != true {
		t.Fatalf("%+v %v", res, err)
	}
	if r.s.Used.ToolCalls != 1 {
		t.Fatalf("the core's spend was not applied: %+v", r.s.Used)
	}
	if got := r.fc.LastOutcome().Body.Result; got != "ok" || r.fc.OutcomeCount() != 1 {
		t.Fatalf("outcome %q (%d)", got, r.fc.OutcomeCount())
	}
	if res.AuditSeq == nil {
		t.Fatal("no audit seq")
	}
}

func TestADenyIsNotActedOnAndIsCharged(t *testing.T) {
	var ran atomic.Bool
	r := newRig(t, map[string]dispatch.Handler{"catalog.read": func(context.Context, *session.Session, map[string]any) (any, error) {
		ran.Store(true)
		return nil, nil
	}}, false)
	r.fc.Decide = func(*client.EvaluationRequest) client.EvaluationResult {
		return fakecore.Deny("R006", "tool is not permitted for this session", client.Spend{Denial: true})
	}
	res, _ := r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": "x"})
	if res.OK || ran.Load() || res.Decision["rule"] != "R006" || r.s.Denials != 1 || r.fc.OutcomeCount() != 0 {
		t.Fatalf("%+v ran=%v denials=%d outcomes=%d", res, ran.Load(), r.s.Denials, r.fc.OutcomeCount())
	}
}

func TestNoBackendIsRefusedAsSuchAndChargedNothing(t *testing.T) {
	r := newRig(t, nil, false)
	res, _ := r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": "x"})
	if res.OK || *res.Error != "no backend for this tool in this deployment" || r.s.Used.ToolCalls != 0 {
		t.Fatalf("%+v %+v", res, r.s.Used)
	}
	if r.fc.LastOutcome().Body.Result != "error:NoHandler" {
		t.Fatal(r.fc.LastOutcome().Body.Result)
	}
}

func TestAHandlerRefusalIsAnOutcomeRefusalAndCountsTowardTheBreaker(t *testing.T) {
	r := newRig(t, map[string]dispatch.Handler{"catalog.read": func(context.Context, *session.Session, map[string]any) (any, error) {
		return nil, &core.Denied{Rule: "READ-QUERY", Message: "empty query", Hint: "Pass one."}
	}}, false)
	for i := 0; i < session.DenialTrip; i++ {
		res, _ := r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": "x"})
		if res.OK || res.Decision["rule"] != "READ-QUERY" || res.Decision["name"] != "refused" {
			t.Fatalf("%+v", res)
		}
	}
	o := r.fc.LastOutcome().Body
	if o.Refusal == nil || o.Refusal.Rule != "READ-QUERY" || !r.s.Tripped {
		t.Fatalf("%+v tripped=%v", o, r.s.Tripped)
	}
}

func TestABackendErrorNeverLeaks(t *testing.T) {
	r := newRig(t, map[string]dispatch.Handler{"catalog.read": func(context.Context, *session.Session, map[string]any) (any, error) {
		return nil, errors.New("secret dsn postgres://u:p@h/db")
	}}, false)
	res, _ := r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": "x"})
	if res.OK || *res.Error != "backend error" || r.fc.LastOutcome().Body.Result != "error:BackendError" {
		t.Fatalf("%+v %s", res, r.fc.LastOutcome().Body.Result)
	}
}

func TestAPanickingBackendIsAnErrorNotACrash(t *testing.T) {
	r := newRig(t, map[string]dispatch.Handler{"catalog.read": func(context.Context, *session.Session, map[string]any) (any, error) {
		panic("boom")
	}}, false)
	res, _ := r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": "x"})
	if res.OK || r.fc.LastOutcome().Body.Result != "error:Panic" {
		t.Fatalf("%+v", res)
	}
}

func TestEveryDispatchRunsUnderADeadlineAndStillReportsItsOutcome(t *testing.T) {
	r := newRig(t, map[string]dispatch.Handler{"catalog.read": func(ctx context.Context, _ *session.Session, _ map[string]any) (any, error) {
		<-ctx.Done() // a backend that hangs
		return nil, ctx.Err()
	}}, false)
	r.g.CallTimeout = 150 * time.Millisecond
	t0 := time.Now()
	res, _ := r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": "x"})
	if took := time.Since(t0); took > 2*time.Second {
		t.Fatalf("the call outlived its deadline: %v", took)
	}
	if res.OK || r.fc.LastOutcome().Body.Result != "error:Timeout" {
		t.Fatalf("%+v outcome %d", res, r.fc.OutcomeCount())
	}
}

func TestOutageDeniesWhatNeedsTheCoreAndFailsClosed(t *testing.T) {
	r := newRig(t, map[string]dispatch.Handler{"repo.pr.open": ok(nil), "catalog.read": ok(nil)}, false)
	r.fc.Down.Store(true)
	for _, tool := range []string{"repo.pr.open", "catalog.read"} {
		res, _ := r.g.Call(context.Background(), "t", "s-1", tool, map[string]any{"query": "x"})
		if res.OK || res.Decision["rule"] != core.OutageRule {
			t.Fatalf("%s: %+v", tool, res)
		}
	}
}

func TestOutageAllowsAnUnscopedT0ReadWhenTheDeploymentSaysSoAndQueuesTheRecord(t *testing.T) {
	r := newRig(t, map[string]dispatch.Handler{"metrics.query": ok(map[string]any{"v": 1}), "catalog.read": ok(nil)}, true)
	r.fc.Down.Store(true)
	res, _ := r.g.Call(context.Background(), "t", "s-1", "metrics.query", map[string]any{"promql": "up"})
	if !res.OK || res.Decision["allow"] != true || r.s.Used.ToolCalls != 1 {
		t.Fatalf("%+v", res)
	}
	rows, _ := r.ob.Pending(context.Background(), 10)
	if len(rows) != 1 || rows[0].Entry.Kind != "tool.call" || rows[0].Entry.Body["coverage"] != "coverage-unavailable" || rows[0].Entry.Body["tool"] != "metrics.query" {
		t.Fatalf("%+v", rows)
	}
	// a read whose arguments name a scope only the core can check is still denied
	res, _ = r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": "x", "app": "flight-api"})
	if res.OK {
		t.Fatalf("a scoped read ran during the outage: %+v", res)
	}
}

func TestTheOutboxIsPostedInOrderOnceTheCoreAnswers(t *testing.T) {
	r := newRig(t, map[string]dispatch.Handler{"catalog.read": ok(nil)}, false)
	r.fc.Down.Store(false)
	res, _ := r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": "x"})
	if !res.OK {
		t.Fatal("setup")
	}
	// the outcome could not be posted: queue one by hand through a Down core, then bring it back
	r.fc.Down.Store(true)
	r.g.Core.Outcome(context.Background(), "d-x", core.OutcomeArgs{Result: "ok"})
	r.g.Core.Outcome(context.Background(), "d-y", core.OutcomeArgs{Result: "ok"})
	if rows, _ := r.ob.Pending(context.Background(), 10); len(rows) != 2 {
		t.Fatalf("queued %d", len(rows))
	}
	r.fc.Down.Store(false)
	if n := r.g.Core.Drain(context.Background(), 10); n != 2 {
		t.Fatalf("drained %d", n)
	}
	if last := r.fc.LastOutcome(); last.DecisionID != "d-y" {
		t.Fatalf("order: %s", last.DecisionID)
	}
}

func TestOpenAdoptsTheCoresGrantAndGivesBackADeniedId(t *testing.T) {
	r := newRig(t, nil, false)
	r.g.Defs["coding-agent"].Limits = client.Limits{TierCeiling: "T1"}
	r.fc.Decide = func(req *client.EvaluationRequest) client.EvaluationResult {
		id := "d-open"
		if req.Resource.ID == "nope" {
			return fakecore.Deny("R003", "unknown agent", client.Spend{})
		}
		return client.EvaluationResult{Decision: true, DecisionID: &id, Record: &client.RecordRef{Seq: 7},
			Context: client.DecisionContext{Grant: &client.Grant{Session: req.Context.NewSessionID, TaskID: "t-9", Principal: "user:alice", Agent: "coding-agent",
				Limits: client.Limits{TierCeiling: "T1", TTLMinutes: 30, Tools: []string{}, Models: []string{}}, ExpiresAt: time.Now().Add(time.Hour)}}}
	}
	r.store.SetIDs(func() string { return "s-new" })
	res, err := r.g.Open(context.Background(), "t", "coding-agent", nil, "")
	if err != nil || !res.OK || r.store.Get("s-new") == nil {
		t.Fatalf("%+v %v", res, err)
	}
	res, _ = r.g.Open(context.Background(), "t", "nope", nil, "")
	if res.OK || res.Decision["rule"] != "R003" {
		t.Fatalf("%+v", res)
	}
	if id := r.store.NewID(); id != "s-new" && id == "" {
		t.Fatal(id)
	}
}

func TestSessionsAreIndependentButOneSessionDoesNotRaceItself(t *testing.T) {
	var inflight, max atomic.Int32
	r := newRig(t, map[string]dispatch.Handler{"catalog.read": func(context.Context, *session.Session, map[string]any) (any, error) {
		n := inflight.Add(1)
		for {
			m := max.Load()
			if n <= m || max.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inflight.Add(-1)
		return nil, nil
	}}, false)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			_, _ = r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": "x"})
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if max.Load() != 1 || r.s.Used.ToolCalls != 8 {
		t.Fatalf("one session ran %d at once, used %d", max.Load(), r.s.Used.ToolCalls)
	}
}

func TestHandlersMustBeRegisteredAndNotCoreOwned(t *testing.T) {
	st, _ := session.NewStore(context.Background(), nil)
	for name, want := range map[string]string{"nope.tool": "not registered", "run.spawn": "Gateway owns"} {
		_, err := dispatch.New(nil, st, nil, auth.Static{}, tools.Default(), map[string]dispatch.Handler{name: ok(nil)})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestEnvTierOf(t *testing.T) {
	reg := tools.Default()
	spec, _ := reg.Get("glidepath.run.rerun")
	for args, want := range map[string]string{"project:x-lower": "lower", "env:pr-7": "lower", "env:agent-x": "lower", "env:prod": "upper",
		"repo:o/gitops-x": "upper", "repo:o/x": "lower"} {
		k, v, _ := cut(args)
		if got := dispatch.EnvTierOf(spec, map[string]any{k: v}); got != want {
			t.Errorf("%s: %s want %s", args, got, want)
		}
	}
	none, _ := reg.Get("catalog.read")
	if dispatch.EnvTierOf(none, map[string]any{"repo": "o/gitops-x"}) != "none" {
		t.Error("a tool with no target is none")
	}
}

func cut(s string) (string, string, bool) { return strings.Cut(s, ":") }
