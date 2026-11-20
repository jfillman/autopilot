package dispatch_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// fakeEffects is the effect paths clearance-api's outcome records name, per session; err is "unknown".
type fakeEffects struct {
	mu    sync.Mutex
	paths map[string][]string
	err   error
	calls []struct{ session, token, obo string }
}

func (f *fakeEffects) EffectPaths(_ context.Context, session, token, obo string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct{ session, token, obo string }{session, token, obo})
	if f.err != nil {
		return nil, f.err
	}
	return f.paths[session], nil
}

// pathsRig is a Preflight run of the researcher that has finished, scored against a case that wants paths touched (W-16: the paths
// come from Clearance's outcome records, so a case with a minimum can pass and a touch outside the allowed paths fails).
func pathsRig(t *testing.T, min int, allowed []string) (*peopleRig, *fakeEffects, map[string]any, string) {
	t.Helper()
	p := newPeopleRig(t)
	p.g.PreflightCases = map[string]*dispatch.Case{"c1": {ID: "c1", Title: "t", RootCause: "r", Verifiers: []string{"v1"}, Agent: "researcher", Input: "fix it", Runs: 1,
		MaxDenials: 0, MaxRetries: 1, TokenBudget: 1000, MinPathsTouched: min, AllowedPaths: allowed,
		Checks: map[string][]map[string]any{"v1": {{"text_contains": []any{"fixed"}}}}}}
	p.g.PreflightLedger = &dispatch.PreflightLedger{}
	p.g.Preflight = p.g.PreflightLedger.Of
	fe := &fakeEffects{paths: map[string][]string{}}
	p.g.Effects = fe
	cand := *p.g.Defs["researcher"]
	cand.Name = "pf-researcher-abcdef01"
	view, err := p.g.RunAgentAs(ctx, adminID, "", "researcher", &cand, dispatch.Fingerprint(p.g.Defs["researcher"]), ptr("go"))
	if err != nil {
		t.Fatal(err)
	}
	sid := view["session"].(string)
	body := map[string]any{"agent": "researcher", "cases": []any{map[string]any{"case": "c1", "sessions": []any{sid}}}}
	return p, fe, body, sid
}

func finish(p *peopleRig, sid string) {
	p.results.Rows[sid] = map[string]any{"expires_at": core.Iso(t0.Add(3600e9)), "status": "done", "final_text": "I fixed it", "tool_calls": []any{}}
}

func scoreOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	return out["scores"].([]any)[0].(map[string]any)
}

func TestScoreReadsThePathsFromClearanceSoAMinPathsCasePasses(t *testing.T) {
	p, fe, body, sid := pathsRig(t, 1, []string{"clusters/**", "docs/*.md"})
	fe.paths[sid] = []string{"clusters/dev/app.yaml", "docs/a.md"}
	finish(p, sid)
	tok := p.person(adminID)
	out, err := p.g.PreflightScore(ctx, tok, body, "")
	if err != nil || out["passed"] != true {
		t.Fatalf("%v %v", out, err)
	}
	if rec := out["record"].(map[string]any); rec["result"] != "pass" {
		t.Fatalf("%v", rec)
	}
	// the read is made for that session with the caller's own token
	if len(fe.calls) != 1 || fe.calls[0].session != sid || fe.calls[0].token != tok || fe.calls[0].obo != "" {
		t.Fatalf("%+v", fe.calls)
	}
}

func TestScoreWithNoEffectsFailsAMinPathsCase(t *testing.T) {
	// A session whose outcome records carry no effect: "no effects" is known, and "nothing was changed" is the score.
	p, _, body, sid := pathsRig(t, 1, []string{"clusters/**"})
	finish(p, sid)
	out, err := p.g.PreflightScore(ctx, p.person(adminID), body, "")
	if err != nil || out["passed"] != false {
		t.Fatalf("%v %v", out, err)
	}
	if r := scoreOf(t, out)["reasons"].([]string); len(r) != 1 || r[0] != "nothing was changed" {
		t.Fatalf("%v", r)
	}
}

func TestScoreFailsAWriteOutsideTheAllowedPaths(t *testing.T) {
	p, fe, body, sid := pathsRig(t, 1, []string{"clusters/**"})
	fe.paths[sid] = []string{"clusters/dev/app.yaml", "secrets/prod.yaml", "../etc/passwd"}
	finish(p, sid)
	out, err := p.g.PreflightScore(ctx, p.person(adminID), body, "")
	if err != nil || out["passed"] != false {
		t.Fatalf("%v %v", out, err)
	}
	// sorted, so the reason is deterministic; the escape is out of scope too
	if r := scoreOf(t, out)["reasons"].([]string); len(r) != 1 || r[0] != "touched paths outside scope: ../etc/passwd, secrets/prod.yaml" {
		t.Fatalf("%v", r)
	}
	if p.g.PreflightLedger.Of(p.g.Defs["researcher"]).Result != "fail" {
		t.Fatal("the failing score is the definition's result")
	}
}

