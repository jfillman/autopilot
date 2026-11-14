package dispatch_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/auth"
	"github.com/jfillman/autopilot/gateway/internal/backends"
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
	n := 0
	r.store.SetIDs(func() string { n++; return fmt.Sprintf("s-new-%d", n) })
	res, err := r.g.Open(context.Background(), "t", "coding-agent", nil, "")
	if err != nil || !res.OK || r.store.Get("s-new-1") == nil {
		t.Fatalf("%+v %v", res, err)
	}
	res, _ = r.g.Open(context.Background(), "t", "nope", nil, "")
	if res.OK || res.Decision["rule"] != "R003" {
		t.Fatalf("%+v", res)
	}
	// the denied open took s-new-2 and must have given it back: the next session gets it, so ids are spent only on sessions that exist
	if id := r.store.NewID(); id != "s-new-2" {
		t.Fatalf("a denied open did not give its id back: next is %q", id)
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

// -- backend failures: what the agent is told, what the outcome records, what the log says (A-11 backend error detail) --

// pyErr is a backend error built from what the Python gateway's exception offered (testdata/backend_errors.json).
type pyErr struct{ class, text, evidence, agentMessage string }

func (e *pyErr) Error() string        { return e.text }
func (e *pyErr) Kind() string         { return e.class }
func (e *pyErr) Evidence() string     { return e.evidence }
func (e *pyErr) AgentMessage() string { return e.agentMessage }

type backendErrorCase struct {
	Class         string  `json:"class"`
	Text          string  `json:"text"`
	Evidence      *string `json:"evidence"`
	AgentMessage  *string `json:"agent_message"`
	AgentError    string  `json:"agent_error"`
	OutcomeResult string  `json:"outcome_result"`
}

func backendErrorCases(t *testing.T) []backendErrorCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/backend_errors.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []backendErrorCase
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) < 7 {
		t.Fatalf("%d cases: %v", len(cases), err)
	}
	return cases
}

func (c backendErrorCase) goError() error {
	switch {
	case c.Class == "ReadError":
		return &backends.ReadError{Msg: c.Text}
	case c.Evidence == nil && c.AgentMessage == nil:
		return errors.New(c.Text) // an exception with no evidence: Go has no class name for it
	}
	e := &pyErr{class: c.Class, text: c.Text}
	if c.Evidence != nil {
		e.evidence = *c.Evidence
	}
	if c.AgentMessage != nil {
		e.agentMessage = *c.AgentMessage
	}
	return e
}

// For each failure a Python backend raises, the Go dispatch tells the agent what the Python one tells it and writes the
// outcome the Python one writes: tool calls and the outage-time reads alike.
func TestABackendFailureIsToldAndRecordedAsThePythonGatewayDoes(t *testing.T) {
	for _, c := range backendErrorCases(t) {
		c := c
		t.Run(c.Class+"/"+c.Text[:12], func(t *testing.T) {
			want := c.OutcomeResult
			if c.Class == "RuntimeError" {
				// the one difference: Go has no name for a Python exception class, so an error with no kind of its own
				// reads error:BackendError where Python writes error:RuntimeError. Nothing in the gateway raises one.
				want = "error:BackendError"
			}
			h := map[string]dispatch.Handler{"catalog.read": func(context.Context, *session.Session, map[string]any) (any, error) { return nil, c.goError() }}
			r := newRig(t, h, true)
			res, _ := r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": "x"})
			if res.OK || *res.Error != c.AgentError || r.fc.LastOutcome().Body.Result != want {
				t.Fatalf("error %q (want %q), outcome %q (want %q)", *res.Error, c.AgentError, r.fc.LastOutcome().Body.Result, want)
			}
			// the same failure during an outage: the coverage-unavailable record carries the same result
			r.fc.Down.Store(true)
			res, _ = r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": "x"})
			rows, _ := r.ob.Pending(context.Background(), 10)
			if res.OK || *res.Error != c.AgentError || len(rows) == 0 || rows[len(rows)-1].Entry.Body["result"] != want {
				t.Fatalf("outage: %+v %+v", res, rows)
			}
		})
	}
}

func TestABackendFailureIsLoggedWithItsEvidenceAndTheAgentHearsNothingOfIt(t *testing.T) {
	var buf bytes.Buffer
	r := newRig(t, map[string]dispatch.Handler{"catalog.read": func(context.Context, *session.Session, map[string]any) (any, error) {
		return nil, &backends.ReadError{Msg: "PromQL backend 502"}
	}}, false)
	r.g.Log = slog.New(slog.NewTextHandler(&buf, nil))
	res, _ := r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": "x"})
	line := buf.String()
	for _, want := range []string{"backend failure", "tool=catalog.read", "session=s-1", "agent=coding-agent", "kind=ReadError", `evidence="PromQL backend 502"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("the log line lacks %q: %s", want, line)
		}
	}
	if *res.Error != "backend error" {
		t.Fatalf("the agent heard %q", *res.Error)
	}
}

func TestEvidenceIsClippedToTheLengthPythonKeeps(t *testing.T) {
	long := strings.Repeat("é", 300)
	if got := []rune(dispatch.ClipEvidence(long)); len(got) != dispatch.MaxEvidence || dispatch.MaxEvidence != 160 {
		t.Fatalf("%d", len(got))
	}
}

// The effect a write's outcome carries is what Python's dispatch._effect makes of the tool's result (testdata/effects.json,
// generated by tools/gen_gateway_registry.py), including a pull request marked for a person to merge (merge,
// needsPerson: A-11). An empty effect is no effect.
func TestTheEffectOfAWriteIsThePythonGatewaysEffect(t *testing.T) {
	raw, err := os.ReadFile("testdata/effects.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Tool   string         `json:"tool"`
		Args   map[string]any `json:"args"`
		Data   any            `json:"data"`
		Effect map[string]any `json:"effect"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) < 8 {
		t.Fatalf("%d cases: %v", len(cases), err)
	}
	sawPerson := false
	for i, c := range cases {
		c := c
		r := newRig(t, map[string]dispatch.Handler{c.Tool: ok(c.Data)}, false)
		res, _ := r.g.Call(context.Background(), "t", "s-1", c.Tool, c.Args)
		if !res.OK {
			t.Fatalf("case %d (%s): %+v", i, c.Tool, res)
		}
		got := r.fc.LastOutcome().Body.Effect
		if len(c.Effect) == 0 && got == nil { // Python sends an empty effect as none
			continue
		}
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(c.Effect)
		var g2, w2 map[string]any
		_ = json.Unmarshal(gj, &g2)
		_ = json.Unmarshal(wj, &w2)
		// The client leaves out an empty list of paths; Python sends []. To the core both are no paths.
		if p, ok := w2["paths"].([]any); ok && len(p) == 0 {
			delete(w2, "paths")
		}
		if !reflect.DeepEqual(g2, w2) {
			t.Fatalf("case %d (%s)\n got %s\nwant %s", i, c.Tool, gj, wj)
		}
		sawPerson = sawPerson || c.Effect["merge"] == "person"
	}
	if !sawPerson {
		t.Fatal("no case carries merge: person: the test would prove nothing about it")
	}
}
