package dispatch

// The run tree: what a session's run is, who may act as its session, how a child is cut and created, how a run ends and
// how the sweep cleans up after it. The Go side of the run parts of autopilot.engine.dispatch (Gateway.launch, _spawn,
// _close, finish_run, close_session, expire, _delete_runs, _backstop_outcomes, _acting, _record).
//
// What is deliberately different from the Python gateway, all of it for the same inputs giving the same answers:
//   - the core cuts a spawned child's grant and the gateway checks it itself (session.Store.AdoptChild) instead of
//     trusting it: a grant wider than the parent has left, for another principal, task or agent, or for a session id that
//     exists, is refused and nothing is created;
//   - the parent's check and its reservation are one step under the parent's lock, and the run tree's operations on one
//     session are serialized, so concurrent spawns cannot overspend a parent;
//   - a run's name is written into its session and saved before the run is created (write-ahead), so a crash between the
//     two leaves a session whose sweep deletes the run it may have started; Python leaves the run to its own deadline;
//   - a run that fails to be created is deleted again, best effort, because a timeout may have come after the API server
//     took it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/clearance/client"
)

// RunBackend starts and stops the workload behind a session (engine.backends.RunBackend, with the optional methods
// required). On Hangar it is runs.Backend: an AgentRun XR.
type RunBackend interface {
	// Name is the name the session's run will have, known before it exists.
	Name(s *session.Session) string
	// Create starts the session's run and returns its name.
	Create(ctx context.Context, s *session.Session, d *definitions.Definition, input map[string]any) (string, error)
	// Delete stops it (already gone is success).
	Delete(ctx context.Context, s *session.Session) error
	// Ended is how the run's workload ended ("" while it runs), for the sweep's backstop.
	Ended(ctx context.Context, s *session.Session) (string, error)
	// Principal is the identity a run's pod authenticates as, or "" for a name that has none: the gateway then treats
	// that caller as its session's owner, for calls into that session only.
	Principal(run string) string
}

// SetRuns installs the run backend and the two tools the gateway owns because they need the session store
// (run.spawn, decided as session.spawn by Call, and run.close).
func (g *Gateway) SetRuns(rb RunBackend) {
	g.Runs = rb
	if g.Handlers == nil {
		g.Handlers = map[string]Handler{}
	}
	if rb != nil {
		g.Handlers["run.spawn"] = func(context.Context, *session.Session, map[string]any) (any, error) {
			return nil, errors.New("run.spawn is decided as session.spawn in Gateway.Call")
		}
		g.Handlers["run.close"] = g.closeHandler
	}
}

// Acting is who the caller acts as in session s: the session's principal when the caller is the session's own run, else
// the caller (Python: Gateway._acting). Records always name the caller itself.
//
// The rule, in full. A caller is the session's own run only when ALL of these hold:
//   - the gateway has a run backend, and the session has a run (its name was written by this gateway when it created
//     the run, never taken from a caller's arguments);
//   - the caller is a workload and is not already the session's principal;
//   - the caller's verified identity (the namespace and service account the token review returned, "ns/sa") is exactly
//     the identity the backend names for that run: agent-<run>/run, for a run name that is r- and 6 to 32 hex digits.
//
// Nothing else is read: not a header, an argument, a label or a name the caller sends. So a sibling run (its own
// namespace), a parent, a person, a run in another namespace with the same service account name, a run whose session has
// no run any more (finished or deleted) and a caller whose token is not a run's all act as themselves, which the core
// refuses as another principal's session (R001).
func (g *Gateway) Acting(s *session.Session, p *Principal) string {
	s.Lock()
	defer s.Unlock()
	return g.actingLocked(s, p)
}

func (g *Gateway) actingLocked(s *session.Session, p *Principal) string {
	if g.Runs == nil || s.Run == nil || *s.Run == "" || p.Kind != "workload" || p.ID == s.Principal {
		return p.ID
	}
	if want := g.Runs.Principal(*s.Run); want != "" && want == p.ID {
		return s.Principal
	}
	return p.ID
}

// ControlError is a refusal a person or a run can act on: a rule, a message, a hint, and the HTTP status the route that
// carries it answers with (control.ControlError).
type ControlError struct {
	Rule, Message, Hint string
	Status              int
}

func (e *ControlError) Error() string { return e.Message }

// MaxControlReason is the longest reason a control or a session_close records.
const MaxControlReason = 500

// CheckReason is the reason a control records, trimmed (control.check_reason).
func CheckReason(reason any) (string, error) {
	s, ok := reason.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", &ControlError{"CT-REASON", "a reason is required", "Say why, in a sentence; it is recorded.", 400}
	}
	if utf8.RuneCountInString(s) > MaxControlReason {
		return "", &ControlError{"CT-REASON", fmt.Sprintf("the reason is longer than %d characters", MaxControlReason), "", 400}
	}
	return strings.TrimSpace(s), nil
}

