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
}

func (a *apiFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.reqs = append(a.reqs, r)
	a.mu.Unlock()
	code, body := a.answer(r)
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
	rq := f.reqs[0]
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
		case "":
			return 200, page("20", outcome(10, "s-1", "repo.pr.open", `{"paths":["a.yaml"]}`), outcome(20, "s-1", "repo.pr.update", `{"paths":["b.yaml"]}`))
		case "20":
			return 200, page("30", outcome(30, "s-1", "repo.pr.update", `{"paths":["c.yaml","a.yaml"]}`))
		case "30":
			return 200, page("null", outcome(40, "s-1", "repo.pr.update", `{"paths":["d.yaml"]}`))
		}
		return 400, `{"error":"after is a seq"}`
	})
	got, err := r.EffectPaths(context.Background(), "s-1", "tok", "")
	if err != nil || strings.Join(got, ",") != "a.yaml,b.yaml,c.yaml,d.yaml" {
		t.Fatalf("%v %v", got, err)
	}
	if len(f.reqs) != 3 {
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
		"a cursor that goes backwards": {func(r *http.Request) (int, string) {
			if r.URL.Query().Get("after") == "" {
				return 200, page("50", outcome(50, "s-1", "repo.pr.open", `{"paths":["a"]}`))
			}
			return 200, page("40", outcome(40, "s-1", "repo.pr.open", `{"paths":["b"]}`))
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
		return 200, page(fmt.Sprint(n*10), outcome(n*10, "s-1", "repo.pr.update", fmt.Sprintf(`{"paths":["f%d.yaml"]}`, n)))
	})
	got, err := r.EffectPaths(context.Background(), "s-1", "tok", "")
	var pu *core.PathsUnknown
	if !errors.As(err, &pu) || got != nil || n != core.OutcomeMaxPages {
		t.Fatalf("%d pages: %v %v", n, got, err)
	}
}
