// Package modelproxy is the gateway's model access: the decision (the core's model.call, charged to the session's
// budget) and the proxy in front of the provider, a streaming reverse proxy that never buffers a response body.
// Runs hold no model API key; they call this proxy with their model-proxy token.
package modelproxy

import (
	"context"
	"errors"
	"math"
	"sort"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/clearance/client"
)

// UsageWeights is what a call's usage costs the session's model_tokens budget, per kind of token, relative to an
// uncached input token (Anthropic: a cache write is 1.25x, a cache read 0.1x; output is counted at 1, because the
// budget bounds work done, not money).
var UsageWeights = []struct {
	Kind   string
	Weight float64
}{{"input_tokens", 1.0}, {"output_tokens", 1.0}, {"cache_creation_input_tokens", 1.25}, {"cache_read_input_tokens", 0.1}}

// ChargedTokens is the budget charge for one call's usage (Anthropic's shape), rounded up.
func ChargedTokens(u map[string]int64) int64 {
	total := 0.0
	for _, w := range UsageWeights {
		if v := u[w.Kind]; v > 0 {
			total += float64(v) * w.Weight
		}
	}
	return int64(math.Ceil(total))
}

// UsageRecord is one call's usage as its outcome record carries it: each kind a non-negative integer (0 when the
// provider left it out) and charged_tokens, the call's weighted charge.
func UsageRecord(usage map[string]any) map[string]int64 {
	out := map[string]int64{}
	for _, w := range UsageWeights {
		out[w.Kind] = nonNegInt(usage[w.Kind])
	}
	out["charged_tokens"] = ChargedTokens(out)
	return out
}

func nonNegInt(v any) int64 {
	switch x := v.(type) {
	case float64:
		if x > 0 {
			return int64(x)
		}
	case int64:
		if x > 0 {
			return x
		}
	case interface{ Int64() (int64, error) }: // json.Number
		if n, err := x.Int64(); err == nil && n > 0 {
			return n
		}
		if f, ok := v.(interface{ Float64() (float64, error) }); ok {
			if g, err := f.Float64(); err == nil && g > 0 {
				return int64(g)
			}
		}
	}
	return 0
}

// Route is where a model alias goes.
type Route struct {
	Alias         string
	Backend       string // "anthropic" | "openai-compatible" | "modelplane"
	Upstream      string
	UpstreamModel string
}

// Decision is a model call's decision.
type Decision struct {
	Allow      bool
	Rule       string
	Message    string
	Route      *Route
	Headers    map[string]string
	DecisionID string
}

// Models decides model calls on a gateway's store and core.
type Models struct {
	Routes map[string]*Route
	G      *dispatch.Gateway
	Auth   dispatch.Authenticator // verifies the run's model-proxy token (its own audience)
}

// Request builds the core's evaluation for a model call (modelproxy.ModelProxy.authorize).
func (m *Models) Request(principalID string, s *session.Session, sessionID, model string, est int64, acting string) *client.EvaluationRequest {
	routes := make([]string, 0, len(m.Routes))
	for r := range m.Routes {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	props := client.SubjectProperties{Kind: "workload", TierCeiling: 1}
	if acting != "" && acting != principalID {
		props.ActingAs = acting
	}
	req := &client.EvaluationRequest{
		Subject:  client.Subject{Type: "principal", ID: principalID, Properties: props},
		Action:   client.Action{Name: "model.call", Properties: client.ActionProperties{EstTokens: &est}},
		Resource: client.Resource{Type: "model", ID: model},
		Context:  client.Context{Now: m.G.Now(), SessionID: sessionID, Models: &client.Models{Routes: routes}},
	}
	if s != nil {
		s.Lock()
		req.Context.Session = s.State()
		s.Unlock()
	}
	return req
}

// Authorize authenticates a run's model-proxy token and decides one model call. An unreachable core is a deny
// (CORE-UNAVAILABLE): a model call is never an outage read. The returned func releases the session's operation lock
// and saves it; call it once the call's outcome and usage are recorded (nil when there is no session).
func (m *Models) Authorize(ctx context.Context, token, sessionID, model string, est int64) (Decision, error) {
	if m == nil || m.Auth == nil || len(m.Routes) == 0 {
		return Decision{Rule: "M006", Message: "this deployment has no model routes"}, nil
	}
	p, err := m.Auth.Authenticate(ctx, token)
	if err != nil {
		return Decision{}, err
	}
	s := m.G.Store.Get(sessionID)
	req := m.Request(p.ID, s, sessionID, model, est, "")
	v, err := m.G.Core.Evaluate(ctx, req)
	if err != nil {
		var un *core.Unreachable
		if errors.As(err, &un) {
			ov := core.OutageVerdict(un, nil)
			return Decision{Rule: ov.Rule, Message: ov.Message}, nil
		}
		return Decision{}, err
	}
	if s != nil {
		s.Lock()
		tree := m.G.Store.Charge(s, v.Spend)
		s.Unlock()
		if tree != "" {
			m.G.Store.TripTree(s.ID, tree)
		}
	}
	if !v.Allow {
		return Decision{Rule: v.Rule, Message: v.Message}, nil
	}
	route := m.Routes[model]
	if route == nil || s == nil { // the core allowed a model it was told is routed
		msg := "no route for model \"" + model + "\""
		m.G.Core.Outcome(ctx, v.DecisionID, core.OutcomeArgs{Result: "refused M006: " + msg, Receipt: map[string]any{"stream": false},
			RefusalRule: "M006", RefusalMsg: msg})
		return Decision{Rule: "M006", Message: msg}, nil
	}
	msg := v.Message
	if msg == "" {
		msg = "allowed"
	}
	return Decision{Allow: true, Message: msg, Route: route, DecisionID: v.DecisionID,
		Headers: map[string]string{"x-modelplane-caller": s.ID, "x-hangar-task": s.TaskID}}, nil
}

// RecordUsage charges what the model actually used. Overrunning the budget trips the breaker. usage (UsageRecord) is
// added to the session's running cost. The budget counts the charge up to its limit, so Remaining never goes
// negative; the running cost and the outcome record keep the whole charge.
func (m *Models) RecordUsage(sessionID string, tokens int64, usage map[string]int64) *session.Session {
	s := m.G.Store.Get(sessionID)
	if s == nil {
		return nil
	}
	s.Lock()
	defer s.Unlock()
	if usage != nil {
		s.AddUsage(usage)
	}
	room := s.Remaining("model_tokens")
	add := tokens
	if room < add {
		add = room
	}
	if add < 0 {
		add = 0
	}
	s.Used.ModelTokens += add
	if tokens > room {
		s.Trip("model-token overrun")
	}
	return s
}

// Outcome reports an allowed model call's result: "ok" or "error:<Kind>", with the receipt (whether it streamed and
// what the upstream answered), the usage when the call used tokens, and the model that answered.
func (m *Models) Outcome(ctx context.Context, decisionID, result string, stream bool, upstreamStatus int, usage map[string]int64, modelVersion string) *int64 {
	receipt := map[string]any{"stream": stream}
	if upstreamStatus != 0 {
		receipt["upstream_status"] = upstreamStatus
	}
	octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.G.OutcomeTimeout)
	defer cancel()
	return m.G.Core.Outcome(octx, decisionID, core.OutcomeArgs{Result: result, Receipt: receipt, Usage: usage, ModelVersion: modelVersion})
}