// ApplySpawn turns a tool.call evaluation of run.spawn into the session.spawn evaluation the core decides: the action's
// name, the child's definition (nil for an agent that is not a live definition) and the id the child would get. It is
// what Call does, exported for the replay.
func (g *Gateway) ApplySpawn(req *client.EvaluationRequest, args map[string]any, newID string) *definitions.Definition {
	req.Action.Name = "session.spawn"
	var child *definitions.Definition
	if name, ok := args["agent"].(string); ok {
		child = g.Defs[name]
	}
	if child != nil {
		req.Context.Agent = child.AgentDef()
	}
	req.Context.NewSessionID = newID
	return child
}

func (g *Gateway) applySpawn(req *client.EvaluationRequest, args map[string]any, newID string) *definitions.Definition {
	return g.ApplySpawn(req, args, newID)
}

// MaxInputChars is the longest an AgentRun's input may be, in the characters Python counts (compose.MAX_INPUT_BYTES).
const MaxInputChars = 4096

// runInput is a spawn's input: nil, or an object whose JSON fits an AgentRun's input. Anything else is refused before a
// child exists (Python lets the API server refuse it after).
func runInput(v any) (map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &core.Denied{Rule: "R008", Message: "input is an object", Hint: "Pass the run's input as an object, or leave it out."}
	}
	if pyJSONLen(m) > MaxInputChars {
		return nil, &core.Denied{Rule: "R008", Message: "input is larger than 4 KiB", Hint: "Pass a reference instead."}
	}
	return m, nil
}

// pyJSONLen is len(json.dumps(v, separators=(",", ":"))): Python escapes everything but printable ASCII.
func pyJSONLen(v any) int {
	var b strings.Builder
	var emit func(any)
	emit = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			b.WriteByte('{')
			first := true
			for k, e := range t {
				if !first {
					b.WriteByte(',')
				}
				first = false
				emit(k)
				b.WriteByte(':')
				emit(e)
			}
			b.WriteByte('}')
		case []any:
			b.WriteByte('[')
			for i, e := range t {
				if i > 0 {
					b.WriteByte(',')
				}
				emit(e)
			}
			b.WriteByte(']')
		case string:
			b.WriteByte('"')
			for _, r := range t {
				switch {
				case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t' || r == '\b' || r == '\f':
					b.WriteString("\\x")
				case r < 0x20 || (r >= 0x7f && r < 0x10000): // everything outside " " to "~" is \uXXXX, DEL included
					b.WriteString("\\uXXXX")
				case r >= 0x10000:
					b.WriteString("\\uXXXX\\uXXXX")
				default:
					b.WriteByte('x')
				}
			}
			b.WriteByte('"')
		default:
			raw, _ := json.Marshal(t)
			b.Write(raw)
		}
	}
	emit(v)
	return b.Len()
}

// callBackend runs a backend call and turns a panic into an error: a backend must not take the gateway down.
func callBackend(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &KindError{Kind: "Panic", Err: fmt.Errorf("%v", r)}
		}
	}()
	return fn()
}

func (g *Gateway) createRun(ctx context.Context, s *session.Session, d *definitions.Definition, input map[string]any) (run string, err error) {
	err = callBackend(func() (e error) { run, e = g.Runs.Create(ctx, s, d, input); return })
	return run, err
}

