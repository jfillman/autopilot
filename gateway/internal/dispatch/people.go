package dispatch

// Person controls (engine/control.py and the control parts of engine/dispatch.py): freeze or end a run, hold or release an
// agent's triggers, a person's own run (run_agent) and Preflight's candidate runs. Decided 2026-10-31 (the delegate model):
// a person signs in themselves, or a delegate (Tower's backend) calls as itself and names them in X-Hangar-On-Behalf-Of.
// Clearance decides who may act, and so does this file, because nothing it does is undone by the core's own checks:
//
//   - an agent's owner group (the definition's `owner`, a Backstage group the Directory resolves),
//   - a platform admin (AUTOPILOT_ADMINS, principal globs),
//   - and for a session, the person whose session it is.
//
// Only a person acts. A caller of kind "user" is a person (their own sign-in). A workload is not: a run, Tower's backend and
// every other service account is refused unless it is a listed delegate AND names a person (on-behalf-of "user:..."), so a run
// can never answer its own human.request, release its own hold, freeze, end or run anything: it is not in the delegates list,
// and the list is the deployment's. Every control is an evidence record naming the person and the delegate.
//
// A hold is an emergency brake with an expiry (24 h by default, renewable, at most 14 days); anything meant to last is a
// definition PR. While held, open_triggered refuses with R024.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/glob"
	"github.com/jfillman/autopilot/gateway/internal/session"
)

// Hold limits (control.py).
const (
	DefaultHoldHours = 24
	MaxHoldHours     = 24 * 14
	MaxOnBehalfOf    = 200
)

// Hold is a hold on an agent's triggers.
type Hold struct {
	Agent  string
	By     string  // the person (user:<login>)
	Via    *string // the delegate they acted through, if any
	Reason string
	Since  time.Time
	Until  time.Time
}

// Active is whether the hold still holds at now.
func (h *Hold) Active(now time.Time) bool { return now.Before(h.Until) }

// View is the hold as the row's JSON document and the controls' answer show it (Hold.view).
func (h *Hold) View() map[string]any {
	return map[string]any{"agent": h.Agent, "by": h.By, "via": optStr(h.Via), "reason": h.Reason, "since": PyIso(h.Since), "until": PyIso(h.Until)}
}

// HoldFromRecord is the inverse of View, for a row either gateway wrote.
func HoldFromRecord(m map[string]any) (*Hold, error) {
	str := func(k string) string { s, _ := m[k].(string); return s }
	since, err := ParsePyIso(str("since"))
	if err != nil {
		return nil, err
	}
	until, err := ParsePyIso(str("until"))
	if err != nil {
		return nil, err
	}
	h := &Hold{Agent: str("agent"), By: str("by"), Reason: str("reason"), Since: since, Until: until}
	if v, ok := m["via"].(string); ok {
		h.Via = &v
	}
	return h, nil
}

// HoldStore keeps the holds (control.HoldStore). Get returns nil when there is none.
type HoldStore interface {
	Get(ctx context.Context, agent string) (*Hold, error)
	Put(ctx context.Context, h *Hold) error
	Delete(ctx context.Context, agent string) error
	All(ctx context.Context) ([]*Hold, error)
}

// MemoryHolds is a HoldStore in memory (tests).
type MemoryHolds struct {
	mu sync.Mutex
	h  map[string]*Hold
}

// Get is a copy of the agent's hold, or nil.
func (m *MemoryHolds) Get(_ context.Context, agent string) (*Hold, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h := m.h[agent]; h != nil {
		c := *h
		return &c, nil
	}
	return nil, nil
}

// Put sets the agent's hold.
func (m *MemoryHolds) Put(_ context.Context, h *Hold) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.h == nil {
		m.h = map[string]*Hold{}
	}
	c := *h
	m.h[h.Agent] = &c
	return nil
}

// Delete removes the agent's hold.
func (m *MemoryHolds) Delete(_ context.Context, agent string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.h, agent)
	return nil
}

// All lists every hold.
func (m *MemoryHolds) All(_ context.Context) ([]*Hold, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Hold
	for _, h := range m.h {
		c := *h
		out = append(out, &c)
	}
	return out, nil
}

