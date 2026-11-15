package triggers

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/session"
)

// What a trigger does once it holds the claim, shared by the bridge and the scheduler.
//
// One rule: handling in this process is serialized, so when a claim is taken no other claim of this process is in flight, and the
// gateway is the only writer of its database. A row that is still `claimed` is therefore always an orphan (a crash, or a write that
// gave up), never a live handling: it is taken over at once (the claim is made with a cut-off of "now") and resumed, not answered as
// a duplicate. A duplicate is a row that is decided.
//
// Nothing in the takeover path is inferred from the clock or from the absence of a field: every fact is recorded durably when it happens
// (the run's name before the run is made, LaunchedAt right after, the CloseReason with the close, the RunRemoved reason before a run is
// deleted) and the takeover decides only from them. What resuming does depends on them. The session is written to the row
// (Ledger.Opened) right after the open and before the launch, so a row with a session says an open was made for this key, and no other
// is opened while it is usable:
//   - removed (RunRemoved): decided from the reason (hold: held; record-failed: launch-failed; closed: refused);
//   - a run named but not recorded as created, session closed: the backend is asked: it exists (launched), it provably does not (go on),
//     nobody can tell (resumable: nothing decided, closed or deleted);
//   - live: launched (a run that exists is the run; a name that was only reserved is made; a hold or a freeze stops it);
//   - closed after its run was made (LaunchedAt): decided `launched`, whatever became of the run;
//   - frozen, or closed by a person's end: `refused` (CT-FROZEN, CT-ENDED); closed with any other reason, or none recorded: `refused`
//     (CT-CLOSED): never launched, never reopened;
//   - closed by its deadline before it was launched: opened afresh and run once, if the trigger is still within its validity;
//   - unknown to this gateway (older than it remembers): decided `deduped`.
// A row with no session starts from the beginning, within the trigger's validity (else `missed`).
//
// An outcome that was determined but could not be written (the decision) is kept in memory, per key, and written as it was by the next
// claim of the key: held, braked and refused are not worked out again under other conditions. A restart loses it, and the trigger is
// then evaluated afresh; the memory is the better of the two choices because the claim is released the same way either way (it is
// simply taken over), and nothing is lost when the row cannot be written.

// RetryWaits are the pauses between the attempts at a ledger write that matters (Opened, Decide): bounded, so a database that is
// away for a moment does not lose a decision, and one that is away for longer ends the stream instead.
var RetryWaits = []time.Duration{50 * time.Millisecond, 250 * time.Millisecond, 1 * time.Second}

func retry(ctx context.Context, waits []time.Duration, fn func() error) error {
	err := fn()
	for _, w := range waits {
		if err == nil || errors.Is(err, ErrNotClaimed) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(w):
		}
		err = fn()
	}
	return err
}

type fireOut struct {
	Result  string // launched, deduped, held, refused, launch-failed, missed
	Session string
	Run     string
	Rule    string
}

type opened struct {
	session string
	deduped bool
}

// resumer carries what one claimed trigger needs.
type resumer struct {
	gw     Gateway
	ledger Ledger
	log    *slog.Logger
	waits  []time.Duration
	agent  string
	key    string
	valid  func() bool // the trigger is still within its validity
	open   func(ctx context.Context) (*opened, *fireOut, error)
	input  func() map[string]any
}

// run does the work of a claimed trigger, starting from prior (the session the row holds, or ""). An error means the trigger is neither
// decided nor to be reported as handled: the claim stays, and the next delivery takes it over.
func (r *resumer) run(ctx context.Context, prior string) (fireOut, error) {
	sid := prior
	if sid != "" {
		out, reopen, err := r.continueSession(ctx, sid)
		if err != nil || !reopen {
			return out, err
		}
		sid = ""
	}
	if !r.valid() {
		return fireOut{Result: "missed"}, nil
	}
	o, refusal, oerr := r.open(ctx)
	if oerr != nil {
		return fireOut{}, oerr // could not tell (a hold that cannot be read): resumable
	}
	if refusal != nil {
		return *refusal, nil
	}
	if err := retry(ctx, r.waits, func() error { return r.ledger.Opened(ctx, r.agent, r.key, o.session) }); err != nil {
		// The session cannot be tied to the claim: nothing launches, and a session this call made is closed again.
		if !o.deduped {
			r.gw.AbandonTriggered(ctx, o.session, false)
		}
		if errors.Is(err, ErrNotClaimed) {
			return fireOut{Result: "deduped", Session: o.session}, nil
		}
		return fireOut{}, err
	}
	out, reopen, err := r.continueSession(ctx, o.session)
	if reopen { // the session just opened is already unusable: nothing more is tried this time
		return fireOut{Result: "launch-failed", Session: o.session}, err
	}
	return out, err
}

