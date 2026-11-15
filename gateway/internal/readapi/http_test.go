package readapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

func apiWith(t *testing.T, store Store) *API {
	t.Helper()
	a := accessWith(mapDir{"user:alice": {"group:default/team-a"}}, mapOwn{})
	api, err := New(&API{Access: a, Store: store, Defs: definitions.NewSet(nil)})
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func get(api http.Handler, path, token string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	return rec
}

// Authentication is decided once, before any route's code runs: every operation of the contract refuses a request with no token, a bad
// token, or a caller who is no one (a workload), and only the contract and the two probes answer without a token.
func TestEveryRouteRefusesTheUnauthenticated(t *testing.T) {
	api := apiWith(t, &captureStore{})
	paths := map[string]string{"listAgents": "/v1/agents", "getAgent": "/v1/agents/x", "listSessions": "/v1/sessions", "getSession": "/v1/sessions/s",
		"getSessionResult": "/v1/sessions/s/result", "getTask": "/v1/tasks/t", "listHumanRequests": "/v1/human/requests", "listEvents": "/v1/events",
		"listRuntimes": "/v1/runtimes", "getContract": "/v1/contract"}
	if len(paths) != len(api.Operations()) {
		t.Fatalf("the test does not know every route: %v", api.Operations())
	}
	for _, id := range api.Operations() {
		p := paths[id]
		open := id == "getContract"
		for _, c := range []struct {
			name, token string
			want        int
		}{{"no token", "", 401}, {"a bad token", "nope", 401}, {"a workload", "other", 403}, {"a run", "run", 403}} {
			rec := get(api, p, c.token)
			if open {
				if rec.Code != 200 {
					t.Errorf("%s %s: %d", id, c.name, rec.Code)
				}
				continue
			}
			if rec.Code != c.want {
				t.Errorf("%s %s: %d %s, want %d", id, c.name, rec.Code, rec.Body.String(), c.want)
			}
		}
		// The delegate's own token, with no person, is no one.
		if !open {
			if rec := get(api, p, "delegate"); rec.Code != 403 {
				t.Errorf("%s: a delegate acting for nobody: %d", id, rec.Code)
			}
		}
	}
	for _, p := range []string{"/healthz", "/readyz"} {
		if rec := get(api, p, ""); rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	// A workload that is not a delegate may not name a person on any route, whatever it names.
	for id, p := range paths {
		if id == "getContract" {
			continue
		}
		if rec := get(api, p, "other", "X-Hangar-On-Behalf-Of", "user:alice"); rec.Code != 403 {
			t.Errorf("%s: a workload naming a person: %d", id, rec.Code)
		}
	}
	// A notifier reaches the human inbox and nothing else.
	api.Access.Notifiers = []string{"hangar-prod/app-x/*"}
	for id, p := range paths {
		want := 403
		if id == "listHumanRequests" || id == "getContract" {
			want = 200
		}
		if rec := get(api, p, "other"); rec.Code != want {
			t.Errorf("notifier %s: %d, want %d", id, rec.Code, want)
		}
	}
}

func TestResponsesAreNotCachedAndThereIsNoCORS(t *testing.T) {
	api := apiWith(t, &captureStore{})
	for _, p := range []string{"/v1/sessions", "/v1/contract", "/healthz", "/v1/nope"} {
		rec := get(api, p, "alice", "Origin", "https://evil.example")
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %v", p, rec.Header())
		}
		for h := range rec.Header() {
			if strings.HasPrefix(strings.ToLower(h), "access-control-") {
				t.Errorf("%s: %s", p, h)
			}
		}
	}
	// A pre-flight is not answered: a browser cannot make a cross-origin call with the person's token to this API.
	req := httptest.NewRequest("OPTIONS", "/v1/sessions", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != 405 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
}

// A failed read says that it failed and nothing about why; an error from a store is logged, never shown.
func TestAFailedReadNamesNothing(t *testing.T) {
	api := apiWith(t, &failingStore{captureStore{}, errors.New("password authentication failed for user \"reader\" at 10.0.0.9: secret-dsn-detail")})
	for _, p := range []string{"/v1/sessions", "/v1/sessions/s-1", "/v1/events", "/v1/human/requests", "/v1/tasks/t", "/v1/agents"} {
		rec := get(api, p, "alice")
		if rec.Code != 503 || rec.Body.String() != `{"error":"read failed"}` {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

type failingStore struct {
	captureStore
	err error
}

func (f failingStore) Sessions(context.Context, SessionFilter, *Viewer, int, string, time.Time) ([]Record, error) {
	return nil, f.err
}
func (f failingStore) Session(context.Context, string, time.Time) (Record, error) { return nil, f.err }
func (f failingStore) Task(context.Context, string, *Viewer, time.Time) ([]Record, error) {
	return nil, f.err
}
func (f failingStore) HumanPending(context.Context, time.Time, string, *Viewer) ([]Record, error) {
	return nil, f.err
}
func (f failingStore) Events(context.Context, EventFilter, *Viewer, int, *time.Time) ([]Record, error) {
	return nil, f.err
}
func (f failingStore) Holds(context.Context, time.Time) (map[string]Record, error) { return nil, f.err }

// Requests are bounded: the query string, the requests in flight, and the time one request may take.
func TestRequestsAreBounded(t *testing.T) {
	api := apiWith(t, &captureStore{})
	if rec := get(api, "/v1/sessions?x="+strings.Repeat("a", MaxQuery+1), "alice"); rec.Code != 414 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := get(api, "/v1/sessions?x="+strings.Repeat("a", MaxQuery-10), "alice"); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	// In flight: with MaxInFlight 2 and two reads blocked, the third is turned away at once.
	release := make(chan struct{})
	slow := &blockingStore{release: release}
	api2, _ := New(&API{Access: accessWith(mapDir{}, mapOwn{}), Store: slow, Defs: definitions.NewSet(nil), MaxInFlight: 2, Timeout: 5 * time.Second})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); get(api2, "/v1/sessions", "alice") }()
	}
	for slow.waiting.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	rec := get(api2, "/v1/sessions", "alice")
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	if rec := get(api2, "/healthz", ""); rec.Code != 200 {
		t.Fatalf("the probe was refused with the API busy: %d", rec.Code)
	}
	close(release)
	wg.Wait()
	// Time: a read that never returns is cut at the request's timeout.
	hang := &blockingStore{release: make(chan struct{}), honourContext: true}
	api3, _ := New(&API{Access: accessWith(mapDir{}, mapOwn{}), Store: hang, Defs: definitions.NewSet(nil), Timeout: 100 * time.Millisecond})
	start := time.Now()
	rec = get(api3, "/v1/sessions", "alice")
	if rec.Code != 503 || time.Since(start) > 2*time.Second || rec.Body.String() != `{"error":"read failed"}` {
		t.Fatalf("%d %s after %v", rec.Code, rec.Body.String(), time.Since(start))
	}
}

type blockingStore struct {
	captureStore
	release       chan struct{}
	waiting       atomicInt
	honourContext bool
}

type atomicInt struct {
	mu sync.Mutex
	n  int
}

func (a *atomicInt) Add(d int) { a.mu.Lock(); a.n += d; a.mu.Unlock() }
func (a *atomicInt) Load() int { a.mu.Lock(); defer a.mu.Unlock(); return a.n }

func (b *blockingStore) Sessions(ctx context.Context, _ SessionFilter, _ *Viewer, _ int, _ string, _ time.Time) ([]Record, error) {
	b.waiting.Add(1)
	defer b.waiting.Add(-1)
	if b.honourContext {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	<-b.release
	return nil, nil
}

// Every list is bounded however the caller asks, and a cursor never reaches the database as anything but a time.
func TestListsAreBoundedAndCursorsAreTimes(t *testing.T) {
	rec := &recordingStore{}
	api, _ := New(&API{Access: accessWith(mapDir{}, mapOwn{}), Store: rec, Defs: definitions.NewSet(nil)})
	for _, c := range []struct {
		q    string
		want int
	}{{"", 50}, {"limit=1", 1}, {"limit=200", 200}, {"limit=201", 200}, {"limit=99999999999999999999", 200}, {"limit=0", 1}, {"limit=-5", 1}, {"limit=%2B7", 7}, {"limit=%207%20", 7}} {
		get(api, "/v1/sessions?"+c.q, "admin")
		get(api, "/v1/events?"+c.q, "admin")
		if rec.sessionsLimit != c.want || rec.eventsLimit != c.want {
			t.Errorf("limit %q: sessions %d events %d, want %d", c.q, rec.sessionsLimit, rec.eventsLimit, c.want)
		}
	}
	for _, bad := range []string{"limit=ten", "limit=1.5", "limit=1e2", "limit=0x10", "limit=1_0", "limit=%D9%A3", "limit=--1"} {
		if r := get(api, "/v1/sessions?"+bad, "admin"); r.Code != 400 {
			t.Errorf("%s: %d", bad, r.Code)
		}
	}
	rec.sessionsAfter = "unset"
	for _, bad := range []string{"'; DROP TABLE gateway_sessions;--", "2026-11-14", "2026-11-14T10:00:00+00:00' OR 1=1", "x", "2026-13-40T99:99:99", "%00", strings.Repeat("9", 50)} {
		rec.sessionsAfter = "unset"
		r := get(api, "/v1/sessions?after="+urlQ(bad), "admin")
		if r.Code != 400 || rec.sessionsAfter != "unset" {
			t.Errorf("%q reached the store or was not refused: %d %q", bad, r.Code, rec.sessionsAfter)
		}
	}
	for _, bad := range []string{"'; DROP", "yesterday", "2026-02-30T10:00:00+00:00", "2026-11-14T25:00:00"} {
		for _, k := range []string{"after", "since", "until"} {
			if r := get(api, "/v1/events?"+k+"="+urlQ(bad), "admin"); r.Code != 400 {
				t.Errorf("events %s=%q: %d", k, bad, r.Code)
			}
		}
	}
	get(api, "/v1/sessions?after="+urlQ("2026-11-14T10:00:00.123456+00:00"), "admin")
	if rec.sessionsAfter != "2026-11-14T10:00:00.123456+00:00" {
		t.Fatalf("a good cursor did not arrive as written: %q", rec.sessionsAfter)
	}
	// A filter that cannot have been stored is the empty answer and never a query.
	rec.calls = 0
	for _, q := range []string{"agent=" + urlQ("a\x00b"), "principal=" + urlQ(strings.Repeat("a", 300)), "task=" + urlQ("x\ny")} {
		r := get(api, "/v1/sessions?"+q, "admin")
		if r.Code != 200 || r.Body.String() != `{"items":[],"next":null}` || rec.calls != 0 {
			t.Errorf("%q: %d %s calls=%d", q, r.Code, r.Body.String(), rec.calls)
		}
	}
}

func urlQ(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

type recordingStore struct {
	captureStore
	sessionsLimit, eventsLimit, calls int
	sessionsAfter                     string
}

func (r *recordingStore) Sessions(_ context.Context, _ SessionFilter, _ *Viewer, limit int, after string, _ time.Time) ([]Record, error) {
	r.sessionsLimit, r.sessionsAfter = limit, after
	r.calls++
	return nil, nil
}
func (r *recordingStore) Events(_ context.Context, _ EventFilter, _ *Viewer, limit int, _ *time.Time) ([]Record, error) {
	r.eventsLimit = limit
	r.calls++
	return nil, nil
}

// One header says who a delegate acts for, and the person is not taken from anywhere else (a query parameter, a second header).
func TestOnlyTheOneHeaderNamesThePerson(t *testing.T) {
	rec := &recordingStore{}
	api, _ := New(&API{Access: accessWith(mapDir{}, mapOwn{}), Store: rec, Defs: definitions.NewSet(nil)})
	if r := get(api, "/v1/sessions", "delegate", "X-Hangar-On-Behalf-Of", "user:bob", "X-Hangar-On-Behalf-Of", "user:admin"); r.Code != 400 {
		t.Fatalf("two headers: %d", r.Code)
	}
	if r := get(api, "/v1/sessions?on_behalf_of=user:admin&person=user:admin&principal=user:admin", "alice", "X-Forwarded-User", "user:admin", "X-Remote-User", "user:admin"); r.Code != 200 {
		t.Fatalf("%d", r.Code)
	}
	var captured *Viewer
	cs := &viewerStore{recordingStore: rec, f: func(v *Viewer) { captured = v }}
	api.Store = cs
	get(api, "/v1/sessions?person=user:admin", "alice", "X-Forwarded-User", "user:admin")
	if captured == nil || captured.Person != "user:alice" || captured.All() {
		t.Fatalf("%+v", captured)
	}
	if r := get(api, "/v1/sessions", "alice", "X-Hangar-On-Behalf-Of", "user:bob"); r.Code != 403 {
		t.Fatalf("a person naming another: %d", r.Code)
	}
	b, _ := json.Marshal(map[string]string{})
	_ = b
}

type viewerStore struct {
	*recordingStore
	f func(*Viewer)
}

func (v *viewerStore) Sessions(ctx context.Context, f SessionFilter, vw *Viewer, limit int, after string, now time.Time) ([]Record, error) {
	v.f(vw)
	return v.recordingStore.Sessions(ctx, f, vw, limit, after, now)
}
