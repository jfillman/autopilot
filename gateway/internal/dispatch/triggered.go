package dispatch

// open_triggered (Gateway.open_triggered): the start of a scheduled or event-driven agent's run. The run's identity is the
// agent's own workload identity (the core decides it), the trigger that started it is recorded and never trusted: nothing a
// trigger source sends (an event's payload, a scheduler's clock) picks the principal, the agent, the budget or the tools. The
// agent is the one the trigger matched, the limits are the core's cut of the definition's, and the gateway checks the grant
// against the definition again (checkOpenGrant).
//
// Triggers fire once. The ledger (internal/triggers) claims a key before anything is opened and writes its decision after; this
// is the session half: a task id that is a hash of the agent and the key, so a live session of the same task is found
// (Store.FindLive), told to the core, and answered as a dedupe instead of a second run.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/clearance/client"
)

// MaxTriggerLen bounds the trigger name a source may put in a record (`glidepath:<event id>`, `interval:24h`).
const MaxTriggerLen = 300

// OpenTriggered opens the session of a triggered run: the core decides (as `trigger:<trigger>`, a workload at tier 1), with the
// agent's hold and any live session of the same task in the evaluation; an allowed open is a new session, or a dedupe of the live
// one. It answers {session, task_id, expires_at, deduped}; a refusal is the core's (R024 while the agent is held).
//
// The hold is read and the session adopted under the hold gate's read lock (see HoldTriggers).
func (g *Gateway) OpenTriggered(ctx context.Context, agent, trigger, taskID string) (*Result, error) {
	if len(trigger) > MaxTriggerLen {
		return nil, fmt.Errorf("a trigger name is at most %d characters", MaxTriggerLen)
	}
	now := g.Now()
	newID := g.Store.NewID()
	// The hold is read under the gate's read lock and the lock is let go: the gate is never held across the core's decision or a
	// save, so a hold (the emergency brake) never waits for a slow core. The hold is read again, under the lock, right before the
	// session is adopted.
	req, live, err := g.TriggeredRequestLive(ctx, agent, trigger, taskID, newID, now)
	if err != nil {
		g.Store.GiveBack(newID)
		return nil, err
	}
	d := g.Defs[agent]
	if d != nil && d.IdentityType == "workload" && (d.ServiceAccount == nil || *d.ServiceAccount == "") {
		// The core grants a triggered session the definition's service account as its principal ("" when it names none), and the
		// schema requires one of a workload definition: a definition without it has no identity a triggered run could have, so it is
		// refused here, before the core is asked, with the reason.
		g.Store.GiveBack(newID)
		msg := agent + " is a workload agent with no service account: a triggered run would have no identity"
		return &Result{Decision: map[string]any{"allow": false, "rule": "R008", "message": msg}, Error: str(msg)}, nil
	}
	v, err := g.Core.Evaluate(ctx, req)
	var un *core.Unreachable
	if errors.As(err, &un) {
		v, err = core.OutageVerdict(un, nil), nil
	}
	if err != nil {
		g.Store.GiveBack(newID)
		return nil, err
	}
	deny := func() (*Result, error) {
		g.Store.GiveBack(newID)
		return &Result{Decision: map[string]any{"allow": false, "rule": nilIfEmpty(v.Rule), "message": v.Message}, Error: str(v.Message), AuditSeq: v.Seq}, nil
	}
	if !v.Allow || v.Grant == nil {
		return deny()
	}
	gr := v.Grant
	if gr.Deduped {
		// The core says a live session of this task is the run. It must be the one this gateway told it about, and it must be a
		// session a trigger of THIS agent opened: the task id is a hash anybody can compute, so a session of a person's (or of
		// another principal's) that carries it is not a run to launch an event into. Python records no marker on a triggered
		// session, so what marks one is its principal: the definition's own workload identity, which only a trigger's open is
		// granted (a person's session is `user:...`, a run's own principal is the person's).
		g.Store.GiveBack(newID)
		if live == nil || gr.Session != live.ID {
			g.Log.Error("the core answered a trigger with a dedupe the gateway did not offer: refused", "agent", agent)
			return refusedGrant(v), nil
		}
		live.Lock()
		okLive := !live.Closed && live.Agent == agent && live.TaskID == taskID && live.ParentID == nil && d != nil && d.IdentityType == "workload" &&
			d.ServiceAccount != nil && live.Principal == *d.ServiceAccount
		out := map[string]any{"session": live.ID, "task_id": live.TaskID, "expires_at": core.Iso(live.ExpiresAt), "deduped": true}
		live.Unlock()
		if !okLive { // closed while the core was deciding, or not a session this trigger opened
			g.Log.Error("a dedupe onto a session that is not this agent's own trigger session: refused", "agent", agent, "session", live.ID)
			return refusedGrant(v), nil
		}
		return &Result{OK: true, Decision: map[string]any{"allow": true}, Data: out, AuditSeq: v.Seq, Session: &live.ID}, nil
	}
	if d == nil {
		g.Store.GiveBack(newID)
		return refusedGrant(v), nil // an agent that is not a live definition has nothing to check a grant against
	}
	if g.Store.Get(gr.Session) != nil {
		g.Store.GiveBack(newID)
		return refusedGrant(v), nil
	}
	// The principal of a triggered run is the definition's own workload identity (its service account) when it names one: the
	// trigger's sender never picks it.
	wantPrincipal := ""
	if d.ServiceAccount != nil {
		wantPrincipal = *d.ServiceAccount
	}
	if why := checkOpenGrant(gr, newID, wantPrincipal, d, now); why != "" || (taskID != "" && gr.TaskID != taskID) {
		g.Store.GiveBack(newID)
		g.Log.Error("the core's grant for a triggered run failed the gateway's own check: refused", "agent", agent, "why", why)
		return refusedGrant(v), nil
	}
	// Adopt under the gate, after reading the hold again: a hold that was saved while the core was deciding refuses the open here
	// (nothing is adopted), and one saved after this point is the launch's to catch (LaunchTriggered).
	g.holdGate.RLock()
	held, herr := g.holdForLocked(ctx, agent)
	var s *session.Session
	if herr == nil && held == nil {
		s = g.Store.Adopt(gr, now, nil, 0)
	}
	g.holdGate.RUnlock()
	if herr != nil {
		g.Store.GiveBack(newID)
		return nil, herr
	}
	if held != nil {
		g.Store.GiveBack(newID)
		return heldResult(agent, held, v), nil
	}
	g.flush(ctx, s)
	s.Lock()
	out := map[string]any{"session": s.ID, "task_id": s.TaskID, "expires_at": core.Iso(s.ExpiresAt), "deduped": false}
	s.Unlock()
	return &Result{OK: true, Decision: map[string]any{"allow": true}, Data: out, AuditSeq: v.Seq, Session: &s.ID}, nil
}

