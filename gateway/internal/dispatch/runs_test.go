package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/fakecore"
	"github.com/jfillman/autopilot/gateway/internal/runs"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/clearance/client"
)

// ownerCore is a fake core that decides the part of the rules the run tree's tests lean on, as the real core does: a
// call is allowed only for the session's principal or for a caller the gateway says acts as it, and a spawn is granted
// what was asked for with NO check against the parent (Permissive: a core that is wrong or hostile), so only the
// gateway's own checks can stop a child that is too wide.
type ownerCore struct {
	fc    *fakecore.Core
	n     atomic.Int64
	mu    sync.Mutex
	evals []*client.EvaluationRequest
	// Grant, when set, can change the grant a spawn gets (a hostile core).
	Grant func(g *client.Grant, sp *client.Spend)
}

func newOwnerCore(r *runRig) *ownerCore {
	o := &ownerCore{fc: r.fc}
	r.fc.Decide = o.decide
	return o
}

func (o *ownerCore) decide(req *client.EvaluationRequest) client.EvaluationResult {
	o.mu.Lock()
	o.evals = append(o.evals, req)
	o.mu.Unlock()
	id := fmt.Sprintf("d-%d", o.n.Add(1))
	ref := &client.RecordRef{Seq: o.n.Load(), Hash: "h"}
	deny := func(rule, msg string) client.EvaluationResult {
		return client.EvaluationResult{DecisionID: &id, Record: ref, Context: client.DecisionContext{Rule: rule, Message: msg, Spend: client.Spend{Denial: true}}}
	}
	s := req.Context.Session
	if s == nil {
		return deny("R004", "no such session")
	}
	if req.Subject.ID != s.Principal && req.Subject.Properties.ActingAs != s.Principal {
		return deny("R001", "not the owner of this session")
	}
	if s.Closed {
		return deny("R004", "session closed")
	}
	if req.Action.Name != "session.spawn" {
		return client.EvaluationResult{Decision: true, DecisionID: &id, Record: ref, Context: client.DecisionContext{Message: "allowed", Spend: client.Spend{ToolCalls: 1}}}
	}
	var args struct {
		Agent  string                 `json:"agent"`
		Limits map[string]json.Number `json:"limits"`
	}
	_ = json.Unmarshal(req.Action.Properties.Args, &args)
	if req.Context.Agent == nil {
		return deny("R003", "unknown agent")
	}
	l := req.Context.Agent.Limits
	for k, v := range args.Limits {
		n, _ := v.Int64()
		switch k {
		case "tool_calls":
			l.ToolCalls = n
		case "model_tokens":
			l.ModelTokens = n
		case "github_calls":
			l.GithubCalls = n
		case "open_prs":
			l.OpenPRs = n
		case "children":
			l.Children = n
		case "ttl_minutes":
			l.TTLMinutes = n
		}
	}
	parent := s.ID
	exp := req.Context.Now.Add(time.Duration(l.TTLMinutes) * time.Minute)
	if s.ExpiresAt.Before(exp) {
		exp = s.ExpiresAt
	}
	g := &client.Grant{Session: req.Context.NewSessionID, TaskID: s.TaskID, Principal: s.Principal, Agent: args.Agent, Parent: &parent,
		Depth: s.Depth + 1, Limits: l, ExpiresAt: exp}
	sp := client.Spend{ToolCalls: 1, Children: 1, Reserve: &client.Budgets{ToolCalls: l.ToolCalls, GithubCalls: l.GithubCalls, OpenPRs: l.OpenPRs, ModelTokens: l.ModelTokens}}
	if o.Grant != nil {
		o.Grant(g, &sp)
	}
	return client.EvaluationResult{Decision: true, DecisionID: &id, Record: ref, Context: client.DecisionContext{Message: "allowed", Spend: sp, Grant: g}}
}

func (o *ownerCore) last() *client.EvaluationRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.evals[len(o.evals)-1]
}

// treeRig is a parent session (user:alice's orchestrator, with a run) a spawn can be made in.
func treeRig(t *testing.T, mod func(*session.Record), opts ...rigOpt) (*runRig, *ownerCore) {
	t.Helper()
	res := loadDefsOnce["researcher"].Limits
	tools := map[string]bool{"run.spawn": true, "run.close": true, "catalog.read": true, "argo.app.get": true}
	for _, x := range res.Tools {
		tools[x] = true
	}
	var tl []string
	for x := range tools {
		tl = append(tl, x)
	}
	parent := sessionRow("s-0000a1", "user:alice", "orchestrator", "r-0000a1", func(r *session.Record) {
		r.Limits = client.Limits{TierCeiling: "T2", TTLMinutes: 60, ToolCalls: 1000, GithubCalls: 10, OpenPRs: 2, ModelTokens: 1_000_000, Children: 20,
			Tools: sortedCopy(tl), Models: res.Models, Network: "clearance+model", Compute: "medium"}
		if mod != nil {
			mod(r)
		}
	})
	r := newRunRig(t, append([]rigOpt{withPersist(newPersist(parent))}, opts...)...)
	return r, newOwnerCore(r)
}

func sortedCopy(a []string) []string {
	b := append([]string(nil), a...)
	for i := 1; i < len(b); i++ {
		for j := i; j > 0 && b[j] < b[j-1]; j-- {
			b[j], b[j-1] = b[j-1], b[j]
		}
	}
	return b
}

func spawnArgs(agent string, limits map[string]any) map[string]any {
	l := map[string]any{}
	for k, v := range limits {
		l[k] = json.Number(fmt.Sprint(v))
	}
	return map[string]any{"agent": agent, "limits": l}
}

