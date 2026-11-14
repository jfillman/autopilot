// Package dispatch is the gateway core, independent of any transport: authenticate, find the session, ask the
// Clearance core, apply its spend, run the backend, report the outcome. It is the Go side of
// autopilot.engine.dispatch.Gateway.call and open_session. Clearance decides and records; this package enforces.
//
// Differences from the Python gateway, all deliberate:
//   - no process-wide lock: a session's operations are serialized (Session.Serial) so one session cannot race its own
//     budget, and different sessions run in parallel;
//   - every call runs under a context deadline (Gateway.CallTimeout), every backend under its own (backends), and the
//     outcome of a call that ran is always reported, under a fresh bounded context, even when the call's ended.
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"github.com/jfillman/clearance/client"
)

// Principal is an authenticated caller.
type Principal struct {
	ID          string
	Kind        string // "user" (delegated agent) or "workload"
	TierCeiling int
	Attrs       map[string]string
}

// Authenticator turns a bearer token into a Principal, or *core.AuthError.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*Principal, error)
}

// Handler runs one tool. A *core.Denied is a refusal the core's rules cannot see; any other error is a backend error
// the agent is never shown.
type Handler func(ctx context.Context, s *session.Session, args map[string]any) (any, error)

// Result is what a call answers (Result.as_dict in the Python gateway).
type Result struct {
	OK       bool           `json:"ok"`
	Decision map[string]any `json:"decision"`
	Data     any            `json:"data"`
	Error    *string        `json:"error"`
	AuditSeq *int64         `json:"audit_seq"`
	Session  *string        `json:"session"`
}

// Gateway is the dispatch core.
type Gateway struct {
	Defs     map[string]*definitions.Definition
	Store    *session.Store
	Core     *core.Core
	Auth     Authenticator
	Now      func() time.Time
	Tools    *tools.Registry
	Handlers map[string]Handler
	Log      *slog.Logger

	// CallTimeout bounds one whole tool call (decision, backend and outcome); OutcomeTimeout bounds the outcome alone.
	CallTimeout    time.Duration
	OutcomeTimeout time.Duration
	// BuildRequest, when set, replaces ToolRequest as the evaluation a tool call sends. Only the conformance control
	// interface sets it: the harness controls the evaluation, and everything after the answer is production code.
	BuildRequest func(p *Principal, sessionID string, s *session.Session, tool string, args map[string]any, now time.Time) (*client.EvaluationRequest, error)
	// OnPersistError is called when the sessions cannot be saved. The in-memory sessions are then ahead of the
	// database and only a restart puts them back in step, so the serve command exits (crash-only).
	OnPersistError func(error)
}

// CoreOwned are the tools the gateway owns because they need the run backend; a deployment cannot replace them.
var CoreOwned = map[string]bool{"run.spawn": true, "run.close": true}

// OutageScopedArgs name scope only the core can check: during an outage a read that names one is denied.
var OutageScopedArgs = []string{"repo", "app", "service"}

var lowerEnvs = map[string]bool{"dev": true, "development": true, "sandbox": true, "scratch": true}

// New builds a Gateway with defaults for the timeouts.
func New(defs map[string]*definitions.Definition, st *session.Store, c *core.Core, auth Authenticator, reg *tools.Registry,
	handlers map[string]Handler) (*Gateway, error) {
	for name := range handlers {
		if _, ok := reg.Get(name); !ok {
			return nil, fmt.Errorf("handlers for tools that are not registered: %s", name)
		}
		if CoreOwned[name] {
			return nil, fmt.Errorf("handlers for tools the Gateway owns: %s", name)
		}
	}
	return &Gateway{Defs: defs, Store: st, Core: c, Auth: auth, Now: func() time.Time { return time.Now().UTC() }, Tools: reg,
		Handlers: handlers, Log: slog.Default(), CallTimeout: 60 * time.Second, OutcomeTimeout: 5 * time.Second,
		OnPersistError: func(err error) { slog.Error("sessions not saved", "err", err.Error()) }}, nil
}

func (g *Gateway) flush(ctx context.Context, ss ...*session.Session) {
	// A save is never abandoned because the caller went away: the state it holds was already changed.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := g.Store.Flush(fctx, ss...); err != nil {
		g.OnPersistError(err)
	}
}