// heldResult is the refusal of an open or a launch that a hold stopped after the core had decided (R024, as the core's own).
func heldResult(agent string, h *Hold, v *core.Verdict) *Result {
	msg := fmt.Sprintf("triggers held by %s until %s: %s", h.By, core.Iso(h.Until), h.Reason)
	r := &Result{Decision: map[string]any{"allow": false, "rule": "R024", "message": msg}, Error: str(msg)}
	if v != nil {
		r.AuditSeq = v.Seq
	}
	return r
}

// HoldReadError is a failure to read the hold (not a hold): the trigger is neither launched nor decided, and may be resumed.
type HoldReadError struct{ Err error }

func (e *HoldReadError) Error() string { return "the hold could not be read: " + e.Err.Error() }
func (e *HoldReadError) Unwrap() error { return e.Err }

// HoldReadWaits are the pauses between the attempts at reading the hold before a launch gives up with a HoldReadError.
var HoldReadWaits = []time.Duration{50 * time.Millisecond, 250 * time.Millisecond, time.Second}

func (g *Gateway) holdForRetry(ctx context.Context, agent string) (*Hold, error) {
	h, err := g.HoldFor(ctx, agent)
	for _, w := range HoldReadWaits {
		if err == nil || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(w):
		}
		h, err = g.HoldFor(ctx, agent)
	}
	if err != nil {
		return nil, &HoldReadError{err}
	}
	return h, nil
}