func (r *runRig) call(tok, sid, tool string, args map[string]any) *dispatch.Result {
	r.t.Helper()
	res, err := r.g.Call(context.Background(), tok, sid, tool, args)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func (r *runRig) outcomes() []fakecore.Outcome { return r.fc.OutcomeList() }

// ---- run identity -------------------------------------------------------------------------------------------------

// A workload acts only as the session of the run it really is. Each attempt below is a way a run might try to be someone
// else, and each is refused by the core (R001) because the gateway does not say the caller acts as the session.
func TestARunActsOnlyAsItsOwnSession(t *testing.T) {
	r, oc := treeRig(t, nil)
	// a sibling (same person, own run), a child of the first, a session of another person, all with runs
	for _, row := range []session.Record{
		sessionRow("s-0000b2", "user:alice", "researcher", "r-0000b2", nil),
		sessionRow("s-0000c3", "user:alice", "researcher", "r-0000c3", func(r *session.Record) { p := "s-0000a1"; r.ParentID = &p; r.Depth = 1 }),
		sessionRow("s-0000d4", "user:carol", "researcher", "r-0000d4", nil),
		sessionRow("s-0000e5", "user:alice", "researcher", "", nil), // no run yet
	} {
		_ = r.persist.Save(context.Background(), []session.Record{row})
	}
	st2, _ := session.NewStore(context.Background(), r.persist)
	r.g.Store = st2
	r.store = st2
	podA, podB, podC := r.pod("r-0000a1"), r.pod("r-0000b2"), r.pod("r-0000c3")
	cases := []struct {
		name, tok, sid string
		ok             bool
		acting         string
	}{
		{"a run acts as its own session", podA, "s-0000a1", true, "user:alice"},
		{"the person acts as herself", r.person("user:alice"), "s-0000a1", true, ""},
		{"a sibling run, though it is the same person's", podB, "s-0000a1", false, ""},
		{"a run in another session of the same person", podA, "s-0000b2", false, ""},
		{"a child run acting as its parent", podC, "s-0000a1", false, ""},
		{"a parent run acting as its child", podA, "s-0000c3", false, ""},
		{"a run acting as another person's session", podA, "s-0000d4", false, ""},
		{"another person", r.person("user:bob"), "s-0000a1", false, ""},
		{"the session's own person is not a run's identity for another's session", r.person("user:alice"), "s-0000d4", false, ""},
		{"a run in another namespace, same service account name", r.who("elsewhere/run", "workload"), "s-0000a1", false, ""},
		{"the run's namespace, another service account", r.who("agent-r-0000a1/default", "workload"), "s-0000a1", false, ""},
		{"a person whose id is a run's", r.who("agent-r-0000a1/run", "user"), "s-0000a1", false, ""},
		{"a run of a session that has no run", podA, "s-0000e5", false, ""},
		{"a run asking about a session that does not exist", podA, "s-nope", false, ""},
	}
	for _, c := range cases {
		before := len(oc.evals)
		res := r.call(c.tok, c.sid, "argo.app.get", map[string]any{"app": "x"})
		if res.OK != c.ok {
			t.Errorf("%s: ok=%v decision=%v", c.name, res.OK, res.Decision)
		}
		if !c.ok && c.sid != "s-nope" && res.Decision["rule"] != "R001" {
			t.Errorf("%s: refused as %v, want R001", c.name, res.Decision["rule"])
		}
		if len(oc.evals) == before {
			t.Errorf("%s: the core was not asked", c.name)
			continue
		}
		if got := oc.last().Subject.Properties.ActingAs; got != c.acting {
			t.Errorf("%s: acting_as %q, want %q", c.name, got, c.acting)
		}
	}
}

// After a run is finished or closed the pod's token (still valid for a while) acts as nothing: its session has no run, or
// is closed, and the core refuses a closed session whatever the caller.
func TestAReplayedRunTokenActsAsNothingAfterItsRunIsGone(t *testing.T) {
	r, _ := treeRig(t, nil)
	pod := r.pod("r-0000a1")
	if res := r.call(pod, "s-0000a1", "argo.app.get", map[string]any{"app": "x"}); !res.OK {
		t.Fatalf("%v", res.Decision)
	}
	// run.close of its own session by the run itself: the session closes and the run is deleted
	res := r.call(pod, "s-0000a1", "run.close", map[string]any{"run_id": "s-0000a1"})
	if !res.OK || r.store.Get("s-0000a1").Run != nil || len(r.api.Deleted) != 1 {
		t.Fatalf("%v %v %v", res.OK, res.Decision, res.Error)
	}
	for i := 0; i < 3; i++ { // the same token, replayed
		replay := r.call(pod, "s-0000a1", "argo.app.get", map[string]any{"app": "x"})
		if replay.OK || replay.Decision["rule"] != "R001" {
			t.Fatalf("a replayed token after run.close: %v", replay.Decision)
		}
	}
	if _, err := r.g.FinishRun(context.Background(), pod, "s-0000a1", map[string]any{"status": "done"}); asCtl(err) == nil || asCtl(err).Rule != "R004" {
		t.Fatalf("finishing a closed session: %v", err)
	}
	if _, err := r.g.CloseSession(context.Background(), pod, "s-0000a1", nil); asCtl(err) == nil || asCtl(err).Rule != "CT-FORBIDDEN" {
		t.Fatalf("closing again, as a run with no run: %v", err)
	}
}

// If the delete failed the session keeps its run, so the pod's identity still maps to it, and it is the closed session that
// the core refuses. Nothing is allowed on a closed session.
func TestAClosedSessionWhoseRunWasNotDeletedStillAllowsNothing(t *testing.T) {
	r, _ := treeRig(t, nil)
	pod := r.pod("r-0000a1")
	r.api.SetFailDelete("r-0000a1")
	if res := r.call(pod, "s-0000a1", "run.close", map[string]any{"run_id": "s-0000a1"}); !res.OK {
		t.Fatalf("%v", res.Error)
	}
	if r.store.Get("s-0000a1").Run == nil {
		t.Fatal("a failed delete must leave the run named, for the sweep to retry")
	}
	if res := r.call(pod, "s-0000a1", "argo.app.get", map[string]any{"app": "x"}); res.OK || res.Decision["rule"] != "R004" {
		t.Fatalf("%v", res.Decision)
	}
	r.api.SetFailDelete()
	r.g.Expire(context.Background())
	if r.store.Get("s-0000a1").Run != nil {
		t.Fatal("the sweep did not retry the delete")
	}
}

func TestSessionToolsAndRunEndsAreForTheSessionsOwnRunOnly(t *testing.T) {
	r, _ := treeRig(t, nil)
	_ = r.persist.Save(context.Background(), []session.Record{sessionRow("s-0000b2", "user:alice", "researcher", "r-0000b2", nil)})
	st2, _ := session.NewStore(context.Background(), r.persist)
	r.g.Store, r.store = st2, st2
	ctx := context.Background()
	if _, ok, _ := r.g.SessionTools(ctx, r.pod("r-0000a1"), "s-0000a1"); !ok {
		t.Error("a run sees its own session's tools")
	}
	if _, ok, _ := r.g.SessionTools(ctx, r.pod("r-0000b2"), "s-0000a1"); ok {
		t.Error("a sibling run sees another session's tools")
	}
	if _, err := r.g.FinishRun(ctx, r.pod("r-0000b2"), "s-0000a1", map[string]any{"status": "done"}); asCtl(err) == nil || asCtl(err).Rule != "R001" {
		t.Errorf("a sibling finishing another's session: %v", err)
	}
	if _, err := r.g.CloseSession(ctx, r.pod("r-0000b2"), "s-0000a1", nil); asCtl(err) == nil || asCtl(err).Rule != "CT-FORBIDDEN" {
		t.Errorf("a sibling closing another's session: %v", err)
	}
	// the same answer for a session that does not exist as for another's
	if _, err := r.g.CloseSession(ctx, r.pod("r-0000b2"), "s-nope", nil); asCtl(err) == nil || asCtl(err).Message != "no such session of yours" {
		t.Errorf("%v", err)
	}
	// a backend with no identity for its runs: nobody acts as a session
	r.g.SetRuns(&noIdentity{r.g.Runs})
	if res := r.call(r.pod("r-0000a1"), "s-0000a1", "argo.app.get", map[string]any{"app": "x"}); res.OK {
		t.Error("a backend without Principal keeps the old rule: only the session's principal owns it")
	}
}

type noIdentity struct{ dispatch.RunBackend }

func (noIdentity) Principal(string) string { return "" }

// ---- spawn --------------------------------------------------------------------------------------------------------

func TestSpawnMakesANarrowerChildAndItsRun(t *testing.T) {
	r, oc := treeRig(t, nil)
	res := r.call(r.person("user:alice"), "s-0000a1", "run.spawn", spawnArgs("researcher", map[string]any{"tool_calls": 10, "model_tokens": 20000}))
	if !res.OK {
		t.Fatalf("%v %v", res.Decision, *res.Error)
	}
	data := res.Data.(map[string]any)
	cid := data["session"].(string)
	child := r.store.Get(cid)
	parent := r.store.Get("s-0000a1")
	if data["run"] != "r-"+cid[2:] || child.Limits.ToolCalls != 10 || child.Limits.ModelTokens != 20000 || child.Depth != 1 || *child.ParentID != "s-0000a1" || child.Principal != "user:alice" {
		t.Fatalf("%v %+v", data, child.Limits)
	}
	if parent.Reserved.ToolCalls != 10 || parent.Reserved.ModelTokens != 20000 || parent.Used.Children != 1 || parent.Used.ToolCalls != 1 || len(parent.Children) != 1 {
		t.Fatalf("%+v %+v", parent.Reserved, parent.Used)
	}
	req := oc.last()
	if req.Action.Name != "session.spawn" || req.Context.NewSessionID != cid || req.Context.Agent == nil || req.Context.Agent.Name != "researcher" || req.Resource.ID != "run.spawn" {
		t.Fatalf("%+v", req)
	}
	m := r.api.Manifests["r-"+cid[2:]]
	if m == nil || m["spec"].(map[string]any)["parentSessionId"] != "s-0000a1" || m["metadata"].(map[string]any)["labels"].(map[string]any)["hangar.io/session"] != cid {
		t.Fatalf("%v", m)
	}
	if o := r.outcomes(); len(o) != 1 || o[0].Body.Result != "ok" {
		t.Fatalf("exactly one outcome: %+v", o)
	}
	// the child is saved, with its run, and so is the parent's reservation
	row, ok := r.persist.row(cid)
	prow, _ := r.persist.row("s-0000a1")
	if !ok || row.Run == nil || *row.Run != data["run"] || prow.Reserved["tool_calls"] != 10 {
		t.Fatalf("%+v %+v", row, prow)
	}
	// the child's run acts as the child's session, and only that
	pod := r.pod(*row.Run)
	if x := r.call(pod, cid, "argo.app.get", map[string]any{"app": "x"}); !x.OK {
		t.Fatalf("%v", x.Decision)
	}
	if x := r.call(pod, "s-0000a1", "argo.app.get", map[string]any{"app": "x"}); x.OK {
		t.Fatal("a child's run acts as its parent")
	}
}

// A core that grants a child wider than the parent has left (or for another principal, another task, a session id that
// exists) cannot make one: the gateway checks every grant itself, creates nothing, and reports the refusal.
func TestSpawnRefusesWhatTheCoreShouldNotHaveGranted(t *testing.T) {
	hostile := map[string]func(*client.Grant, *client.Spend){
		"a tool the parent lacks": func(g *client.Grant, _ *client.Spend) { g.Limits.Tools = append(g.Limits.Tools, "repo.pr.open_upper") },
		"a higher tier":           func(g *client.Grant, _ *client.Spend) { g.Limits.TierCeiling = "T3" },
		"more budget than the parent": func(g *client.Grant, sp *client.Spend) {
			g.Limits.ModelTokens = 5_000_000
			sp.Reserve.ModelTokens = 5_000_000
		},
		"a longer life":                func(g *client.Grant, _ *client.Spend) { g.ExpiresAt = g.ExpiresAt.Add(24 * time.Hour) },
		"another principal":            func(g *client.Grant, _ *client.Spend) { g.Principal = "user:root" },
		"the parent's own session id":  func(g *client.Grant, _ *client.Spend) { g.Session = "s-0000a1" },
		"a child at the wrong depth":   func(g *client.Grant, _ *client.Spend) { g.Depth = 3 },
		"a reservation too small":      func(_ *client.Grant, sp *client.Spend) { sp.Reserve.ToolCalls = 0 },
		"no child counted":             func(_ *client.Grant, sp *client.Spend) { sp.Children = 0 },
		"another agent than asked for": func(g *client.Grant, _ *client.Spend) { g.Agent = "orchestrator" },
	}
	for name, mod := range hostile {
		t.Run(name, func(t *testing.T) {
			r, oc := treeRig(t, nil)
			oc.Grant = mod
			res := r.call(r.person("user:alice"), "s-0000a1", "run.spawn", spawnArgs("researcher", map[string]any{"tool_calls": 10, "model_tokens": 20000}))
			if res.OK || res.Decision["rule"] != "R007" {
				t.Fatalf("%v %v", res.OK, res.Decision)
			}
			parent := r.store.Get("s-0000a1")
			if len(r.api.Manifests) != 0 || len(r.store.All()) != 1 || parent.Reserved != (client.Budgets{}) || parent.Used.Children != 0 || len(parent.Children) != 0 {
				t.Fatalf("something was created: %v %d %+v %+v", r.api.Manifests, len(r.store.All()), parent.Reserved, parent.Used)
			}
			o := r.outcomes()
			if len(o) != 1 || o[0].Body.Refusal == nil || o[0].Body.Refusal.Rule != "R007" || parent.Denials != 1 {
				t.Fatalf("one refused outcome, one denial: %+v denials=%d", o, parent.Denials)
			}
		})
	}
}

func TestSpawnLimitsPassFromParentToChildNarrowedOnly(t *testing.T) {
	// the parent has spent most of its model tokens and has one child slot left: a core that cuts from the DEFINITION
	// instead of from what the parent has left makes a child too big
	r, oc := treeRig(t, func(rec *session.Record) {
		rec.Used = map[string]int64{"model_tokens": 950_000, "children": 19}
	})
	oc.Grant = nil
	res := r.call(r.person("user:alice"), "s-0000a1", "run.spawn", spawnArgs("researcher", map[string]any{"model_tokens": 100_000}))
	if res.OK {
		t.Fatalf("a child of 100k tokens from a parent with 50k left was made")
	}
	res = r.call(r.person("user:alice"), "s-0000a1", "run.spawn", spawnArgs("researcher", map[string]any{"model_tokens": 50_000}))
	if !res.OK {
		t.Fatalf("%v %v", res.Decision, *res.Error)
	}
	// that was the last child slot and the last of the tokens
	res = r.call(r.person("user:alice"), "s-0000a1", "run.spawn", spawnArgs("researcher", map[string]any{"model_tokens": 1}))
	if res.OK {
		t.Fatal("a twentieth child")
	}
	p := r.store.Get("s-0000a1")
	if p.Remaining("model_tokens") != 0 || p.Remaining("children") != 0 {
		t.Fatalf("%d %d", p.Remaining("model_tokens"), p.Remaining("children"))
	}
}

func TestSpawnIsRefusedAtTheDepthLimitAndByAParentThatCannotHaveChildren(t *testing.T) {
	for name, mod := range map[string]func(*session.Record){
		"too deep":    func(r *session.Record) { r.Depth = 3 },
		"tripped":     func(r *session.Record) { r.Tripped = true },
		"no children": func(r *session.Record) { r.Used = map[string]int64{"children": 20} },
	} {
		r, _ := treeRig(t, mod)
		res := r.call(r.person("user:alice"), "s-0000a1", "run.spawn", spawnArgs("researcher", map[string]any{"tool_calls": 5}))
		if res.OK || len(r.api.Manifests) != 0 {
			t.Errorf("%s: a child was made", name)
		}
	}
}

func TestSpawnOfAnUnknownAgentInputAndNoBackend(t *testing.T) {
	r, oc := treeRig(t, nil)
	oc.Grant = nil
	tok := r.person("user:alice")
	if res := r.call(tok, "s-0000a1", "run.spawn", spawnArgs("nope", nil)); res.OK {
		t.Fatalf("an unknown agent: %v", res.Decision)
	}
	// the core granting something for an agent the gateway does not know: R003, nothing made
	oc.mu.Lock()
	oc.mu.Unlock()
	for name, in := range map[string]any{"a string": "do it", "a list": []any{1}, "too large": map[string]any{"prompt": strings.Repeat("x", 5000)}} {
		args := spawnArgs("researcher", map[string]any{"tool_calls": 5})
		args["input"] = in
		res := r.call(tok, "s-0000a1", "run.spawn", args)
		if res.OK || res.Decision["rule"] != "R008" || len(r.api.Manifests) != 0 || len(r.store.All()) != 1 {
			t.Errorf("%s: %v %v", name, res.OK, res.Decision)
		}
	}
	args := spawnArgs("researcher", map[string]any{"tool_calls": 5})
	args["input"] = map[string]any{"prompt": "hi"}
	if res := r.call(tok, "s-0000a1", "run.spawn", args); !res.OK {
		t.Fatalf("%v", res.Decision)
	}
	for _, in := range r.api.Manifests {
		if got := in["spec"].(map[string]any)["input"].(map[string]any)["prompt"]; got != "hi" {
			t.Errorf("%v", got)
		}
	}
	// a gateway with no run backend: no handler, one outcome, nothing made
	r2, _ := treeRig(t, nil)
	r2.g.SetRuns(nil)
	delete(r2.g.Handlers, "run.spawn")
	delete(r2.g.Handlers, "run.close")
	res := r2.call(r2.person("user:alice"), "s-0000a1", "run.spawn", spawnArgs("researcher", nil))
	if res.OK || *res.Error != "no backend for this tool in this deployment" || len(r2.outcomes()) != 1 || r2.outcomes()[0].Body.Result != "error:NoHandler" {
		t.Fatalf("%v %v", res.Error, r2.outcomes())
	}
}

// Whatever the run backend does, an allowed spawn has exactly one outcome, the agent is told only "backend error", and
// the budget a child reserved comes back.
func TestSpawnWhenTheRunBackendFailsTimesOutOrPanics(t *testing.T) {
	type fake = *runs.Fake
	cases := []struct {
		name   string
		setup  func(r *runRig, f fake)
		result string
	}{
		{"an API error", func(r *runRig, f fake) { f.CreateErr = &runs.Error{Msg: "API server 500 creating AgentRun r-x"} }, "error:KubeRunError: API server 500 creating AgentRun r-x"},
		{"a refusal before any request", func(r *runRig, f fake) { f.CreateErr = &runs.Refused{Msg: "an AgentRun is named r-<6 to 32 hex>"} }, "error:RunRefused: an AgentRun is named r-<6 to 32 hex>"},
		{"a failure after the API server took the run", func(r *runRig, f fake) { f.CreateThenFail = &runs.Error{Msg: "API server unreachable: TimeoutError"} }, "error:KubeRunError: API server unreachable: TimeoutError"},
		{"a panic", func(r *runRig, f fake) { f.OnCreate = func() { panic("boom: secret-token-123") } }, "error:Panic"},
		{"a call that ran out of time", func(r *runRig, f fake) {
			r.g.CallTimeout = 50 * time.Millisecond
			f.OnCreate = func() { time.Sleep(200 * time.Millisecond) }
		}, "error:Timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := treeRig(t, nil)
			f := runs.NewFake()
			r.g.SetRuns(f)
			c.setup(r, f)
			res := r.call(r.person("user:alice"), "s-0000a1", "run.spawn", spawnArgs("researcher", map[string]any{"tool_calls": 10}))
			if res.OK || *res.Error != "backend error" {
				t.Fatalf("%v %v", res.OK, *res.Error)
			}
			o := r.outcomes()
			if len(o) != 1 || !strings.HasPrefix(o[0].Body.Result, c.result) || strings.Contains(o[0].Body.Result, "secret-token") && c.name != "a panic" {
				t.Fatalf("one outcome %q: %+v", c.result, o)
			}
			p := r.store.Get("s-0000a1")
			if p.Reserved != (client.Budgets{}) || p.Used.Children != 1 || p.Remaining("tool_calls") != 1000-1 {
				t.Fatalf("the reservation did not come back: %+v %+v", p.Reserved, p.Used)
			}
			var child *session.Session
			for _, s := range r.store.All() {
				if s.ID != "s-0000a1" {
					child = s
				}
			}
			if child == nil || !child.Closed {
				t.Fatal("the child is closed")
			}
			// the run that may have been made is deleted again, so nothing is left running
			if len(f.Live) != 0 || child.Run != nil {
				t.Fatalf("a run was left: %v %v", f.Live, child.Run)
			}
		})
	}
}

