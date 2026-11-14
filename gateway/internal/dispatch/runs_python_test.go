package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/runs"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/clearance/client"
)

// The operations that end a run, held equal to the Python gateway's: the sessions, the answers, the result store and the
// records the core was sent are generated from the Python code driving Gateway.finish_run, close_session, expire and _spawn
// (tools/gen_gateway_registry.py), and the Go gateway is driven through the same steps.

func readJSON(t testing.TB, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

type opsFixture map[string]json.RawMessage

func ops(t testing.TB) opsFixture {
	var f opsFixture
	readJSON(t, "testdata/runs.json", &f)
	return f
}

func rowsOf(t testing.TB, raw json.RawMessage) []session.Record {
	var recs []session.Record
	if err := json.Unmarshal(raw, &recs); err != nil {
		var one session.Record
		if err2 := json.Unmarshal(raw, &one); err2 != nil {
			t.Fatal(err)
		}
		recs = []session.Record{one}
	}
	return recs
}

func kindOf(id string) string {
	if len(id) > 6 && id[:6] == "agent-" {
		return "workload"
	}
	return "user"
}

type ctlErr struct {
	Rule    string `json:"rule"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

func asCtl(err error) *ctlErr {
	var ce *dispatch.ControlError
	if errors.As(err, &ce) {
		return &ctlErr{ce.Rule, ce.Message, ce.Status}
	}
	return nil
}

func finalRows(t testing.TB, r *runRig, ids ...string) map[string]any {
	out := map[string]any{}
	for _, id := range ids {
		s := r.store.Get(id)
		if s == nil {
			t.Fatalf("no session %s", id)
		}
		s.Lock()
		rec := s.ToRecord()
		s.Unlock()
		rec.DeleteFail = nil // the Go gateway's own key for a delete that keeps failing; the Python row has none
		out[id] = norm(t, rec)
	}
	return out
}

func wantRows(t testing.TB, raw json.RawMessage) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range m {
		m[k] = dropEmpty(v, "")
	}
	return m
}

func TestFinishRunEqualsPython(t *testing.T) {
	f := ops(t)
	compared := 0
	for name, raw := range f {
		if len(name) < 7 || name[:7] != "finish/" {
			continue
		}
		var c struct {
			Session json.RawMessage `json:"session"`
			Steps   []struct {
				Caller string         `json:"caller"`
				Result map[string]any `json:"result"`
				Error  *ctlErr        `json:"error"`
			} `json:"steps"`
			Final   json.RawMessage  `json:"final"`
			Results map[string]any   `json:"results"`
			Records []map[string]any `json:"records"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		recs := rowsOf(t, c.Session)
		r := newRunRig(t, withPersist(newPersist(recs...)))
		if c.Results == nil && name == "finish/no-result-store" {
			r.g.Results = nil
		}
		body := func(caller string) any { return nil }
		_ = body
		// the body is the same for every step: read it from the fixture's name
		for i, st := range c.Steps {
			bodyIn := finishBodies[name[7:]]
			tok := r.who(st.Caller, kindOf(st.Caller))
			got, err := r.g.FinishRun(context.Background(), tok, recs[0].ID, bodyIn)
			if st.Error != nil {
				ce := asCtl(err)
				if ce == nil || *ce != *st.Error {
					t.Errorf("%s step %d (%s): want %+v, got %v", name, i, st.Caller, *st.Error, err)
				}
				compared++
				continue
			}
			if err != nil {
				t.Errorf("%s step %d: %v", name, i, err)
				continue
			}
			wantR := dropEmpty(st.Result, "").(map[string]any)
			gotR := norm(t, got).(map[string]any)
			if _, has := wantR["audit_seq"]; has != (gotR["audit_seq"] != nil) {
				t.Errorf("%s: audit_seq present python=%v go=%v", name, has, gotR["audit_seq"] != nil)
			}
			delete(wantR, "audit_seq")
			delete(gotR, "audit_seq")
			if !reflect.DeepEqual(gotR, wantR) {
				t.Errorf("%s answer:\n go     %v\n python %v", name, gotR, wantR)
			}
			compared++
		}
		if !reflect.DeepEqual(finalRows(t, r, recs[0].ID), wantRows(t, c.Final)) {
			t.Errorf("%s: the session differs:\n go     %v\n python %v", name, finalRows(t, r, recs[0].ID), wantRows(t, c.Final))
		}
		if c.Results != nil {
			if !reflect.DeepEqual(norm(t, r.results.Rows), dropEmpty(c.Results, "")) {
				t.Errorf("%s: the kept result differs:\n go     %v\n python %v", name, norm(t, r.results.Rows), c.Results)
			}
		}
		if got, want := r.records(), sorted(c.Records); !reflect.DeepEqual(norm(t, got), norm(t, want)) {
			t.Errorf("%s: the records differ:\n go     %v\n python %v", name, norm(t, got), norm(t, want))
		}
	}
	if compared < 20 {
		t.Fatalf("only %d steps compared", compared)
	}
}

