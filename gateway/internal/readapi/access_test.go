package readapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

type who map[string]dispatch.Principal

func (w who) Authenticate(_ context.Context, token string) (*dispatch.Principal, error) {
	if token == "outage" {
		return nil, errors.New("the TokenReview API is unreachable")
	}
	if p, ok := w[token]; ok {
		return &p, nil
	}
	return nil, &core.AuthError{Msg: "token rejected"}
}

type errDir struct{ calls atomic.Int64 }

func (d *errDir) Groups(context.Context, string) (map[string]bool, error) {
	d.calls.Add(1)
	return map[string]bool{}, errors.New("the catalog is down")
}
func (d *errDir) Apps(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, errors.New("the catalog is down")
}

func accessWith(dir Directory, own Ownership) *Access {
	return &Access{Auth: who{
		"admin":    {ID: "user:admin", Kind: "user"},
		"alice":    {ID: "user:alice", Kind: "user"},
		"delegate": {ID: "hangar-prod/app-backstage-prod/backstage-ingestor", Kind: "workload"},
		"other":    {ID: "hangar-prod/app-x/worker", Kind: "workload"},
		"run":      {ID: "agent-r-0123456789ab/run", Kind: "workload"},
		"fake":     {ID: "user:hangar-prod/app-backstage-prod/backstage-ingestor", Kind: "user"},
	}, Delegates: []string{"hangar-prod/app-backstage-prod/backstage-*"}, Auditors: []string{"user:admin"}, Notifiers: []string{"hangar-prod/monitoring/*"},
		Directory: dir, Ownership: own, Defs: definitions.NewSet(nil)}
}

// The delegate rule: a delegate may assert a person, and nothing else may. A person, a run, an unlisted workload and anything that
// merely looks like the delegate's name are refused; the asserted person must be a principal form; one header only.
func TestOnlyADelegateMayAssertAPerson(t *testing.T) {
	a := accessWith(mapDir{"user:bob": {"group:default/team-a"}}, mapOwn{})
	ctx := context.Background()
	for _, c := range []struct {
		token string
		obo   []string
		code  int
	}{
		{"delegate", []string{"user:bob"}, 0}, {"delegate", []string{"user:bob@example.com"}, 0}, {"delegate", []string{"user:b.o-b_9+x"}, 0},
		{"alice", []string{"user:bob"}, 403}, {"admin", []string{"user:bob"}, 403}, {"other", []string{"user:bob"}, 403}, {"run", []string{"user:bob"}, 403},
		{"fake", []string{"user:bob"}, 403}, // a person whose name is the delegate's is still a person
		{"delegate", []string{"bob"}, 400}, {"delegate", []string{"user:"}, 400}, {"delegate", []string{"user:" + strings.Repeat("a", 200)}, 400},
		{"delegate", []string{"user:bob\nuser:admin"}, 400}, {"delegate", []string{"user:bob admin"}, 400}, {"delegate", []string{"user:*"}, 400},
		{"delegate", []string{"user:bob", "user:admin"}, 400}, // two headers: not one person
		{"delegate", []string{"group:default/team-a"}, 400}, {"delegate", []string{"hangar-prod/x/y"}, 400}, {"delegate", []string{"user:‮evil"}, 400},
		{"nope", []string{"user:bob"}, 401}, {"", []string{"user:bob"}, 401},
	} {
		v, err := a.Viewer(ctx, c.token, c.obo)
		if c.code == 0 {
			if err != nil || v.Role != RolePerson || v.Via != "hangar-prod/app-backstage-prod/backstage-ingestor" || v.Person != c.obo[0] || v.RouteRole() != RoleDelegate {
				t.Errorf("%s for %v: %+v %v", c.token, c.obo, v, err)
			}
			continue
		}
		if status(err) != c.code {
			t.Errorf("%s for %q: %v (%d), want %d", c.token, c.obo, err, status(err), c.code)
		}
	}
	// What the delegate asserts is never an auditor, whoever the person is: the auditors list is not consulted for them.
	v, err := a.Viewer(ctx, "delegate", []string{"user:admin"})
	if err != nil || v.All() {
		t.Fatalf("an asserted admin is an auditor: %+v %v", v, err)
	}
}