// Subject is the evaluation's subject for a caller.
func Subject(p *Principal, acting string) client.Subject {
	props := client.SubjectProperties{Kind: p.Kind, TierCeiling: p.TierCeiling}
	if acting != "" && acting != p.ID {
		props.ActingAs = acting
	}
	return client.Subject{Type: "principal", ID: p.ID, Properties: props}
}

// ToolRequest builds the core's evaluation for a tool call, exactly as the Python gateway builds it (Gateway._tool_request).
// s may be nil (an unknown session: the core denies it). s must not be in use by another goroutine; the caller holds
// the session's Serial lock and this takes its field lock.
func (g *Gateway) ToolRequest(p *Principal, sessionID string, s *session.Session, tool string, args map[string]any, now time.Time) (*client.EvaluationRequest, error) {
	raw, err := json.Marshal(orEmpty(args))
	if err != nil {
		return nil, err
	}
	req := &client.EvaluationRequest{
		Subject:  Subject(p, ""),
		Action:   client.Action{Name: "tool.call", Properties: client.ActionProperties{Args: raw}},
		Resource: client.Resource{Type: "tool", ID: tool},
		Context:  client.Context{Now: now, SessionID: sessionID},
	}
	if s != nil {
		d := g.Defs[s.Agent]
		if d == nil {
			return nil, fmt.Errorf("no definition for agent %q", s.Agent)
		}
		s.Lock()
		req.Context.Session = s.State()
		s.Unlock()
		req.Context.Scope = d.Scope()
	}
	return req, nil
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func str(s string) *string { return &s }

// Call is one tool call: the Python gateway's Gateway.call.
func (g *Gateway) Call(ctx context.Context, token, sessionID, tool string, args map[string]any) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, g.CallTimeout)
	defer cancel()
	p, err := g.Auth.Authenticate(ctx, token)
	if err != nil {
		return nil, err
	}
	s := g.Store.Get(sessionID)
	if s != nil {
		defer s.Serial()()
		defer g.flush(ctx, s)
	}
	now := g.Now()
	build := g.ToolRequest
	if g.BuildRequest != nil {
		build = g.BuildRequest
	}
	req, err := build(p, sessionID, s, tool, args, now)
	if err != nil {
		return nil, err
	}
	v, err := g.Core.Evaluate(ctx, req)
	if err != nil {
		var un *core.Unreachable
		if errors.As(err, &un) {
			return g.outageCall(ctx, p, sessionID, s, tool, args, now, un), nil
		}
		return nil, err
	}
	if s == nil || !v.Allow {
		g.charge(s, v)
		return &Result{Decision: v.AsMap(), Error: str(v.Message), AuditSeq: v.Seq, Session: sessionRef(s, sessionID)}, nil
	}
	spec, _ := g.Tools.Get(tool)
	handler := g.Handlers[tool]
	if spec == nil || handler == nil {
		// Registered (its policy is known) but nothing in this deployment backs it. Charged nothing.
		seq := g.outcome(ctx, v.DecisionID, core.OutcomeArgs{Result: "error:NoHandler"})
		return &Result{Decision: v.AsMap(), Error: str("no backend for this tool in this deployment"), AuditSeq: firstSeq(seq, v.Seq), Session: &s.ID}, nil
	}
	g.charge(s, v)
	data, herr := g.run(ctx, handler, s, args)
	switch {
	case herr == nil:
		seq := g.outcome(ctx, v.DecisionID, core.OutcomeArgs{Result: "ok", Effect: effect(spec, args, data)})
		return &Result{OK: true, Decision: v.AsMap(), Data: data, AuditSeq: firstSeq(seq, v.Seq), Session: &s.ID}, nil
	default:
		var den *core.Denied
		if errors.As(herr, &den) {
			s.Lock()
			s.RecordDenial()
			s.Unlock()
			// The handler's rule goes on the outcome as its refusal (claimed), beside the decision's own rule.
			seq := g.outcome(ctx, v.DecisionID, core.OutcomeArgs{Result: clip(fmt.Sprintf("refused %s: %s", den.Rule, den.Message), 4000),
				RefusalRule: den.Rule, RefusalMsg: den.Message})
			tier := int(spec.Tier)
			dd := &core.Verdict{Rule: den.Rule, Name: "refused", Message: den.Message, Hint: den.Hint, Tier: &tier}
			return &Result{Decision: dd.AsMap(), Error: str(den.Message), AuditSeq: firstSeq(seq, v.Seq), Session: &s.ID}, nil
		}
		g.logBackendFailure(s, tool, herr)
		seq := g.outcome(ctx, v.DecisionID, core.OutcomeArgs{Result: ErrorResult(herr)})
		// Never leak backend internals to the agent: only an error that offers an agent message adds to "backend error".
		return &Result{Decision: v.AsMap(), Error: str(BackendError(herr)), AuditSeq: firstSeq(seq, v.Seq), Session: &s.ID}, nil
	}
}

