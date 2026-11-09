// Package conformance is the gateway's conformance control interface (Clearance's provider protocol,
// docs/provider-protocol.md): what `clearance conformance --provider <url> --role runtime` drives to prove the gateway
// is a conformant runtime provider. The Go side of autopilot.conformance.
//
// TEST ONLY, OFF BY DEFAULT. It is served by its own subcommand, `autopilot-gateway conformance-control`, which refuses
// to start unless GATEWAY_CONFORMANCE_CONTROL=1 is set as well, and binds only a loopback address, on its own port
// (default 127.0.0.1:9091). It never starts the deployable gateway: it reads no GATEWAY_DSN, no core token file, no
// model key, no backend credential.
//
// Routes: GET /conformance/v1/info; POST /conformance/v1/apply (a runtime ApplyRequest, strict, 1 MiB; 202 {"id"});
// GET /conformance/v1/applies/{id} ({id, state, reason, decision_id, spend, outcome {seq, hash}}).
//
// Each apply runs the gateway's own tool-call path (dispatch.Gateway.Call) on a throwaway Gateway: an in-memory session
// store holding the session the evaluation names, a Core on the harness's core URL with the bearer it was handed (the
// client's real outage policy), and one handler, the harness's fake target. The only thing the harness controls is the
// evaluation itself. Everything after the answer is production code. The spend the status reports is measured on the
// session afterwards, not echoed from the core's answer.
package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/auth"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/clearance/client"
)

const (
	EnableEnv     = "GATEWAY_CONFORMANCE_CONTROL"
	DefaultListen = "127.0.0.1:9091"
	MaxBody       = 1 << 20
	MaxApplies    = 256
	TargetTimeout = 5 * time.Second

	callerToken = "conformance-caller"
)

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Endpoint is a URL and a bearer.
type Endpoint struct{ URL, Bearer string }

// Apply is a decoded runtime ApplyRequest.
type Apply struct {
	ID         string
	Core       Endpoint
	Target     Endpoint
	Evaluation client.EvaluationRequest
	Raw        json.RawMessage
}

func endpoint(name string, raw json.RawMessage) (Endpoint, error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return Endpoint{}, fmt.Errorf("%s is {url, bearer}", name)
	}
	for k := range m {
		if k != "url" && k != "bearer" {
			return Endpoint{}, fmt.Errorf("%s is {url, bearer}", name)
		}
	}
	var e Endpoint
	var u, b string
	if json.Unmarshal(m["url"], &u) != nil || len(u) > 2048 {
		return e, fmt.Errorf("%s.url is an http(s) URL", name)
	}
	if v, ok := m["bearer"]; ok && json.Unmarshal(v, &b) != nil {
		return e, errors.New("a bearer is a string of at most 4096 bytes")
	}
	if len(b) > 4096 {
		return e, errors.New("a bearer is a string of at most 4096 bytes")
	}
	pu, err := url.Parse(u)
	if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" || pu.User != nil || pu.RawQuery != "" || pu.Fragment != "" {
		return e, fmt.Errorf("%s.url is an http(s) URL without credentials, query or fragment", name)
	}
	return Endpoint{URL: strings.TrimRight(u, "/"), Bearer: b}, nil
}

