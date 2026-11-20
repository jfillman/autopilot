package core_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
)

// The records are shaped as clearance-api serves them (GET /v1/evidence, EvidencePage {items, next}; a record as the core wrote it:
// the outcome of an allowed call links its decision by outcome_of, and its effect is clearance client.Effect). Shape recorded from the
// Clearance repo at origin/main: internal/api/routes.go (EvidencePage), client/types.go (Effect), internal/api/api_test.go (fixtureEvents).
func outcome(seq int, session, tool string, effect string) string {
	return fmt.Sprintf(`{"schema":"clearance.evidence/v1","seq":%d,"hash":"%s","prev_hash":"%s","ts":"2026-11-20T10:%02d:00Z","task_id":"t-1","session":%q,
"principal":"user:alice","agent":"coding-agent","tool":%q,"tier":1,"decision":"allow","result":"opened PR %d","coverage":"authorized-and-verified",
"effect":%s,"outcome_of":{"seq":%d,"hash":"%s"}}`, seq, strings.Repeat("b", 64), strings.Repeat("a", 64), seq%60, session, tool, seq, effect, seq-1, strings.Repeat("a", 64))
}

func page(next string, items ...string) string {
	return `{"items":[` + strings.Join(items, ",") + `],"next":` + next + `}`
}

type apiFake struct {
	mu     sync.Mutex
	reqs   []*http.Request
	answer func(r *http.Request) (int, string)
	// probe answers the visibility probe (the session's non-outcome records, limit 1); nil: one is visible, as for the person who started the run.
	probe func(r *http.Request) (int, string)
}

// sessionOpen is the session.open record the core wrote when the session opened (a decision: it has no outcome_of).
func sessionOpen(session string) string {
	return fmt.Sprintf(`{"schema":"clearance.evidence/v1","seq":3,"hash":"%s","session":%q,"task_id":"t-1","principal":"user:alice","agent":"pf-coding-agent-abcdef01","tool":"session.open","decision":"allow","result":"opened"}`,
		strings.Repeat("c", 64), session)
}