// run calls a handler under the call's deadline, turning a panic into an error (a backend must not take the gateway down).
func (g *Gateway) run(ctx context.Context, h Handler, s *session.Session, args map[string]any) (data any, err error) {
	defer func() {
		if r := recover(); r != nil {
			data, err = nil, &KindError{Kind: "Panic", Err: fmt.Errorf("%v", r)}
		}
	}()
	data, err = h(ctx, s, args)
	if err == nil && ctx.Err() != nil {
		return nil, &KindError{Kind: "Timeout", Err: ctx.Err()}
	}
	return data, err
}

// charge applies the spend the core's decision names to the session.
func (g *Gateway) charge(s *session.Session, v *core.Verdict) {
	if s == nil {
		return
	}
	s.Lock()
	tree := g.Store.Charge(s, v.Spend)
	s.Unlock()
	if tree != "" {
		g.Store.TripTree(s.ID, tree)
	}
}

func (g *Gateway) outcome(ctx context.Context, decisionID string, a core.OutcomeArgs) *int64 {
	octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.OutcomeTimeout)
	defer cancel()
	return g.Core.Outcome(octx, decisionID, a)
}

func firstSeq(a, b *int64) *int64 {
	if a != nil {
		return a
	}
	return b
}

func sessionRef(s *session.Session, fallback string) *string {
	if s != nil {
		return &s.ID
	}
	return &fallback
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// KindError names a backend failure's kind for the outcome's result line ("error:<Kind>").
type KindError struct {
	Kind string
	Err  error
}

func (e *KindError) Error() string { return e.Kind + ": " + e.Err.Error() }
func (e *KindError) Unwrap() error { return e.Err }

// Kinded is a backend error that names its own kind (the Python class name: "ReadError"), for the outcome's result line.
type Kinded interface{ Kind() string }

// Evidenced is a backend error that offers evidence: the short, non-secret internal reason (a status, a rule id, a
// reason class; never a token, a header or a response body) that goes to the gateway's log and onto the call's outcome
// record, so a failure is diagnosable from evidence (Python: BackendError.evidence).
type Evidenced interface{ Evidence() string }

// AgentMessaged is a backend error whose reason the agent may be told (Python: agent_message; the GitHub error: a status
// and the other side's own reason, never a token). Any other error stays opaque to the agent.
type AgentMessaged interface{ AgentMessage() string }

// MaxEvidence is the longest evidence kept (Python: BackendError.evidence, str(self)[:160]).
const MaxEvidence = 160

// ClipEvidence cuts s to MaxEvidence characters.
func ClipEvidence(s string) string {
	if r := []rune(s); len(r) > MaxEvidence {
		return string(r[:MaxEvidence])
	}
	return s
}

// BackendError is what the agent is told when a backend failed: "backend error", and the error's agent message after it
// when it offers one (Python: dispatch._backend_error).
func BackendError(err error) string {
	var m AgentMessaged
	if errors.As(err, &m) {
		if msg := m.AgentMessage(); msg != "" {
			return "backend error: " + msg
		}
	}
	return "backend error"
}

// ErrorResult is the outcome's result for a backend that failed: "error:<kind>", and for an error with evidence the
// short reason after it (Python: dispatch._error_result: error:GitHubError: GitHub token broker 502: ...).
func ErrorResult(err error) string {
	return "error:" + ErrorKind(err) + evidenceSuffix(err)
}

func evidenceSuffix(err error) string {
	var e Evidenced
	if errors.As(err, &e) {
		if ev := e.Evidence(); ev != "" {
			return ": " + ev
		}
	}
	return ""
}

// logBackendFailure is the gateway's own account of a backend failure, for whoever reads its log: the session, agent
// and tool, the error's kind and its evidence when it offers one. The agent is told none of it (Python:
// dispatch._log_backend_failure).
func (g *Gateway) logBackendFailure(s *session.Session, tool string, err error) {
	run := "-"
	s.Lock()
	if s.Run != nil {
		run = *s.Run
	}
	s.Unlock()
	attrs := []any{"tool", tool, "agent", s.Agent, "session", s.ID, "run", run, "kind", ErrorKind(err)}
	if ev := evidenceSuffix(err); ev != "" {
		attrs = append(attrs, "evidence", strings.TrimPrefix(ev, ": "))
	}
	g.Log.Warn("backend failure", attrs...)
}

// ErrorKind is the kind a backend error reports.
func ErrorKind(err error) string {
	var k *KindError
	var kd Kinded
	switch {
	case errors.As(err, &k):
		return k.Kind
	case errors.As(err, &kd):
		return kd.Kind()
	case errors.Is(err, context.DeadlineExceeded):
		return "Timeout"
	case errors.Is(err, context.Canceled):
		return "Canceled"
	}
	return "BackendError"
}

// outageCall is a call with no decision from the core. Denied, unless the deployment allows reads and this is one the
// gateway can stand behind on its own: a T0 tool with a handler, in a live, untripped session of the caller's, granted
// to it, with budget left, whose arguments name no repo, app or service (scope only the core can check). Such a read
// runs, is charged, and is written to the outbox as a coverage-unavailable record.
func (g *Gateway) outageCall(ctx context.Context, p *Principal, sessionID string, s *session.Session, tool string, args map[string]any, now time.Time, un *core.Unreachable) *Result {
	spec, _ := g.Tools.Get(tool)
	var tier *int
	if spec != nil {
		t := int(spec.Tier)
		tier = &t
	}
	handler := g.Handlers[tool]
	ok := !un.Refused && g.Core.AllowReads && spec != nil && spec.Tier == tools.T0 && handler != nil && !CoreOwned[tool] &&
		!strings.HasPrefix(tool, "chat.") && s != nil && args != nil
	if ok {
		s.Lock()
		ok = !s.Expired(now) && !s.Tripped && s.Principal == p.ID && contains(s.Limits.Tools, tool) &&
			hasAll(args, spec.RequiredArgs) && !hasAny(args, OutageScopedArgs) &&
			s.CanSpend("tool_calls", 1) && s.CanSpend("github_calls", spec.GithubCost)
		s.Unlock()
	}
	if !ok {
		v := core.OutageVerdict(un, tier)
		return &Result{Decision: v.AsMap(), Error: str(v.Message), Session: sessionRef(s, sessionID)}
	}
	s.Lock()
	s.Used.ToolCalls++
	s.Used.GithubCalls += spec.GithubCost
	taskID, parent, agent := s.TaskID, s.ParentID, s.Agent
	s.Unlock()
	allowed := &core.Verdict{Allow: true, Message: "allowed during a Clearance outage (GATEWAY_OUTAGE_ALLOW_READS)", Tier: tier}
	fields := map[string]any{"task_id": taskID, "session": s.ID, "parent": derefStr(parent), "principal": p.ID, "agent": agent,
		"tool": tool, "tier": *tier, "decision": "allow", "target": EnvTierOf(spec, args), "args_sha256": core.ArgsDigest(args),
		"reason": fmt.Sprintf("the core was unreachable at %s; allowed as an unscoped T0 read (GATEWAY_OUTAGE_ALLOW_READS)", core.Iso(now))}
	data, herr := g.run(ctx, handler, s, args)
	rec := func(result string) {
		octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.OutcomeTimeout)
		defer cancel()
		f := map[string]any{}
		for k, v := range fields {
			f[k] = v
		}
		f["result"] = result
		g.Core.OutageRead(octx, f)
	}
	var den *core.Denied
	switch {
	case herr == nil:
		rec("ok")
		return &Result{OK: true, Decision: allowed.AsMap(), Data: data, Session: &s.ID}
	case errors.As(herr, &den):
		s.Lock()
		s.RecordDenial()
		s.Unlock()
		rec(clip(fmt.Sprintf("refused %s: %s", den.Rule, den.Message), 4000))
		dd := &core.Verdict{Rule: den.Rule, Name: "refused", Message: den.Message, Hint: den.Hint, Tier: tier}
		return &Result{Decision: dd.AsMap(), Error: str(den.Message), Session: &s.ID}
	default:
		g.logBackendFailure(s, tool, herr)
		rec(ErrorResult(herr))
		return &Result{Decision: allowed.AsMap(), Error: str(BackendError(herr)), Session: &s.ID}
	}
}

func derefStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func contains(a []string, x string) bool {
	for _, y := range a {
		if y == x {
			return true
		}
	}
	return false
}

func hasAll(m map[string]any, keys []string) bool {
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}

func hasAny(m map[string]any, keys []string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// EnvTierOf says which environment tier these arguments point at (the record's `target`; the core computes the same).
func EnvTierOf(spec *tools.Spec, args map[string]any) string {
	if spec.Target == "none" {
		return "none"
	}
	if project, ok := args["project"].(string); ok {
		if strings.HasSuffix(project, "-lower") {
			return "lower"
		}
		return "upper"
	}
	if env, ok := args["env"].(string); ok {
		if lowerEnvs[env] || strings.HasPrefix(env, "pr-") || strings.HasPrefix(env, "agent-") {
			return "lower"
		}
		return "upper"
	}
	if repo, ok := args["repo"].(string); ok {
		parts := strings.Split(repo, "/")
		if strings.HasPrefix(parts[len(parts)-1], "gitops-") {
			return "upper"
		}
		return "lower"
	}
	return spec.Target
}

// effect is what a successful write made, from its result (spec.Receipt), with the repo and the paths it wrote: the
// receipt a PR's provenance is checked against. nil for a tool without a receipt.
func effect(spec *tools.Spec, args map[string]any, data any) *client.Effect {
	m, ok := data.(map[string]any)
	if len(spec.Receipt) == 0 || !ok {
		return nil
	}
	e := &client.Effect{}
	for _, k := range spec.Receipt {
		v, present := m[k]
		if !present {
			continue
		}
		switch k {
		case "pr":
			e.PR = toInt(v)
		case "commit":
			e.Commit, _ = v.(string)
		case "branch":
			e.Branch, _ = v.(string)
		case "merge":
			// "person": the pull request is marked for a person to merge (A-11 propose scope), with the paths that need one.
			e.Merge, _ = v.(string)
		case "needsPerson":
			if list, ok := v.([]any); ok {
				for _, p := range list {
					if ps, ok := p.(string); ok {
						e.NeedsPerson = append(e.NeedsPerson, ps)
					}
				}
			}
		}
	}
	if spec.WritesFiles {
		repo, _ := args["repo"].(string)
		e.Repo = strings.ToLower(repo)
		var paths []string
		if files, ok := args["files"].([]any); ok {
			for _, f := range files {
				if fm, ok := f.(map[string]any); ok {
					p, _ := fm["path"].(string)
					paths = append(paths, normal(p))
				}
			}
		}
		e.Paths = uniqSorted(paths)
	} else if ps, ok := m["paths"].([]any); ok {
		repo, _ := m["repo"].(string)
		if repo == "" {
			repo, _ = args["repo"].(string)
		}
		e.Repo = strings.ToLower(repo)
		var paths []string
		for _, p := range ps {
			if s, ok := p.(string); ok {
				paths = append(paths, normal(s))
			}
		}
		e.Paths = uniqSorted(paths)
	}
	if e.Repo == "" && e.PR == 0 && e.Commit == "" && e.Branch == "" && len(e.Paths) == 0 && e.Merge == "" && len(e.NeedsPerson) == 0 {
		return nil // an empty effect is no effect (the Python gateway sends none)
	}
	return e
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case json.Number:
		n, _ := x.Int64()
		return n
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	}
	return 0
}

