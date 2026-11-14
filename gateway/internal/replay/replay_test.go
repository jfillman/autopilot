// Package replay proves the Go gateway decides and records exactly as the Python gateway did: Clearance's golden
// corpus (testdata/golden/corpus.jsonl in the clearance checkout, recorded from the Python engine over its whole test
// suite and the fixture agents' calls) is replayed through the Go gateway's own code. For every line it can take:
//
//  1. the evaluation the Go gateway BUILDS from the corpus line's inputs (principal, session, scope, arguments) equals
//     the evaluation the Python gateway built, field for field;
//  2. sent to a real Clearance core (the unmodified binary, with Hangar's bundle, or without for the corpus lines
//     recorded without the Hangar pack), the decision the Go gateway returns equals the Python one: allow, rule id,
//     name, message, hint, tier, problems;
//  3. the spend the Go gateway applies leaves the session as the Python one left it;
//  4. the decision record the core writes has the corpus line's fields (agent, args_sha256, decision, parent,
//     principal, rule, session, target, task_id, tier, tool, ts, and result for a deny).
//
// Needs the core binary (CLEARANCE_CORE_BIN, as tests/conftest.py builds it) and a clearance checkout with the corpus
// (CLEARANCE_SRC); without them it skips. A replay that compares nothing proves nothing, so it also fails when a
// category it should have compared is empty, and TestReplayDetectsDrift shows a changed field is noticed.
package replay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/auth"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/modelproxy"
	"github.com/jfillman/autopilot/gateway/internal/runs"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/clearance/client"
)

type afterState struct {
	Denials    int              `json:"denials"`
	Reserved   map[string]int64 `json:"reserved"`
	TripReason *string          `json:"trip_reason"`
	Tripped    bool             `json:"tripped"`
	Used       map[string]int64 `json:"used"`
}

type line struct {
	n       int
	Kind    string          `json:"kind"`
	Packs   []string        `json:"packs"`
	Source  string          `json:"source"`
	Request json.RawMessage `json:"request"`
	Python  struct {
		Allow    bool            `json:"allow"`
		Hint     *string         `json:"hint"`
		Message  *string         `json:"message"`
		Name     *string         `json:"name"`
		Problems json.RawMessage `json:"problems"`
		Rule     *string         `json:"rule"`
		Tier     json.RawMessage `json:"tier"`
	} `json:"python"`
	Record  json.RawMessage `json:"record"`
	Grant   json.RawMessage `json:"grant"`
	After   *afterState     `json:"after"`
	Compare struct {
		Record bool `json:"record"`
		Spend  bool `json:"spend"`
	} `json:"compare"`
}

func loadCorpus(t testing.TB, src string) []line {
	t.Helper()
	f, err := os.Open(filepath.Join(src, "testdata", "golden", "corpus.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	n := 0
	for sc.Scan() {
		n++
		if n == 1 {
			continue // the header: the Python engine's rules and tools
		}
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("corpus line %d: %v", n, err)
		}
		l.n = n
		l.Request = unsqueeze(t, l.Request)
		out = append(out, l)
	}
	return out
}

// repeatKey marks a long one-character string the recorder squeezed (clearance tools/golden/golden_recorder.py
// _squeeze): {"__golden_repeat__": [char, count]}. The replay sends the whole string.
const repeatKey = "__golden_repeat__"

func unsqueeze(t testing.TB, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !bytes.Contains(raw, []byte(repeatKey)) {
		return raw
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			if rep, ok := x[repeatKey].([]any); ok && len(x) == 1 && len(rep) == 2 {
				n, _ := rep[1].(json.Number).Int64()
				return strings.Repeat(rep[0].(string), int(n))
			}
			for k, e := range x {
				x[k] = walk(e)
			}
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
		}
		return v
	}
	out, err := json.Marshal(walk(v))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// norm parses JSON into comparable values, with the time fields reduced to an instant (Python writes +00:00, Go Z).
func norm(t testing.TB, v any) any {
	t.Helper()
	var raw []byte
	switch x := v.(type) {
	case json.RawMessage:
		raw = x
	case []byte:
		raw = x
	default:
		var err error
		if raw, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return timefix(out)
}

func timefix(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && (k == "now" || k == "expires_at" || k == "until" || k == "ts") {
				if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
					x[k] = ts.UTC().Format("2006-01-02T15:04:05.000000000Z")
					continue
				}
			}
			x[k] = timefix(e)
		}
	case []any:
		for i := range x {
			x[i] = timefix(x[i])
		}
	}
	return v
}