// CheckHours is control.check_hours: a whole number of hours, 1 to MaxHoldHours; nothing means the default. Python's int() is
// followed: a number is cut to a whole, a numeric string is read, a boolean is 1 or 0.
func CheckHours(hours any) (int, error) {
	bad := func(msg, hint string) error { return &ControlError{"CT-HOURS", msg, hint, 400} }
	var n int64
	switch x := hours.(type) {
	case nil:
		return DefaultHoldHours, nil
	case string:
		if x == "" {
			return DefaultHoldHours, nil
		}
		v, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return 0, bad("hours is a whole number", "")
		}
		n = v
	case bool:
		if x {
			n = 1
		}
	case json.Number:
		if v, err := x.Int64(); err == nil {
			n = v
		} else if f, ferr := x.Float64(); ferr == nil && !math.IsInf(f, 0) && !math.IsNaN(f) && math.Abs(f) < 1e15 {
			n = int64(f)
		} else {
			return 0, bad("hours is a whole number", "")
		}
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) || math.Abs(x) >= 1e15 {
			return 0, bad("hours is a whole number", "")
		}
		n = int64(x)
	case int:
		n = int64(x)
	case int64:
		n = x
	default:
		return 0, bad("hours is a whole number", "")
	}
	if n < 1 || n > MaxHoldHours {
		return 0, bad(fmt.Sprintf("hours is 1 to %d", MaxHoldHours), "A hold is a brake, not a policy: anything longer is a definition PR.")
	}
	return int(n), nil
}

// MayManage is control.may_manage: the agent's owner group, or a platform admin.
func MayManage(person string, owner *string, groups map[string]bool, admins []string) bool {
	return (owner != nil && groups[*owner]) || glob.MatchAny(person, admins)
}

// Person is who is acting, and through which delegate: a person signed in themselves, or a delegate naming one (Gateway._person).
// The token is verified first, and a token the authenticator refuses is a *core.AuthError whatever else is asked.
//
// A caller that names a person must be a listed delegate (a glob of AUTOPILOT_DELEGATES) and the name must be user:<name>; a
// caller that names nobody must be a user. So a workload that is not a delegate (every run) is refused both ways, and a
// delegate's own identity is never the person.
func (g *Gateway) Person(ctx context.Context, token, onBehalfOf string) (person, via string, err error) {
	p, err := g.Auth.Authenticate(ctx, token)
	if err != nil {
		return "", "", err
	}
	if onBehalfOf != "" {
		if !glob.MatchAny(p.ID, g.Delegates) {
			return "", "", &InboxError{"HR-DELEGATE", p.ID + " may not act for a person", "", 403}
		}
		if !strings.HasPrefix(onBehalfOf, "user:") || len(onBehalfOf) > MaxOnBehalfOf || len(onBehalfOf) == len("user:") || strings.ContainsAny(onBehalfOf, "\x00\r\n") {
			return "", "", &InboxError{"HR-PERSON", "on-behalf-of must name a person (user:<name>)", "", 400}
		}
		return onBehalfOf, p.ID, nil
	}
	if p.Kind != "user" {
		return "", "", &InboxError{"HR-PERSON", p.ID + " is not a person", "", 403}
	}
	return p.ID, "", nil
}

// personFor is Person for a control: the inbox refusals are the control's (CT-...).
func (g *Gateway) personFor(ctx context.Context, token, onBehalfOf string) (string, string, error) {
	person, via, err := g.Person(ctx, token, onBehalfOf)
	var ie *InboxError
	if errors.As(err, &ie) {
		return "", "", &ControlError{strings.Replace(ie.Rule, "HR-", "CT-", 1), ie.Message, ie.Hint, ie.Status}
	}
	return person, via, err
}