// A storm of concurrent spawns, through the whole call path, cannot overspend the parent. The core here grants whatever it
// is asked (it checks nothing), so what stops the parent being overspent is the gateway. Run with -race.
func TestSpawnStormCannotOverspendTheParent(t *testing.T) {
	r, _ := treeRig(t, nil)
	tok := r.person("user:alice")
	var wg sync.WaitGroup
	var ok, refused atomic.Int64
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := r.g.Call(context.Background(), tok, "s-0000a1", "run.spawn", spawnArgs("researcher", map[string]any{"tool_calls": 100, "model_tokens": 100_000}))
			if err != nil {
				t.Error(err)
				return
			}
			if res.OK {
				ok.Add(1)
			} else {
				refused.Add(1)
			}
		}()
	}
	wg.Wait()
	p := r.store.Get("s-0000a1")
	p.Lock()
	defer p.Unlock()
	// 1000 tool calls, 1,000,000 tokens: 60 calls were charged (60 of the 1000), so at most 9 children of 100 fit by calls
	// and 10 by tokens
	if p.Remaining("tool_calls") < 0 || p.Remaining("model_tokens") < 0 || p.Remaining("children") < 0 {
		t.Fatalf("overspent: used %+v reserved %+v", p.Used, p.Reserved)
	}
	if ok.Load() == 0 || ok.Load() > 9 || refused.Load() == 0 || int64(len(r.api.Manifests)) != ok.Load() || int(p.Used.Children) != int(ok.Load()) {
		t.Fatalf("%d made, %d refused, %d runs, %+v", ok.Load(), refused.Load(), len(r.api.Manifests), p.Used)
	}
	if n := len(r.outcomes()); n != 60 {
		t.Fatalf("every call has one outcome: %d", n)
	}
	if p.Reserved.ToolCalls != ok.Load()*100 || p.Reserved.ModelTokens != ok.Load()*100_000 {
		t.Fatalf("%+v", p.Reserved)
	}
}

