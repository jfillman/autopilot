package readapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

func do(a *API, method, path string, token, obo *string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != nil {
		req.Header.Set("Authorization", "Bearer tok-"+*token)
	}
	if obo != nil {
		req.Header.Set("X-Hangar-On-Behalf-Of", *obo)
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec
}

func norm(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("not JSON: %q", raw)
	}
	return v
}

// downStore fails every read.
type downStore struct{ Store }

func (downStore) fail() error { return errors.New("the database is down") }
func (d downStore) Sessions(context.Context, SessionFilter, *Viewer, int, string, time.Time) ([]Record, error) {
	return nil, d.fail()
}
func (d downStore) Session(context.Context, string, time.Time) (Record, error) { return nil, d.fail() }
func (d downStore) Result(context.Context, string, time.Time) (Record, error)  { return nil, d.fail() }
func (d downStore) Task(context.Context, string, *Viewer, time.Time) ([]Record, error) {
	return nil, d.fail()
}
func (d downStore) HumanPending(context.Context, time.Time, string, *Viewer) ([]Record, error) {
	return nil, d.fail()
}
func (d downStore) Events(context.Context, EventFilter, *Viewer, int, *EventCursor) ([]Record, error) {
	return nil, d.fail()
}
func (d downStore) Holds(context.Context, time.Time) (map[string]Record, error) { return nil, d.fail() }
func (d downStore) Definitions(context.Context) ([]definitions.Row, error)      { return nil, d.fail() }
func (d downStore) Ping(context.Context) error                                  { return d.fail() }

// Every request the Python API was asked, as each caller, is answered by the Go API over a real database seeded with the same rows: the
// same status, content type, Allow header and JSON body, except where the case carries a "go" and a "why".
func TestTheAPIEqualsPython(t *testing.T) {
	for _, loc := range locales { // the text compares in C whatever the database's locale is
		t.Run(loc, func(t *testing.T) { equalsPython(t, loc) })
	}
}

func equalsPython(t *testing.T, locale string) {
	f := loadFixture(t)
	dsn, st := scratchLocale(t, locale)
	f.seed(t, st)
	store, err := OpenPg(context.Background(), dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	live, down := f.api(t, store), f.api(t, downStore{store})
	if len(f.Cases) < 800 {
		t.Fatalf("%d cases", len(f.Cases))
	}
	diffs, statuses := 0, map[int]int{}
	for _, c := range f.Cases {
		if (c.Go == nil) != (c.Why == "") {
			t.Fatalf("%s: a \"go\" needs a \"why\" and a \"why\" needs a \"go\"", c.Name)
		}
		want := c.Python
		if c.Go != nil {
			want, diffs = *c.Go, diffs+1
		}
		caller := f.Callers[c.Caller]
		api := live
		if c.Down {
			api = down
		}
		rec := do(api, c.Method, c.Path, caller.Token, caller.OnBehalfOf)
		statuses[rec.Code]++
		if rec.Code != want.Status {
			t.Errorf("%s: status %d, want %d (%s)", c.Name, rec.Code, want.Status, rec.Body.String())
			continue
		}
		if want.Allow != nil && rec.Header().Get("Allow") != *want.Allow {
			t.Errorf("%s: Allow %q, want %q", c.Name, rec.Header().Get("Allow"), *want.Allow)
		}
		if c.Method == "HEAD" {
			continue // the recorder keeps a HEAD's body; the server drops it (TestAHeadHasNoBody)
		}
		if want.ContentType != nil && rec.Header().Get("Content-Type") != *want.ContentType {
			t.Errorf("%s: content type %q, want %q", c.Name, rec.Header().Get("Content-Type"), *want.ContentType)
		}
		if want.ContentType != nil && strings.Contains(*want.ContentType, "json") {
			got := norm(t, rec.Body.String())
			if m, ok := got.(map[string]any); ok {
				stripCursorID(t, c.Name, m)
			}
			if !reflect.DeepEqual(got, any(map[string]any(want.Body))) {
				g, _ := json.Marshal(got)
				w, _ := json.Marshal(want.Body)
				t.Errorf("%s: the body differs\n go:     %.600s\n python: %.600s", c.Name, g, w)
			}
		} else if txt, _ := want.Body["text"].(string); rec.Body.String() != txt {
			t.Errorf("%s: body %q, want %q", c.Name, rec.Body.String(), txt)
		}
		if h := rec.Header().Get("Cache-Control"); h != "no-store" {
			t.Errorf("%s: Cache-Control %q", c.Name, h)
		}
	}
	t.Logf("%d cases, %d answered differently on purpose, statuses %v", len(f.Cases), diffs, statuses)
	if statuses[200] < 300 || statuses[403] < 200 || statuses[401] < 90 || statuses[404] < 100 {
		t.Fatalf("the cases do not cover the callers: %v", statuses)
	}
	// What only Go refuses.
	for _, c := range f.GoOnly {
		if c.Why == "" {
			t.Fatalf("%s: no why", c.Name)
		}
		caller := f.Callers[c.Caller]
		rec := do(live, "GET", c.Path, caller.Token, caller.OnBehalfOf)
		if rec.Code != c.Status || !reflect.DeepEqual(norm(t, rec.Body.String()), any(c.Body)) {
			t.Errorf("%s: %d %s, want %d %v", c.Name, rec.Code, rec.Body.String(), c.Status, c.Body)
		}
	}
}

func TestEveryRouteOfTheContractHasAHandlerAndTheOtherWayRound(t *testing.T) {
	a, err := New(&API{Access: &Access{}, Defs: definitions.NewSet(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Operations()) != 10 {
		t.Fatalf("%v", a.Operations())
	}
	for id := range a.fns {
		found := false
		for _, o := range a.Operations() {
			found = found || o == id
		}
		if !found {
			t.Errorf("a handler with no route: %s", id)
		}
	}
	_ = http.StatusOK
}

func TestAHeadHasNoBody(t *testing.T) {
	f := loadFixture(t)
	a := f.api(t, downStore{})
	srv := httptest.NewServer(a)
	defer srv.Close()
	resp, err := http.Head(srv.URL + "/v1/contract")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := make([]byte, 10)
	if n, _ := resp.Body.Read(b); n != 0 || resp.StatusCode != 200 {
		t.Fatalf("status %d, %d body bytes", resp.StatusCode, n)
	}
}

// stripCursorID turns the cursor Go returns, "<time>|<id>", into Python's, "<time>": the id must be the last row's, and the time is what
// Python's cursor is. (A difference carried on purpose: a time alone loses the rows that share it.)
func stripCursorID(t *testing.T, name string, body map[string]any) {
	t.Helper()
	next, ok := body["next"].(string)
	if !ok {
		return
	}
	items, _ := body["items"].([]any)
	last, _ := items[len(items)-1].(map[string]any)
	ts, id, hasID := SplitCursor(next)
	if !hasID || last["id"] != id {
		t.Errorf("%s: the cursor %q is not the time and id of the last row", name, next)
	}
	body["next"] = ts
}
