package mcpserver_test

import (
	"context"
	"net/http/httptest"
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
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// run_finish and session_close through the MCP server, as a run pod and as a person call them: the Python names, arguments
// and result shapes ({"ok": true, ...} or {"ok": false, "rule", "error"}).
func runsSetup(t *testing.T) (*httptest.Server, *session.Store, *runs.FakeStore) {
	fc := fakecore.New()
	t.Cleanup(fc.Close)
	now := time.Now().UTC()
	l := client.Limits{TierCeiling: "T1", TTLMinutes: 30, ToolCalls: 10, Tools: []string{"catalog.read"}, Models: []string{}, Network: "clearance", Compute: "small"}
	st, _ := session.NewStore(context.Background(), nil)
	st.Adopt(&client.Grant{Session: "s-0000a1", TaskID: "t-0000a1", Principal: "user:alice", Agent: "coding-agent", Limits: l, ExpiresAt: now.Add(time.Hour)}, now, nil, 0)
	run := "r-0000a1"
	st.Get("s-0000a1").Run = &run
	c := core.New(fc.Client(time.Second), core.NewMemoryOutbox(), false)
	image := "ghcr.io/x/y@sha256:" + "a"
	defs := map[string]*definitions.Definition{"coding-agent": {Name: "coding-agent", Kind: "task", IdentityType: "delegated", Limits: l, Image: &image}}
	au := auth.Static{"run": {ID: "agent-r-0000a1/run", Kind: "workload", TierCeiling: 1}, "alice": {ID: "user:alice", Kind: "user", TierCeiling: 1},
		"bob": {ID: "user:bob", Kind: "user", TierCeiling: 1}}
	g, err := dispatch.New(defs, st, c, au, tools.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	api := runs.NewFakeStore()
	g.SetRuns(&runs.Backend{Store: api})
	g.Results = &dispatch.MemoryResults{}
	srv := httptest.NewServer(mcpserver.Handler(g, "/mcp", "http://gw.test", "http://dex.test"))
	t.Cleanup(srv.Close)
	return srv, st, api
}

func TestRunFinishThroughMCP(t *testing.T) {
	srv, st, _ := runsSetup(t)
	for _, who := range []string{"bob", "alice"} { // another person, and the session's own person: only the run (or its principal) once
		cs, err := connect(t, srv.URL, who)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		if who == "bob" {
			r, _ := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_finish", Arguments: map[string]any{"session": "s-0000a1", "status": "done"}})
			if m := structured(t, r); m["ok"] != false || m["rule"] != "R001" {
				t.Fatalf("%v", m)
			}
		}
	}
	cs, err := connect(t, srv.URL, "run")
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, _ := cs.ListTools(context.Background(), nil)
	names := map[string]bool{}
	for _, tl := range list.Tools {
		names[tl.Name] = true
	}
	if !names["run_finish"] || !names["session_close"] {
		t.Fatalf("%v", names)
	}
	r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_finish", Arguments: map[string]any{
		"session": "s-0000a1", "status": "done", "final_text": "ok", "turns": 1,
		"tool_calls": []any{map[string]any{"tool": "catalog.read", "ok": true, "rule": nil}}}})
	m := structured(t, r)
	if err != nil || r.IsError || m["ok"] != true || m["closed"] != true || m["status"] != "done" || m["session"] != "s-0000a1" || m["kept"] == nil {
		t.Fatalf("%v %+v", err, m)
	}
	if !st.Get("s-0000a1").Closed {
		t.Fatal("the session is closed")
	}
	r, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_finish", Arguments: map[string]any{"session": "s-0000a1", "status": "done"}})
	if m := structured(t, r); m["ok"] != false || m["rule"] != "R004" {
		t.Fatalf("finishing twice: %v", m)
	}
}

func TestSessionCloseThroughMCP(t *testing.T) {
	srv, st, api := runsSetup(t)
	_ = api.Create(context.Background(), map[string]any{"metadata": map[string]any{"name": "r-0000a1"}})
	bob, _ := connect(t, srv.URL, "bob")
	defer bob.Close()
	r, _ := bob.CallTool(context.Background(), &mcp.CallToolParams{Name: "session_close", Arguments: map[string]any{"session": "s-0000a1"}})
	if m := structured(t, r); m["ok"] != false || m["rule"] != "CT-FORBIDDEN" || m["error"] != "no such session of yours" {
		t.Fatalf("%v", m)
	}
	alice, _ := connect(t, srv.URL, "alice")
	defer alice.Close()
	r, _ = alice.CallTool(context.Background(), &mcp.CallToolParams{Name: "session_close", Arguments: map[string]any{"session": "s-0000a1", "reason": "done here"}})
	m := structured(t, r)
	if m["ok"] != true || m["closed"] != true || m["agent"] != "coding-agent" || len(m["runs_deleted"].([]any)) != 1 || !st.Get("s-0000a1").Closed || len(api.Deleted) != 1 {
		t.Fatalf("%v", m)
	}
}