// continueSession is what a trigger does with the session recorded for it, deciding only from facts the session recorded (see the top of
// the file). reopen says the session is of no use and a fresh one may be opened. An error means "could not tell": nothing is undone and
// nothing is decided, and the claim stays for the next delivery (after bounded retries here).
func (r *resumer) continueSession(ctx context.Context, sid string) (out fireOut, reopen bool, err error) {
	st := r.gw.TriggerSessionState(sid)
	run := st.Run
	switch {
	case !st.Known:
		return fireOut{Result: "deduped", Session: sid}, false, nil
	case st.Removed != "":
		// A run was made and taken away again, and the session says why (written before anything was decided).
		switch st.Removed {
		case session.RemovedHold:
			return fireOut{Result: "held", Session: sid, Rule: "R024"}, false, nil
		case session.RemovedNoRecord:
			return fireOut{Result: "launch-failed", Session: sid}, false, nil
		}
		return fireOut{Result: "refused", Session: sid, Rule: closeRule(st)}, false, nil
	case !st.Live:
		if run != "" && !st.Launched {
			// The run's name was reserved and the session records no creation: the run may or may not exist. Ask the backend before
			// anything else is decided: it exists (launched), it provably does not (go on), or nobody could tell (resumable).
			var exists bool
			if err := r.resumable(ctx, func() (e error) { exists, e = r.gw.RunExists(ctx, sid); return }); err != nil {
				return fireOut{}, false, err
			}
			if exists {
				return fireOut{Result: "launched", Session: sid, Run: run}, false, nil
			}
		}
		switch {
		case st.Launched:
			return fireOut{Result: "launched", Session: sid, Run: run}, false, nil
		case st.Frozen:
			return fireOut{Result: "refused", Session: sid, Rule: "CT-FROZEN"}, false, nil
		case st.CloseControl == session.CloseDeadline:
			return fireOut{}, true, nil // closed by its own deadline before it was launched: the only reason a fresh session may follow
		}
		// Closed by a person, the gateway, its parent or a path that recorded nothing: not launched, and not reopened.
		return fireOut{Result: "refused", Session: sid, Rule: closeRule(st)}, false, nil
	}
	out = fireOut{Result: "launch-failed", Session: sid}
	var res *dispatch.Result
	var lerr error
	lerr = r.resumable(ctx, func() (e error) { res, e = r.gw.LaunchTriggered(ctx, r.agent, sid, r.input()); return })
	if lerr != nil && dispatch.IsResumable(lerr) {
		return fireOut{}, false, lerr // could not tell: nothing is closed, nothing is deleted, nothing is decided
	}
	if lerr != nil {
		// A final error: the run was not made (the create failed and was undone). The session is closed and the trigger decided.
		r.gw.AbandonTriggered(ctx, sid, true)
		return out, false, nil
	}
	if res.OK {
		out.Result = "launched"
		if d, ok := res.Data.(map[string]any); ok {
			out.Run, _ = d["run"].(string)
		}
		return out, false, nil
	}
	if rule, _ := res.Decision["rule"].(string); rule == "R024" {
		out.Result, out.Rule = "held", rule
		return out, false, nil
	}
	if frozen, _ := res.Decision["frozen"].(bool); frozen {
		out.Result, out.Rule = "refused", "CT-FROZEN"
		return out, false, nil
	}
	r.gw.AbandonTriggered(ctx, sid, false)
	return out, false, nil
}

// closeRule names why a closed session is not launched: a person's end, a freeze, or any other close.
func closeRule(st dispatch.TriggerSession) string {
	switch {
	case st.Frozen:
		return "CT-FROZEN"
	case st.CloseControl == session.CloseEnd:
		return "CT-ENDED"
	}
	return "CT-CLOSED"
}

// resumable runs fn, retrying a ResumableError (a read that could not tell) with the bounded backoff; any other result, or the last
// error, is returned.
func (r *resumer) resumable(ctx context.Context, fn func() error) error {
	err := fn()
	for _, w := range r.waits {
		if err == nil || !dispatch.IsResumable(err) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(w):
		}
		err = fn()
	}
	return err
}

// pendingDecisions are outcomes that were determined and could not be written, by agent and key; bounded, the oldest evicted.
type pendingDecisions struct {
	m     map[string]fireOut
	order []string
}

const maxPending = 1000

func (p *pendingDecisions) put(agent, key string, o fireOut) {
	if p.m == nil {
		p.m = map[string]fireOut{}
	}
	k := agent + "\x00" + key
	if _, there := p.m[k]; !there {
		p.order = append(p.order, k)
	}
	p.m[k] = o
	for len(p.m) > maxPending && len(p.order) > 0 {
		delete(p.m, p.order[0])
		p.order = p.order[1:]
	}
}

func (p *pendingDecisions) take(agent, key string) (fireOut, bool) {
	o, ok := p.m[agent+"\x00"+key]
	return o, ok
}

func (p *pendingDecisions) drop(agent, key string) {
	k := agent + "\x00" + key
	if _, ok := p.m[k]; !ok {
		return
	}
	delete(p.m, k)
	for i, x := range p.order {
		if x == k {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
}