// ---- run.close ------------------------------------------------------------------------------------------------------

func TestRunCloseClosesOnlyYourOwnRunAndItsChildren(t *testing.T) {
	r, _ := treeRig(t, nil)
	alice := r.person("user:alice")
	res := r.call(alice, "s-0000a1", "run.spawn", spawnArgs("researcher", map[string]any{"tool_calls": 10, "model_tokens": 1000}))
	kid := res.Data.(map[string]any)["session"].(string)
	// the child spawns nothing of its own here; a stranger's session and an unknown id are not this session's runs
	_ = r.persist.Save(context.Background(), []session.Record{sessionRow("s-0000f6", "user:alice", "researcher", "r-0000f6", nil)})
	st2, _ := session.NewStore(context.Background(), r.persist)
	r.g.Store, r.store = st2, st2
	for _, id := range []string{"s-0000f6", "s-nope", "", "s-0000a1x"} {
		if x := r.call(alice, "s-0000a1", "run.close", map[string]any{"run_id": id}); x.OK || x.Decision["rule"] != "R001" {
			t.Errorf("closing %q: %v", id, x.Decision)
		}
	}
	// the child cannot close its parent
	if x := r.call(r.pod(runName(kid)), kid, "run.close", map[string]any{"run_id": "s-0000a1"}); x.OK || x.Decision["rule"] != "R001" {
		t.Errorf("a child closed its parent: %v", x.Decision)
	}
	if x := r.call(alice, "s-0000a1", "run.close", map[string]any{"run_id": kid}); !x.OK {
		t.Fatalf("%v", x.Error)
	}
	child, parent := r.store.Get(kid), r.store.Get("s-0000a1")
	if !child.Closed || child.Run != nil || parent.Closed || parent.Reserved != (client.Budgets{}) || len(r.api.Manifests) != 0 {
		t.Fatalf("%+v %+v", child, parent.Reserved)
	}
	var kinds []string
	for _, rec := range r.records() {
		kinds = append(kinds, fmt.Sprint(rec["kind"], ":", rec["result"], ":", rec["principal"]))
	}
	if len(kinds) != 1 || kinds[0] != "run.delete:deleted:user:alice" {
		t.Fatalf("%v", kinds)
	}
}