// mayManage checks that the person may manage the agent: its owner group, a platform admin, or for a session the person whose
// session it is. The refusal is the same for every person that may not.
func (g *Gateway) mayManage(ctx context.Context, person, agent string, s *session.Session) error {
	if s != nil {
		s.Lock()
		own := s.Principal == person
		s.Unlock()
		if own {
			return nil
		}
	}
	var owner *string
	if d := g.Defs[agent]; d != nil {
		owner = d.Owner
	}
	if !MayManage(person, owner, g.groupsOf(ctx, person), g.Admins) {
		return &ControlError{"CT-FORBIDDEN", person + " may not manage " + agent,
			"Only the agent's owner group, a platform admin, or the person whose session it is may.", 403}
	}
	return nil
}

// personRecord is a person control's record (session.freeze, session.end, trigger.hold, trigger.release): the control has already
// taken effect, so an unreachable core queues the record and never undoes it (Python: Gateway._control_record).
func (g *Gateway) personRecord(ctx context.Context, person, via, kind string, s *session.Session, agent, target string, args map[string]any, result string) *int64 {
	fields := map[string]any{"principal": person, "agent": agent, "decision": "allow", "target": target, "args_sha256": core.ArgsDigest(args),
		"result": result + viaSuffix(via)}
	if s != nil {
		s.Lock()
		fields["task_id"], fields["session"], fields["parent"] = s.TaskID, s.ID, derefStr(s.ParentID)
		s.Unlock()
	}
	rctx, cancel := g.bg(ctx)
	defer cancel()
	seq, _ := g.Core.Record(rctx, kind, fields, false)
	return seq
}

// FreezeSession trips a live session's breaker by hand: every further call of the run (and its children) is refused; the run
// stays for a person to read. Recorded as session.freeze (Gateway.freeze_session).
//
// The check that the session is live and not yet frozen, and the trip, are one step under the session's lock, so two freezes
// of one session give one freeze and one CT-TRIPPED, and a session that was ended meanwhile is CT-CLOSED. It does not wait for
// a call in flight on the session (a brake that waited for the call it is braking would not be one): the next call sees it.
func (g *Gateway) FreezeSession(ctx context.Context, token, sessionID string, reason any, onBehalfOf string) (map[string]any, error) {
	person, via, err := g.personFor(ctx, token, onBehalfOf)
	if err != nil {
		return nil, err
	}
	s := g.Store.Get(sessionID)
	if s == nil {
		return nil, &ControlError{"CT-UNKNOWN", "no such session", "", 404}
	}
	s.Lock()
	agent := s.Agent
	s.Unlock()
	if err := g.mayManage(ctx, person, agent, s); err != nil {
		return nil, err
	}
	text, err := CheckReason(reason)
	if err != nil {
		return nil, err
	}
	why := fmt.Sprintf("frozen by %s: %s", person, text)
	s.Lock()
	switch {
	case s.Closed:
		s.Unlock()
		return nil, &ControlError{"CT-CLOSED", "this session is already closed", "", 409}
	case s.Tripped:
		prev := ""
		if s.TripReason != nil {
			prev = *s.TripReason
		}
		s.Unlock()
		return nil, &ControlError{"CT-TRIPPED", "this session is already frozen: " + prev, "", 409}
	}
	s.Trip(why)
	reasonOut := why
	s.Unlock()
	g.Store.TripTree(s.ID, why) // its children: "parent tripped: ..."
	g.flush(ctx, append([]*session.Session{s}, g.sessionsOf(g.descendants(s))...)...)
	rec := g.personRecord(ctx, person, via, "session.freeze", s, agent, s.ID, map[string]any{"reason": text}, "frozen: "+text)
	return map[string]any{"session": s.ID, "agent": agent, "tripped": true, "trip_reason": reasonOut, "audit_seq": nilSeq(rec)}, nil
}

func (g *Gateway) sessionsOf(ids []string) []*session.Session {
	var out []*session.Session
	for _, id := range ids {
		if x := g.Store.Get(id); x != nil {
			out = append(out, x)
		}
	}
	return out
}

