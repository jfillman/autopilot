package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/auth"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/fakecore"
	"github.com/jfillman/autopilot/gateway/internal/mcpserver"
	"github.com/jfillman/autopilot/gateway/internal/runs"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/clearance/client"
)

// The person-facing routes, over HTTP, as Python's create_http_app serves them: a bearer token, X-Hangar-On-Behalf-Of for a
// delegate, {"error", "rule", "hint"} with the refusal's status, 201 for a run, 401 for a token nobody vouches for.
func peopleSetup(t *testing.T) (*httptest.Server, *session.Store, *dispatch.MemoryInbox, *dispatch.MemoryHolds) {
	fc := fakecore.New()
	t.Cleanup(fc.Close)
	now := time.Now().UTC()
	l := client.Limits{TierCeiling: "T1", TTLMinutes: 30, ToolCalls: 10, Tools: []string{"catalog.read", "human.request"}, Models: []string{}, Network: "clearance", Compute: "small"}
	st, _ := session.NewStore(context.Background(), nil)
	st.Adopt(&client.Grant{Session: "s-0000a1", TaskID: "t-0000a1", Principal: "user:alice", Agent: "coding-agent", Limits: l, ExpiresAt: now.Add(time.Hour)}, now, nil, 0)
	fc.Decide = func(req *client.EvaluationRequest) client.EvaluationResult {
		id := "d-1"
		ref := &client.RecordRef{Seq: 1, Hash: "h"}
		if req.Action.Name == "session.open" && req.Context.Agent != nil {
			a := req.Context.Agent
			g := &client.Grant{Session: req.Context.NewSessionID, TaskID: "t-" + req.Context.NewSessionID[2:], Principal: req.Subject.ID, Agent: a.Name, Limits: a.Limits,
				ExpiresAt: req.Context.Now.Add(time.Duration(a.Limits.TTLMinutes) * time.Minute)}
			return client.EvaluationResult{Decision: true, DecisionID: &id, Record: ref, Context: client.DecisionContext{Message: "allowed", Grant: g}}
		}
		return client.EvaluationResult{Decision: true, DecisionID: &id, Record: ref, Context: client.DecisionContext{Message: "allowed"}}
	}
	c := core.New(fc.Client(time.Second), core.NewMemoryOutbox(), false)
	image := "ghcr.io/x/y@sha256:" + strings.Repeat("a", 64)
	instr := "do the work"
	owner := "group:default/team"
	defs := map[string]*definitions.Definition{
		"coding-agent": {Name: "coding-agent", Kind: "task", IdentityType: "delegated", Limits: l, Image: &image, Instructions: &instr, Owner: &owner},
		"triage-agent": {Name: "triage-agent", Kind: "event", IdentityType: "workload", Limits: l, Owner: &owner},
	}
	au := auth.Static{
		"run":      {ID: "agent-r-0000a1/run", Kind: "workload", TierCeiling: 1, Pod: "agent"},
		"alice":    {ID: "user:alice", Kind: "user", TierCeiling: 1},
		"bob":      {ID: "user:bob", Kind: "user", TierCeiling: 1},
		"root":     {ID: "user:root", Kind: "user", TierCeiling: 1},
		"delegate": {ID: "hangar-prod/tower/backend", Kind: "workload", TierCeiling: 1},
	}
	g, err := dispatch.New(defs, st, c, au, tools.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	g.SetRuns(&runs.Backend{Store: runs.NewFakeStore()})
	g.Results = &dispatch.MemoryResults{}
	inbox, holds := &dispatch.MemoryInbox{}, &dispatch.MemoryHolds{}
	g.SetInbox(inbox, nil, []string{"hangar-prod/tower/*"})
	g.Admins = []string{"user:root"}
	g.Holds = holds
	srv := httptest.NewServer(mcpserver.Handler(g, "/mcp", "http://gw.test", "http://dex.test"))
	t.Cleanup(srv.Close)
	return srv, st, inbox, holds
}

func do(t *testing.T, srv *httptest.Server, method, path, token, on, body string) (int, map[string]any, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if on != "" {
		req.Header.Set(mcpserver.OnBehalfOfHeader, on)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, string(raw)
}

func TestThePeopleRoutes(t *testing.T) {
	srv, st, inbox, holds := peopleSetup(t)
	// no token, a token nobody vouches for
	if code, m, _ := do(t, srv, "POST", "/v1/sessions/s-0000a1/freeze", "", "", `{"reason":"x"}`); code != 401 || m["error"] != "a bearer token is required" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ := do(t, srv, "POST", "/v1/sessions/s-0000a1/freeze", "nope", "", `{"reason":"x"}`); code != 401 || m["error"] != "token not accepted" {
		t.Fatalf("%d %v", code, m)
	}
	// the run's own token opens none of the routes: it is not a person, and not a delegate
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/sessions/s-0000a1/freeze", `{"reason":"x"}`}, {"POST", "/v1/sessions/s-0000a1/end", `{"reason":"x"}`},
		{"POST", "/v1/agents/triage-agent/hold", `{"reason":"x"}`}, {"DELETE", "/v1/agents/triage-agent/hold", ``},
		{"POST", "/v1/agents/coding-agent/runs", `{"input":"x"}`}, {"GET", "/v1/human/requests", ``},
		{"POST", "/v1/human/requests/h-000000000001/answer", `{"answer":"x"}`}, {"POST", "/v1/preflight/score", `{}`},
	} {
		for _, on := range []string{"", "user:root", "user:alice"} {
			code, m, _ := do(t, srv, c.method, c.path, "run", on, c.body)
			if code != 403 {
				t.Errorf("%s %s (on behalf of %q) with the run's token: %d %v", c.method, c.path, on, code, m)
			}
		}
	}
	if s := st.Get("s-0000a1"); s.Tripped || s.Closed {
		t.Fatal("a refused call changed the session")
	}
	// freeze: a body that is not JSON, one that is too large, no reason, then the person, through a delegate
	if code, m, _ := do(t, srv, "POST", "/v1/sessions/s-0000a1/freeze", "alice", "", `{not json`); code != 400 || m["error"] != "the body is not JSON" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ := do(t, srv, "POST", "/v1/sessions/s-0000a1/freeze", "alice", "", `{"reason":"`+strings.Repeat("x", mcpserver.MaxBody)+`"}`); code != 413 || m["error"] != "body too large" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ := do(t, srv, "POST", "/v1/sessions/s-0000a1/freeze", "alice", "", ``); code != 400 || m["rule"] != "CT-REASON" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ := do(t, srv, "POST", "/v1/sessions/s-0000a1/freeze", "bob", "", `{"reason":"x"}`); code != 403 || m["rule"] != "CT-FORBIDDEN" || m["hint"] == "" {
		t.Fatalf("%d %v", code, m)
	}
	code, m, _ := do(t, srv, "POST", "/v1/sessions/s-0000a1/freeze", "delegate", "user:alice", `{"reason":"it opens bad PRs"}`)
	if code != 200 || m["tripped"] != true || !st.Get("s-0000a1").Tripped {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ = do(t, srv, "POST", "/v1/sessions/s-0000a1/freeze", "alice", "", `{"reason":"again"}`); code != 409 || m["rule"] != "CT-TRIPPED" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ = do(t, srv, "POST", "/v1/sessions/s-nope/end", "alice", "", `{"reason":"x"}`); code != 404 || m["rule"] != "CT-UNKNOWN" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ = do(t, srv, "POST", "/v1/sessions/s-0000a1/end", "root", "", `{"reason":"enough"}`); code != 200 || m["closed"] != true || !st.Get("s-0000a1").Closed {
		t.Fatalf("%d %v", code, m)
	}
	// hold and release
	if code, m, _ = do(t, srv, "POST", "/v1/agents/triage-agent/hold", "root", "", `{"reason":"storm","hours":2}`); code != 200 || m["by"] != "user:root" || m["via"] != nil {
		t.Fatalf("%d %v", code, m)
	}
	if h, _ := holds.Get(context.Background(), "triage-agent"); h == nil {
		t.Fatal("no hold")
	}
	if code, m, _ = do(t, srv, "DELETE", "/v1/agents/triage-agent/hold", "bob", "", ``); code != 403 {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ = do(t, srv, "DELETE", "/v1/agents/triage-agent/hold", "root", "", ``); code != 200 || m["released"] != true {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ = do(t, srv, "DELETE", "/v1/agents/triage-agent/hold", "root", "", ``); code != 404 || m["rule"] != "CT-NOHOLD" {
		t.Fatalf("%d %v", code, m)
	}
	// a run of a task agent: 201; of an event agent: 409; a candidate document: 501 (the schema is not carried)
	if code, m, _ = do(t, srv, "POST", "/v1/agents/coding-agent/runs", "root", "", `{"input":"hello"}`); code != 201 || m["by"] != "user:root" || m["agent"] != "coding-agent" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ = do(t, srv, "POST", "/v1/agents/triage-agent/runs", "root", "", `{"input":"hello"}`); code != 409 || m["rule"] != "R001" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ = do(t, srv, "POST", "/v1/agents/coding-agent/runs", "root", "", `{"input":"hello","document":{"name":"coding-agent"}}`); code != 501 || m["rule"] != "CT-NOTCARRIED" {
		t.Fatalf("%d %v", code, m)
	}
	// the inbox
	_ = inbox.Add(context.Background(), &dispatch.HumanRequest{ID: "h-000000000001", TaskID: "t-0000a1", Session: "s-0000a1", Agent: "coding-agent", Owner: "user:alice",
		Question: "ok?", Options: []string{"yes", "no"}, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour), Status: "pending"})
	if code, m, raw := do(t, srv, "GET", "/v1/human/requests", "bob", "", ``); code != 200 || raw != "[]\n" {
		t.Fatalf("%d %v %q", code, m, raw)
	}
	if code, _, raw := do(t, srv, "GET", "/v1/human/requests", "alice", "", ``); code != 200 || !strings.Contains(raw, `"h-000000000001"`) {
		t.Fatalf("%d %q", code, raw)
	}
	if code, m, _ := do(t, srv, "POST", "/v1/human/requests/h-000000000001/answer", "alice", "", ``); code != 400 || m["error"] != "the body is not JSON" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ := do(t, srv, "POST", "/v1/human/requests/h-000000000001/answer", "alice", "", `[1]`); code != 400 || m["rule"] != "HR-ANSWER" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ := do(t, srv, "POST", "/v1/human/requests/h-000000000001/answer", "bob", "", `{"answer":"yes"}`); code != 404 || m["rule"] != "HR-UNKNOWN" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ := do(t, srv, "POST", "/v1/human/requests/h-000000000001/answer", "alice", "", `{"answer":"yes"}`); code != 200 || m["status"] != "answered" || m["answer"] != "yes" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ := do(t, srv, "POST", "/v1/human/requests/h-000000000001/answer", "alice", "", `{"answer":"no"}`); code != 409 || m["rule"] != "HR-ANSWERED" {
		t.Fatalf("%d %v", code, m)
	}
	// Preflight
	if code, m, _ := do(t, srv, "GET", "/v1/preflight/cases?agent=coding-agent", "bob", "", ``); code != 200 || m["agent"] != "coding-agent" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ := do(t, srv, "GET", "/v1/preflight/cases", "bob", "", ``); code != 400 || m["rule"] != "R008" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ := do(t, srv, "POST", "/v1/preflight/score", "root", "", `{"agent":"coding-agent","cases":[{"case":"nope","sessions":["s-1"]}]}`); code != 404 || m["rule"] != "R003" {
		t.Fatalf("%d %v", code, m)
	}
	// other methods are not routes
	if code, _, _ := do(t, srv, "GET", "/v1/sessions/s-0000a1/freeze", "alice", "", ``); code != 405 {
		t.Fatalf("%d", code)
	}
	// a Bearer prefix in another case works, as Python's lower()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/human/requests", bytes.NewReader(nil))
	req.Header.Set("Authorization", "bEaReR alice")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	_ = resp.Body.Close()
}