type replayer struct {
	t      testing.TB
	reg    *tools.Registry
	cores  map[string]*coreProc // "hangar" and "plain"; nil in the request-only pass
	counts map[string]int
	skips  map[string]int
	mism   map[string]int
	shown  int
	// drift, when set, is applied to the evaluation the Go gateway built before it is compared and sent: the
	// detection test's way of showing a changed field is noticed.
	drift func(*client.EvaluationRequest)
}

func newReplayer(t testing.TB) *replayer {
	return &replayer{t: t, reg: tools.Default(), counts: map[string]int{}, skips: map[string]int{}, mism: map[string]int{}}
}

// decidedDifferentlySince are the corpus lines whose decision changed on purpose after the Python engine recorded them: a
// cicd.yaml scope decision under Glidepath's contract version 2 (the `propose` scope class, A-11b). A new Flight environment's
// cluster and production read "a propose field" instead of "a human-only field" (the same refusal, under GP-SCOPE-004 on the
// file tool, which cannot mark its pull request for a person), and the Flight environment's hint names the tools that can
// propose it. Each is still a refusal with the same first rule; tests/test_cicd_scope.py asserts the new text through the
// real core. The corpus itself is Clearance's and stays as recorded; a line is listed here only with its reason.
var decidedDifferentlySince = map[string]string{
	"tests/test_cicd_scope.py::test_denied_cicd_change_carries_the_gp_scope_rule_and_hint[flight-env-added]": "the Flight environment hint names the proposing tools",
	"tests/test_cicd_scope.py::test_a_new_cicd_yaml_is_a_human_change":                                       "a new environment's cluster and production are propose",
	"tools/golden/test_fixture_calls.py::test_cicd_scope_probe[2]":                                           "the Flight environment hint names the proposing tools",
	"tools/golden/test_fixture_calls.py::test_cicd_scope_probe[9]":                                           "the Flight environment hint names the proposing tools",
}

func (r *replayer) fail(l line, what string, got, want any) {
	if why, ok := decidedDifferentlySince[l.Source]; ok {
		r.skips[l.Kind+": decided differently on purpose since the corpus was recorded ("+why+")"]++
		return
	}
	r.mism[l.Kind+" "+what]++
	if r.shown < 25 {
		r.shown++
		path, g, w := firstDiff("", got, want)
		r.t.Errorf("line %d (%s, %s): %s differs at %q:\n  go     %s\n  python %s", l.n, l.Kind, l.Source, what, path, short(g), short(w))
	}
}

// firstDiff finds the first path where two decoded JSON values differ.
func firstDiff(path string, a, b any) (string, any, any) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		var ks []string
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			if p, x, y := firstDiff(path+"/"+k, am[k], bm[k]); p != "" {
				return p, x, y
			}
		}
		return "", nil, nil
	}
	if !reflect.DeepEqual(a, b) {
		return path, a, b
	}
	return "", nil, nil
}

func short(v any) string {
	b, _ := json.Marshal(v)
	if len(b) > 700 {
		return string(b[:700]) + "..."
	}
	return string(b)
}

func (r *replayer) eq(l line, what string, got, want any) bool {
	if reflect.DeepEqual(got, want) {
		return true
	}
	r.fail(l, what, got, want)
	return false
}