// EndSession closes a session (and its children) by hand: its budget is settled, every further call is refused. The run's own
// pod ends with the next expiry sweep, which deletes the run of a closed session (or the AgentRun's TTL). Recorded as session.end
// (Gateway.end_session).
//
// Exactly one of two Ends closes the session: the close is the store's, under the session's lock, and an End that did not do
// it is CT-CLOSED and writes nothing. A close that races a spawn or a launch is the run tree's (the operation in flight checks
// again and undoes what it made).
func (g *Gateway) EndSession(ctx context.Context, token, sessionID string, reason any, onBehalfOf string) (map[string]any, error) {
	person, via, err := g.personFor(ctx, token, onBehalfOf)
	if err != nil {
		return nil, err
	}
	s := g.Store.Get(sessionID)
	if s == nil {
		return nil, &ControlError{"CT-UNKNOWN", "no such session", "", 404}
	}
	s.Lock()
	agent := s.Agent
	s.Unlock()
	if err := g.mayManage(ctx, person, agent, s); err != nil {
		return nil, err
	}
	text, err := CheckReason(reason)
	if err != nil {
		return nil, err
	}
	touched := g.Store.CloseTouched(s.ID)
	mine := false
	for _, t := range touched {
		mine = mine || t == s
	}
	if !mine {
		return nil, &ControlError{"CT-CLOSED", "this session is already closed", "", 409}
	}
	g.flush(ctx, touched...)
	rec := g.personRecord(ctx, person, via, "session.end", s, agent, s.ID, map[string]any{"reason": text}, "ended: "+text)
	return map[string]any{"session": s.ID, "agent": agent, "closed": true, "audit_seq": nilSeq(rec)}, nil
}

// holdForLocked is the active hold on an agent's triggers, if any. The caller holds the hold gate (read or write). An expired
// hold is not dropped here (the sweep drops it): a reader must not delete under a read lock.
func (g *Gateway) holdForLocked(ctx context.Context, agent string) (*Hold, error) {
	if g.Holds == nil {
		return nil, nil
	}
	h, err := g.Holds.Get(ctx, agent)
	if err != nil || h == nil || !h.Active(g.Now()) {
		return nil, err
	}
	return h, nil
}

// HoldFor is the active hold on an agent's triggers, if any (Gateway.hold_for).
func (g *Gateway) HoldFor(ctx context.Context, agent string) (*Hold, error) {
	g.holdGate.RLock()
	defer g.holdGate.RUnlock()
	return g.holdForLocked(ctx, agent)
}

// HoldTriggers stops an agent's triggers starting runs, for `hours` (24 by default, 336 at most); renewing replaces the hold.
// open_triggered refuses with R024 while it lasts. Recorded as trigger.hold (Gateway.hold_triggers).
//
// The hold is saved under the hold gate's write lock, and a trigger's open (and a person's run) reads the hold and opens its
// session under the read lock. So when this returns, no open that read "no hold" is still in flight, and every later open sees
// the hold: an open that was already past the check is finished first, never half-undone.
func (g *Gateway) HoldTriggers(ctx context.Context, token, agent string, reason, hours any, onBehalfOf string) (map[string]any, error) {
	person, via, err := g.personFor(ctx, token, onBehalfOf)
	if err != nil {
		return nil, err
	}
	if g.Holds == nil {
		return nil, &ControlError{"CT-OFF", "this deployment keeps no holds", "", 404}
	}
	if g.Defs[agent] == nil {
		return nil, &ControlError{"CT-UNKNOWN", "no such agent", "", 404}
	}
	if err := g.mayManage(ctx, person, agent, nil); err != nil {
		return nil, err
	}
	text, err := CheckReason(reason)
	if err != nil {
		return nil, err
	}
	n, err := CheckHours(hours)
	if err != nil {
		return nil, err
	}
	if err := g.rateLimited(person, "holds"); err != nil {
		return nil, err
	}
	now := g.Now()
	h := &Hold{Agent: agent, By: person, Reason: text, Since: now, Until: now.Add(time.Duration(n) * time.Hour)}
	if via != "" {
		h.Via = &via
	}
	g.holdGate.Lock()
	err = g.Holds.Put(ctx, h)
	g.holdGate.Unlock()
	if err != nil {
		return nil, err
	}
	rec := g.personRecord(ctx, person, via, "trigger.hold", nil, agent, agent, map[string]any{"reason": text, "hours": n},
		fmt.Sprintf("held until %s: %s", core.Iso(h.Until), text))
	out := h.View()
	out["audit_seq"] = nilSeq(rec)
	return out, nil
}

