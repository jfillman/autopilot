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
	g.holdGate.RLock()
	defer g.holdGate.RUnlock()
	now := g.Now()
	newID := g.Store.NewID()
	req, live, err := g.triggeredRequest(ctx, agent, trigger, taskID, newID, now)
	if err != nil {
		g.Store.GiveBack(newID)
		return nil, err
	}
	d := g.Defs[agent]
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
		// The core says a live session of this task is the run. It must be the one this gateway told it about: anything else is
		// not a dedupe the gateway can stand behind.
		g.Store.GiveBack(newID)
		if live == nil || gr.Session != live.ID {
			g.Log.Error("the core answered a trigger with a dedupe the gateway did not offer: refused", "agent", agent)
			return refusedGrant(v), nil
		}
		live.Lock()
		gone := live.Closed
		out := map[string]any{"session": live.ID, "task_id": live.TaskID, "expires_at": core.Iso(live.ExpiresAt), "deduped": true}
		live.Unlock()
		if gone { // closed while the core was deciding: not a run a trigger can be answered with
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
	s := g.Store.Adopt(gr, now, nil, 0)
	g.flush(ctx, s)
	s.Lock()
	out := map[string]any{"session": s.ID, "task_id": s.TaskID, "expires_at": core.Iso(s.ExpiresAt), "deduped": false}
	s.Unlock()
	return &Result{OK: true, Decision: map[string]any{"allow": true}, Data: out, AuditSeq: v.Seq, Session: &s.ID}, nil
}

func refusedGrant(v *core.Verdict) *Result {
	return &Result{Decision: map[string]any{"allow": false, "rule": core.RefusedRule, "message": "Clearance's grant was refused"},
		Error: str("Clearance's grant was refused"), AuditSeq: v.Seq}
}

// TriggeredRequest is the core's evaluation of a triggered open as OpenTriggered builds it (Gateway.open_triggered): the agent's
// hold and any live session of the same task are in the context. Exported for the replay.
func (g *Gateway) TriggeredRequest(ctx context.Context, agent, trigger, taskID, newID string, now time.Time) (*client.EvaluationRequest, error) {
	g.holdGate.RLock()
	defer g.holdGate.RUnlock()
	req, _, err := g.triggeredRequest(ctx, agent, trigger, taskID, newID, now)
	return req, err
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
