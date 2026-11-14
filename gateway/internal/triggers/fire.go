package triggers

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// What a trigger does once it holds the claim, shared by the bridge and the scheduler.
//
// One rule: handling in this process is serialized, so when a claim is taken no other claim of this process is in flight, and the
// gateway is the only writer of its database. A row that is still `claimed` is therefore always an orphan (a crash, or a write that
// gave up), never a live handling: it is taken over at once (the claim is made with a cut-off of "now") and resumed, not answered as
// a duplicate. A duplicate is a row that is decided.
//
// What resuming does depends on what the row holds. The session is written to the row (Ledger.Opened) right after the open and
// before the launch, so a row with a session says an open was made for this key, and no other is opened while it is usable:
//   - live: launched (a run that exists is the run; a name that was only reserved is made; a hold or a freeze stops it);
//   - closed after its run was made (LaunchedAt): decided `launched`, whatever became of the run;
//   - closed by a person (an end) or frozen before it was launched: decided `refused`, with the rule saying so, and not relaunched;
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
	open   func(ctx context.Context) (*opened, *fireOut)
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
	o, refusal := r.open(ctx)
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

// continueSession is what a trigger does with the session recorded for it (see the top of the file). reopen says the session is of no
// use and a fresh one is to be opened.
func (r *resumer) continueSession(ctx context.Context, sid string) (out fireOut, reopen bool, err error) {
	st := r.gw.TriggerSessionState(sid)
	switch {
	case !st.Known:
		return fireOut{Result: "deduped", Session: sid}, false, nil
	case !st.Live:
		switch {
		case st.Launched:
			return fireOut{Result: "launched", Session: sid, Run: st.Run}, false, nil
		case st.Frozen:
			return fireOut{Result: "refused", Session: sid, Rule: "CT-FROZEN"}, false, nil
		case st.Ended:
			return fireOut{Result: "refused", Session: sid, Rule: "CT-ENDED"}, false, nil
		}
		return fireOut{}, true, nil // closed by its deadline before it was launched
	}
	out = fireOut{Result: "launch-failed", Session: sid}
	res, lerr := r.gw.LaunchTriggered(ctx, r.agent, sid, r.input())
	var hr *dispatch.HoldReadError
	if errors.As(lerr, &hr) {
		return fireOut{}, false, lerr // the hold could not be read: not decided, resumable
	}
	if lerr != nil {
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

// pendingDecisions are outcomes that were determined and could not be written, by agent and key; bounded.
type pendingDecisions struct {
	m map[string]fireOut
}

const maxPending = 1000

func (p *pendingDecisions) put(agent, key string, o fireOut) {
	if p.m == nil {
		p.m = map[string]fireOut{}
	}
	if len(p.m) >= maxPending {
		for k := range p.m { // arbitrary: the oldest is not kept in order; a dropped one is evaluated afresh
			delete(p.m, k)
			break
		}
	}
	p.m[agent+"\x00"+key] = o
}

func (p *pendingDecisions) take(agent, key string) (fireOut, bool) {
	o, ok := p.m[agent+"\x00"+key]
	return o, ok
}

func (p *pendingDecisions) drop(agent, key string) { delete(p.m, agent+"\x00"+key) }

// maxPanics is how many times one trigger may panic before it is decided launch-failed (it would panic on every delivery).
const maxPanics = 3
