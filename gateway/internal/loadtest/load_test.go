// Package loadtest measures the gateway's own overhead per tool call: the time a call spends in the gateway itself,
// not waiting on the Clearance core's decision, its outcome or the tool's backend (A-9's bar: p99 under 5 ms at 200
// concurrent sessions).
//
// The calls are real MCP tools/call requests over HTTP to the real handler (bearer check, JSON-RPC, the SDK, dispatch,
// the session store, the spend, the outcome), against a fake core and a fake backend that each wait a fixed time, so
// everything the gateway does beyond waiting is what is measured. Every wait is timed where it happens (the core
// client's transport, the backend handler) and subtracted from the request's wall time, per call, so the overhead
// excludes the decision call exactly. With AUTOPILOT_TEST_PG set the sessions are also saved durably to a real
// Postgres after every call (the group-committed save), and that is part of the overhead.
//
// AUTOPILOT_LOADTEST=1 runs the full load (200 sessions) and asserts the bar (in memory; with a Postgres the numbers are
// logged, since commit latency is the database's); AUTOPILOT_LOADTEST=report runs it and asserts nothing (CI, on shared
// runners); without it a small run checks the harness itself.
package loadtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/auth"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/fakecore"
	"github.com/jfillman/autopilot/gateway/internal/mcpserver"
	"github.com/jfillman/autopilot/gateway/internal/pgstate"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/clearance/client"
)

type timing struct {
	core    atomic.Int64 // ns spent waiting on the core (evaluate and outcome)
	evalNs  atomic.Int64 // ns of that in the decision call
	backend atomic.Int64
}

type ctxKey struct{}

// timedTransport times every request to the core against the call the context belongs to.
type timedTransport struct{ rt http.RoundTripper }

func (t timedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.rt.RoundTrip(r)
	if tm, ok := r.Context().Value(ctxKey{}).(*timing); ok {
		d := time.Since(start).Nanoseconds()
		tm.core.Add(d)
		if r.URL.Path == "/access/v1/evaluation" {
			tm.evalNs.Add(d)
		}
	}
	return resp, err
}

const (
	coreWait    = 2 * time.Millisecond
	backendWait = 1 * time.Millisecond
)

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

// TestGatewayOverheadPerToolCall: with AUTOPILOT_LOADTEST=1, 200 concurrent sessions each calling a tool every
// ~100 ms (about ten times faster than an agent's model turns allow) and the bar asserted; then the same sessions in a
// closed loop with no think time at all, which saturates this machine (the load generator, the fake core and the
// gateway share it) and is reported, not asserted.
func TestGatewayOverheadPerToolCall(t *testing.T) {
	mode := os.Getenv("AUTOPILOT_LOADTEST")
	if mode != "1" && mode != "report" {
		runLoad(t, 20, 10, 5*time.Millisecond, false)
		return
	}
	runLoad(t, 200, 100, 100*time.Millisecond, mode == "1")
	t.Log("--- saturation: no think time (reported, not asserted)")
	runLoad(t, 200, 100, 0, false)
}