func uniqSorted(a []string) []string {
	set := map[string]bool{}
	for _, x := range a {
		set[x] = true
	}
	out := make([]string, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

// normal is scope.normalize, falling back to the path as written when it is not a repository-relative path.
func normal(p string) string {
	if p == "" || strings.ContainsAny(p, "\x00\\") || strings.HasPrefix(p, "/") {
		return p
	}
	n := path.Clean(p)
	if n == ".." || strings.HasPrefix(n, "../") || n == "." {
		return p
	}
	return n
}

// OpenRequest builds the core's evaluation for a session.open, as the Python gateway builds it (Gateway._open_request).
func (g *Gateway) OpenRequest(p *Principal, agent string, limits json.RawMessage, taskID, newSessionID string, now time.Time) *client.EvaluationRequest {
	req := &client.EvaluationRequest{
		Subject:  Subject(p, ""),
		Action:   client.Action{Name: "session.open", Properties: client.ActionProperties{Limits: limits, TaskID: taskID}},
		Resource: client.Resource{Type: "agent", ID: agent},
		Context:  client.Context{Now: now, NewSessionID: newSessionID},
	}
	if d := g.Defs[agent]; d != nil {
		req.Context.Agent = d.AgentDef()
	}
	return req
}

// Open opens a session for an agent definition (Gateway.open_session). limits may only narrow the definition's limits;
// the core decides and grants.
func (g *Gateway) Open(ctx context.Context, token, agent string, limits json.RawMessage, taskID string) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, g.CallTimeout)
	defer cancel()
	p, err := g.Auth.Authenticate(ctx, token)
	if err != nil {
		return nil, err
	}
	now := g.Now()
	newID := g.Store.NewID()
	req := g.OpenRequest(p, agent, limits, taskID, newID, now)
	v, err := g.Core.Evaluate(ctx, req)
	var un *core.Unreachable
	if errors.As(err, &un) {
		v, err = core.OutageVerdict(un, nil), nil
	}
	if err != nil {
		g.Store.GiveBack(newID)
		return nil, err
	}
	if !v.Allow || v.Grant == nil {
		g.Store.GiveBack(newID)
		return &Result{Decision: map[string]any{"allow": false, "rule": nilIfEmpty(v.Rule), "message": v.Message},
			Error: str(v.Message), AuditSeq: v.Seq}, nil
	}
	s := g.Store.Adopt(v.Grant, now, nil, 0)
	g.flush(ctx, s)
	s.Lock()
	data := map[string]any{"session": s.ID, "task_id": s.TaskID, "expires_at": core.Iso(s.ExpiresAt), "limits": s.Limits}
	s.Unlock()
	return &Result{OK: true, Decision: map[string]any{"allow": true}, Data: data, AuditSeq: v.Seq, Session: &s.ID}, nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ToolInfo is a registry entry as tools_list shows it.
type ToolInfo struct {
	Name         string   `json:"name"`
	Tier         string   `json:"tier"`
	Description  string   `json:"description"`
	RequiredArgs []string `json:"required_args"`
	ProposeOnly  bool     `json:"propose_only"`
}

// SessionTools lists the tools a caller may use in its own session that this deployment can run (the session's granted
// tools, less any without a handler); ok is false when the session is unknown or not the caller's.
func (g *Gateway) SessionTools(ctx context.Context, token, sessionID string) (names map[string]bool, ok bool, err error) {
	p, err := g.Auth.Authenticate(ctx, token)
	if err != nil {
		return nil, false, err
	}
	s := g.Store.Get(sessionID)
	if s == nil {
		return nil, false, nil
	}
	s.Lock()
	defer s.Unlock()
	if s.Closed || s.Principal != p.ID {
		return nil, false, nil
	}
	names = map[string]bool{}
	for _, t := range s.Limits.Tools {
		if _, reg := g.Tools.Get(t); reg && g.Handlers[t] != nil {
			names[t] = true
		}
	}
	return names, true, nil
}

// ToolList is tools_list: the registry (optionally narrowed to names) with tiers and required arguments.
func (g *Gateway) ToolList(names map[string]bool) []ToolInfo {
	out := []ToolInfo{}
	for _, t := range g.Tools.All() {
		if names != nil && !names[t.Name] {
			continue
		}
		args := t.RequiredArgs
		if args == nil {
			args = []string{}
		}
		out = append(out, ToolInfo{Name: t.Name, Tier: t.Tier.String(), Description: t.Description, RequiredArgs: args, ProposeOnly: t.ProposeOnly})
	}
	return out
}

// AgentInfo is an agent definition as agents_list shows it.
type AgentInfo struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Kind           string   `json:"kind"`
	Identity       string   `json:"identity"`
	RunsIn         string   `json:"runs_in"`
	TierCeiling    string   `json:"tier_ceiling"`
	Tools          []string `json:"tools"`
	CanOpenSession bool     `json:"can_open_session"`
}

// Agents lists the definitions (any authenticated caller; instructions are never included).
func (g *Gateway) Agents(ctx context.Context, token string) ([]AgentInfo, error) {
	if _, err := g.Auth.Authenticate(ctx, token); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(g.Defs))
	for n := range g.Defs {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]AgentInfo, 0, len(names))
	for _, n := range names {
		d := g.Defs[n]
		out = append(out, AgentInfo{Name: d.Name, Description: d.Description, Kind: d.Kind, Identity: d.IdentityType, RunsIn: d.RunsIn(),
			TierCeiling: d.Limits.TierCeiling, Tools: d.Limits.Tools, CanOpenSession: d.IdentityType == "delegated"})
	}
	return out, nil
}