// ReleaseTriggers lifts a hold before it expires. Recorded as trigger.release (Gateway.release_triggers). Whether an agent is held
// is told only to a person who may manage it (Python answers "not held" before it checks who is asking).
func (g *Gateway) ReleaseTriggers(ctx context.Context, token, agent, onBehalfOf string) (map[string]any, error) {
	person, via, err := g.personFor(ctx, token, onBehalfOf)
	if err != nil {
		return nil, err
	}
	if err := g.mayManage(ctx, person, agent, nil); err != nil {
		return nil, err
	}
	g.holdGate.Lock()
	h, err := g.holdForLocked(ctx, agent)
	if err == nil && h != nil {
		err = g.Holds.Delete(ctx, agent)
	}
	g.holdGate.Unlock()
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, &ControlError{"CT-NOHOLD", agent + " is not held", "", 404}
	}
	rec := g.personRecord(ctx, person, via, "trigger.release", nil, agent, agent, map[string]any{},
		fmt.Sprintf("released (held by %s since %s: %s)", h.By, core.Iso(h.Since), h.Reason))
	return map[string]any{"agent": agent, "released": true, "audit_seq": nilSeq(rec)}, nil
}

// PruneHolds drops the holds that have expired (Python drops one when it reads it). The sweep calls it.
func (g *Gateway) PruneHolds(ctx context.Context) int {
	if g.Holds == nil {
		return 0
	}
	g.holdGate.Lock()
	defer g.holdGate.Unlock()
	all, err := g.Holds.All(ctx)
	if err != nil {
		return 0
	}
	n := 0
	for _, h := range all {
		if !h.Active(g.Now()) && g.Holds.Delete(ctx, h.Agent) == nil {
			n++
		}
	}
	return n
}

// MaxScoreSessions is how many sessions one case of a score names at most (Python: unbounded; the body is 64 KiB).
const MaxScoreSessions = 50

// maxCandidateRuns is PreflightKeep: candidate runs remembered for scoring.

// RunAgent is a person starting an in-cluster run of a task agent (Tower's Run, or their own token): a session opened as the
// person, then Launch with their input as the run's task. Only a task agent with a runtime image; only the agent's owner group
// or a platform admin (the person controls' rule). With a `document` (a candidate definition, a PR's) and an optional `budget`, a
// budgeted Preflight run of that candidate instead. Recorded as session.open (via the delegate) and run.launch (Gateway.run_agent).
//
// A candidate document is untrusted input to the definition loader. This gateway does not carry the definition schema (a later
// slice), so Candidate is not supplied a validated parse and a document is refused (CT-NOTCARRIED, 501) after the person has been
// checked; a live agent's run does not need it.
func (g *Gateway) RunAgent(ctx context.Context, token, agent string, runInput any, onBehalfOf string, document, budget any) (map[string]any, error) {
	person, via, err := g.personFor(ctx, token, onBehalfOf)
	if err != nil {
		return nil, err
	}
	d, fp := g.Defs[agent], ""
	switch {
	case document != nil:
		// A candidate under Preflight: the person must manage the live agent (or be an admin, for a new one).
		if err := g.mayManage(ctx, person, agent, nil); err != nil {
			return nil, err
		}
		var cerr error
		if d, fp, cerr = g.Candidate(agent, document, budget); cerr != nil {
			if errors.Is(cerr, ErrNotCarried) {
				return nil, &ControlError{"CT-NOTCARRIED", "candidate definitions are validated against the definition schema, which this gateway does not carry yet (A-9 S4)",
					"Run Preflight against the Python gateway, or run the live definition.", 501}
			}
			return nil, cerr
		}
	case d == nil:
		return nil, &ControlError{"R003", "no such agent", "", 404}
	default:
		if err := g.mayManage(ctx, person, agent, nil); err != nil {
			return nil, err
		}
	}
	if err := g.rateLimited(person, "runs"); err != nil {
		return nil, err
	}
	return g.runAgentAs(ctx, person, via, agent, d, fp, runInput, document == nil)
}

