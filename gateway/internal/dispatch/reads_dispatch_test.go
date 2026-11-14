package dispatch_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/backends"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/clearance/client"
)

// The S5a read backends under the real dispatch: every allowed call ends in exactly one outcome, an upstream's failure is
// "backend error" to the agent and Python's text on the record, and an artifact is the task's.

func realReads(t *testing.T, cfg backends.Config) *rig {
	t.Helper()
	h, err := backends.NewHandlers(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return newRig(t, h, false)
}

func TestAReadBackendsFailureIsOneOutcomeWithPythonsText(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		_, _ = w.Write([]byte("secret upstream body"))
	}))
	defer up.Close()
	tok := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(tok, []byte("secret-token"), 0o600)
	r := realReads(t, backends.Config{BackstageURL: up.URL, BackstageTokenFile: tok, AppAPIServices: map[string]string{"svc": up.URL}})
	res, _ := r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": "x"})
	if res.OK || res.Error == nil || *res.Error != "backend error" {
		t.Fatalf("%+v", res)
	}
	if r.fc.OutcomeCount() != 1 || r.fc.LastOutcome().Body.Result != "error:ReadError: catalog 502" {
		t.Fatalf("%d outcomes, %q", r.fc.OutcomeCount(), r.fc.LastOutcome().Body.Result)
	}
	// a refusal of the handler's own (an empty query) is one outcome with its rule, and nothing was asked of Backstage
	res, _ = r.g.Call(context.Background(), "t", "s-1", "catalog.read", map[string]any{"query": " "})
	if res.OK || res.Decision["rule"] != "READ-QUERY" || r.fc.OutcomeCount() != 2 || r.fc.LastOutcome().Body.Refusal == nil {
		t.Fatalf("%+v %d", res, r.fc.OutcomeCount())
	}
	// an app API answer that is an error status is data, one outcome ok
	res, _ = r.g.Call(context.Background(), "t", "s-1", "app.api.get", map[string]any{"service": "svc", "path": "/x"})
	if !res.OK || r.fc.OutcomeCount() != 3 || r.fc.LastOutcome().Body.Result != "ok" {
		t.Fatalf("%+v %d", res, r.fc.OutcomeCount())
	}
}

func TestASlowUpstreamEndsTheCallAtItsDeadlineWithOneOutcome(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer func() { close(release); up.Close() }()
	r := realReads(t, backends.Config{AppAPIServices: map[string]string{"svc": up.URL}, Timeout: 30 * time.Second})
	r.g.CallTimeout = 200 * time.Millisecond
	start := time.Now()
	res, _ := r.g.Call(context.Background(), "t", "s-1", "app.api.get", map[string]any{"service": "svc", "path": "/x"})
	if time.Since(start) > 3*time.Second || res.OK || r.fc.OutcomeCount() != 1 {
		t.Fatalf("%+v after %s, %d outcomes", res, time.Since(start), r.fc.OutcomeCount())
	}
	if got := r.fc.LastOutcome().Body.Result; !strings.HasPrefix(got, "error:") {
		t.Fatal(got)
	}
}

// Through the dispatch: a task's sessions share artifacts, and a session of another task reads and replaces none of them.
func TestArtifactsThroughTheDispatchAreTheTasks(t *testing.T) {
	r := realReads(t, backends.Config{Artifacts: &backends.MemoryArtifacts{}})
	now := time.Now().UTC()
	limits := client.Limits{TierCeiling: "T1", TTLMinutes: 30, ToolCalls: 100, GithubCalls: 100, OpenPRs: 3, Tools: []string{"artifact.put", "artifact.get"}, Models: []string{}, Network: "clearance", Compute: "small"}
	parent := "s-1"
	r.store.Adopt(&client.Grant{Session: "s-2", TaskID: "t-1", Principal: "user:alice", Agent: "coding-agent", Limits: limits, Parent: &parent, Depth: 1, ExpiresAt: now.Add(30 * time.Minute)}, now, nil, 0)
	r.store.Adopt(&client.Grant{Session: "s-3", TaskID: "t-9", Principal: "user:alice", Agent: "coding-agent", Limits: limits, ExpiresAt: now.Add(30 * time.Minute)}, now, nil, 0)
	call := func(sid, tool string, a map[string]any) *dispatch.Result {
		res, err := r.g.Call(context.Background(), "t", sid, tool, a)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := call("s-1", "artifact.put", map[string]any{"name": "plan.md", "content": "v1"}); !res.OK {
		t.Fatalf("%+v", res)
	}
	if res := call("s-2", "artifact.get", map[string]any{"name": "plan.md"}); !res.OK || res.Data != "v1" {
		t.Fatalf("a child reads its parent's task: %+v", res)
	}
	if res := call("s-3", "artifact.get", map[string]any{"name": "plan.md"}); !res.OK || res.Data != nil {
		t.Fatalf("another task reads %+v", res)
	}
	if res := call("s-3", "artifact.put", map[string]any{"name": "plan.md", "content": "other"}); !res.OK {
		t.Fatalf("%+v", res)
	}
	if res := call("s-1", "artifact.get", map[string]any{"name": "plan.md"}); res.Data != "v1" {
		t.Fatalf("another task replaced the tree's: %+v", res)
	}
	// a refused name is one refused outcome, and stores nothing
	n := r.fc.OutcomeCount()
	if res := call("s-1", "artifact.put", map[string]any{"name": "../x", "content": "y"}); res.OK || res.Decision["rule"] != "ART-NAME" || r.fc.OutcomeCount() != n+1 {
		t.Fatalf("%+v", res)
	}
}