var finishBodies = map[string]any{
	"done": map[string]any{"status": "done", "final_text": "Python and Go.", "turns": json.Number("2"),
		"tool_calls": []any{map[string]any{"tool": "catalog.read", "ok": true, "rule": nil}, map[string]any{"tool": "x", "ok": false, "rule": "R001"}}},
	"long":            map[string]any{"status": "budget", "final_text": repeat("é", 20000), "turns": json.Number("9"), "reason": repeat("r", 700)},
	"empty":           map[string]any{"status": "error"},
	"bad-status":      map[string]any{"status": "great"},
	"bad-turns":       map[string]any{"status": "done", "turns": "2"},
	"bool-turns":      map[string]any{"status": "done", "turns": true},
	"bad-text":        map[string]any{"status": "done", "final_text": json.Number("5")},
	"no-result-store": map[string]any{"status": "done", "final_text": "ok"},
	"no-run":          map[string]any{"status": "done"},
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func TestCloseSessionEqualsPython(t *testing.T) {
	f := ops(t)
	compared := 0
	for name, raw := range f {
		if len(name) < 14 || name[:14] != "close_session/" || name == "close_session/twice" {
			continue
		}
		var c struct {
			Sessions []session.Record `json:"sessions"`
			Caller   string           `json:"caller"`
			Reason   *string          `json:"reason"`
			Fail     []string         `json:"fail"`
			Result   map[string]any   `json:"result"`
			Error    *ctlErr          `json:"error"`
			Final    json.RawMessage  `json:"final"`
			Records  []map[string]any `json:"records"`
			Deleted  []string         `json:"deleted"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		r := newRunRig(t, withPersist(newPersist(c.Sessions...)))
		r.api.SetFailDelete(c.Fail...)
		for _, s := range c.Sessions {
			_ = r.api.Create(context.Background(), map[string]any{"metadata": map[string]any{"name": "r-" + s.ID[2:]}})
		}
		var reason any
		if c.Reason != nil {
			reason = *c.Reason
		}
		got, err := r.g.CloseSession(context.Background(), r.who(c.Caller, kindOf(c.Caller)), c.Sessions[0].ID, reason)
		if c.Error != nil {
			if ce := asCtl(err); ce == nil || *ce != *c.Error {
				t.Errorf("%s: want %+v, got %v", name, *c.Error, err)
			}
		} else {
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			wantR := dropEmpty(c.Result, "").(map[string]any)
			gotR := norm(t, got).(map[string]any)
			if _, has := wantR["audit_seq"]; has != (gotR["audit_seq"] != nil) {
				t.Errorf("%s: audit_seq", name)
			}
			delete(wantR, "audit_seq")
			delete(gotR, "audit_seq")
			if !reflect.DeepEqual(gotR, wantR) {
				t.Errorf("%s answer:\n go     %v\n python %v", name, gotR, wantR)
			}
		}
		ids := []string{"s-0000a1", "s-0000c1", "s-0000d1"}
		if !reflect.DeepEqual(finalRows(t, r, ids...), wantRows(t, c.Final)) {
			t.Errorf("%s: the sessions differ:\n go     %v\n python %v", name, finalRows(t, r, ids...), wantRows(t, c.Final))
		}
		if got, want := r.records(), sorted(c.Records); !reflect.DeepEqual(norm(t, got), norm(t, want)) {
			t.Errorf("%s: the records differ:\n go     %v\n python %v", name, norm(t, got), norm(t, want))
		}
		if len(r.api.Deleted) != len(c.Deleted) {
			t.Errorf("%s: runs deleted %v, python %v", name, r.api.Deleted, c.Deleted)
		}
		compared++
	}
	if compared < 6 {
		t.Fatalf("%d scenarios", compared)
	}
	// a closed session is closed once
	var tw struct {
		Again *ctlErr `json:"again"`
	}
	_ = json.Unmarshal(f["close_session/twice"], &tw)
	r := newRunRig(t, withPersist(newPersist(sessionRow("s-0000a1", "user:alice", "orchestrator", "r-0000a1", nil))))
	tok := r.pod("r-0000a1")
	if _, err := r.g.CloseSession(context.Background(), tok, "s-0000a1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.g.CloseSession(context.Background(), tok, "s-0000a1", nil); asCtl(err) == nil || *asCtl(err) != *tw.Again {
		t.Fatalf("%v, python %+v", err, tw.Again)
	}
}

func TestExpireEqualsPython(t *testing.T) {
	var c struct {
		Sessions []session.Record  `json:"sessions"`
		Now      time.Time         `json:"now"`
		Phases   map[string]string `json:"phases"`
		Fail     []string          `json:"fail"`
		First    []string          `json:"first"`
		Second   []string          `json:"second"`
		Final    json.RawMessage   `json:"final"`
		Records  []map[string]any  `json:"records"`
		Deleted  []string          `json:"deleted"`
	}
	if err := json.Unmarshal(ops(t)["expire"], &c); err != nil {
		t.Fatal(err)
	}
	r := newRunRig(t, withPersist(newPersist(c.Sessions...)))
	ctx := context.Background()
	var ids []string
	for _, s := range c.Sessions {
		ids = append(ids, s.ID)
		if s.Run != nil {
			_ = r.api.Create(ctx, map[string]any{"metadata": map[string]any{"name": *s.Run}})
		}
	}
	for run, phase := range c.Phases {
		r.api.Outcomes[run] = phase
	}
	r.api.SetFailDelete(c.Fail...)
	sortedIDs := func(a []string) []string { sort.Strings(a); return a }
	first := r.g.ExpireAt(ctx, c.Now)
	if !reflect.DeepEqual(sortedIDs(first), c.First) {
		t.Errorf("the first sweep deleted the runs of %v, python %v", first, c.First)
	}
	r.api.SetFailDelete()                                 // the API server answers again: the sweep retries what failed
	second := r.g.ExpireAt(ctx, c.Now.Add(2*time.Minute)) // past the failed delete's backoff (Python retries at once)
	if !reflect.DeepEqual(sortedIDs(second), c.Second) {
		t.Errorf("the second sweep deleted the runs of %v, python %v", second, c.Second)
	}
	if !reflect.DeepEqual(finalRows(t, r, ids...), wantRows(t, c.Final)) {
		t.Errorf("the sessions differ:\n go     %v\n python %v", finalRows(t, r, ids...), wantRows(t, c.Final))
	}
	if got, want := r.records(), sorted(c.Records); !reflect.DeepEqual(norm(t, got), norm(t, want)) {
		t.Errorf("the records differ:\n go     %v\n python %v", norm(t, got), norm(t, want))
	}
	if len(c.First) == 0 || len(c.Second) == 0 || len(c.Records) < 6 {
		t.Fatal("the fixture compares too little")
	}
	// what was saved is what the sessions are: a restart sees the same
	if err := r.store.FlushAll(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		row, _ := r.persist.row(id)
		if !reflect.DeepEqual(norm(t, row), finalRows(t, r, id)[id]) {
			t.Errorf("%s: the saved row is not the session", id)
		}
	}
}

func TestSpawnStepEqualsPython(t *testing.T) {
	var c struct {
		Parent  session.Record  `json:"parent"`
		Grant   client.Grant    `json:"grant"`
		Spend   client.Spend    `json:"spend"`
		Result  map[string]any  `json:"result"`
		Final   json.RawMessage `json:"final"`
		Created []string        `json:"created"`
	}
	if err := json.Unmarshal(ops(t)["spawn"], &c); err != nil {
		t.Fatal(err)
	}
	r := newRunRig(t, withPersist(newPersist(c.Parent)))
	d := r.g.Defs["researcher"]
	parent := r.store.Get(c.Parent.ID)
	v := &core.Verdict{Allow: true, Spend: c.Spend, Grant: &c.Grant}
	got, err := r.g.Spawn(context.Background(), parent, d, v, c.Grant.Session, map[string]any{"agent": "researcher", "limits": map[string]any{}, "input": map[string]any{"prompt": "hi"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(norm(t, got), dropEmpty(c.Result, "")) {
		t.Errorf("result:\n go     %v\n python %v", norm(t, got), c.Result)
	}
	if !reflect.DeepEqual(finalRows(t, r, c.Parent.ID, c.Grant.Session), wantRows(t, c.Final)) {
		t.Errorf("the sessions differ:\n go     %v\n python %v", finalRows(t, r, c.Parent.ID, c.Grant.Session), wantRows(t, c.Final))
	}
	if len(r.api.Manifests) != 1 || r.api.Manifests[c.Created[0]] == nil {
		t.Errorf("the AgentRun created: %v, python %v", r.api.Manifests, c.Created)
	}
}

func TestActingEqualsPython(t *testing.T) {
	var cases []struct {
		SessionPrincipal string  `json:"session_principal"`
		Run              *string `json:"run"`
		Caller           string  `json:"caller"`
		Kind             string  `json:"kind"`
		Python           string  `json:"python"`
		Go               string  `json:"go"`
	}
	readJSON(t, "testdata/acting.json", &cases)
	r := newRunRig(t)
	acts, stricter := 0, 0
	for _, c := range cases {
		s, _ := session.FromRecord(sessionRow("s-0000a1", c.SessionPrincipal, "orchestrator", "", func(rec *session.Record) { rec.Run = c.Run }))
		got := r.g.Acting(s, &dispatch.Principal{ID: c.Caller, Kind: c.Kind, Pod: runs.PodName})
		if got != c.Go {
			t.Errorf("run %v caller %q (%s): go %q want %q (python %q)", c.Run, c.Caller, c.Kind, got, c.Go, c.Python)
		}
		if c.Go != c.Python {
			stricter++ // a caller Python lets act that Go does not: a user whose id is a run's, or a run name no run has
		}
		if c.Go == c.SessionPrincipal && c.Caller != c.SessionPrincipal {
			acts++
		}
	}
	if acts == 0 || stricter == 0 {
		t.Fatalf("the fixture has no acting case (%d) or no stricter case (%d)", acts, stricter)
	}
}

func TestResultsHelpersEqualPython(t *testing.T) {
	var f struct {
		Observe []struct {
			Tool     string         `json:"tool"`
			Data     any            `json:"data"`
			Signals  map[string]any `json:"signals"`
			Warnings []string       `json:"warnings"`
		} `json:"observe"`
		Warnings []struct {
			Signals  map[string]any `json:"signals"`
			Warnings []string       `json:"warnings"`
		} `json:"warnings"`
		Clips []struct {
			Len        int    `json:"len"`
			TextSHA    string `json:"text_sha"`
			ClippedLen int    `json:"clipped_len_bytes"`
			ClippedSHA string `json:"clipped_sha"`
			Truncated  bool   `json:"truncated"`
		} `json:"clips"`
		Summary struct {
			In   []any `json:"in"`
			Out  any   `json:"out"`
			Many int   `json:"many"`
		} `json:"summary"`
	}
	readJSON(t, "testdata/results.json", &f)
	sig := map[string]any{}
	for i, o := range f.Observe {
		dispatch.Observe(sig, o.Tool, o.Data)
		if !reflect.DeepEqual(norm(t, sig), norm(t, o.Signals)) {
			t.Errorf("observe %d (%s): go %v python %v", i, o.Tool, sig, o.Signals)
		}
		if w := dispatch.Warnings(sig); !reflect.DeepEqual(w, o.Warnings) {
			t.Errorf("warnings %d: go %q python %q", i, w, o.Warnings)
		}
	}
	for _, w := range f.Warnings {
		if got := dispatch.Warnings(w.Signals); !reflect.DeepEqual(got, w.Warnings) {
			t.Errorf("%v: go %q python %q", w.Signals, got, w.Warnings)
		}
	}
	texts := map[int][]string{0: {""}, 5: {"short"}}
	_ = texts
	for _, tx := range []string{"", "short", repeat("é", 20000), repeat("a", 16384), repeat("a", 16385), repeat("é", 8192), repeat("é", 8193), repeat("😀", 5000)} {
		got, cut := dispatch.ClipText(tx)
		found := false
		for _, c := range f.Clips {
			if c.TextSHA == core.ArgsDigestBytes([]byte(tx)) {
				found = true
				if len(got) != c.ClippedLen || core.ArgsDigestBytes([]byte(got)) != c.ClippedSHA || cut != c.Truncated {
					t.Errorf("clip of %d bytes: go %d %v, python %d %v", len(tx), len(got), cut, c.ClippedLen, c.Truncated)
				}
			}
		}
		if !found {
			t.Errorf("no fixture for a text of %d bytes", len(tx))
		}
	}
	// the summary
	var in []any
	for _, e := range f.Summary.In {
		in = append(in, e)
	}
	if got := norm(t, dispatch.ToolCallSummary(in)); !reflect.DeepEqual(got, dropEmpty(f.Summary.Out, "")) {
		// the nulls the Python summary keeps ("rule": null) are dropped on both sides by norm
		t.Errorf("summary:\n go     %v\n python %v", got, norm(t, f.Summary.Out))
	}
	many := make([]any, 300)
	for i := range many {
		many[i] = map[string]any{"tool": "t"}
	}
	if n := len(dispatch.ToolCallSummary(many)); n != f.Summary.Many {
		t.Errorf("a list of 300 is cut to %d, python %d", n, f.Summary.Many)
	}
}

func TestCandidateClampEqualsPython(t *testing.T) {
	var cases []struct {
		Agent       string          `json:"agent"`
		Doc         json.RawMessage `json:"doc"`
		Budget      map[string]any  `json:"budget"`
		Name        string          `json:"name"`
		Fingerprint string          `json:"fingerprint"`
		Kind        string          `json:"kind"`
		Identity    string          `json:"identity_type"`
		Limits      client.Limits   `json:"limits"`
		Error       string          `json:"error"`
	}
	readJSON(t, "testdata/candidates.json", &cases)
	r := newRunRig(t)
	for _, c := range cases {
		if c.Error != "" {
			continue
		}
		cand, err := definitions.Parse(c.Doc, tools.Default())
		if err != nil {
			t.Fatal(err)
		}
		// a live definition by this name would make the candidate's name collide: the Python test does not have one
		got, err := r.g.ClampCandidate(cand, c.Agent, c.Budget, c.Fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != c.Name || got.Kind != c.Kind || got.IdentityType != c.Identity || !reflect.DeepEqual(got.Limits, c.Limits) {
			t.Errorf("%s %v:\n go     %s %s %s %+v\n python %s %s %s %+v", c.Agent, c.Budget, got.Name, got.Kind, got.IdentityType, got.Limits, c.Name, c.Kind, c.Identity, c.Limits)
		}
		if got.ServiceAccount != nil {
			t.Errorf("a candidate has no service account")
		}
	}
	if len(cases) < 30 {
		t.Fatal(len(cases))
	}
}