// -- Preflight's routes -----------------------------------------------------------------------------------------------------

// PreflightCaseList is the Preflight cases of an agent, for the PR check that launches their runs: id, title, the run's input and
// how many runs the case reads. Any authenticated caller; the cases are in the repo (Gateway.preflight_case_list).
func (g *Gateway) PreflightCaseList(ctx context.Context, token string, agent any) (map[string]any, error) {
	if _, err := g.Auth.Authenticate(ctx, token); err != nil {
		return nil, err
	}
	name, ok := agent.(string)
	if !ok || name == "" {
		return nil, &ControlError{"R008", "agent is the definition's name", "", 400}
	}
	cases := []map[string]any{}
	for _, c := range CasesFor(g.PreflightCases, name) {
		cases = append(cases, map[string]any{"id": c.ID, "title": c.Title, "input": c.Input, "runs": c.Runs})
	}
	return map[string]any{"agent": name, "cases": cases}, nil
}

// PreflightScore scores the Preflight runs of a candidate: {"agent": name, "cases": [{"case": id, "sessions": [sid, ...]}]} (one
// session per run the case reads, in order). Each case's results are scored with the session's model usage as the evidence; when
// every case has finished, the definition's result lands in the ledger (`pass` only if every case passed) and so on the run
// records of that definition while it is unchanged (Gateway.preflight_score). Only a person who manages the agent may score.
func (g *Gateway) PreflightScore(ctx context.Context, token string, body any, onBehalfOf string) (map[string]any, error) {
	person, _, err := g.personFor(ctx, token, onBehalfOf)
	if err != nil {
		return nil, err
	}
	b, _ := body.(map[string]any)
	agent, _ := b["agent"].(string)
	asked, _ := b["cases"].([]any)
	if agent == "" || len(asked) == 0 || len(asked) > 50 {
		return nil, &ControlError{"R008", `body is {"agent": name, "cases": [{"case": id, "sessions": [id, ...]}]}`, "", 400}
	}
	if err := g.mayManage(ctx, person, agent, nil); err != nil {
		return nil, err
	}
	scores := []any{}
	finished := true
	fps := map[string]bool{}
	for _, it := range asked {
		item, _ := it.(map[string]any)
		cid, _ := item["case"].(string)
		sids, _ := item["sessions"].([]any)
		c := g.PreflightCases[cid]
		if c == nil || c.Agent != agent {
			return nil, &ControlError{"R003", fmt.Sprintf("no Preflight case %s for %s", pyRepr(item["case"]), agent), "", 404}
		}
		ids := []string{}
		for _, x := range sids {
			sid, ok := x.(string)
			if !ok {
				ids = nil
				break
			}
			ids = append(ids, sid)
		}
		if len(ids) == 0 || len(ids) > MaxScoreSessions {
			return nil, &ControlError{"R008", "sessions is the list of the case's run sessions", "", 400}
		}
		var results, ev []map[string]any
		unfinished := []string{}
		for _, sid := range ids {
			_, _, fp, ok := g.candidateRunOf(sid, agent)
			if !ok {
				return nil, &ControlError{"R003", fmt.Sprintf("%s is not a Preflight run of %s", sid, agent), "", 404}
			}
			fps[fp] = true
			var r map[string]any
			if g.Results != nil {
				if r, err = g.Results.Get(ctx, sid, g.Now()); err != nil {
					return nil, err
				}
			}
			sess := g.Store.Get(sid)
			if r == nil && sess != nil {
				sess.Lock()
				if sess.Outcome != nil { // ended without handing a result back
					r = map[string]any{"status": sess.Outcome["status"], "final_text": "", "tool_calls": []any{}}
				}
				sess.Unlock()
			}
			if r == nil {
				unfinished = append(unfinished, sid)
				continue
			}
			results = append(results, r)
			// The run's refused calls are denials in the evidence (a refused call to a tripwire tool is a tripwire attempt): without
			// them the usage record alone would hide them from the score.
			for _, c := range callsOf(r) {
				if !truthy(c["ok"]) && truthy(c["rule"]) {
					ev = append(ev, map[string]any{"tool": c["tool"], "decision": "deny", "rule": c["rule"]})
				}
			}
			if sess != nil {
				sess.Lock()
				ct := sess.Usage["charged_tokens"]
				sess.Unlock()
				ev = append(ev, map[string]any{"tool": "model.call", "decision": "allow", "usage": map[string]any{"charged_tokens": int(ct)}})
			}
		}
		if len(unfinished) > 0 || len(results) < c.Runs {
			finished = false
			scores = append(scores, map[string]any{"case": cid, "finished": false, "waiting_on": unfinished})
			continue
		}
		rec := g.RecordFromRuns(c, results, ev)
		v := ScoreView(ScoreRun(c, rec), rec)
		v["finished"] = true
		scores = append(scores, v)
	}
	out := map[string]any{"agent": agent, "finished": finished, "scores": scores}
	if finished {
		passed := true
		for _, s := range scores {
			passed = passed && s.(map[string]any)["passed"] == true
		}
		out["passed"] = passed
		if g.PreflightLedger != nil && len(fps) == 1 {
			for fp := range fps {
				out["record"] = g.PreflightLedger.Put(agent, fp, passed, len(scores))
			}
		}
	}
	return out, nil
}