// Decode is conformance.DecodeApply for the runtime role: strictly decoded, at most MaxBody bytes.
func Decode(raw []byte) (*Apply, error) {
	if len(raw) > MaxBody {
		return nil, errors.New("the body is over its cap")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("the body is not a JSON object: %v", err)
	}
	allowed := map[string]bool{"id": true, "role": true, "provider": true, "core": true, "target": true, "poll_interval_ms": true,
		"evaluation": true, "change": true, "manifest": true}
	var unknown []string
	for k := range m {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown fields %v", unknown)
	}
	var a Apply
	var id, role, provider string
	if json.Unmarshal(m["id"], &id) != nil || !idRe.MatchString(id) {
		return nil, errors.New("id is 1 to 64 lowercase letters, digits and dashes")
	}
	if json.Unmarshal(m["role"], &role) != nil || role != "runtime" {
		return nil, errors.New("this provider is tested in the runtime role only")
	}
	if json.Unmarshal(m["provider"], &provider) != nil || provider != core.Provider {
		return nil, fmt.Errorf("provider is %q, the name GET /conformance/v1/info gave", core.Provider)
	}
	if v, ok := m["poll_interval_ms"]; ok {
		var p int
		if json.Unmarshal(v, &p) != nil || p < 0 || p > 60000 {
			return nil, errors.New("poll_interval_ms is 0 to 60000")
		}
	}
	var manifest string
	if v, ok := m["manifest"]; ok && string(v) != "null" && (json.Unmarshal(v, &manifest) != nil || manifest != "") {
		return nil, errors.New("a runtime apply carries evaluation (an object) and no change or manifest")
	}
	if v, ok := m["change"]; ok && string(v) != "null" {
		return nil, errors.New("a runtime apply carries evaluation (an object) and no change or manifest")
	}
	ev := m["evaluation"]
	var parts map[string]json.RawMessage
	if json.Unmarshal(ev, &parts) != nil {
		return nil, errors.New("a runtime apply carries evaluation (an object) and no change or manifest")
	}
	for _, k := range []string{"subject", "action", "resource", "context"} {
		var obj map[string]json.RawMessage
		if json.Unmarshal(parts[k], &obj) != nil {
			return nil, fmt.Errorf("evaluation.%s is an object", k)
		}
	}
	if err := json.Unmarshal(ev, &a.Evaluation); err != nil {
		return nil, fmt.Errorf("evaluation: %v", err)
	}
	if a.Evaluation.Action.Name != "tool.call" {
		return nil, errors.New("the runtime apply is a tool.call evaluation")
	}
	if a.Evaluation.Resource.ID == "" || a.Evaluation.Subject.ID == "" {
		return nil, errors.New("evaluation names its subject and its tool")
	}
	var err error
	if a.Core, err = endpoint("core", m["core"]); err != nil {
		return nil, err
	}
	if a.Target, err = endpoint("target", m["target"]); err != nil {
		return nil, err
	}
	a.ID, a.Raw = id, ev
	return &a, nil
}

// States an apply ends in.
const (
	StateAccepted = "accepted"
	StateApplied  = "applied"
	StateRefused  = "refused"
	StateFailed   = "failed"
)

func targetHandler(applyID string, t Endpoint) dispatch.Handler {
	hc := &http.Client{Timeout: TargetTimeout}
	return func(ctx context.Context, _ *session.Session, args map[string]any) (any, error) {
		body, _ := json.Marshal(map[string]any{"id": applyID, "content": string(core.Canonical(args))})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL+"/v1/apply", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/json")
		if t.Bearer != "" {
			req.Header.Set("authorization", "Bearer "+t.Bearer)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("the target answered %d", resp.StatusCode)
		}
		var out map[string]any
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
			return nil, errors.New("the target answered no receipt")
		}
		return map[string]any{"seq": out["seq"], "sha256": out["sha256"]}, nil
	}
}

// sessionOf is the session the evaluation names, as the gateway would hold it: adopted with its limits and expiry, its
// used and reserved budgets, denials and breaker. nil when the evaluation names no session (the core denies it).
func sessionOf(st *session.Store, ctx client.Context, now time.Time) *session.Session {
	s := ctx.Session
	if s == nil {
		return nil
	}
	g := &client.Grant{Session: s.ID, TaskID: s.TaskID, Principal: s.Principal, Agent: s.Agent, Parent: s.ParentID, Depth: s.Depth, Limits: s.Limits, ExpiresAt: s.ExpiresAt}
	sess := st.Adopt(g, now, nil, 0)
	sess.Used, sess.Reserved, sess.Denials = s.Used, s.Reserved, s.Denials
	if s.Tripped {
		sess.Trip("tripped")
	}
	return sess
}