func decodeArgs(raw json.RawMessage) map[string]any {
	var m map[string]any
	if len(raw) == 0 {
		return map[string]any{}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	_ = d.Decode(&m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// adopt holds a session as the request names it.
func adopt(st *session.Store, state *client.SessionState, now time.Time, a *afterState) *session.Session {
	s := st.Adopt(&client.Grant{Session: state.ID, TaskID: state.TaskID, Principal: state.Principal, Agent: state.Agent,
		Parent: state.ParentID, Depth: state.Depth, Limits: state.Limits, ExpiresAt: state.ExpiresAt}, now, nil, 0)
	s.Used, s.Reserved, s.Denials, s.Closed = state.Used, state.Reserved, state.Denials, state.Closed
	if state.Tripped {
		s.Trip("tripped before this call")
	}
	return s
}

// withRun gives the session of a line recorded with a run acting as it the run the Python gateway had then: the run named
// after the session, which is what the caller's identity (agent-<run>/run) is checked against.
func withRun(s *session.Session, want *client.EvaluationRequest) {
	if s != nil && want.Subject.Properties.ActingAs != "" {
		name := runs.RunName(s.ID)
		s.Lock()
		s.Run = &name
		s.Unlock()
	}
}

func principalOf(sub client.Subject) *dispatch.Principal {
	p := &dispatch.Principal{ID: sub.ID, Kind: sub.Properties.Kind, TierCeiling: sub.Properties.TierCeiling}
	if sub.Properties.ActingAs != "" {
		p.Pod = runs.PodName // a run's token is bound to its pod (a corpus line of a run acting as its session)
	}
	return p
}

func (r *replayer) newGateway(core *core.Core, st *session.Store, defs map[string]*definitions.Definition, p *dispatch.Principal, tool string, now time.Time) *dispatch.Gateway {
	handlers := map[string]dispatch.Handler{}
	if _, ok := r.reg.Get(tool); ok && !dispatch.CoreOwned[tool] {
		handlers[tool] = func(context.Context, *session.Session, map[string]any) (any, error) {
			return map[string]any{"ok": true}, nil
		}
	}
	g, err := dispatch.New(defs, st, core, auth.Static{"t": p}, r.reg, handlers)
	if err != nil {
		r.t.Fatal(err)
	}
	// The run backend the run tree needs: a fake that starts nothing and names a run's identity as the real one does.
	g.SetRuns(runs.NewFake())
	if tool == "run.close" {
		// The corpus decided the call; the session it names its children in is not in this replay, so the handler's own
		// check (tests in dispatch) would refuse it. The decision and the spend are what is compared.
		g.Handlers[tool] = func(context.Context, *session.Session, map[string]any) (any, error) {
			return map[string]any{"ok": true}, nil
		}
	}
	g.Now = func() time.Time { return now }
	g.OnPersistError = func(err error) { r.t.Error(err) }
	return g
}

func (r *replayer) coreFor(l line) *coreProc {
	if r.cores == nil {
		return nil
	}
	for _, p := range l.Packs {
		if p == "hangar" {
			return r.cores["hangar"]
		}
	}
	return r.cores["plain"]
}

func goCore(t testing.TB, url string) *core.Core {
	cl, err := client.New(client.Options{BaseURL: url, Token: client.StaticToken(gatewayPrincipal), Provider: core.Provider,
		Timeout: 10 * time.Second, BreakerThreshold: -1})
	if err != nil {
		t.Fatal(err)
	}
	return core.New(cl, core.NewMemoryOutbox(), false)
}

// run replays one line.
func (r *replayer) run(ctx context.Context, l line) {
	if len(l.Request) == 0 {
		r.skips[l.Kind+": the core's own decision engine (no evaluation request), not the gateway's"]++
		return
	}
	var want client.EvaluationRequest
	if err := json.Unmarshal(l.Request, &want); err != nil {
		// Python integers are unbounded; the core's types are int64. Nothing to build a request from.
		r.skips[l.Kind+": a number beyond int64 in the request ("+strings.SplitN(err.Error(), " into", 2)[0]+")"]++
		return
	}
	switch l.Kind {
	case "tool.call":
		r.toolCall(ctx, l, &want)
	case "session.open":
		p := want.Action.Properties
		if p.Trigger != "" || want.Context.Hold != nil || want.Context.Live != nil {
			r.triggeredOpen(ctx, l, &want)
			return
		}
		r.open(ctx, l, &want)
	case "model.call":
		r.modelCall(ctx, l, &want)
	case "session.spawn":
		r.spawn(ctx, l, &want)
	default:
		r.skips[l.Kind+": the core's own decision engine, not the gateway's"]++
	}
}

func (r *replayer) decision(l line, got map[string]any) {
	py := map[string]any{"allow": l.Python.Allow, "rule": l.Python.Rule, "message": l.Python.Message}
	g := map[string]any{"allow": got["allow"], "rule": got["rule"], "message": got["message"]}
	if l.Kind == "tool.call" {
		py["name"], py["hint"] = l.Python.Name, l.Python.Hint
		g["name"], g["hint"] = got["name"], got["hint"]
		var tier any
		_ = json.Unmarshal(l.Python.Tier, &tier)
		py["tier"], g["tier"] = tier, norm(r.t, got["tier"])
		var wp any
		_ = json.Unmarshal(l.Python.Problems, &wp)
		if ws, _ := wp.([]any); len(ws) > 0 {
			py["problems"], g["problems"] = wp, norm(r.t, got["problems"])
		}
	}
	// normalise nil vs empty strings the way the corpus has them (None for a missing message)
	for _, m := range []map[string]any{py, g} {
		for k, v := range m {
			switch x := v.(type) {
			case *string:
				if x == nil {
					m[k] = nil
				} else {
					m[k] = *x
				}
			}
		}
	}
	r.eq(l, "decision", norm(r.t, g), norm(r.t, py))
}

func (r *replayer) spend(l line, s *session.Session, before *client.SessionState) {
	if !l.Compare.Spend || l.After == nil || s == nil {
		return
	}
	s.Lock()
	got := afterState{Denials: s.Denials, Tripped: s.Tripped, Used: map[string]int64{}, Reserved: map[string]int64{}}
	for _, f := range session.BudgetFields {
		got.Used[f], got.Reserved[f] = session.Budget(&s.Used, f), session.Budget(&s.Reserved, f)
	}
	if s.TripReason != nil {
		got.TripReason = s.TripReason
	}
	s.Unlock()
	want := *l.After
	if before != nil && before.Tripped {
		got.TripReason = want.TripReason // tripped before this call, for a reason the request does not carry
	}
	r.counts["spend compared"]++
	r.eq(l, "spend (session after)", norm(r.t, got), norm(r.t, want))
}

// recordFields compares the decision record the core wrote with the corpus line's.
func (r *replayer) record(l line, c *coreProc, head int64) {
	if !l.Compare.Record || len(l.Record) == 0 || string(l.Record) == "null" || c == nil {
		return
	}
	recs := c.records(r.t, head)
	if len(recs) == 0 {
		r.fail(l, "record", "none written", norm(r.t, l.Record))
		return
	}
	got := recs[0]
	want := norm(r.t, l.Record).(map[string]any)
	if l.Python.Allow && want["tool"] != "session.open" {
		delete(want, "result")
		delete(want, "effect")
	}
	sub := map[string]any{}
	gotN := norm(r.t, got).(map[string]any)
	for k := range want {
		sub[k] = gotN[k]
	}
	r.counts["record compared"]++
	r.eq(l, "decision record", sub, want)
}

func (r *replayer) toolCall(ctx context.Context, l line, want *client.EvaluationRequest) {
	st, _ := session.NewStore(ctx, nil)
	now := want.Context.Now
	p := principalOf(want.Subject)
	defs := map[string]*definitions.Definition{}
	var s *session.Session
	var before *client.SessionState
	if want.Context.Session != nil {
		before = want.Context.Session
		s = adopt(st, want.Context.Session, now, nil)
		withRun(s, want)
		sc := want.Context.Scope
		defs[s.Agent] = &definitions.Definition{Name: s.Agent, Kind: sc.AgentKind, DenyPaths: sc.DenyPaths, ReposAllow: sc.ReposAllow,
			APIAllow: sc.APIAllow, FieldDeny: sc.FieldDeny, FieldAllow: sc.FieldAllow, RawProfile: sc.Profile}
	}
	tool := want.Resource.ID
	var c *core.Core
	cp := r.coreFor(l)
	if cp != nil {
		c = goCore(r.t, cp.URL)
	} else {
		c = core.New(nil, nil, false)
	}
	g := r.newGateway(c, st, defs, p, tool, now)
	args := decodeArgs(want.Action.Properties.Args)
	got, err := g.ToolRequest(p, want.Context.SessionID, s, tool, args, now)
	if err != nil {
		r.t.Fatalf("line %d: %v", l.n, err)
	}
	if r.drift != nil {
		r.drift(got)
	}
	r.counts["tool.call request"]++
	reqOK := r.eq(l, "evaluation request", norm(r.t, got), norm(r.t, l.Request))
	if cp == nil || !reqOK {
		return
	}
	g.BuildRequest = func(*dispatch.Principal, string, *session.Session, string, map[string]any, time.Time) (*client.EvaluationRequest, error) {
		return got, nil
	}
	head := cp.head(r.t)
	res, err := g.Call(ctx, "t", want.Context.SessionID, tool, args)
	if err != nil {
		r.fail(l, "call", err.Error(), "a decision")
		return
	}
	r.counts["tool.call decision"]++
	r.decision(l, res.Decision)
	r.spend(l, s, before)
	r.record(l, cp, head)
}

// spawn replays a session.spawn: the evaluation the Go gateway builds for run.spawn equals the Python one, and sent to the
// real core the decision, the parent's spend and reservation, the grant the child is created from and the record equal
// the Python gateway's. The grant is the real core's, so the gateway's own check of it (session.Store.AdoptChild) must
// accept every grant the core makes and the Python gateway accepted.
func (r *replayer) spawn(ctx context.Context, l line, want *client.EvaluationRequest) {
	st, _ := session.NewStore(ctx, nil)
	st.SetIDs(func() string { return want.Context.NewSessionID })
	now := want.Context.Now
	p := principalOf(want.Subject)
	defs := map[string]*definitions.Definition{}
	var s *session.Session
	var before *client.SessionState
	if want.Context.Session != nil {
		before = want.Context.Session
		s = adopt(st, want.Context.Session, now, nil)
		sc := want.Context.Scope
		defs[s.Agent] = &definitions.Definition{Name: s.Agent, Kind: sc.AgentKind, DenyPaths: sc.DenyPaths, ReposAllow: sc.ReposAllow,
			APIAllow: sc.APIAllow, FieldDeny: sc.FieldDeny, FieldAllow: sc.FieldAllow, RawProfile: sc.Profile}
	}
	if a := want.Context.Agent; a != nil {
		d := defs[a.Name] // an agent that spawns itself is both the parent (its scope) and the child (its limits)
		if d == nil {
			d = &definitions.Definition{Name: a.Name}
			defs[a.Name] = d
		}
		d.Kind, d.IdentityType, d.ServiceAccount, d.Limits = a.Kind, a.IdentityType, a.ServiceAccount, a.Limits
		image := "ghcr.io/example/agent@sha256:" + strings.Repeat("a", 64)
		d.Image, d.Framework, d.Sandbox = &image, "generic", "standard"
	}
	cp := r.coreFor(l)
	c := core.New(nil, nil, false)
	if cp != nil {
		c = goCore(r.t, cp.URL)
	}
	g := r.newGateway(c, st, defs, p, "run.spawn", now)
	args := decodeArgs(want.Action.Properties.Args)
	got, err := g.ToolRequest(p, want.Context.SessionID, s, "run.spawn", args, now)
	if err != nil {
		r.t.Fatalf("line %d: %v", l.n, err)
	}
	g.ApplySpawn(got, args, st.NewID())
	st.GiveBack(want.Context.NewSessionID)
	if r.drift != nil {
		r.drift(got)
	}
	r.counts["session.spawn request"]++
	if !r.eq(l, "evaluation request", norm(r.t, got), norm(r.t, l.Request)) || cp == nil {
		return
	}
	g.BuildRequest = func(*dispatch.Principal, string, *session.Session, string, map[string]any, time.Time) (*client.EvaluationRequest, error) {
		return got, nil
	}
	head := cp.head(r.t)
	res, err := g.Call(ctx, "t", want.Context.SessionID, "run.spawn", args)
	if err != nil {
		r.fail(l, "call", err.Error(), "a decision")
		return
	}
	r.counts["session.spawn decision"]++
	r.decision(l, res.Decision)
	r.spend(l, s, before)
	if len(l.Grant) > 0 && string(l.Grant) != "null" {
		child := st.Get(want.Context.NewSessionID)
		if child == nil {
			r.fail(l, "grant", "no child session", norm(r.t, l.Grant))
		} else {
			child.Lock()
			g := map[string]any{"session": child.ID, "principal": child.Principal, "agent": child.Agent, "task_id": child.TaskID,
				"parent": child.ParentID, "depth": child.Depth, "limits": child.Limits, "expires_at": child.ExpiresAt}
			child.Unlock()
			wantG := norm(r.t, l.Grant).(map[string]any)
			delete(wantG, "deduped")
			r.counts["grant compared"]++
			r.eq(l, "grant", norm(r.t, g), wantG)
		}
	} else if res.OK {
		r.fail(l, "grant", "a child was created", "no grant")
	}
	r.record(l, cp, head)
}

func (r *replayer) open(ctx context.Context, l line, want *client.EvaluationRequest) {
	st, _ := session.NewStore(ctx, nil)
	st.SetIDs(func() string { return want.Context.NewSessionID })
	now := want.Context.Now
	p := principalOf(want.Subject)
	defs := map[string]*definitions.Definition{}
	if a := want.Context.Agent; a != nil {
		defs[a.Name] = &definitions.Definition{Name: a.Name, Kind: a.Kind, IdentityType: a.IdentityType, ServiceAccount: a.ServiceAccount, Limits: a.Limits}
	}
	cp := r.coreFor(l)
	c := core.New(nil, nil, false)
	if cp != nil {
		c = goCore(r.t, cp.URL)
	}
	g := r.newGateway(c, st, defs, p, "", now)
	got := g.OpenRequest(p, want.Resource.ID, want.Action.Properties.Limits, want.Action.Properties.TaskID, st.NewID(), now)
	st.GiveBack(want.Context.NewSessionID)
	if r.drift != nil {
		r.drift(got)
	}
	r.counts["session.open request"]++
	if !r.eq(l, "evaluation request", norm(r.t, got), norm(r.t, l.Request)) || cp == nil {
		return
	}
	head := cp.head(r.t)
	res, err := g.Open(ctx, "t", want.Resource.ID, want.Action.Properties.Limits, want.Action.Properties.TaskID)
	if err != nil {
		r.fail(l, "open", err.Error(), "a decision")
		return
	}
	r.counts["session.open decision"]++
	r.decision(l, res.Decision)
	if len(l.Grant) > 0 && string(l.Grant) != "null" {
		s := st.Get(want.Context.NewSessionID)
		if s == nil {
			r.fail(l, "grant", "no session", norm(r.t, l.Grant))
		} else {
			s.Lock()
			g := map[string]any{"session": s.ID, "principal": s.Principal, "agent": s.Agent, "task_id": s.TaskID, "parent": s.ParentID,
				"depth": s.Depth, "limits": s.Limits, "expires_at": s.ExpiresAt}
			s.Unlock()
			wantG := norm(r.t, l.Grant).(map[string]any)
			delete(wantG, "deduped")
			r.counts["grant compared"]++
			r.eq(l, "grant", norm(r.t, g), wantG)
		}
	} else if res.OK {
		r.fail(l, "grant", "a session was opened", "no grant")
	}
	r.record(l, cp, head)
}

// triggeredOpen replays a session.open of a trigger: the evaluation open_triggered builds (the agent's hold and the live session of
// the task in its context), the core's decision, and the session it grants (a new one, or the live one when the core answers a
// dedupe).
func (r *replayer) triggeredOpen(ctx context.Context, l line, want *client.EvaluationRequest) {
	st, _ := session.NewStore(ctx, nil)
	st.SetIDs(func() string { return want.Context.NewSessionID })
	now := want.Context.Now
	defs := map[string]*definitions.Definition{}
	if a := want.Context.Agent; a != nil {
		defs[a.Name] = &definitions.Definition{Name: a.Name, Kind: a.Kind, IdentityType: a.IdentityType, ServiceAccount: a.ServiceAccount, Limits: a.Limits}
	}
	if want.Context.Live != nil {
		adopt(st, want.Context.Live, now, nil)
	}
	cp := r.coreFor(l)
	c := core.New(nil, nil, false)
	if cp != nil {
		c = goCore(r.t, cp.URL)
	}
	g := r.newGateway(c, st, defs, &dispatch.Principal{}, "", now)
	holds := &dispatch.MemoryHolds{}
	if h := want.Context.Hold; h != nil {
		_ = holds.Put(ctx, &dispatch.Hold{Agent: want.Resource.ID, By: h.By, Reason: h.Reason, Since: now.Add(-time.Minute), Until: h.Until})
	}
	g.Holds = holds
	got, err := g.TriggeredRequest(ctx, want.Resource.ID, want.Action.Properties.Trigger, want.Action.Properties.TaskID, st.NewID(), now)
	if err != nil {
		r.t.Fatalf("line %d: %v", l.n, err)
	}
	st.GiveBack(want.Context.NewSessionID)
	if r.drift != nil {
		r.drift(got)
	}
	r.counts["session.open request"]++
	r.counts["triggered session.open request"]++
	if !r.eq(l, "evaluation request", norm(r.t, got), norm(r.t, l.Request)) || cp == nil {
		return
	}
	head := cp.head(r.t)
	res, err := g.OpenTriggered(ctx, want.Resource.ID, want.Action.Properties.Trigger, want.Action.Properties.TaskID)
	if err != nil {
		r.fail(l, "open", err.Error(), "a decision")
		return
	}
	r.counts["session.open decision"]++
	r.counts["triggered session.open decision"]++
	r.decision(l, res.Decision)
	if len(l.Grant) > 0 && string(l.Grant) != "null" {
		var wantGrant struct {
			Session string `json:"session"`
		}
		_ = json.Unmarshal(l.Grant, &wantGrant)
		s := st.Get(wantGrant.Session)
		if s == nil {
			r.fail(l, "grant", "no session", norm(r.t, l.Grant))
		} else {
			s.Lock()
			g := map[string]any{"session": s.ID, "principal": s.Principal, "agent": s.Agent, "task_id": s.TaskID, "parent": s.ParentID,
				"depth": s.Depth, "limits": s.Limits, "expires_at": s.ExpiresAt}
			s.Unlock()
			wantG := norm(r.t, l.Grant).(map[string]any)
			delete(wantG, "deduped")
			r.counts["grant compared"]++
			r.eq(l, "grant", norm(r.t, g), wantG)
		}
	} else if res.OK {
		r.fail(l, "grant", "a session was opened", "no grant")
	}
	r.record(l, cp, head)
}

func (r *replayer) modelCall(ctx context.Context, l line, want *client.EvaluationRequest) {
	if want.Context.Models == nil {
		r.skips["model.call: a gateway with no model routes answers M006 without asking the core"]++
		return
	}
	st, _ := session.NewStore(ctx, nil)
	now := want.Context.Now
	p := principalOf(want.Subject)
	var s *session.Session
	var before *client.SessionState
	if want.Context.Session != nil {
		before = want.Context.Session
		s = adopt(st, want.Context.Session, now, nil)
		withRun(s, want)
	}
	cp := r.coreFor(l)
	c := core.New(nil, nil, false)
	if cp != nil {
		c = goCore(r.t, cp.URL)
	}
	g := r.newGateway(c, st, nil, p, "", now)
	routes := map[string]*modelproxy.Route{}
	if want.Context.Models != nil {
		for _, name := range want.Context.Models.Routes {
			routes[name] = &modelproxy.Route{Alias: name, Backend: "anthropic", Upstream: "https://example.invalid", UpstreamModel: name}
		}
	}
	m := &modelproxy.Models{Routes: routes, G: g, Auth: auth.Static{"t": p}}
	est := int64(0)
	if want.Action.Properties.EstTokens != nil {
		est = *want.Action.Properties.EstTokens
	}
	acting := p.ID
	if s != nil {
		acting = g.Acting(s, p)
	}
	got := m.Request(p.ID, s, want.Context.SessionID, want.Resource.ID, est, acting)
	if r.drift != nil {
		r.drift(got)
	}
	r.counts["model.call request"]++
	if !r.eq(l, "evaluation request", norm(r.t, got), norm(r.t, l.Request)) || cp == nil {
		return
	}
	head := cp.head(r.t)
	d, err := m.Authorize(ctx, "t", want.Context.SessionID, want.Resource.ID, est)
	if err != nil {
		r.fail(l, "authorize", err.Error(), "a decision")
		return
	}
	r.counts["model.call decision"]++
	var rule any
	if d.Rule != "" {
		rule = d.Rule
	}
	r.decision(l, map[string]any{"allow": d.Allow, "rule": rule, "message": d.Message})
	r.spend(l, s, before)
	r.record(l, cp, head)
}

func (r *replayer) report() {
	keys := func(m map[string]int) []string {
		var k []string
		for x := range m {
			k = append(k, x)
		}
		sort.Strings(k)
		return k
	}
	var b strings.Builder
	for _, k := range keys(r.counts) {
		fmt.Fprintf(&b, "\n  %-28s %d", k, r.counts[k])
	}
	for _, k := range keys(r.skips) {
		fmt.Fprintf(&b, "\n  not replayed: %s: %d", k, r.skips[k])
	}
	r.t.Logf("replay summary:%s", b.String())
}

// ---------------------------------------------------------------------------------------------------------------------

// TestCorpusRequestsIdentical needs only the corpus: every evaluation the Go gateway builds equals the Python one.
func TestCorpusRequestsIdentical(t *testing.T) {
	src := clearanceSrc(t)
	if src == "" {
		t.Skip("no clearance checkout with the golden corpus (set CLEARANCE_SRC)")
	}
	r := newReplayer(t)
	ctx := context.Background()
	for _, l := range loadCorpus(t, src) {
		r.run(ctx, l)
	}
	r.report()
	for _, k := range []string{"tool.call request", "session.open request", "model.call request", "session.spawn request", "triggered session.open request"} {
		if r.counts[k] == 0 {
			t.Errorf("no corpus line compares %s", k)
		}
	}
	if len(r.mism) > 0 {
		t.Errorf("mismatches: %v", r.mism)
	}
}

func coresOrSkip(t testing.TB) map[string]*coreProc {
	bin := os.Getenv("CLEARANCE_CORE_BIN")
	if bin == "" {
		bin = os.Getenv("AUTOPILOT_TEST_CORE_BIN")
	}
	if bin == "" {
		t.Skip("no Clearance core binary (set CLEARANCE_CORE_BIN)")
	}
	return map[string]*coreProc{"hangar": startCore(t, bin, true), "plain": startCore(t, bin, false)}
}

// TestCorpusReplaysThroughGateway: every decision the Python gateway got, the Go gateway gets, from a real core, and
// leaves its session and the core's record as the Python one did.
func TestCorpusReplaysThroughGateway(t *testing.T) {
	src := clearanceSrc(t)
	if src == "" {
		t.Skip("no clearance checkout with the golden corpus (set CLEARANCE_SRC)")
	}
	r := newReplayer(t)
	r.cores = coresOrSkip(t)
	ctx := context.Background()
	for _, l := range loadCorpus(t, src) {
		r.run(ctx, l)
	}
	r.report()
	for _, k := range []string{"tool.call decision", "session.open decision", "model.call decision", "session.spawn decision", "triggered session.open decision", "spend compared", "record compared", "grant compared"} {
		if r.counts[k] == 0 {
			t.Errorf("no corpus line compares %s: a replay that compares nothing proves nothing", k)
		}
	}
	if len(r.mism) > 0 {
		t.Errorf("%d kinds of mismatch: %v", len(r.mism), r.mism)
	}
}

// TestReplayDetectsDrift: a one-field change to the evaluation the gateway builds makes the replay fail, so a pass is
// not vacuous.
func TestReplayDetectsDrift(t *testing.T) {
	src := clearanceSrc(t)
	if src == "" {
		t.Skip("no clearance checkout with the golden corpus (set CLEARANCE_SRC)")
	}
	r := newReplayer(t)
	r.shown = 1 << 30 // quiet
	r.drift = func(q *client.EvaluationRequest) { q.Subject.Properties.TierCeiling++ }
	rec := &quietT{T: t}
	r.t = rec
	ctx := context.Background()
	for _, l := range loadCorpus(t, src) {
		r.run(ctx, l)
	}
	if len(r.mism) == 0 {
		t.Fatal("a drifted tier ceiling went unnoticed by the replay")
	}
}

// quietT swallows Errorf so the detection test can expect failures.
type quietT struct{ *testing.T }

func (q *quietT) Errorf(string, ...any) {}