// A read-only case (no minimum, no allowed paths) still checks scope: a run that wrote anything fails it.
func TestScoreOfAReadOnlyCaseFailsOnAnyWrite(t *testing.T) {
	p, fe, body, sid := pathsRig(t, 0, nil)
	fe.paths[sid] = []string{"clusters/dev/app.yaml"}
	finish(p, sid)
	out, err := p.g.PreflightScore(ctx, p.person(adminID), body, "")
	if err != nil || out["passed"] != false {
		t.Fatalf("%v %v", out, err)
	}
	fe.paths[sid] = nil
	if out, err = p.g.PreflightScore(ctx, p.person(adminID), body, ""); err != nil || out["passed"] != true {
		t.Fatalf("%v %v", out, err)
	}
}

// The paths cannot be read: the score is refused (503), not made from an empty list, and nothing is recorded for the definition.
func TestScoreIsRefusedWhenThePathsAreUnknown(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		rule string
	}{
		"clearance-api unreachable": {&core.PathsUnknown{Msg: "the Clearance API could not be reached"}, core.OutageRule},
		"clearance-api refuses":     {&core.PathsUnknown{Msg: "the Clearance API refused the read: 403", Refused: true}, core.RefusedRule},
		"some other failure":        {errors.New("boom"), core.OutageRule},
	} {
		for _, min := range []int{1, 0} { // a read-only case is no exception
			p, fe, body, sid := pathsRig(t, min, []string{"clusters/**"})
			fe.err = tc.err
			finish(p, sid)
			out, err := p.g.PreflightScore(ctx, p.person(adminID), body, "")
			var ce *dispatch.ControlError
			if !errors.As(err, &ce) || ce.Status != 503 || ce.Rule != tc.rule || out != nil {
				t.Fatalf("%s min %d: %v %v", name, min, out, err)
			}
			if p.g.PreflightLedger.Of(p.g.Defs["researcher"]) != nil {
				t.Fatalf("%s: a refused score recorded a result", name)
			}
			// back up: the same request now scores
			fe.err, fe.paths[sid] = nil, []string{"clusters/dev/app.yaml"}
			if out, err = p.g.PreflightScore(ctx, p.person(adminID), body, ""); err != nil || out["passed"] != true {
				t.Fatalf("%s: %v %v", name, out, err)
			}
		}
	}
}

func TestScoreIsRefusedWithNoClearanceAPIConfigured(t *testing.T) {
	p, _, body, sid := pathsRig(t, 0, nil)
	p.g.Effects = nil
	finish(p, sid)
	_, err := p.g.PreflightScore(ctx, p.person(adminID), body, "")
	var ce *dispatch.ControlError
	if !errors.As(err, &ce) || ce.Status != 503 || ce.Rule != core.RefusedRule {
		t.Fatalf("%v", err)
	}
}

// The paths are read only for a run that has finished (a run still going is "not finished", with no read made).
func TestScoreReadsNothingForARunThatHasNotFinished(t *testing.T) {
	p, fe, body, _ := pathsRig(t, 1, []string{"clusters/**"})
	out, err := p.g.PreflightScore(ctx, p.person(adminID), body, "")
	if err != nil || out["finished"] != false || len(fe.calls) != 0 {
		t.Fatalf("%v %v %v", out, err, fe.calls)
	}
}

// A delegate (Tower's backend) naming a person: the read goes out with the delegate's token and that person.
func TestScoreReadsAsTheDelegateForThePerson(t *testing.T) {
	p, fe, body, sid := pathsRig(t, 1, []string{"clusters/**"})
	fe.paths[sid] = []string{"clusters/dev/app.yaml"}
	finish(p, sid)
	tok := p.who(delegateID, "workload")
	out, err := p.g.PreflightScore(ctx, tok, body, adminID)
	if err != nil || out["passed"] != true {
		t.Fatalf("%v %v", out, err)
	}
	if len(fe.calls) != 1 || fe.calls[0].token != tok || fe.calls[0].obo != adminID {
		t.Fatalf("%+v", fe.calls)
	}
}