// appliedSpend is what was applied to the session, measured: the counters' growth, a denial counted, a breaker tripped.
func appliedSpend(before snapshot, s *session.Session) map[string]any {
	out := map[string]any{}
	s.Lock()
	defer s.Unlock()
	for _, f := range []string{"tool_calls", "github_calls", "open_prs", "children"} {
		if d := session.Budget(&s.Used, f) - session.Budget(&before.used, f); d != 0 {
			out[f] = d
		}
	}
	reserve := map[string]int64{}
	for _, f := range session.BudgetFields {
		if d := session.Budget(&s.Reserved, f) - session.Budget(&before.reserved, f); d != 0 {
			reserve[f] = d
		}
	}
	if len(reserve) > 0 {
		out["reserve"] = reserve
	}
	if s.Denials > before.denials {
		out["denial"] = true
	}
	if s.Tripped && !before.tripped {
		reason := "tripped"
		if s.TripReason != nil {
			reason = *s.TripReason
		}
		out["trip"] = reason
	}
	return out
}

type snapshot struct {
	used, reserved client.Budgets
	denials        int
	tripped        bool
}

// Run is one apply, start to finish: the status it ends in.
func Run(ctx context.Context, a *Apply, reg *tools.Registry, now func() time.Time) map[string]any {
	status := map[string]any{"id": a.ID, "state": StateAccepted}
	ev := a.Evaluation
	tool := ev.Resource.ID
	var args map[string]any
	if len(ev.Action.Properties.Args) > 0 {
		d := json.NewDecoder(bytes.NewReader(ev.Action.Properties.Args))
		d.UseNumber()
		_ = d.Decode(&args)
	}
	principal := &dispatch.Principal{ID: ev.Subject.ID, Kind: ev.Subject.Properties.Kind, TierCeiling: ev.Subject.Properties.TierCeiling}
	if principal.Kind == "" {
		principal.Kind = "user"
	}
	cl, err := client.New(client.Options{BaseURL: a.Core.URL, Token: client.StaticToken(a.Core.Bearer), Provider: core.Provider})
	if err != nil {
		status["state"], status["reason"] = StateFailed, "the gateway cannot hold this call: "+err.Error()
		return status
	}
	c := core.New(cl, core.NewMemoryOutbox(), false)
	var mu sync.Mutex
	var decisionID string
	var outcomeRef *client.RecordRef
	c.OnDecision = func(id string) { mu.Lock(); decisionID = id; mu.Unlock() }
	c.OnPosted = func(e core.Entry, ref client.RecordRef) {
		if e.Op == "outcome" {
			mu.Lock()
			r := ref
			outcomeRef = &r
			mu.Unlock()
		}
	}
	st, _ := session.NewStore(ctx, nil)
	s := sessionOf(st, ev.Context, now())
	g, err := dispatch.New(map[string]*definitions.Definition{}, st, c, auth.Static{callerToken: principal}, reg, map[string]dispatch.Handler{tool: targetHandler(a.ID, a.Target)})
	if err != nil {
		status["state"], status["reason"] = StateFailed, "the gateway cannot hold this call: "+err.Error()
		return status
	}
	g.Now = now
	g.BuildRequest = func(*dispatch.Principal, string, *session.Session, string, map[string]any, time.Time) (*client.EvaluationRequest, error) {
		var req client.EvaluationRequest
		if err := json.Unmarshal(a.Raw, &req); err != nil {
			return nil, err
		}
		return &req, nil
	}
	var before snapshot
	if s != nil {
		s.Lock()
		before = snapshot{s.Used, s.Reserved, s.Denials, s.Tripped}
		s.Unlock()
	}
	r, err := g.Call(ctx, callerToken, ev.Context.SessionID, tool, args)
	mu.Lock()
	defer mu.Unlock()
	if decisionID != "" {
		status["decision_id"] = decisionID
	}
	if s != nil {
		status["spend"] = appliedSpend(before, s)
	} else {
		status["spend"] = map[string]any{}
	}
	if err != nil {
		status["state"], status["reason"] = StateFailed, "the gateway failed: "+err.Error()
		return status
	}
	rule, _ := r.Decision["rule"].(string)
	switch allow, _ := r.Decision["allow"].(bool); {
	case !allow && (rule == core.OutageRule || rule == core.RefusedRule):
		status["state"], status["reason"] = StateRefused, fmt.Sprintf("core-unavailable: failed closed (%s): %s", rule, deref(r.Error))
	case !allow:
		msg, _ := r.Decision["message"].(string)
		if msg == "" {
			msg = deref(r.Error)
		}
		status["state"], status["reason"] = StateRefused, fmt.Sprintf("denied %s: %s", rule, msg)
	case r.OK:
		status["state"] = StateApplied
	default:
		status["state"], status["reason"] = StateFailed, "the target refused: "+deref(r.Error)
	}
	if outcomeRef != nil {
		status["outcome"] = map[string]any{"seq": outcomeRef.Seq, "hash": outcomeRef.Hash}
	} else if allow, _ := r.Decision["allow"].(bool); allow {
		reason, _ := status["reason"].(string)
		status["reason"] = strings.TrimSpace(reason + " (outcome not recorded yet: it waits in the outbox)")
	}
	return status
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Control is the three routes, over a table of applies run in the background.
type Control struct {
	Version string
	Tools   *tools.Registry
	Now     func() time.Time
	mu      sync.Mutex
	applies map[string]map[string]any
}

// NewControl returns a control interface.
func NewControl(version string, reg *tools.Registry) *Control {
	return &Control{Version: version, Tools: reg, Now: func() time.Time { return time.Now().UTC() }, applies: map[string]map[string]any{}}
}

// Handler serves the routes.
func (c *Control) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /conformance/v1/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"name": core.Provider, "version": c.Version, "roles": []string{"runtime"}})
	})
	mux.HandleFunc("POST /conformance/v1/apply", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
		if err != nil || len(raw) > MaxBody {
			writeJSON(w, 413, map[string]any{"error": "the body is over its cap"})
			return
		}
		a, err := Decode(raw)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		c.mu.Lock()
		if _, dup := c.applies[a.ID]; dup {
			c.mu.Unlock()
			writeJSON(w, 409, map[string]any{"error": "apply " + a.ID + " exists"})
			return
		}
		if len(c.applies) >= MaxApplies {
			c.mu.Unlock()
			writeJSON(w, 429, map[string]any{"error": "too many applies: restart the control interface"})
			return
		}
		c.applies[a.ID] = map[string]any{"id": a.ID, "state": StateAccepted}
		c.mu.Unlock()
		go func() {
			st := Run(context.Background(), a, c.Tools, c.Now)
			c.mu.Lock()
			c.applies[a.ID] = st
			c.mu.Unlock()
		}()
		writeJSON(w, 202, map[string]any{"id": a.ID})
	})
	mux.HandleFunc("GET /conformance/v1/applies/{id}", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		st, ok := c.applies[r.PathValue("id")]
		c.mu.Unlock()
		if !ok {
			writeJSON(w, 404, map[string]any{"error": "no such apply"})
			return
		}
		writeJSON(w, 200, st)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ListenAddress parses host:port and requires a loopback host: the control interface is never reachable off the
// machine (or pod).
func ListenAddress(v string) (string, error) {
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return "", fmt.Errorf("--listen is host:port, not %q", v)
	}
	if n, err := net.LookupPort("tcp", port); err != nil || n == 0 {
		return "", fmt.Errorf("--listen is host:port, not %q", v)
	}
	ip := net.ParseIP(host)
	if !(ip != nil && ip.IsLoopback()) && host != "localhost" {
		return "", fmt.Errorf("the conformance control interface binds a loopback address only, not %q", host)
	}
	return v, nil
}