func (a *apiFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.reqs = append(a.reqs, r)
	a.mu.Unlock()
	answer := a.answer
	if r.URL.Query().Get("outcomes") == "exclude" {
		answer = func(*http.Request) (int, string) { return 200, page("null", sessionOpen(r.URL.Query().Get("session"))) }
		if a.probe != nil {
			answer = a.probe
		}
	}
	code, body := answer(r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

func serve(t *testing.T, answer func(r *http.Request) (int, string)) (*core.OutcomeReader, *apiFake) {
	t.Helper()
	f := &apiFake{answer: answer}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &core.OutcomeReader{BaseURL: srv.URL + "/"}, f
}

func TestEffectPathsReadsTheSessionsOutcomesAndDedupes(t *testing.T) {
	r, f := serve(t, func(*http.Request) (int, string) {
		return 200, page("null",
			outcome(5, "s-1", "repo.pr.open", `{"repo":"jfillman/flight-api","pr":7,"branch":"agent/fix","commit":"c0ffee","paths":["clusters/dev/app.yaml","docs/a.md"]}`),
			outcome(7, "s-1", "repo.file.put", `{"repo":"jfillman/flight-api","pr":7,"paths":["docs/a.md","clusters/dev/b.yaml"]}`),
			outcome(9, "s-1", "catalog.read", `null`), // an outcome with no effect (a read)
			outcome(11, "s-OTHER", "repo.pr.open", `{"repo":"x/y","pr":1,"paths":["not/mine.yaml"]}`))
	})
	got, err := r.EffectPaths(context.Background(), "s-1", "tok", "user:alice")
	if err != nil {
		t.Fatal(err)
	}
	if want := "clusters/dev/app.yaml,clusters/dev/b.yaml,docs/a.md"; strings.Join(got, ",") != want {
		t.Fatalf("got %v want %s", got, want)
	}
	// the visibility probe first (non-outcome records, one), then the outcomes
	if pq := f.reqs[0].URL.Query(); pq.Get("session") != "s-1" || pq.Get("outcomes") != "exclude" || pq.Get("limit") != "1" {
		t.Fatalf("%s", f.reqs[0].URL)
	}
	rq := f.reqs[1]
	if rq.URL.Path != "/v1/evidence" || rq.URL.Query().Get("session") != "s-1" || rq.URL.Query().Get("outcomes") != "only" || rq.URL.Query().Get("limit") != "200" ||
		rq.URL.Query().Has("after") {
		t.Fatalf("%s", rq.URL)
	}
	if rq.Header.Get("Authorization") != "Bearer tok" || rq.Header.Get("X-Hangar-On-Behalf-Of") != "user:alice" {
		t.Fatalf("the caller's own credentials are what is sent: %v", rq.Header)
	}
}

func TestEffectPathsNoEffectsIsAnEmptyListNotAnError(t *testing.T) {
	for name, body := range map[string]string{"no records": page("null"),
		"only reads": page("null", outcome(5, "s-1", "catalog.read", `null`), outcome(7, "s-1", "model.call", `{}`))} {
		r, _ := serve(t, func(*http.Request) (int, string) { return 200, body })
		got, err := r.EffectPaths(context.Background(), "s-1", "tok", "")
		if err != nil || len(got) != 0 {
			t.Fatalf("%s: %v %v", name, got, err)
		}
	}
}

func TestEffectPathsFollowsThePagination(t *testing.T) {
	r, f := serve(t, func(r *http.Request) (int, string) {
		switch r.URL.Query().Get("after") {
		// newest first, as clearance-api lists (ORDER BY seq DESC, seq < after): next is the last, smallest seq of a full page
		case "":
			return 200, page("30", outcome(40, "s-1", "repo.pr.open", `{"paths":["a.yaml"]}`), outcome(30, "s-1", "repo.pr.update", `{"paths":["b.yaml"]}`))
		case "30":
			return 200, page("10", outcome(20, "s-1", "repo.pr.update", `{"paths":["c.yaml","a.yaml"]}`), outcome(10, "s-1", "repo.pr.update", `{"paths":["e.yaml"]}`))
		case "10":
			return 200, page("null", outcome(5, "s-1", "repo.pr.update", `{"paths":["d.yaml"]}`))
		}
		return 400, `{"error":"after is a seq"}`
	})
	got, err := r.EffectPaths(context.Background(), "s-1", "tok", "")
	if err != nil || strings.Join(got, ",") != "a.yaml,b.yaml,c.yaml,d.yaml,e.yaml" {
		t.Fatalf("%v %v", got, err)
	}
	if len(f.reqs) != 4 { // the probe and three pages
		t.Fatalf("%d requests", len(f.reqs))
	}
}

// Unknown is never "none": every way the list is uncertain is a *PathsUnknown, with the rule the outage or refusal side gives.
func TestEffectPathsUnknownIsAnErrorNeverAnEmptyList(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		answer  func(*http.Request) (int, string)
		refused bool
	}{
		"clearance-api down (503)":    {func(*http.Request) (int, string) { return 503, `{"error":"read failed"}` }, false},
		"clearance-api failing (500)": {func(*http.Request) (int, string) { return 500, `x` }, false},
		"a token it refuses (401)":    {func(*http.Request) (int, string) { return 401, `{"error":"no"}` }, true},
		"a read it refuses (403)":     {func(*http.Request) (int, string) { return 403, `{"error":"no"}` }, true},
		"a redirect":                  {func(*http.Request) (int, string) { return 302, `` }, true},
		"not an evidence page":        {func(*http.Request) (int, string) { return 200, `{"hello":"world"}` }, true},
		"not JSON":                    {func(*http.Request) (int, string) { return 200, `<html>` }, true},
		"a path that is not a string": {func(*http.Request) (int, string) {
			return 200, page("null", outcome(5, "s-1", "repo.pr.open", `{"paths":[7]}`))
		}, true},
		"a cursor that is not a seq": {func(*http.Request) (int, string) {
			return 200, page(`"abc"`, outcome(5, "s-1", "repo.pr.open", `{"paths":["a"]}`))
		}, true},
		"a cursor that does not move": {func(*http.Request) (int, string) {
			return 200, page("5", outcome(5, "s-1", "repo.pr.open", `{"paths":["a"]}`))
		}, true},
		"a cursor that goes up": {func(r *http.Request) (int, string) {
			if r.URL.Query().Get("after") == "" {
				return 200, page("50", outcome(50, "s-1", "repo.pr.open", `{"paths":["a"]}`))
			}
			return 200, page("60", outcome(60, "s-1", "repo.pr.open", `{"paths":["b"]}`))
		}, true},
	}
	for name, c := range cases {
		r, _ := serve(t, c.answer)
		got, err := r.EffectPaths(ctx, "s-1", "tok", "")
		var pu *core.PathsUnknown
		if !errors.As(err, &pu) || got != nil || pu.Refused != c.refused {
			t.Errorf("%s: %v %v", name, got, err)
			continue
		}
		if want := map[bool]string{true: core.RefusedRule, false: core.OutageRule}[c.refused]; pu.Rule() != want {
			t.Errorf("%s: rule %s want %s", name, pu.Rule(), want)
		}
	}
	// unreachable: a server that is gone
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if got, err := (&core.OutcomeReader{BaseURL: url}).EffectPaths(ctx, "s-1", "tok", ""); err == nil || got != nil || err.(*core.PathsUnknown).Rule() != core.OutageRule {
		t.Errorf("unreachable: %v %v", got, err)
	}
	// not configured, no session, unusable credentials
	for name, rd := range map[string]*core.OutcomeReader{"no API": {}, "nil reader": nil} {
		if got, err := rd.EffectPaths(ctx, "s-1", "tok", ""); err == nil || got != nil {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	r, f := serve(t, func(*http.Request) (int, string) { return 200, page("null") })
	for name, a := range map[string][3]string{"no session": {"", "tok", ""}, "no token": {"s", "", ""}, "a token with a newline": {"s", "a\nb", ""},
		"a person with a newline": {"s", "tok", "user:a\r\nX: y"}} {
		if got, err := r.EffectPaths(ctx, a[0], a[1], a[2]); err == nil || got != nil {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	if len(f.reqs) != 0 {
		t.Fatal("a request was made without usable arguments")
	}
}

func TestEffectPathsStopsAtThePageCap(t *testing.T) {
	n := 0
	r, _ := serve(t, func(*http.Request) (int, string) {
		n++
		return 200, page(fmt.Sprint(100000-n*10), outcome(100000-n*10, "s-1", "repo.pr.update", fmt.Sprintf(`{"paths":["f%d.yaml"]}`, n)))
	})
	got, err := r.EffectPaths(context.Background(), "s-1", "tok", "")
	var pu *core.PathsUnknown
	if !errors.As(err, &pu) || got != nil || n != core.OutcomeMaxPages { // (the probe is answered by the fake, not counted)
		t.Fatalf("%d pages: %v %v", n, got, err)
	}
}

// clearance-api scopes evidence per viewer and answers 200 with nothing for what the caller may not see: with none of the session's
// records visible, no outcomes are not "no effects", they are unknown (and no outcome page is even asked for).
func TestEffectPathsNothingVisibleIsUnknownNotEmpty(t *testing.T) {
	r, f := serve(t, func(*http.Request) (int, string) { return 200, page("null") })
	f.probe = func(*http.Request) (int, string) { return 200, page("null") }
	got, err := r.EffectPaths(context.Background(), "s-1", "tok", "user:bob")
	var pu *core.PathsUnknown
	if !errors.As(err, &pu) || got != nil || !pu.NotVisible || pu.Rule() != core.NotVisibleRule {
		t.Fatalf("%v %v", got, err)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("%d requests: only the probe is made", len(f.reqs))
	}
	// the probe answered with something that is not a page, or refused
	for name, pr := range map[string]func(*http.Request) (int, string){"not a page": func(*http.Request) (int, string) { return 200, `{}` },
		"refused": func(*http.Request) (int, string) { return 403, `{}` }} {
		f.probe = pr
		if got, err := r.EffectPaths(context.Background(), "s-1", "tok", ""); err == nil || got != nil {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
}

func outcomeEntry(paths ...any) core.Entry {
	eff := map[string]any{"repo": "o/r", "pr": 7}
	if paths != nil {
		eff["paths"] = paths
	}
	return core.Entry{Op: "outcome", DecisionID: "d", Body: map[string]any{"result": "ok", "effect": eff}}
}

// Before the read: a write that carries paths and is pending (not in Clearance yet) or refused (never will be) blocks the score.
func TestEffectGuardBlocksAPendingOrRefusedWriteBeforeTheRead(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	rule := func(err error) string {
		var ob *core.OutboxBlocked
		if errors.As(err, &ob) {
			return ob.Rule
		}
		return fmt.Sprint(err)
	}
	for name, tc := range map[string]struct {
		e      core.Entry
		reason string
		at     time.Time
		since  time.Time
		want   string // "" is not blocked
	}{
		"pending with paths":               {outcomeEntry("a.yaml"), "core-unavailable", t0, t0, core.OutboxPendingRule},
		"pending, paths as strings":        {core.Entry{Op: "outcome", Body: map[string]any{"effect": map[string]any{"paths": []string{"a"}}}}, "core-unavailable", t0, t0, core.OutboxPendingRule},
		"pending, effect not an object":    {core.Entry{Op: "outcome", Body: map[string]any{"effect": "x"}}, "core-unavailable", t0, t0, core.OutboxPendingRule},
		"pending with no paths":            {outcomeEntry(), "core-unavailable", t0, t0, ""},
		"pending, no effect":               {core.Entry{Op: "outcome", Body: map[string]any{"result": "ok"}}, "core-unavailable", t0, t0, ""},
		"a pending record, not an outcome": {core.Entry{Op: "record", Kind: "run.finish", Body: map[string]any{"effect": map[string]any{"paths": []any{"a"}}}}, "outage", t0, t0, ""},
		"refused after the run started":    {outcomeEntry("a.yaml"), "refused", t0.Add(time.Hour), t0, core.OutcomeRefusedRule},
		"refused, run start unknown":       {outcomeEntry("a.yaml"), "refused", t0.Add(-100 * time.Hour), time.Time{}, core.OutcomeRefusedRule},
		"refused long before the run":      {outcomeEntry("a.yaml"), "refused", t0.Add(-time.Hour), t0, ""},
		"refused within the clock slack":   {outcomeEntry("a.yaml"), "refused", t0.Add(-30 * time.Second), t0, core.OutcomeRefusedRule},
	} {
		ob := core.NewMemoryOutbox()
		ob.Now = func() time.Time { return tc.at }
		if _, err := ob.Add(ctx, tc.e, tc.reason, ""); err != nil {
			t.Fatal(err)
		}
		_, err := core.New(nil, ob, false).BeginEffectCheck(ctx, tc.since)
		if tc.want == "" && err != nil || tc.want != "" && rule(err) != tc.want {
			t.Errorf("%s: %v", name, err)
		}
	}
	// delivered (done) is in Clearance: no block; an empty outbox: none
	ob := core.NewMemoryOutbox()
	id, _ := ob.Add(ctx, outcomeEntry("a.yaml"), "core-unavailable", "")
	_ = ob.Done(ctx, id, nil)
	if _, err := core.New(nil, ob, false).BeginEffectCheck(ctx, time.Time{}); err != nil {
		t.Fatalf("a delivered write blocked: %v", err)
	}
}

// Across the read: a write queued after the guard began, in whatever state it has reached by Finish (still pending, or delivered to
// Clearance after the read was made, so in neither place for the read), blocks; one delivered before the guard began does not.
func TestEffectGuardFinishBlocksAWriteThatMovedDuringTheRead(t *testing.T) {
	ctx := context.Background()
	for name, move := range map[string]func(ob *core.MemoryOutbox, id int64){
		"still pending":            func(*core.MemoryOutbox, int64) {},
		"delivered after the read": func(ob *core.MemoryOutbox, id int64) { _ = ob.Done(ctx, id, nil) },
		"refused":                  func(ob *core.MemoryOutbox, id int64) { _ = ob.Refused(ctx, id, "no") },
	} {
		ob := core.NewMemoryOutbox()
		before, _ := ob.Add(ctx, outcomeEntry("old.yaml"), "core-unavailable", "")
		_ = ob.Done(ctx, before, nil) // delivered before the guard: in Clearance for the read
		g, err := core.New(nil, ob, false).BeginEffectCheck(ctx, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		// no-effect outcomes and records queued meanwhile do not matter
		_, _ = ob.Add(ctx, core.Entry{Op: "outcome", Body: map[string]any{"result": "ok"}}, "core-unavailable", "")
		_, _ = ob.Add(ctx, core.Entry{Op: "record", Kind: "tool.call", Body: map[string]any{}}, "outage", "")
		if err := g.Finish(ctx); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		id, _ := ob.Add(ctx, outcomeEntry("new.yaml"), "core-unavailable", "")
		move(ob, id)
		var blocked *core.OutboxBlocked
		if err := g.Finish(ctx); !errors.As(err, &blocked) || blocked.Rule != core.OutboxPendingRule {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Entries that do not qualify, however many, never count towards a block: more than the scan's old size of refused outcomes from long before
// the run must not hold back (or be counted as "too many") a run that began after them.
func TestEffectGuardIgnoresAnyNumberOfOldRefusalsAndNonQualifyingEntries(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ob := core.NewMemoryOutbox()
	ob.Now = func() time.Time { return t0.Add(-48 * time.Hour) }
	for i := 0; i < 1500; i++ {
		_, _ = ob.Add(ctx, outcomeEntry("old.yaml"), "refused", "no") // refused with paths, long before
	}
	for i := 0; i < 1500; i++ {
		_, _ = ob.Add(ctx, core.Entry{Op: "outcome", Body: map[string]any{"result": "ok"}}, "core-unavailable", "") // pending, no effect
	}
	ob.Now = func() time.Time { return t0.Add(time.Hour) }
	c := core.New(nil, ob, false)
	g, err := c.BeginEffectCheck(ctx, t0)
	if err != nil {
		t.Fatalf("old refusals and entries with no paths blocked a new run: %v", err)
	}
	if err := g.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	// one qualifying refusal among them, since the run began, blocks; the first one that qualifies is enough
	_, _ = ob.Add(ctx, outcomeEntry("new.yaml"), "refused", "no")
	var blocked *core.OutboxBlocked
	if _, err := c.BeginEffectCheck(ctx, t0); !errors.As(err, &blocked) || blocked.Rule != core.OutcomeRefusedRule {
		t.Fatalf("%v", err)
	}
}

// Quiesce holds every Add off while fn runs and lets them through when it returns, by any path (fn's error too).
func TestQuiesceHoldsOffAddsAndReleasesOnEveryPath(t *testing.T) {
	ctx := context.Background()
	for name, fnErr := range map[string]error{"fn returns": nil, "fn fails": errors.New("boom")} {
		ob := core.NewMemoryOutbox()
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			done <- ob.Quiesce(ctx, func(r core.OutboxReader) error {
				close(entered)
				<-release
				if _, err := r.HighWater(ctx); err != nil { // the reader works inside the hold
					return err
				}
				return fnErr
			})
		}()
		<-entered
		added := make(chan struct{})
		go func() { _, _ = ob.Add(ctx, outcomeEntry("a.yaml"), "core-unavailable", ""); close(added) }()
		select {
		case <-added:
			t.Fatalf("%s: an add went through while quiesced", name)
		case <-time.After(150 * time.Millisecond):
		}
		close(release)
		if err := <-done; err != fnErr {
			t.Fatalf("%s: %v", name, err)
		}
		select {
		case <-added:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: the add was not let through after Quiesce", name)
		}
	}
}