func runName(sid string) string { return "r-" + strings.TrimPrefix(sid, "s-") }

func TestRunCloseWithAFailedDeleteIsRecordedAndRetriedBytheSweep(t *testing.T) {
	r, _ := treeRig(t, nil)
	alice := r.person("user:alice")
	kid := r.call(alice, "s-0000a1", "run.spawn", spawnArgs("researcher", map[string]any{"tool_calls": 10})).Data.(map[string]any)["session"].(string)
	r.api.SetFailDelete(runName(kid))
	if x := r.call(alice, "s-0000a1", "run.close", map[string]any{"run_id": kid}); !x.OK {
		t.Fatalf("%v", x.Error)
	}
	child := r.store.Get(kid)
	if !child.Closed || child.Run == nil {
		t.Fatalf("closed, still named: %v %v", child.Closed, child.Run)
	}
	var results []string
	for _, rec := range r.records() {
		results = append(results, fmt.Sprint(rec["result"]))
	}
	if len(results) != 1 || results[0] != "error:KubeRunError" {
		t.Fatalf("%v", results)
	}
	r.api.SetFailDelete()
	r.g.Expire(context.Background())
	if child.Run != nil || len(r.api.Manifests) != 0 {
		t.Fatal("the sweep did not retry")
	}
}

// ---- launch -------------------------------------------------------------------------------------------------------

func launchRig(t *testing.T, agent string, mod func(*session.Record)) *runRig {
	row := sessionRow("s-0000a1", "user:alice", agent, "", mod)
	r := newRunRig(t, withPersist(newPersist(row)))
	return r
}