// The real reader against a clearance-api that serves a record as the core wrote it: the paths of the outcome of a pull request.
func TestScoreThroughTheRealOutcomeReader(t *testing.T) {
	p, _, body, sid := pathsRig(t, 1, []string{"clusters/**"})
	finish(p, sid)
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"items":[{"schema":"clearance.evidence/v1","seq":12,"session":"` + sid + `","tool":"repo.pr.open","decision":"allow",` +
			`"result":"opened PR 7","effect":{"repo":"jfillman/flight-api","pr":7,"branch":"agent/fix","paths":["clusters/dev/app.yaml"]},` +
			`"outcome_of":{"seq":11,"hash":"aa"}}],"next":null}`))
	}))
	defer srv.Close()
	p.g.Effects = &core.OutcomeReader{BaseURL: srv.URL}
	out, err := p.g.PreflightScore(ctx, p.person(adminID), body, "")
	if err != nil || out["passed"] != true {
		t.Fatalf("%v %v (%s)", out, err, query)
	}
	srv.Close()
	if _, err = p.g.PreflightScore(ctx, p.person(adminID), body, ""); err == nil || ctlRule(err) != core.OutageRule {
		t.Fatalf("an API that went away: %v", err)
	}
}

// A caller clearance-api shows none of the run's records (it scopes evidence per viewer: a person sees the runs they started): the
// real reader finds nothing visible and the score is refused with its own rule, even for a read-only case that an empty list would
// have passed, and nothing is recorded.
func TestScoreIsRefusedForARunTheCallerCannotSee(t *testing.T) {
	p, _, body, sid := pathsRig(t, 0, nil)
	finish(p, sid)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[],"next":null}`))
	}))
	defer srv.Close()
	p.g.Effects = &core.OutcomeReader{BaseURL: srv.URL}
	out, err := p.g.PreflightScore(ctx, p.person(adminID), body, "")
	var ce *dispatch.ControlError
	if !errors.As(err, &ce) || ce.Status != 503 || ce.Rule != core.NotVisibleRule || out != nil {
		t.Fatalf("%v %v", out, err)
	}
	if p.g.PreflightLedger.Of(p.g.Defs["researcher"]) != nil {
		t.Fatal("a refused score recorded a result")
	}
}

// One budget covers every read of a score request: a clearance-api that answers slowly cannot hold the request for a timeout per
// page, session or case.
func TestScoreReadsShareOneTimeBudget(t *testing.T) {
	p, _, body, sid := pathsRig(t, 1, []string{"clusters/**"})
	finish(p, sid)
	p.g.PreflightBudget = 200 * time.Millisecond
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	p.g.Effects = &core.OutcomeReader{BaseURL: srv.URL}
	start := time.Now()
	out, err := p.g.PreflightScore(ctx, p.person(adminID), body, "")
	if ctlRule(err) != core.OutageRule || out != nil {
		t.Fatalf("%v %v", out, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s with a 200ms budget (the reader's own timeout is %s)", d, core.OutcomeTimeout)
	}
	if p.g.PreflightLedger.Of(p.g.Defs["researcher"]) != nil {
		t.Fatal("a refused score recorded a result")
	}
}

// An outcome that carries an effect and is still queued in the outbox (Clearance was unreachable when the write finished) is not in
// Clearance's records: a score now would miss its paths. It is refused until the outbox has drained, with nothing recorded, even for
// a case an empty list would pass.
func TestScoreIsRefusedWhileAWriteIsQueuedInTheOutbox(t *testing.T) {
	p, fe, body, sid := pathsRig(t, 0, []string{"clusters/**"})
	finish(p, sid)
	id, err := p.g.Core.Outbox.Add(ctx, core.Entry{Op: "outcome", DecisionID: "d-1", Body: map[string]any{"result": "opened PR 7",
		"effect": map[string]any{"repo": "jfillman/flight-api", "pr": float64(7), "paths": []any{"secrets/prod.yaml"}}}}, "core-unavailable", "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.g.PreflightScore(ctx, p.person(adminID), body, "")
	var ce *dispatch.ControlError
	if !errors.As(err, &ce) || ce.Status != 503 || ce.Rule != core.OutboxPendingRule || out != nil {
		t.Fatalf("%v %v", out, err)
	}
	if len(fe.calls) != 0 || p.g.PreflightLedger.Of(p.g.Defs["researcher"]) != nil {
		t.Fatalf("a refused score read %d times / recorded a result", len(fe.calls))
	}
	// outcomes with no effect, and records, do not hold a score back; delivered, the write no longer does
	_ = p.g.Core.Outbox.Done(ctx, id, nil)
	if _, err := p.g.Core.Outbox.Add(ctx, core.Entry{Op: "outcome", DecisionID: "d-2", Body: map[string]any{"result": "ok"}}, "core-unavailable", ""); err != nil {
		t.Fatal(err)
	}
	if out, err = p.g.PreflightScore(ctx, p.person(adminID), body, ""); err != nil || out["passed"] != true {
		t.Fatalf("%v %v", out, err)
	}
}