func runLoad(t *testing.T, sessions, calls int, think time.Duration, assert bool) {
	t.Helper()

	fc := fakecore.New()
	defer fc.Close()
	fc.Latency = coreWait
	cl, err := client.New(client.Options{BaseURL: fc.URL, Token: client.StaticToken("autopilot-system/autopilot-gateway"), Provider: core.Provider,
		Timeout: 2 * time.Second, BreakerThreshold: -1, Retries: -1,
		HTTPClient: &http.Client{Transport: timedTransport{&http.Transport{MaxIdleConnsPerHost: 256}}}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var persist session.Persistence
	persistence := "none (in memory)"
	if dsn := os.Getenv("AUTOPILOT_TEST_PG"); dsn != "" {
		st, err := pgstate.Open(ctx, dsn+"?application_name=gwload", 16)
		if err != nil {
			t.Skipf("scratch Postgres in use or unreachable: %v", err)
		}
		defer st.Close(context.Background())
		_, _ = st.Pool.Exec(ctx, "DELETE FROM gateway_sessions")
		persist = pgstate.NewSessions(ctx, st)
		persistence = "Postgres, group-committed after every call"
	}
	store, err := session.NewStore(ctx, persist)
	if err != nil {
		t.Fatal(err)
	}
	c := core.New(cl, core.NewMemoryOutbox(), false)
	defs := map[string]*definitions.Definition{"coding-agent": {Name: "coding-agent", Kind: "task", DenyPaths: []string{".github/**"}, ReposAllow: []string{"jfillman/*"}}}
	handler := func(ctx context.Context, _ *session.Session, _ map[string]any) (any, error) {
		start := time.Now()
		time.Sleep(backendWait)
		if tm, ok := ctx.Value(ctxKey{}).(*timing); ok {
			tm.backend.Add(time.Since(start).Nanoseconds())
		}
		return map[string]any{"entities": []any{map[string]any{"kind": "component", "name": "flight-api"}}}, nil
	}
	g, err := dispatch.New(defs, store, c, auth.Static{"tok": {ID: "user:alice", Kind: "user", TierCeiling: 1}}, tools.Default(),
		map[string]dispatch.Handler{"catalog.read": handler})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 0; i < sessions; i++ {
		s := store.Adopt(&client.Grant{Session: fmt.Sprintf("s-%04d", i), TaskID: fmt.Sprintf("t-%04d", i), Principal: "user:alice", Agent: "coding-agent",
			Limits:    client.Limits{TierCeiling: "T1", TTLMinutes: 60, ToolCalls: 1 << 20, GithubCalls: 1 << 20, OpenPRs: 3, Tools: []string{"catalog.read"}, Models: []string{}, Network: "clearance", Compute: "small"},
			ExpiresAt: now.Add(time.Hour)}, now, nil, 0)
		if err := store.Flush(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	mcp := mcpserver.Handler(g, "/mcp", "http://gw.test", "http://dex.test")
	type sample struct{ wall, overhead, overheadExclEvalOnly time.Duration }
	var mu sync.Mutex
	var samples []sample
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			mcp.ServeHTTP(w, r)
			return
		}
		tm := &timing{}
		start := time.Now()
		mcp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, tm)))
		wall := time.Since(start)
		waited := time.Duration(tm.core.Load() + tm.backend.Load())
		mu.Lock()
		samples = append(samples, sample{wall, wall - waited, wall - time.Duration(tm.evalNs.Load()+tm.backend.Load())})
		mu.Unlock()
	}))
	defer srv.Close()

	hc := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: sessions + 8}}
	var wg sync.WaitGroup
	var failures atomic.Int64
	t0 := time.Now()
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(i), 7))
			if think > 0 {
				time.Sleep(time.Duration(rng.Int64N(int64(think)))) // sessions start spread over one think time
			}
			for n := 0; n < calls; n++ {
				if think > 0 && n > 0 {
					time.Sleep(think/2 + time.Duration(rng.Int64N(int64(think)))) // 0.5x to 1.5x
				}
				body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": n + 1, "method": "tools/call", "params": map[string]any{
					"name": "call", "arguments": map[string]any{"session": fmt.Sprintf("s-%04d", i), "tool": "catalog.read", "args": map[string]any{"query": "kind=component"}}}})
				req, _ := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer tok")
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				resp, err := hc.Do(req)
				if err != nil {
					failures.Add(1)
					continue
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 || !bytes.Contains(b, []byte(`\"ok\":true`)) && !bytes.Contains(b, []byte(`"ok":true`)) {
					failures.Add(1)
					if failures.Load() < 3 {
						t.Logf("unexpected answer: %d %.300s", resp.StatusCode, b)
					}
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(t0)
	if failures.Load() != 0 {
		t.Fatalf("%d of %d calls failed", failures.Load(), sessions*calls)
	}
	if len(samples) != sessions*calls {
		t.Fatalf("%d samples for %d calls", len(samples), sessions*calls)
	}
	over := make([]time.Duration, len(samples))
	overEval := make([]time.Duration, len(samples))
	wall := make([]time.Duration, len(samples))
	for i, s := range samples {
		over[i], overEval[i], wall[i] = s.overhead, s.overheadExclEvalOnly, s.wall
	}
	for _, x := range [][]time.Duration{over, overEval, wall} {
		sort.Slice(x, func(i, j int) bool { return x[i] < x[j] })
	}
	// every session's spend landed once per call
	for i := 0; i < sessions; i++ {
		if got := store.Get(fmt.Sprintf("s-%04d", i)).Used.ToolCalls; got != int64(calls) {
			t.Fatalf("session %d spent %d, want %d", i, got, calls)
		}
	}
	t.Logf("%d concurrent sessions x %d calls = %d calls in %v (%.0f calls/s); think time %v; persistence: %s; core wait %v, backend wait %v per call",
		sessions, calls, sessions*calls, elapsed.Round(time.Millisecond), float64(sessions*calls)/elapsed.Seconds(), think, persistence, coreWait, backendWait)
	t.Logf("gateway overhead per call, excluding the decision, the outcome and the backend: p50 %v  p95 %v  p99 %v  max %v",
		percentile(over, .5), percentile(over, .95), percentile(over, .99), over[len(over)-1])
	t.Logf("gateway overhead per call, excluding only the decision call and the backend (the outcome post counts): p50 %v  p99 %v",
		percentile(overEval, .5), percentile(overEval, .99))
	t.Logf("whole call wall time: p50 %v  p99 %v", percentile(wall, .5), percentile(wall, .99))
	if assert && persist == nil && percentile(over, .99) >= 5*time.Millisecond {
		t.Fatalf("p99 overhead %v is not under 5 ms", percentile(over, .99))
	}
}