func TestLaunchCreatesTheRunSavesItsNameAndRecordsIt(t *testing.T) {
	r := launchRig(t, "researcher", nil)
	res, err := r.g.Launch(context.Background(), "s-0000a1", map[string]any{"prompt": "hi", "started_by": "user:alice", "instructions": "secret instructions"})
	if err != nil || !res.OK || res.Data.(map[string]any)["run"] != "r-0000a1" {
		t.Fatalf("%v %v", res, err)
	}
	s := r.store.Get("s-0000a1")
	if *s.Run != "r-0000a1" || s.Input["prompt"] != "hi" || s.Input["started_by"] != "user:alice" || s.Input["instructions"] != nil {
		t.Fatalf("%+v", s.Input)
	}
	row, _ := r.persist.row("s-0000a1")
	if row.Run == nil || *row.Run != "r-0000a1" {
		t.Fatal("not saved")
	}
	recs := r.records()
	if len(recs) != 1 || recs[0]["kind"] != "run.launch" || recs[0]["principal"] != "user:alice" || recs[0]["result"] != "created" {
		t.Fatalf("%v", recs)
	}
	if m := r.api.Manifests["r-0000a1"]; m == nil || m["spec"].(map[string]any)["input"].(map[string]any)["instructions"] != "secret instructions" {
		t.Fatalf("the run gets the instructions: %v", m)
	}
	// a run is launched once
	again, _ := r.g.Launch(context.Background(), "s-0000a1", nil)
	if again.OK || again.Decision["rule"] != "R001" || len(r.api.Manifests) != 1 {
		t.Fatalf("%v", again)
	}
}

func TestLaunchRefusals(t *testing.T) {
	ctx := context.Background()
	r := launchRig(t, "researcher", nil)
	if res, _ := r.g.Launch(ctx, "s-nope", nil); res.OK || res.Decision["rule"] != "R004" {
		t.Errorf("%v", res.Decision)
	}
	rc := launchRig(t, "researcher", func(rec *session.Record) { rec.Closed = true })
	if res, _ := rc.g.Launch(ctx, "s-0000a1", nil); res.OK || res.Decision["rule"] != "R004" {
		t.Errorf("%v", res.Decision)
	}
	// an agent that runs in a person's client has no runtime image
	rn := launchRig(t, "researcher", nil)
	noImg := *rn.g.Defs["researcher"]
	noImg.Image = nil
	rn.g.Defs["researcher"] = &noImg
	if res, _ := rn.g.Launch(ctx, "s-0000a1", nil); res.OK || res.Decision["rule"] != "R001" || !strings.Contains(*res.Error, "no runtime image") {
		t.Errorf("%v", res)
	}
	rs := launchRig(t, "researcher", nil)
	svc := *rs.g.Defs["researcher"]
	svc.Kind = "service"
	rs.g.Defs["researcher"] = &svc
	if res, _ := rs.g.Launch(ctx, "s-0000a1", nil); res.OK || !strings.Contains(*res.Error, "deployed as applications") {
		t.Errorf("%v", res)
	}
	rb := launchRig(t, "researcher", nil)
	rb.g.SetRuns(nil)
	if res, _ := rb.g.Launch(ctx, "s-0000a1", nil); res.OK || *res.Error != "this deployment has no run backend" {
		t.Errorf("%v", res)
	}
	for _, x := range []*runRig{r, rc, rn, rs, rb} {
		if len(x.api.Manifests) != 0 && x != r {
			t.Error("a run was made")
		}
	}
}

// The run.launch record must reach the core: when it cannot, the run is deleted again and the session keeps none.
func TestLaunchFailsClosedWhenTheLaunchRecordCannotBeWritten(t *testing.T) {
	r := launchRig(t, "researcher", nil)
	r.fc.Down.Store(true)
	res, err := r.g.Launch(context.Background(), "s-0000a1", map[string]any{"prompt": "hi"})
	if err != nil || res.OK || res.Decision["rule"] != core.OutageRule {
		t.Fatalf("%v %v", res, err)
	}
	s := r.store.Get("s-0000a1")
	if s.Run != nil || s.Input != nil || len(r.api.Manifests) != 0 || len(r.api.Deleted) != 1 {
		t.Fatalf("%v %v %v %v", s.Run, s.Input, r.api.Manifests, r.api.Deleted)
	}
	row, _ := r.persist.row("s-0000a1")
	if row.Run != nil {
		t.Fatal("the saved session still names a run")
	}
}

func TestLaunchWhenTheRunBackendFails(t *testing.T) {
	r := launchRig(t, "researcher", nil)
	f := runs.NewFake()
	f.CreateErr = &runs.Error{Msg: "API server 500 creating AgentRun r-0000a1"}
	r.g.SetRuns(f)
	res, err := r.g.Launch(context.Background(), "s-0000a1", nil)
	var ke *runs.Error
	if res != nil || !errors.As(err, &ke) || r.store.Get("s-0000a1").Run != nil {
		t.Fatalf("%v %v", res, err)
	}
	// a panic is an error, not a crash
	f2 := runs.NewFake()
	f2.OnCreate = func() { panic("boom") }
	r.g.SetRuns(f2)
	if _, err := r.g.Launch(context.Background(), "s-0000a1", nil); err == nil || dispatch.ErrorKind(err) != "Panic" || r.store.Get("s-0000a1").Run != nil {
		t.Fatalf("%v", err)
	}
}

// ---- crash points ---------------------------------------------------------------------------------------------------