// bg is a context for work that must finish although the call that asked for it ended: bounded, never cancelled with it.
func (g *Gateway) bg(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

// spawn is an allowed session.spawn: the child is the core's grant, cut from what the parent has left after this call's
// charge and checked here, and the parent reserves its budget. A run that fails to start closes the child again. now is the
// time the core decided at: the grant is checked as of then, so a minute boundary crossed since cannot refuse a grant that was
// in bounds when it was cut.
func (g *Gateway) spawn(ctx context.Context, parent *session.Session, d *definitions.Definition, v *core.Verdict, newID string, args map[string]any, now time.Time) (any, error) {
	if d == nil || v.Grant == nil {
		g.Store.GiveBack(newID)
		return nil, &core.Denied{Rule: "R003", Message: "unknown agent definition", Hint: "Spawn an agent that exists in the catalog."}
	}
	input, err := runInput(args["input"])
	if err != nil {
		g.Store.GiveBack(newID)
		return nil, err
	}
	child, release, err := g.Store.AdoptChild(v.Grant, v.Spend, parent.ID, newID, d.Name, d.Limits, now)
	if err != nil {
		g.Store.GiveBack(newID)
		g.Log.Warn("a spawn grant failed the gateway's own check: refused", "session", parent.ID, "agent", d.Name, "why", err.Error())
		return nil, &core.Denied{Rule: "R007", Message: "the child's limits are not within what this run has left",
			Hint: "Ask for less, or wait for budget to come back from a closed child."}
	}
	// The child's operation lock is held from its creation to the end of this function (parent first, then child: the only
	// order anywhere), so the sweep cannot read or settle a child whose run is still being made.
	defer release()
	// Write-ahead: the run's name is saved with the child, and the parent's reservation with it, before the run exists.
	name := g.Runs.Name(child)
	child.Lock()
	child.Run = &name
	child.Unlock()
	child.BumpRunEpoch()
	g.flush(ctx, child, parent)
	run, cerr := g.createRun(ctx, child, d, input)
	if cerr != nil {
		g.abandonRun(ctx, child)
		return nil, cerr
	}
	// The run's name is set whatever it is: a close or a sweep that ran during the create may have cleared it (finding
	// "already gone" for a run that did not exist yet), and the run must be named for it to be deleted.
	child.Lock()
	child.Run = &run
	closed := child.Closed
	child.Unlock()
	child.BumpRunEpoch()
	g.flush(ctx, child)
	if closed || ctx.Err() != nil {
		// The child was closed (a person's End, its parent's close, the sweep) while its run was being made, or the call's time
		// ran out: the run is deleted and the agent told, so no run lives outside the accounting. The refund is the close's,
		// once; abandonRun closes an already closed child as a no-op.
		// Either the agent gets the success it earned or the child is undone, never the mix (Call does not turn a spawn's
		// success into a timeout after this point).
		g.abandonRun(ctx, child)
		if closed {
			return nil, &core.Denied{Rule: "R004", Message: "the child was closed while its run was being made",
				Hint: "Spawn again if you still need it."}
		}
		return nil, ctx.Err()
	}
	child.Lock()
	exp := child.ExpiresAt
	child.Unlock()
	return map[string]any{"session": child.ID, "run": run, "expires_at": core.Iso(exp)}, nil
}

// abandonRun closes a session whose run could not be started and deletes the run it may have started anyway (a timeout
// can come after the API server took the object). The session keeps its run name when the delete fails, so the sweep
// retries it: the session is closed, which is what finds it.
func (g *Gateway) abandonRun(ctx context.Context, s *session.Session) {
	touched := g.Store.CloseTouched(s.ID)
	bctx, cancel := g.bg(ctx)
	defer cancel()
	if err := callBackend(func() error { return g.Runs.Delete(bctx, s) }); err == nil {
		s.Lock()
		s.Run = nil
		s.Unlock()
	}
	s.BumpRunEpoch()
	g.flush(ctx, append(touched, s)...)
}

func (g *Gateway) closeHandler(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
	id, _ := a["run_id"].(string)
	target := g.Store.Get(id)
	own := false
	if target != nil {
		s.Lock()
		own = target.ID == s.ID || contains(s.Children, target.ID)
		s.Unlock()
	}
	if !own {
		return nil, &core.Denied{Rule: "R001", Message: "not one of this session's runs", Hint: "You may only close your own run or its children."}
	}
	touched := g.deleteRuns(ctx, target, s, "closed", nil)
	g.flush(ctx, touched...)
	return map[string]any{"closed": target.ID}, nil
}

// sweepRun is what one expiry sweep carries: its clock, and the runs it has already handled (each at most once per sweep,
// however many ancestors and pending entries name it).
type sweepRun struct {
	now   time.Time
	seen  map[string]bool
	acted map[string]bool // the sessions whose delete was actually attempted (not skipped for a backoff)
}

const (
	deleteBackoffMin    = time.Minute
	deleteBackoffMax    = 30 * time.Minute
	deleteFailureRecord = time.Hour
)

// deleteRuns closes a session and deletes its run and its descendants' runs, each with a run.delete record. A delete that
// fails is recorded and the session keeps its run, so a later sweep retries it. It returns the sessions it changed.
//
// A sweep (sw not nil) is bounded, which the Python sweep is not: a run whose delete keeps failing is retried on a backoff
// (1, 2, 4 ... 30 minutes), its failure is recorded when it starts failing and then at most once an hour while it keeps
// failing, and its success is recorded once; and each run is handled at most once per sweep. A close by a person or a run
// (sw nil) always tries.
func (g *Gateway) deleteRuns(ctx context.Context, target, actor *session.Session, why string, sw *sweepRun) []*session.Session {
	touched := g.Store.CloseTouched(target.ID)
	actor.Lock()
	actorPrincipal := actor.Principal
	actor.Unlock()
	for _, sid := range append([]string{target.ID}, g.descendants(target)...) {
		s := g.Store.Get(sid)
		if s == nil || g.Runs == nil {
			continue
		}
		s.Lock()
		var run string
		if s.Run != nil {
			run = *s.Run
		}
		s.Unlock()
		if run == "" {
			continue
		}
		now := g.Now()
		if sw != nil {
			if sw.seen[sid] || ctx.Err() != nil {
				continue
			}
			sw.seen[sid] = true
			now = sw.now
			s.Lock()
			skip := s.DeleteFail != nil && now.Before(s.DeleteFail.Next)
			s.Unlock()
			if skip {
				continue
			}
			sw.acted[sid] = true
		}
		var bctx context.Context
		var cancel context.CancelFunc
		if sw != nil {
			bctx, cancel = context.WithTimeout(ctx, 10*time.Second) // the sweep's own deadline is honoured
		} else {
			bctx, cancel = g.bg(ctx)
		}
		derr := callBackend(func() error { return g.Runs.Delete(bctx, s) })
		cancel()
		args := map[string]any{"run": run, "why": why}
		if derr != nil {
			s.Lock()
			first := s.DeleteFail == nil
			if first {
				s.DeleteFail = &session.DeleteFail{FirstFailed: now}
			}
			st := s.DeleteFail
			st.Fails++
			delay := deleteBackoffMin << min(st.Fails-1, 10)
			if delay > deleteBackoffMax {
				delay = deleteBackoffMax
			}
			st.Next = now.Add(delay)
			// A sweep records when a run starts failing and then at most once an hour; a person's or a run's own close always
			// records its own result.
			write := sw == nil || first || now.Sub(st.LastRecorded) >= deleteFailureRecord
			if write {
				st.LastRecorded = now
			}
			attempts := st.Fails
			s.Unlock()
			if sw == nil || write {
				g.Log.Error("a run could not be deleted", "run", run, "session", sid, "kind", ErrorKind(derr), "attempts", attempts)
			}
			if write {
				g.record(ctx, "run.delete", s, actorPrincipal, args, "error:"+ErrorKind(derr), false) //nolint:errcheck // not required
			}
			touched = append(touched, s)
			continue
		}
		s.Lock()
		s.DeleteFail = nil
		s.Unlock()
		g.record(ctx, "run.delete", s, actorPrincipal, args, "deleted", false) //nolint:errcheck
		s.Lock()
		s.Run = nil
		s.Unlock()
		s.BumpRunEpoch()
		touched = append(touched, s)
	}
	return touched
}

func (g *Gateway) descendants(s *session.Session) []string {
	s.Lock()
	kids := append([]string(nil), s.Children...)
	s.Unlock()
	var out []string
	for _, c := range kids {
		out = append(out, c)
		if child := g.Store.Get(c); child != nil {
			out = append(out, g.descendants(child)...)
		}
	}
	return out
}

// MaxRunInput is the longest a person's task input to an in-cluster run is (an AgentRun's input is 4 KiB in all).
const MaxRunInput = 2000

// InputView is what a run was asked, kept on its session for the read API: the prompt (clipped to MaxRunInput) and who
// started it. Never the definition's instructions or an event's payload (Python: _input_view).
func InputView(input map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"prompt", "started_by"} {
		if s, ok := input[k].(string); ok {
			out[k] = firstChars(s, MaxRunInput)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Launch starts the run for a top-level session (a triggered or delegated in-cluster run; on Hangar, an AgentRun XR).
// Children are created by run.spawn; service agents are deployed as applications and have no run. The run.launch record
// must reach the core: when it cannot, the run is deleted again (fail closed). A backend failure is the returned error,
// as Python raises it.
func (g *Gateway) Launch(ctx context.Context, sessionID string, input map[string]any) (*Result, error) {
	s := g.Store.Get(sessionID)
	if s == nil {
		return &Result{Decision: map[string]any{"allow": false, "rule": "R004"}, Error: str("no live session")}, nil
	}
	defer s.Serial()()
	defer g.flush(ctx, s)
	s.Lock()
	closed, agent, hasRun, principal := s.Closed, s.Agent, s.Run != nil, s.Principal
	s.Unlock()
	if closed {
		return &Result{Decision: map[string]any{"allow": false, "rule": "R004"}, Error: str("no live session")}, nil
	}
	refuse := func(msg string) (*Result, error) {
		return &Result{Decision: map[string]any{"allow": false, "rule": "R001"}, Error: str(msg), Session: &s.ID}, nil
	}
	d := g.Def(agent)
	if d == nil {
		return &Result{Decision: map[string]any{"allow": false, "rule": "R003"}, Error: str("unknown agent definition"), Session: &s.ID}, nil
	}
	if d.RunsIn() != "cluster" {
		return refuse(fmt.Sprintf("%s has no runtime image: it runs in a person's MCP client, not as an in-cluster run", d.Name))
	}
	if d.Kind == "service" {
		return refuse("service agents are deployed as applications, not launched as runs")
	}
	if g.Runs == nil {
		return refuse("this deployment has no run backend")
	}
	if hasRun {
		return refuse("this session already has a run")
	}
	name := g.Runs.Name(s)
	s.Lock()
	s.Run = &name
	s.Unlock()
	s.BumpRunEpoch()
	g.flush(ctx, s) // write-ahead
	run, cerr := g.createRun(ctx, s, d, input)
	if cerr != nil {
		defer s.BumpRunEpoch()
		bctx, cancel := g.bg(ctx)
		if derr := callBackend(func() error { return g.Runs.Delete(bctx, s) }); derr == nil {
			s.Lock()
			s.Run = nil
			s.Unlock()
		}
		cancel()
		return nil, cerr
	}
	s.Lock()
	s.Run = &run
	s.Input = InputView(input)
	closedNow := s.Closed
	s.Unlock()
	s.BumpRunEpoch()
	if closedNow {
		// Closed while its run was being made (a person's End, the sweep): the run is deleted, not left outside the accounting.
		bctx, cancel := g.bg(ctx)
		if derr := callBackend(func() error { return g.Runs.Delete(bctx, s) }); derr == nil {
			s.Lock()
			s.Run = nil
			s.Unlock()
			s.BumpRunEpoch()
		}
		cancel()
		return &Result{Decision: map[string]any{"allow": false, "rule": "R004"}, Error: str("no live session"), Session: &s.ID}, nil
	}
	seq, rerr := g.record(ctx, "run.launch", s, principal, map[string]any{"run": run}, "created", true)
	if rerr != nil {
		var un *core.Unreachable
		if !errors.As(rerr, &un) {
			return nil, rerr
		}
		bctx, cancel := g.bg(ctx)
		derr := callBackend(func() error { return g.Runs.Delete(bctx, s) })
		cancel()
		s.Lock()
		if derr == nil {
			s.Run = nil // deleted: the session keeps none
		} // else it keeps the run's name, so the sweep (which needs it) retries the delete
		s.Input = nil
		s.Unlock()
		s.BumpRunEpoch()
		v := core.OutageVerdict(un, nil)
		return &Result{Decision: map[string]any{"allow": false, "rule": v.Rule}, Error: str(v.Message), Session: &s.ID}, nil
	}
	return &Result{OK: true, Decision: map[string]any{"allow": true}, Data: map[string]any{"run": run, "session": s.ID}, AuditSeq: seq, Session: &s.ID}, nil
}

func intOf(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	case float64:
		if x == float64(int64(x)) {
			return int64(x), true
		}
	}
	return 0, false
}

// FinishRun is a run handing back its result as it exits (the MCP tool run_finish): the answer goes to the result store
// for Retention, the outcome onto the session, a run.finish record to the core (the status, never the text), and the
// session closes, so Tower shows the run finished and the next sweep deletes its run. Only the session's own run, or its
// principal, may finish it, and only once.
func (g *Gateway) FinishRun(ctx context.Context, token, sessionID string, body any) (map[string]any, error) {
	p, err := g.Auth.Authenticate(ctx, token)
	if err != nil {
		return nil, err
	}
	// An unknown session, another's, and one whose run is not the caller get the same answer, so a caller learns nothing of
	// which session ids exist (Python answers an unknown or closed one R004 first).
	notYours := &ControlError{"R001", "only this session's own run may finish it", "", 403}
	s := g.Store.Get(sessionID)
	if s == nil {
		return nil, notYours
	}
	defer s.Serial()()
	defer g.flush(ctx, s)
	s.Lock()
	closed, hasRun, acting, owner, expires := s.Closed, s.Run != nil && *s.Run != "", g.actingLocked(s, p), s.Principal, s.ExpiresAt
	expiredOutcome := s.Outcome != nil && s.Outcome["status"] == "expired"
	s.Unlock()
	if acting != owner {
		return nil, notYours
	}
	// The session's own caller learns it has ended. Past its deadline, or once the sweep has written "expired", a late
	// finish cannot overwrite that outcome.
	if closed || expiredOutcome || !g.Now().Before(expires) {
		return nil, &ControlError{"R004", "no live session", "The session already ended: its outcome is kept.", 409}
	}
	if !hasRun {
		return nil, notYours
	}
	b, _ := body.(map[string]any)
	status, _ := b["status"].(string)
	if !contains(Statuses, status) {
		return nil, &ControlError{"R008", "status is one of " + strings.Join(Statuses, ", "), "", 400}
	}
	text, reason := "", ""
	turns := int64(0)
	if truthy(b["final_text"]) {
		t, ok := b["final_text"].(string)
		if !ok {
			return nil, &ControlError{"R008", "final_text and reason are strings, turns an integer", "", 400}
		}
		text = t
	}
	if truthy(b["reason"]) {
		r, ok := b["reason"].(string)
		if !ok {
			return nil, &ControlError{"R008", "final_text and reason are strings, turns an integer", "", 400}
		}
		reason = r
	}
	if truthy(b["turns"]) {
		if _, isBool := b["turns"].(bool); isBool {
			return nil, &ControlError{"R008", "final_text and reason are strings, turns an integer", "", 400}
		}
		n, ok := intOf(b["turns"])
		if !ok {
			return nil, &ControlError{"R008", "final_text and reason are strings, turns an integer", "", 400}
		}
		turns = n
	}
	text, truncated := ClipText(text)
	calls := ToolCallSummary(b["tool_calls"])
	now := g.Now()
	var kept map[string]any
	putDone := false
	s.Lock()
	warns := Warnings(s.Signals)
	s.Unlock()
	if g.Results != nil {
		expires := now.Add(Retention)
		s.Lock()
		task, agent, sprincipal := s.TaskID, s.Agent, s.Principal
		s.Unlock()
		rec := map[string]any{"session": s.ID, "task_id": task, "agent": agent, "principal": sprincipal, "status": status,
			"final_text": text, "truncated": truncated, "turns": turns, "tool_calls": calls, "reason": firstChars(reason, MaxReason),
			"warnings": warns, "written_at": core.Iso(now), "expires_at": core.Iso(expires)}
		if perr := g.Results.Put(ctx, rec, now); perr != nil {
			g.Log.Error("a run's result was not kept: the run may finish again", "session", s.ID, "err", perr.Error())
			return nil, perr // nothing else has happened: the session is still open for the run to try again
		}
		putDone = true
		kept = map[string]any{"bytes": len(text), "sha256": core.ArgsDigestBytes([]byte(text)), "truncated": truncated,
			"expires_at": core.Iso(expires)}
	}
	notOK := 0
	for _, c := range calls {
		if !c.OK {
			notOK++
		}
	}
	outcome := map[string]any{"status": status, "finished_at": core.Iso(now), "turns": turns, "tool_calls": len(calls),
		"not_ok": notOK, "reason": firstChars(reason, MaxReason), "result": kept, "via": "runtime"}
	if len(warns) > 0 {
		outcome["warnings"] = warns
	}
	// The deadline, the session's closing and the sweep's "expired" are checked again here, under the session's lock and just
	// before the outcome is set: the answer was stored slowly, and the sweep may have ended the session meanwhile.
	s.Lock()
	lost := s.Closed || (s.Outcome != nil && s.Outcome["status"] == "expired") || !g.Now().Before(s.ExpiresAt)
	if !lost {
		s.Outcome = outcome
	}
	s.Unlock()
	if lost {
		if putDone {
			dctx, cancel := g.bg(ctx)
			_ = g.Results.Delete(dctx, s.ID) // the answer just kept is taken back: this finish did not happen
			cancel()
		}
		return nil, &ControlError{"R004", "no live session", "The session already ended: its outcome is kept.", 409}
	}
	seq, _ := g.record(ctx, "run.finish", s, p.ID, map[string]any{"status": status, "turns": turns}, status, false)
	touched := g.Store.CloseTouched(s.ID)
	g.flush(ctx, touched...)
	var keptOut any
	if kept != nil {
		keptOut = kept
	}
	out := map[string]any{"session": s.ID, "status": status, "closed": true, "kept": keptOut, "audit_seq": nil}
	if seq != nil {
		out["audit_seq"] = *seq
	}
	return out, nil
}

// CloseSession is the MCP tool session_close: the session's own caller ends it (a person's MCP client, or the session's
// own in-cluster run), instead of leaving it live until its TTL. Owner-only: anyone else is refused, the agent's owner
// group and platform admins included (they use the person control, Tower's End). The session and its children close,
// session.end is written through the core, and its run and its children's runs are deleted as the expiry sweep deletes
// them, each with a run.delete record.
func (g *Gateway) CloseSession(ctx context.Context, token, sessionID string, reason any) (map[string]any, error) {
	p, err := g.Auth.Authenticate(ctx, token)
	if err != nil {
		return nil, err
	}
	s := g.Store.Get(sessionID)
	forbidden := &ControlError{"CT-FORBIDDEN", "no such session of yours",
		"session_close ends only your own session; a person ends another's from Tower.", 403}
	if s == nil {
		return nil, forbidden
	}
	defer s.Serial()()
	s.Lock()
	acting, owner, closed, agent := g.actingLocked(s, p), s.Principal, s.Closed, s.Agent
	s.Unlock()
	if acting != owner {
		// The same answer for an unknown session and another's: a caller learns nothing of sessions not its own.
		return nil, forbidden
	}
	if closed {
		return nil, &ControlError{"CT-CLOSED", "this session is already closed", "", 409}
	}
	text := "closed by its owner (session_close)"
	if reason != nil && reason != "" {
		if text, err = CheckReason(reason); err != nil {
			return nil, err
		}
	}
	touched := g.Store.CloseTouched(s.ID)
	rec := g.controlRecord(ctx, p.ID, "session.end", s, agent, map[string]any{"reason": text}, "ended: "+text)
	withRuns := []string{}
	for _, sid := range append([]string{s.ID}, g.descendants(s)...) {
		if x := g.Store.Get(sid); x != nil {
			x.Lock()
			has := x.Run != nil
			x.Unlock()
			if has {
				withRuns = append(withRuns, sid)
			}
		}
	}
	touched = append(touched, g.deleteRuns(ctx, s, s, "closed", nil)...)
	g.flush(ctx, touched...)
	deleted := []string{}
	for _, sid := range withRuns {
		x := g.Store.Get(sid)
		x.Lock()
		gone := x.Run == nil
		x.Unlock()
		if gone {
			deleted = append(deleted, sid)
		}
	}
	sortStrings(deleted)
	out := map[string]any{"session": s.ID, "agent": agent, "closed": true, "audit_seq": nil, "runs_deleted": deleted}
	if rec != nil {
		out["audit_seq"] = *rec
	}
	return out, nil
}

func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// controlRecord is a control's record (session.end): the control has already taken effect, so an unreachable core queues
// the record and never undoes it (Python: _control_record).
func (g *Gateway) controlRecord(ctx context.Context, person, kind string, s *session.Session, agent string, args map[string]any, result string) *int64 {
	s.Lock()
	task, parent := s.TaskID, s.ParentID
	s.Unlock()
	rctx, cancel := g.bg(ctx)
	defer cancel()
	seq, _ := g.Core.Record(rctx, kind, map[string]any{"task_id": task, "session": s.ID, "parent": derefStr(parent), "principal": person,
		"agent": agent, "decision": "allow", "target": s.ID, "args_sha256": core.ArgsDigest(args), "result": result}, false)
	return seq
}

// Expire closes every session past its deadline and deletes its run, and its children's, each with a record; then posts
// what waits in the outbox, if the core answers (Python: Gateway.expire). The run itself already ended at its deadline
// (the run backend enforces that without Clearance); this is the cleanup. A deletion that fails is recorded and retried by a
// later sweep, because the session keeps its run. It returns the sessions whose runs it actually tried to delete (not those waiting out a backoff).
//
// One deadline bounds the whole sweep (ctx): the closing of expired sessions comes first and needs no API server, so a slow
// one cannot delay it, and what the sweep has not reached is carried to the next.
func (g *Gateway) Expire(ctx context.Context) []string { return g.ExpireAt(ctx, g.Now()) }

// ExpireAt is Expire as of a given time (Python: expire(now)).
func (g *Gateway) ExpireAt(ctx context.Context, now time.Time) []string {
	g.deadlineOutcomes(now)
	g.Store.Sweep(now)
	g.backstopOutcomes(ctx, now)
	var pending []*session.Session
	for _, s := range g.Store.All() {
		s.Lock()
		if s.Run != nil && s.Closed {
			pending = append(pending, s)
		}
		s.Unlock()
	}
	sw := &sweepRun{now: now, seen: map[string]bool{}, acted: map[string]bool{}}
	for _, s := range pending {
		if g.Runs == nil || ctx.Err() != nil {
			break
		}
		s.Lock()
		why := "expired"
		if via, _ := s.Outcome["via"].(string); via == "runtime" || via == "run" {
			why = "finished"
		}
		hasRun := s.Run != nil
		s.Unlock()
		if hasRun {
			g.deleteRuns(ctx, s, s, why, sw)
		}
	}
	done := make([]string, 0, len(sw.acted))
	for id := range sw.acted {
		done = append(done, id)
	}
	sort.Strings(done)
	fctx, cancel := g.bg(ctx)
	if err := g.Store.FlushAll(fctx); err != nil {
		g.OnPersistError(err)
	}
	cancel()
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	if n := g.Core.Drain(dctx, 500); n > 0 {
		g.Log.Info("outbox drained", "posted", n)
	}
	cancel()
	return done
}

// deadlineOutcomes gives a session that reached its deadline the outcome "expired". It needs no API server and no session
// operation lock: a finish at the deadline is refused (FinishRun), so nothing writes an outcome beside it.
func (g *Gateway) deadlineOutcomes(now time.Time) {
	for _, s := range g.Store.All() {
		s.Lock()
		if !s.Closed && s.Outcome == nil && !now.Before(s.ExpiresAt) {
			s.Outcome = map[string]any{"status": "expired", "finished_at": core.Iso(s.ExpiresAt), "result": nil, "via": "deadline"}
		}
		s.Unlock()
	}
}

// MaxOutcomeReads is how many run states a sweep asks the API server for at once.
const MaxOutcomeReads = 8

type endedRead struct {
	s     *session.Session
	run   string
	epoch uint64
	mu    sync.Mutex
	done  bool // the read finished; phase and err are valid
	phase string
	err   error
}

// readGrace is how long past the sweep's deadline the sweep waits for a read that ignores its context before it goes on.
const readGrace = 2 * time.Second

// backstopOutcomes closes the sessions whose run ended without run_finish (an older runtime, or a crash): the run backend
// says how the workload ended, and the session closes with that as its outcome. The reads happen with no session lock held
// (a live call on the session is never stalled by a slow API server), a few at a time, within the sweep's deadline; the
// outcome is applied only if the session has not changed since it was read (no operation in flight, the same run, no run
// made or settled in between), so a run that was being created cannot be mistaken for one that is gone.
func (g *Gateway) backstopOutcomes(ctx context.Context, now time.Time) {
	if g.Runs == nil {
		return
	}
	var reads []*endedRead
	for _, s := range g.Store.All() {
		s.Lock()
		if !s.Closed && s.Outcome == nil && s.Run != nil && *s.Run != "" && now.Before(s.ExpiresAt) {
			reads = append(reads, &endedRead{s: s, run: *s.Run, epoch: s.RunEpochLocked()})
		}
		s.Unlock()
	}
	// The reads start after the last session the previous sweep reached, and wrap, so with a mostly hung API server the
	// sessions at the end of the list are not starved.
	g.readMu.Lock()
	cursor := g.readCursor
	g.readMu.Unlock()
	start := 0
	for start < len(reads) && reads[start].s.ID <= cursor {
		start++
	}
	reads = append(append([]*endedRead{}, reads[start:]...), reads[:start]...)
	sem := make(chan struct{}, MaxOutcomeReads)
	var wg sync.WaitGroup
	reached := ""
	complete := true
	for _, r := range reads {
		if ctx.Err() != nil {
			complete = false
			break // the rest wait for the next sweep
		}
		sem <- struct{}{}
		reached = r.s.ID
		wg.Add(1)
		go func(r *endedRead) {
			defer wg.Done()
			defer func() { <-sem }()
			rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			var phase string
			err := callBackend(func() (e error) { phase, e = g.Runs.Ended(rctx, r.s); return })
			r.mu.Lock()
			r.phase, r.err, r.done = firstChars(phase, 40), err, true
			r.mu.Unlock()
		}(r)
	}
	g.readMu.Lock()
	if complete {
		g.readCursor = ""
	} else {
		g.readCursor = reached
	}
	g.readMu.Unlock()
	// The wait has a deadline of its own: a backend that ignores its context cannot hang the sweep. A read that has not
	// finished by then is dropped for this sweep (its goroutine ends with its own timeout).
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	limit := 30 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		limit = time.Until(dl)
		if limit < 0 {
			limit = 0
		}
		limit += readGrace
	}
	select {
	case <-finished:
	case <-time.After(limit):
	}
	for _, r := range reads {
		r.mu.Lock()
		done, rerr, phase := r.done, r.err, r.phase
		r.mu.Unlock()
		if !done || rerr != nil || phase == "" {
			continue // asked again on the next sweep
		}
		func() {
			release, ok := r.s.TrySerial()
			if !ok {
				return // an operation is in flight on it; the next sweep looks again
			}
			defer release()
			s := r.s
			s.Lock()
			unchanged := !s.Closed && s.Outcome == nil && s.Run != nil && *s.Run == r.run && s.RunEpochLocked() == r.epoch
			principal := s.Principal
			if unchanged {
				s.Outcome = map[string]any{"status": "no-result", "run_phase": phase, "finished_at": core.Iso(now), "result": nil, "via": "run"}
			}
			s.Unlock()
			if !unchanged {
				return
			}
			g.record(ctx, "run.finish", s, principal, map[string]any{"run": r.run, "phase": phase}, "no-result", false) //nolint:errcheck
			g.flush(ctx, g.Store.CloseTouched(s.ID)...)
		}()
	}
}

// -- the gateway's own records ---------------------------------------------------------------------------------------

// record writes a runtime record of the session's (run.launch, run.delete, run.finish) through the core's record API: the
// fields the in-process engine wrote, less the ones the core sets, and the flight recorder's: the run's pinned model
// version and its definition's Preflight result on every one, and on run.finish the run's cost (Python: _record).
func (g *Gateway) record(ctx context.Context, kind string, s *session.Session, principalID string, args map[string]any, result string, required bool) (*int64, error) {
	s.Lock()
	task, id, parent, agent := s.TaskID, s.ID, s.ParentID, s.Agent
	s.Unlock()
	fields := map[string]any{"task_id": task, "session": id, "parent": derefStr(parent), "principal": principalID, "agent": agent,
		"decision": "allow", "args_sha256": core.ArgsDigest(args), "result": result}
	d := g.Def(agent)
	if mv := g.pinnedModel(d); mv != "" {
		fields["model_version"] = mv
	}
	fields["preflight"] = g.preflightOf(d, agent)
	if kind == "run.finish" {
		fields["cost"] = g.runCost(s)
	}
	rctx, cancel := g.bg(ctx)
	defer cancel()
	return g.Core.Record(rctx, kind, fields, required)
}

// PreflightResult is a definition's Preflight result: pass, fail or not-run, and how many cases it ran.
type PreflightResult struct {
	Result string
	Cases  *int64
}

// PreflightResults are the values a Preflight result may take (clearance evidence preflight.result).
var PreflightResults = []string{"pass", "fail", "not-run"}

func (g *Gateway) preflightOf(d *definitions.Definition, name string) map[string]any {
	out := map[string]any{"result": "not-run", "definition": name}
	if g.Preflight != nil && d != nil {
		if got := g.Preflight(d); got != nil && contains(PreflightResults, got.Result) {
			out["result"] = got.Result
			if got.Cases != nil {
				out["cases"] = *got.Cases
			}
		}
	}
	return out
}

// pinnedModel is the model version a run of d runs on: its first listed model (the one its runtime asks for) through this
// deployment's route to the upstream model, or the alias itself when there is no route. "" without a model.
func (g *Gateway) pinnedModel(d *definitions.Definition) string {
	if d == nil || len(d.RawProfile) == 0 {
		return ""
	}
	var p struct {
		Models []any `json:"models"`
	}
	if json.Unmarshal(d.RawProfile, &p) != nil || len(p.Models) == 0 {
		return ""
	}
	alias, ok := p.Models[0].(string)
	if !ok {
		return ""
	}
	version := alias
	if up, ok := g.ModelVersions[alias]; ok {
		version = up
	}
	if core.ValidModelVersion(version) {
		return version
	}
	return ""
}

// runCost is a run's cost when it ends (run.finish `cost`): its model usage as the proxy charged it, its model and tool
// calls, its wall seconds from open to now, and the compute class its session was granted.
func (g *Gateway) runCost(s *session.Session) map[string]any {
	s.Lock()
	defer s.Unlock()
	cost := map[string]any{}
	for _, k := range []string{"charged_tokens", "input_tokens", "output_tokens", "cache_creation_input_tokens",
		"cache_read_input_tokens", "model_calls"} {
		cost[k] = s.Usage[k]
	}
	cost["tool_calls"] = s.Used.ToolCalls
	wall := int64(g.Now().Sub(s.CreatedAt).Seconds())
	if wall < 0 {
		wall = 0
	}
	cost["wall_seconds"] = wall
	cost["compute"] = s.Limits.Compute
	return cost
}