func (g *Gateway) candidateRunOf(sessionID, agent string) (string, string, string, bool) {
	a, run, fp, ok := g.CandidateRun(sessionID)
	if !ok || a != agent {
		return "", "", "", false
	}
	return a, run, fp, true
}

// housekeeping is what the sweep keeps tidy beside the sessions: expired holds go, and human requests that expired more than
// InboxRetention ago (Python keeps them for ever). Each is best effort and hourly at most.
func (g *Gateway) housekeeping(ctx context.Context, now time.Time) {
	g.houseMu.Lock()
	due := g.lastHouse.IsZero() || now.Sub(g.lastHouse) >= time.Hour
	if due {
		g.lastHouse = now
	}
	g.houseMu.Unlock()
	if !due {
		return
	}
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if n := g.PruneHolds(hctx); n > 0 {
		g.Log.Info("expired holds dropped", "n", n)
	}
	if g.Inbox != nil {
		if n, err := g.Inbox.Prune(hctx, now.Add(-InboxRetention)); err != nil {
			g.Log.Warn("old human requests were not pruned", "err", err.Error())
		} else if n > 0 {
			g.Log.Info("old human requests dropped", "n", n)
		}
	}
}

// ControlRate is how many of the repeatable writes (a hold, which renews, and a run) one person may make a minute. Python has no
// limit; a person, or a delegate naming one, who loops on them would otherwise write a record and take a run for every call.
const ControlRate = 30

// controlLimiter is a sliding one-minute window per (person, kind). It is only consulted after the person has been checked, so an
// unauthenticated or unauthorized caller consumes nobody's allowance.
type controlLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (c *controlLimiter) allow(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hits == nil {
		c.hits = map[string][]time.Time{}
	}
	kept := c.hits[key][:0]
	for _, h := range c.hits[key] {
		if now.Sub(h) < time.Minute {
			kept = append(kept, h)
		}
	}
	if len(kept) >= ControlRate {
		c.hits[key] = kept
		return false
	}
	c.hits[key] = append(kept, now)
	if len(c.hits) > 4096 { // bounded: a table of people never grows past what the allow-list can produce
		for k, v := range c.hits {
			if len(v) == 0 || now.Sub(v[len(v)-1]) >= time.Minute {
				delete(c.hits, k)
			}
		}
	}
	return true
}

func (g *Gateway) rateLimited(person, kind string) error {
	if !g.limiter.allow(kind+"/"+person, g.Now()) {
		return &ControlError{"CT-RATE", fmt.Sprintf("more than %d %s a minute", ControlRate, kind), "Wait a minute.", 429}
	}
	return nil
}