// LaunchTriggered launches the run of a session a trigger opened, with the hold read again right before the launch and after it: a
// session opened before a hold was acknowledged must not run after it. A held agent's session is closed and the answer is R024; a
// launch that raced a hold (the run was made, and the hold was saved meanwhile) is undone: the session is closed and its run deleted.
// A session that already names a run is not refused (the launch is resumed): a run that exists is the answer (Data "existing"), one that
// was only reserved is made. A hold that cannot be read is a *HoldReadError after bounded retries.
func (g *Gateway) LaunchTriggered(ctx context.Context, agent, sessionID string, input map[string]any) (*Result, error) {
	var res *Result
	var err error
	named := false
	if s := g.Store.Get(sessionID); s != nil {
		s.Lock()
		named = s.Run != nil && *s.Run != "" && !s.Closed
		s.Unlock()
	}
	if named {
		// A run is named: the run that is there is the answer, whatever a hold now says; one that was only reserved is made.
		if res, err = g.launch(ctx, sessionID, input, true); err != nil || !res.OK || res.Data.(map[string]any)[ExistingRunKey] == true {
			return res, err
		}
	} else {
		var h *Hold
		if h, err = g.holdForRetry(ctx, agent); err != nil {
			return nil, err
		}
		if h != nil {
			g.AbandonTriggered(ctx, sessionID, false)
			return heldResult(agent, h, nil), nil
		}
		if res, err = g.launch(ctx, sessionID, input, true); err != nil || !res.OK {
			return res, err
		}
	}
	h, herr := g.HoldFor(ctx, agent)
	if herr != nil || h == nil {
		return res, nil
	}
	if s := g.Store.Get(sessionID); s != nil {
		g.flush(ctx, g.deleteRuns(ctx, s, s, "closed", nil)...)
	}
	return heldResult(agent, h, nil), nil
}

// TriggerSession is what a trigger needs to know of a session an earlier holder of its claim opened.
type TriggerSession struct {
	Known, Live bool
	Run         string // the run it names ("" none); a name may be only reserved
	Launched    bool   // its run was created (LaunchedAt)
	Frozen      bool   // tripped
	Ended       bool   // closed by someone (a person's end, a parent's close), not by its deadline
}

// TriggerSessionState reads a session for a trigger that takes over a claim.
func (g *Gateway) TriggerSessionState(sessionID string) TriggerSession {
	s := g.Store.Get(sessionID)
	if s == nil {
		return TriggerSession{}
	}
	s.Lock()
	defer s.Unlock()
	t := TriggerSession{Known: true, Live: !s.Expired(g.Now()), Launched: s.LaunchedAt != nil, Frozen: s.Tripped}
	if s.Run != nil {
		t.Run = *s.Run
	}
	t.Ended = s.Closed && (s.Outcome == nil || s.Outcome["via"] != "deadline") && g.Now().Before(s.ExpiresAt)
	return t
}

func refusedGrant(v *core.Verdict) *Result {
	return &Result{Decision: map[string]any{"allow": false, "rule": core.RefusedRule, "message": "Clearance's grant was refused"},
		Error: str("Clearance's grant was refused"), AuditSeq: v.Seq}
}

// TriggeredRequest is the core's evaluation of a triggered open as OpenTriggered builds it (Gateway.open_triggered): the agent's
// hold and any live session of the same task are in the context. Exported for the replay.
func (g *Gateway) TriggeredRequest(ctx context.Context, agent, trigger, taskID, newID string, now time.Time) (*client.EvaluationRequest, error) {
	req, _, err := g.TriggeredRequestLive(ctx, agent, trigger, taskID, newID, now)
	return req, err
}