// What the database holds when the process dies at each point of a spawn, and what the next gateway does with it.
func TestCrashDuringASpawnLeavesASessionTheSweepCleansUp(t *testing.T) {
	r, _ := treeRig(t, nil)
	f := runs.NewFake()
	var snapshot map[string]session.Record
	f.OnCreate = func() { // the process dies here: the run has not been created, the database holds what it holds
		r.persist.mu.Lock()
		snapshot = map[string]session.Record{}
		for k, v := range r.persist.rows {
			raw, _ := json.Marshal(v)
			var c session.Record
			_ = json.Unmarshal(raw, &c)
			snapshot[k] = c
		}
		r.persist.mu.Unlock()
	}
	r.g.SetRuns(f)
	res := r.call(r.person("user:alice"), "s-0000a1", "run.spawn", spawnArgs("researcher", map[string]any{"tool_calls": 10}))
	if !res.OK {
		t.Fatal(res.Decision)
	}
	kid := res.Data.(map[string]any)["session"].(string)
	// at the crash: the child is saved, named after its run, live; the parent's reservation is saved with it, and so is the
	// call's own charge (it was applied before the handler ran)
	child, parent := snapshot[kid], snapshot["s-0000a1"]
	if child.ID == "" || child.Run == nil || *child.Run != runName(kid) || child.Closed || parent.Reserved["tool_calls"] != 10 || parent.Used["children"] != 1 || parent.Used["tool_calls"] != 1 {
		t.Fatalf("%+v %+v", child, parent)
	}
	// the next gateway starts from that database: its sweep closes the child at its deadline and deletes the run it names
	// (a delete of a run that never existed is success), and the parent's budget comes back
	r2 := newRunRig(t, withPersist(&memPersist{rows: snapshot}))
	r2.g.ExpireAt(context.Background(), t0.Add(2*time.Hour))
	c2, p2 := r2.store.Get(kid), r2.store.Get("s-0000a1")
	if !c2.Closed || c2.Run != nil || p2.Reserved.ToolCalls != 0 || len(r2.api.Deleted) == 0 {
		t.Fatalf("%+v %+v %v", c2, p2.Reserved, r2.api.Deleted)
	}
	if recs := r2.records(); len(recs) == 0 {
		t.Fatal("the deletion is recorded")
	}
}

func TestCrashDuringALaunchLeavesASessionTheSweepCleansUp(t *testing.T) {
	r := launchRig(t, "researcher", nil)
	f := runs.NewFake()
	var snap session.Record
	f.OnCreate = func() { snap, _ = r.persist.row("s-0000a1") }
	r.g.SetRuns(f)
	if res, err := r.g.Launch(context.Background(), "s-0000a1", map[string]any{"prompt": "x"}); err != nil || !res.OK {
		t.Fatal(err)
	}
	if snap.Run == nil || *snap.Run != "r-0000a1" || snap.Closed {
		t.Fatalf("the name is saved before the run exists: %+v", snap)
	}
	r2 := newRunRig(t, withPersist(newPersist(snap)))
	r2.g.ExpireAt(context.Background(), t0.Add(2*time.Hour))
	if s := r2.store.Get("s-0000a1"); !s.Closed || s.Run != nil || s.Outcome["status"] != "expired" {
		t.Fatalf("%+v", s)
	}
}

// A run's answer is kept before its session closes: a crash between leaves the session open, and the run finishes again.
func TestCrashBetweenTheKeptResultAndTheClosedSessionLetsTheRunFinishAgain(t *testing.T) {
	r, _ := treeRig(t, nil)
	pod := r.pod("r-0000a1")
	body := map[string]any{"status": "done", "final_text": "first"}
	// the first finish stores the answer, then the process dies before the session closes: undo the close in the database
	if _, err := r.g.FinishRun(context.Background(), pod, "s-0000a1", body); err != nil {
		t.Fatal(err)
	}
	row, _ := r.persist.row("s-0000a1")
	row.Closed, row.Outcome = false, nil
	r2 := newRunRig(t, withPersist(newPersist(row)))
	r2.results.Rows = r.results.Rows
	body["final_text"] = "second"
	if _, err := r2.g.FinishRun(context.Background(), r2.pod("r-0000a1"), "s-0000a1", body); err != nil {
		t.Fatal(err)
	}
	if got := r2.results.Rows["s-0000a1"]["final_text"]; got != "first" {
		t.Fatalf("the first answer is the one kept: %v", got)
	}
	if !r2.store.Get("s-0000a1").Closed {
		t.Fatal("the session closed")
	}
}

// A result store that fails leaves the session open and the run able to try again: nothing half happens.
func TestAFailedResultStoreLeavesTheSessionOpen(t *testing.T) {
	r, _ := treeRig(t, nil)
	r.g.Results = failingResults{}
	if _, err := r.g.FinishRun(context.Background(), r.pod("r-0000a1"), "s-0000a1", map[string]any{"status": "done", "final_text": "x"}); err == nil {
		t.Fatal("a finish whose answer was not kept succeeded")
	}
	s := r.store.Get("s-0000a1")
	if s.Closed || s.Outcome != nil || len(r.records()) != 0 {
		t.Fatalf("%+v", s.Outcome)
	}
}

type failingResults struct{}

func (failingResults) Put(context.Context, map[string]any, time.Time) error {
	return errors.New("down")
}
func (failingResults) Get(context.Context, string, time.Time) (map[string]any, error) {
	return nil, nil
}

// ---- the sweep ------------------------------------------------------------------------------------------------------

func TestTheSweepSurvivesABackendThatPanicsOrFails(t *testing.T) {
	rows := []session.Record{sessionRow("s-0000a1", "user:alice", "researcher", "r-0000a1", nil), sessionRow("s-0000a2", "user:alice", "researcher", "r-0000a2", nil)}
	r := newRunRig(t, withPersist(newPersist(rows...)))
	f := runs.NewFake()
	f.EndedErr = errors.New("boom")
	r.g.SetRuns(f)
	r.g.Expire(context.Background()) // asked again next time; nothing closed
	if r.store.Get("s-0000a1").Closed {
		t.Fatal("closed on a backend error")
	}
	f.Set(func(f *runs.Fake) { f.EndedErr = nil; f.Phases["r-0000a1"] = "Failed" })
	f.DeleteErr = errors.New("api down")
	r.g.Expire(context.Background())
	s := r.store.Get("s-0000a1")
	if !s.Closed || s.Run == nil || s.Outcome["status"] != "no-result" || s.Outcome["run_phase"] != "Failed" || r.store.Get("s-0000a2").Closed {
		t.Fatalf("%+v %v", s.Outcome, s.Run)
	}
	f.DeleteErr = nil
	r.g.Expire(context.Background())
	if r.store.Get("s-0000a1").Run != nil {
		t.Fatal("not retried")
	}
}

// ---- Preflight candidates and a person's run ---------------------------------------------------------------------

