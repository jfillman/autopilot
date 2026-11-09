package modelproxy_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/auth"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/fakecore"
	"github.com/jfillman/autopilot/gateway/internal/modelproxy"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/clearance/client"
)

type rig struct {
	fc       *fakecore.Core
	store    *session.Store
	s        *session.Session
	proxy    *httptest.Server
	upstream *httptest.Server
	seen     chan *http.Request
	bodies   chan string
}

func newRig(t *testing.T, upstream http.HandlerFunc) *rig {
	t.Helper()
	fc := fakecore.New()
	t.Cleanup(fc.Close)
	r := &rig{fc: fc, seen: make(chan *http.Request, 8), bodies: make(chan string, 8)}
	r.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		b, _ := io.ReadAll(q.Body)
		r.bodies <- string(b)
		r.seen <- q
		upstream(w, q)
	}))
	t.Cleanup(r.upstream.Close)
	fc.Decide = func(req *client.EvaluationRequest) client.EvaluationResult {
		id := "d-model"
		return client.EvaluationResult{Decision: true, DecisionID: &id, Record: &client.RecordRef{Seq: 1}, Context: client.DecisionContext{Message: "allowed"}}
	}
	r.store, _ = session.NewStore(context.Background(), nil)
	now := time.Now().UTC()
	r.s = r.store.Adopt(&client.Grant{Session: "s-1", TaskID: "t-1", Principal: "autopilot-runs/x", Agent: "a",
		Limits: client.Limits{ModelTokens: 1000, Tools: []string{}, Models: []string{"claude"}}, ExpiresAt: now.Add(time.Hour)}, now, nil, 0)
	c := core.New(fc.Client(time.Second), core.NewMemoryOutbox(), false)
	g, err := dispatch.New(nil, r.store, c, auth.Static{}, tools.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := &modelproxy.Models{Routes: map[string]*modelproxy.Route{"claude": {Alias: "claude", Backend: "anthropic", Upstream: r.upstream.URL, UpstreamModel: "claude-up-1"}},
		G: g, Auth: auth.Static{"tok": {ID: "autopilot-runs/x", Kind: "workload", TierCeiling: 1}}}
	key := filepath.Join(t.TempDir(), "key")
	_ = os.WriteFile(key, []byte("sk-test\n"), 0o600)
	r.proxy = httptest.NewServer(modelproxy.NewProxy(m, key, 10*time.Second).Handler())
	t.Cleanup(r.proxy.Close)
	return r
}

func (r *rig) post(ctx context.Context, body string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", r.proxy.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "tok")
	req.Header.Set("x-hangar-session", "s-1")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20, prompt-caching-2024-07-31")
	return http.DefaultClient.Do(req)
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A stream is relayed as it comes: the first event reaches the client while the provider is still producing the rest.
// The usage the events carry is charged when the stream ends.
func TestAStreamIsRelayedAsItComesAndItsUsageCharged(t *testing.T) {
	release := make(chan struct{})
	r := newRig(t, func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-up-1-20261101\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":100,\"output_tokens\":1}}}\n\n")
		fl.Flush()
		<-release
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":20}}\n\n")
		fl.Flush()
	})
	resp, err := r.post(context.Background(), `{"model":"claude","max_tokens":50,"stream":true,"messages":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("content-type") != "text/event-stream" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("content-type"))
	}
	br := bufio.NewReader(resp.Body)
	first, err := br.ReadString('\n') // arrives before the upstream was released: nothing buffers the body
	if err != nil || !strings.HasPrefix(first, "event: message_start") {
		t.Fatalf("%q %v", first, err)
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "message_delta") {
		t.Fatalf("%q", rest)
	}
	up := <-r.seen
	if up.Header.Get("x-api-key") != "sk-test" || up.Header.Get("anthropic-beta") != "prompt-caching-2024-07-31" {
		t.Fatalf("headers: key=%v beta=%q", up.Header.Get("x-api-key") == "sk-test", up.Header.Get("anthropic-beta"))
	}
	waitFor(t, "the outcome", func() bool { return r.fc.OutcomeCount() == 1 })
	o := r.fc.LastOutcome().Body
	// 10 input + 20 output + 100 cache reads at 0.1 = 40
	if o.Result != "ok" || o.Usage == nil || *o.Usage.ChargedTokens != 40 || o.ModelVersion == nil || *o.ModelVersion != "claude-up-1-20261101" ||
		o.Receipt["stream"] != true {
		t.Fatalf("%+v usage=%+v", o, o.Usage)
	}
	r.s.Lock()
	defer r.s.Unlock()
	if r.s.Used.ModelTokens != 40 || r.s.Usage["model_calls"] != 1 || r.s.Usage["output_tokens"] != 20 {
		t.Fatalf("%+v %+v", r.s.Used, r.s.Usage)
	}
}

func TestAStreamTheRunAbandonsIsRecordedAsInterruptedAndWhatWasMeteredIsCharged(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	r := newRig(t, func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7}}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-hold:
		case <-q.Context().Done():
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := r.post(ctx, `{"model":"claude","max_tokens":5,"stream":true}`)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = bufio.NewReader(resp.Body).ReadString('\n')
	cancel()
	resp.Body.Close()
	waitFor(t, "the outcome", func() bool { return r.fc.OutcomeCount() == 1 })
	if o := r.fc.LastOutcome().Body; o.Result != "error:StreamInterrupted" || o.Usage == nil || *o.Usage.ChargedTokens != 7 {
		t.Fatalf("%+v %+v", o, o.Usage)
	}
}

func TestAPlainCallPassesThroughWithItsStatusAndIsCharged(t *testing.T) {
	r := newRig(t, func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"id":"m","model":"claude-up-1","usage":{"input_tokens":5,"output_tokens":6}}`)
	})
	resp, err := r.post(context.Background(), `{"model":"claude","max_tokens":9,"messages":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"id":"m"`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	up := <-r.seen
	_ = up
	body := <-r.bodies
	if !strings.Contains(body, `"model":"claude-up-1"`) {
		t.Fatalf("the upstream model was not substituted: %s", body)
	}
	waitFor(t, "the outcome", func() bool { return r.fc.OutcomeCount() == 1 })
	if o := r.fc.LastOutcome().Body; o.Result != "ok" || *o.Usage.ChargedTokens != 11 || o.Receipt["stream"] != false || o.Receipt["upstream_status"] != float64(200) {
		t.Fatalf("%+v %+v", o, o.Receipt)
	}
}