// TriggeredRequestLive is TriggeredRequest, and the live session of the task it showed the core, if any. The hold is read under
// the gate's read lock, which is let go before it returns.
func (g *Gateway) TriggeredRequestLive(ctx context.Context, agent, trigger, taskID, newID string, now time.Time) (*client.EvaluationRequest, *session.Session, error) {
	g.holdGate.RLock()
	defer g.holdGate.RUnlock()
	return g.triggeredRequest(ctx, agent, trigger, taskID, newID, now)
}

// triggeredRequest builds the evaluation; the caller holds the hold gate. live is the live session of the task, if any.
func (g *Gateway) triggeredRequest(ctx context.Context, agent, trigger, taskID, newID string, now time.Time) (*client.EvaluationRequest, *session.Session, error) {
	var held *Hold
	if g.Defs[agent] != nil {
		var err error
		if held, err = g.holdForLocked(ctx, agent); err != nil {
			return nil, nil, err
		}
	}
	var live *session.Session
	if taskID != "" {
		live = g.Store.FindLive(agent, taskID, now)
	}
	req := &client.EvaluationRequest{
		Subject:  client.Subject{Type: "principal", ID: "trigger:" + trigger, Properties: client.SubjectProperties{Kind: "workload", TierCeiling: 1}},
		Action:   client.Action{Name: "session.open", Properties: client.ActionProperties{TaskID: taskID, Trigger: trigger}},
		Resource: client.Resource{Type: "agent", ID: agent},
		Context:  client.Context{Now: now, NewSessionID: newID},
	}
	if def := g.Def(agent); def != nil {
		req.Context.Agent = def.AgentDef()
	}
	if held != nil {
		req.Context.Hold = &client.Hold{By: held.By, Until: held.Until, Reason: held.Reason}
	}
	if live != nil {
		live.Lock()
		req.Context.Live = live.State()
		live.Unlock()
	}
	return req, live, nil
}

// Definitions is the definition set (the triggers read it for what each agent's triggers are).
func (g *Gateway) Definitions() map[string]*definitions.Definition { return g.Defs }

// SessionState says whether a session is known to this gateway, whether it is live (not closed, not past its deadline) and the run
// it names, "" when it has none.
func (g *Gateway) SessionState(sessionID string) (known, live bool, run string) {
	s := g.Store.Get(sessionID)
	if s == nil {
		return false, false, ""
	}
	s.Lock()
	defer s.Unlock()
	if s.Run != nil {
		run = *s.Run
	}
	return true, !s.Expired(g.Now()), run
}

// SessionHasRun says whether a live session has a run (its name written, or made): a session a trigger opened and nothing launched
// has none.
func (g *Gateway) SessionHasRun(sessionID string) bool {
	s := g.Store.Get(sessionID)
	if s == nil {
		return false
	}
	s.Lock()
	defer s.Unlock()
	return s.Run != nil && *s.Run != ""
}

// AbandonTriggered closes a session a trigger opened and could not launch, so it does not stay live, with its budget, until its
// deadline: the trigger's decision is final (launch-failed) and nothing will launch it. Python leaves it live. failed says the
// launch raised (a backend error): the session may then name a run that may exist (its name is written before the run is created),
// and closing it is what lets the sweep delete that run. Otherwise (the launch was refused) a session that names a run is left
// alone: it has one. A session that is not found is ignored.
func (g *Gateway) AbandonTriggered(ctx context.Context, sessionID string, failed bool) {
	s := g.Store.Get(sessionID)
	if s == nil {
		return
	}
	defer s.Serial()()
	s.Lock()
	has := s.Run != nil && *s.Run != ""
	s.Unlock()
	if has && !failed {
		return
	}
	g.flush(ctx, g.Store.CloseTouched(sessionID)...)
}