func TestCandidateRefusesUntilTheDefinitionSchemaIsCarried(t *testing.T) {
	r := newRunRig(t)
	if _, _, err := r.g.Candidate("researcher", map[string]any{"name": "researcher"}, nil); !errors.Is(err, dispatch.ErrNotCarried) {
		t.Fatalf("%v", err)
	}
	// with a validated parse and a fingerprint it builds the clamped definition, and every refusal Python has
	r.g.CandidateParse = func(doc any) (*definitions.Definition, error) {
		d := *r.g.Defs["researcher"]
		if m, _ := doc.(map[string]any); m["name"] != nil {
			d.Name = m["name"].(string)
		}
		if m, _ := doc.(map[string]any); m["bad"] != nil {
			return nil, errors.New("profile/limits: nope")
		}
		if m, _ := doc.(map[string]any); m["tag"] != nil {
			img := "ghcr.io/x/y:latest"
			d.Image = &img
		}
		return &d, nil
	}
	r.g.Fingerprint = func(*definitions.Definition) string { return "abcdef0123456789" }
	cand, fp, err := r.g.Candidate("researcher", map[string]any{"name": "researcher"}, map[string]any{"tool_calls": json.Number("5")})
	if err != nil || fp != "abcdef0123456789" || cand.Name != "pf-researcher-abcdef01" || cand.Limits.ToolCalls != 5 || cand.Limits.GithubCalls != 0 || r.g.Defs[cand.Name] != nil {
		t.Fatalf("%+v %v", cand, err)
	}
	for name, doc := range map[string]any{"a list": []any{}, "bad": map[string]any{"bad": 1}, "another name": map[string]any{"name": "other"}, "a tag": map[string]any{"tag": 1}} {
		if _, _, err := r.g.Candidate("researcher", doc, nil); asCtl(err) == nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	r.g.RuntimeImages = []string{"ghcr.io/other/*"}
	if _, _, err := r.g.Candidate("researcher", map[string]any{"name": "researcher"}, nil); asCtl(err) == nil || !strings.Contains(asCtl(err).Message, "not on this deployment's list") {
		t.Errorf("%v", err)
	}
	// a name that is a live definition is never shadowed
	r.g.Defs["pf-researcher-abcdef01"] = r.g.Defs["researcher"]
	r.g.RuntimeImages = nil
	if _, _, err := r.g.Candidate("researcher", map[string]any{"name": "researcher"}, nil); asCtl(err) == nil || asCtl(err).Rule != "R001" {
		t.Errorf("%v", err)
	}
}

func TestRunAgentAsOpensTheSessionAsThePersonAndLaunchesIt(t *testing.T) {
	r := newRunRig(t)
	oc := newOwnerCore(r)
	var open *client.EvaluationRequest
	r.fc.Decide = func(req *client.EvaluationRequest) client.EvaluationResult {
		if req.Action.Name != "session.open" {
			return oc.decide(req)
		}
		open = req
		id := "d-open"
		d := req.Context.Agent
		g := &client.Grant{Session: req.Context.NewSessionID, TaskID: "t-" + req.Context.NewSessionID[2:], Principal: req.Subject.ID, Agent: d.Name, Limits: d.Limits, ExpiresAt: req.Context.Now.Add(30 * time.Minute)}
		return client.EvaluationResult{Decision: true, DecisionID: &id, Record: &client.RecordRef{Seq: 1, Hash: "h"}, Context: client.DecisionContext{Message: "allowed", Grant: g}}
	}
	// researcher is a task agent with a runtime image
	d := r.g.Defs["researcher"]
	text := "what languages can I build apps in?"
	view, err := r.g.RunAgentAs(context.Background(), "user:root", "tower/ns", "researcher", d, "", &text)
	if err != nil {
		t.Fatal(err)
	}
	if open.Subject.ID != "user:root" || open.Subject.Properties.Kind != "user" || !open.Action.Properties.Run || open.Action.Properties.Via != "tower/ns" {
		t.Fatalf("%+v", open)
	}
	sid := view["session"].(string)
	s := r.store.Get(sid)
	if view["run"] != runName(sid) || view["by"] != "user:root" || view["via"] != "tower/ns" || s.Input["prompt"] != text || s.Input["started_by"] != "user:root" || s.Run == nil {
		t.Fatalf("%v %+v", view, s.Input)
	}
	// the refusals before anything is opened
	long := strings.Repeat("x", dispatch.MaxRunInput+1)
	if _, err := r.g.RunAgentAs(context.Background(), "user:root", "", "researcher", d, "", &long); asCtl(err) == nil || asCtl(err).Rule != "R008" {
		t.Errorf("%v", err)
	}
	service := *d
	service.Kind = "service"
	if _, err := r.g.RunAgentAs(context.Background(), "user:root", "", "researcher", &service, "", &text); asCtl(err) == nil || asCtl(err).Rule != "R001" {
		t.Errorf("%v", err)
	}
	noInstr := *d
	noInstr.Instructions = nil
	empty := "  "
	if _, err := r.g.RunAgentAs(context.Background(), "user:root", "", "researcher", &noInstr, "", &empty); asCtl(err) == nil || asCtl(err).Rule != "R008" {
		t.Errorf("%v", err)
	}
	// a candidate is remembered for scoring, and forgotten when it ages out of the last PreflightKeep
	cand := *d
	cand.Name = "pf-researcher-00000001"
	view, err = r.g.RunAgentAs(context.Background(), "user:root", "", "researcher", &cand, "fp1", &text)
	if err != nil {
		t.Fatal(err)
	}
	if a, run, fp, ok := r.g.CandidateRun(view["session"].(string)); !ok || a != "researcher" || run != cand.Name || fp != "fp1" || r.g.Def(cand.Name) == nil {
		t.Fatal("the candidate is not remembered")
	}
	if r.g.Defs[cand.Name] != nil {
		t.Fatal("a candidate is never in the live set")
	}
}

var _ = definitions.Parse

// An input's size is counted the way Python's json.dumps counts it (ensure_ascii): everything outside " " to "~" is six
// characters, DEL included, an astral character twelve. The limit is the AgentRun's 4096.
func TestRunInputSizeIsCountedLikePythonCountsIt(t *testing.T) {
	cases := []struct {
		in   string
		want int // len(json.dumps({"p": in}, separators=(",", ":")))
	}{
		{"abc", 11}, {"\x7f", 14}, {"é", 14}, {"😀", 20}, {"\n", 10}, {"\x01", 14}, {`"`, 10}, {`\`, 10}, {"a\x7fb", 16},
	}
	for _, c := range cases {
		pad := dispatch.MaxInputChars - c.want
		// exactly at the limit is accepted; one more character is refused
		ok := map[string]any{"p": c.in + strings.Repeat("x", pad)}
		over := map[string]any{"p": c.in + strings.Repeat("x", pad+1)}
		if _, err := dispatch.RunInput(ok); err != nil {
			t.Errorf("%q at the limit: %v", c.in, err)
		}
		if _, err := dispatch.RunInput(over); err == nil {
			t.Errorf("%q one over the limit was accepted", c.in)
		}
	}
}
