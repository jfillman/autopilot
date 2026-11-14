package triggers

import (
	"context"
	"log/slog"
	"time"
)

// What a claimed trigger does once it holds the claim, shared by the bridge and the scheduler: open the session (or find the one an
// earlier holder of the claim opened), record the session in the ledger before anything launches, and launch it.
//
// The session is written to the ledger row (Ledger.Opened) right after the open and before the launch. A later holder of the claim
// (after a restart, a replay, the reconciliation sweep, a decision that could not be written) reads it and must not open another
// session, whatever became of the one recorded: live with a run is a dedupe, live without one is launched (the repair), closed or
// gone means the trigger already ran and is decided. Without this a run that finished before its row was decided could be launched
// a second time by a stale claim.

// RetryWaits are the pauses between the attempts at a ledger write that matters (Opened, Decide): bounded, so a database that is
// away for a moment does not lose a decision, and one that is away for longer ends the stream instead.
var RetryWaits = []time.Duration{50 * time.Millisecond, 250 * time.Millisecond, 1 * time.Second}

func retry(ctx context.Context, waits []time.Duration, fn func() error) error {
	err := fn()
	for _, w := range waits {
		if err == nil || ctx.Err() != nil {
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
	Result  string // launched, deduped, held, refused, launch-failed
	Session string
	Run     string
	Rule    string
}

// fireClaimed does the work of a trigger whose claim is held. open opens the session when none is recorded; input is what a launch
// is given. A ledger error is returned (the claim is then neither decided nor to be reported as handled).
func fireClaimed(ctx context.Context, gw Gateway, ledger Ledger, log *slog.Logger, waits []time.Duration, agent, key string,
	prior string, open func(ctx context.Context) (*opened, *fireOut), input func() map[string]any) (fireOut, error) {
	sid := prior
	if sid == "" {
		o, refusal := open(ctx)
		if refusal != nil {
			return *refusal, nil
		}
		sid = o.session
		if err := retry(ctx, waits, func() error { return ledger.Opened(ctx, agent, key, sid) }); err != nil {
			// The session cannot be tied to the claim: nothing launches, and a session this call made is closed again, so a later
			// holder of the claim is not left with a live session it cannot see.
			if !o.deduped {
				gw.AbandonTriggered(ctx, sid, false)
			}
			return fireOut{}, err
		}
	}
	return continueSession(ctx, gw, agent, sid, input), nil
}

type opened struct {
	session string
	deduped bool
}

// continueSession is what a trigger does with the session recorded for it: a live one with a run is the run (a dedupe); a live one
// with none is launched (the repair of a crash between the open and the launch); one that is closed, expired or no longer known
// already ran or was closed: the trigger is decided as a dedupe, and nothing opens.
func continueSession(ctx context.Context, gw Gateway, agent, sid string, input func() map[string]any) fireOut {
	known, live, run := gw.SessionState(sid)
	switch {
	case !known || !live:
		return fireOut{Result: "deduped", Session: sid, Run: run}
	case run != "":
		return fireOut{Result: "deduped", Session: sid, Run: run}
	}
	out := fireOut{Result: "launch-failed", Session: sid}
	launched, err := gw.LaunchTriggered(ctx, agent, sid, input())
	if err != nil {
		gw.AbandonTriggered(ctx, sid, true)
		return out
	}
	if launched.OK {
		out.Result = "launched"
		if d, ok := launched.Data.(map[string]any); ok {
			out.Run, _ = d["run"].(string)
		}
		return out
	}
	if rule, _ := launched.Decision["rule"].(string); rule == "R024" {
		out.Result, out.Rule = "held", rule
		return out
	}
	gw.AbandonTriggered(ctx, sid, false)
	return out
}