func TestEachCallerIsOneOfTheFiveOrRefused(t *testing.T) {
	a := accessWith(mapDir{}, mapOwn{})
	ctx := context.Background()
	for _, c := range []struct {
		token, role string
		code        int
	}{{"admin", RoleAuditor, 0}, {"alice", RolePerson, 0}, {"other", "", 403}, {"run", "", 403}, {"delegate", "", 403}, {"nope", "", 401}, {"", "", 401}, {"outage", "", 503}} {
		v, err := a.Viewer(ctx, c.token, nil)
		if c.code != 0 {
			if status(err) != c.code {
				t.Errorf("%s: %v (%d), want %d", c.token, err, status(err), c.code)
			}
			continue
		}
		if err != nil || v.Role != c.role {
			t.Errorf("%s: %+v %v", c.token, v, err)
		}
	}
	// A token outage is not a bad token: it is a failed read (503), so nobody is told their good token is wrong.
	if _, err := a.Viewer(ctx, "outage", nil); status(err) != 503 {
		t.Fatalf("%v", err)
	}
	a.Notifiers = []string{"hangar-prod/app-x/*"}
	if v, err := a.Viewer(ctx, "other", nil); err != nil || v.Role != RoleNotifier {
		t.Fatalf("%+v %v", v, err)
	}
}

// Owner-group resolution fails closed: a catalog that cannot answer leaves a person with their own sessions and says so; it never gives them
// a team's, and never touches an auditor, who does not ask the catalog at all.
func TestAnUnavailableCatalogFailsClosedAndNeverTouchesAnAuditor(t *testing.T) {
	dir := &errDir{}
	a := accessWith(dir, dir)
	v, err := a.Viewer(context.Background(), "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !v.OwnersUnknown || len(v.Groups) != 0 || len(v.Agents) != 0 || len(v.Apps) != 0 || v.Person != "user:alice" {
		t.Fatalf("a person is given more than their own when the catalog is down: %+v", v)
	}
	before := dir.calls.Load()
	adm, err := a.Viewer(context.Background(), "admin", nil)
	if err != nil || !adm.All() || adm.OwnersUnknown {
		t.Fatalf("an outage reached an auditor: %+v %v", adm, err)
	}
	if dir.calls.Load() != before {
		t.Fatal("an auditor's request asked the catalog")
	}
	// The answer says the groups are unknown, and the person still reads their own sessions (the query is scoped to them alone).
	set := definitions.NewSet(nil)
	api, _ := New(&API{Access: a, Defs: set, Store: &captureStore{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer alice")
	api.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("X-Autopilot-Owners") != "unknown" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	cs := api.Store.(*captureStore)
	if cs.viewer == nil || cs.viewer.Person != "user:alice" || len(cs.viewer.Agents) != 0 || cs.viewer.All() {
		t.Fatalf("the query was not scoped to the person alone: %+v", cs.viewer)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer admin")
	api.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("X-Autopilot-Owners") != "" || !cs.viewer.All() {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
}

// What a person who is a member of a group sees changes with the catalog's answer, never past it: a person removed from a group has the
// sessions of that group's agents gone as soon as the cache lapses, and a failure after a good answer is not served from the old one.
func TestAStaleAnswerIsNeverUsedPastItsTTL(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", 503)
			return
		}
		_, _ = w.Write([]byte(`{"relations":[{"type":"memberOf","targetRef":"group:default/team-a"}]}`))
	}))
	defer srv.Close()
	b := newTestBackstage(t, srv.URL)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	b.Now = func() time.Time { return now }
	g, err := b.Groups(context.Background(), "user:bob")
	if err != nil || !g["group:default/team-a"] {
		t.Fatalf("%v %v", g, err)
	}
	up.Store(false)
	now = now.Add(30 * time.Second) // inside the TTL: the answer stands
	if g, err = b.Groups(context.Background(), "user:bob"); err != nil || !g["group:default/team-a"] {
		t.Fatalf("%v %v", g, err)
	}
	now = now.Add(31 * time.Second) // past it: the catalog is asked again, fails, and the person is held to nothing
	if g, err = b.Groups(context.Background(), "user:bob"); err == nil || len(g) != 0 {
		t.Fatalf("a stale answer was used: %v %v", g, err)
	}
}