func TestAnUpstreamErrorIsRelayedAndRecordedAsSuch(t *testing.T) {
	r := newRig(t, func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(529)
		fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
	})
	resp, _ := r.post(context.Background(), `{"model":"claude","max_tokens":9}`)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 529 || !strings.Contains(string(b), "overloaded_error") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	waitFor(t, "the outcome", func() bool { return r.fc.OutcomeCount() == 1 })
	if got := r.fc.LastOutcome().Body.Result; got != "error:Upstream529" {
		t.Fatal(got)
	}
}

func TestRefusalsAreAnthropicShapedWithTheClearanceRuleAndNeverReachTheProvider(t *testing.T) {
	r := newRig(t, func(http.ResponseWriter, *http.Request) { t.Error("the provider was called") })
	for _, c := range []struct {
		rule   string
		status int
	}{{"M005", 403}, {"M007", 429}} {
		r.fc.Decide = func(*client.EvaluationRequest) client.EvaluationResult {
			return fakecore.Deny(c.rule, "refused", client.Spend{})
		}
		resp, _ := r.post(context.Background(), `{"model":"claude","max_tokens":9}`)
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != c.status || !strings.Contains(string(b), `"rule":"`+c.rule+`"`) {
			t.Fatalf("%s: %d %s", c.rule, resp.StatusCode, b)
		}
	}
	r.fc.Down.Store(true)
	resp, _ := r.post(context.Background(), `{"model":"claude","max_tokens":9}`)
	if resp.StatusCode != 503 {
		t.Fatalf("an unreachable core must fail closed: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", r.proxy.URL+"/v1/messages", strings.NewReader(`{"model":"claude"}`))
	req.Header.Set("x-api-key", "wrong")
	req.Header.Set("x-hangar-session", "s-1")
	r.fc.Down.Store(false)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("a bad token: %d", resp.StatusCode)
	}
	if len(r.seen) != 0 {
		t.Fatal("the provider saw a refused call")
	}
}

func TestChargedTokensAndAnOverrunTripsTheBreakerWithoutOverdrawing(t *testing.T) {
	if got := modelproxy.ChargedTokens(map[string]int64{"input_tokens": 3, "output_tokens": 1, "cache_creation_input_tokens": 4, "cache_read_input_tokens": 5}); got != 10 {
		t.Fatalf("%d", got) // 3 + 1 + 4*1.25 + 5*0.1 = 9.5, rounded up
	}
	r := newRig(t, func(http.ResponseWriter, *http.Request) {})
	m := &modelproxy.Models{G: &dispatch.Gateway{Store: r.store}}
	m.RecordUsage("s-1", 5000, map[string]int64{"charged_tokens": 5000})
	r.s.Lock()
	defer r.s.Unlock()
	if r.s.Used.ModelTokens != 1000 || !r.s.Tripped || *r.s.TripReason != "model-token overrun" || r.s.Usage["charged_tokens"] != 5000 {
		t.Fatalf("%+v tripped=%v %+v", r.s.Used, r.s.Tripped, r.s.Usage)
	}
}
