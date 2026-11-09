package mcpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/auth"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/fakecore"
	"github.com/jfillman/autopilot/gateway/internal/mcpserver"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/clearance/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func setup(t *testing.T) (*httptest.Server, *fakecore.Core) {
	fc := fakecore.New()
	t.Cleanup(fc.Close)
	st, _ := session.NewStore(context.Background(), nil)
	c := core.New(fc.Client(time.Second), core.NewMemoryOutbox(), false)
	defs := map[string]*definitions.Definition{"coding-agent": {Name: "coding-agent", Kind: "task", IdentityType: "delegated", Description: "codes",
		Limits: client.Limits{TierCeiling: "T1", Tools: []string{"catalog.read"}, Models: []string{}}}}
	g, err := dispatch.New(defs, st, c, auth.Static{"good": {ID: "user:alice", Kind: "user", TierCeiling: 1}}, tools.Default(),
		map[string]dispatch.Handler{"catalog.read": func(context.Context, *session.Session, map[string]any) (any, error) {
			return map[string]any{"entities": []any{}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	fc.Decide = func(req *client.EvaluationRequest) client.EvaluationResult {
		id := "d-1"
		res := client.EvaluationResult{Decision: true, DecisionID: &id, Record: &client.RecordRef{Seq: 3}, Context: client.DecisionContext{Message: "allowed"}}
		if req.Action.Name == "session.open" {
			res.Context.Grant = &client.Grant{Session: req.Context.NewSessionID, TaskID: "t-1", Principal: "user:alice", Agent: "coding-agent",
				Limits: client.Limits{TierCeiling: "T1", Tools: []string{"catalog.read"}, Models: []string{}}, ExpiresAt: time.Now().Add(time.Hour)}
		} else {
			res.Context.Spend.ToolCalls = 1
		}
		return res
	}
	srv := httptest.NewServer(mcpserver.Handler(g, "/mcp", "http://gw.test", "http://dex.test"))
	t.Cleanup(srv.Close)
	return srv, fc
}

func connect(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{token}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
}

func structured(t *testing.T, r *mcp.CallToolResult) map[string]any {
	t.Helper()
	b, _ := json.Marshal(r.StructuredContent)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func TestAnUnauthenticatedCallIs401BeforeAnyToolRuns(t *testing.T) {
	srv, fc := setup(t)
	for _, tok := range []string{"", "bad"} {
		resp, err := (&http.Client{Transport: bearer{tok}}).Post(srv.URL+"/mcp", "application/json", nil)
		if err != nil || resp.StatusCode != 401 {
			t.Fatalf("%q: %v %v", tok, resp, err)
		}
	}
	if fc.Evals.Load() != 0 {
		t.Fatal("the core was asked")
	}
}

func TestTheToolsTheGatewayOffersAndAFullCallThroughMCP(t *testing.T) {
	srv, fc := setup(t)
	cs, err := connect(t, srv.URL, "good")
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range list.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"tools_list", "agents_list", "session_open", "call"} {
		if !names[want] {
			t.Errorf("no %s tool: %v", want, names)
		}
	}

	r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "agents_list"})
	if err != nil || r.IsError {
		t.Fatalf("%v %+v", err, r)
	}
	if got := structured(t, r)["result"].([]any); len(got) != 1 || got[0].(map[string]any)["name"] != "coding-agent" || got[0].(map[string]any)["can_open_session"] != true {
		t.Fatalf("%v", got)
	}

	r, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "session_open", Arguments: map[string]any{"agent": "coding-agent"}})
	open := structured(t, r)
	if r.IsError || open["ok"] != true || open["session"] == nil {
		t.Fatalf("%+v %v", r, open)
	}
	sid := open["session"].(string)

	r, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "call", Arguments: map[string]any{"session": sid, "tool": "catalog.read", "args": map[string]any{"query": "x"}}})
	call := structured(t, r)
	if r.IsError || call["ok"] != true || call["audit_seq"] == nil || fc.OutcomeCount() != 1 {
		t.Fatalf("%+v %v outcomes=%d", r, call, fc.OutcomeCount())
	}

	r, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "tools_list", Arguments: map[string]any{"session": sid}})
	if got := structured(t, r)["result"].([]any); len(got) != 1 || got[0].(map[string]any)["name"] != "catalog.read" {
		t.Fatalf("tools_list(session) is the session's granted tools that this deployment runs: %v", got)
	}
	r, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "tools_list", Arguments: map[string]any{"session": "s-someone-elses"}})
	if !r.IsError {
		t.Fatal("another's session must be refused")
	}
	if all, _ := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "tools_list"}); len(structured(t, all)["result"].([]any)) != tools.Default().Len() {
		t.Fatal("the whole registry is offered")
	}
}

func TestHealthzIsOpenAndReportsTheCoreBreaker(t *testing.T) {
	srv, _ := setup(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	if m["ok"] != true || m["core"] == nil {
		t.Fatalf("%v", m)
	}
}
