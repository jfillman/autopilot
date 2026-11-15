package readapi

import (
	"context"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestBackstage(t *testing.T, base string) *Backstage {
	t.Helper()
	f := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(f, []byte("catalog-token-xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewBackstage(base, f)
}

type captureStore struct {
	viewer *Viewer
}

func (c *captureStore) Sessions(_ context.Context, _ SessionFilter, v *Viewer, _ int, _ string, _ time.Time) ([]Record, error) {
	c.viewer = v
	return nil, nil
}
func (c *captureStore) Session(context.Context, string, time.Time) (Record, error) { return nil, nil }
func (c *captureStore) Result(context.Context, string, time.Time) (Record, error)  { return nil, nil }
func (c *captureStore) Task(context.Context, string, *Viewer, time.Time) ([]Record, error) {
	return nil, nil
}
func (c *captureStore) HumanPending(context.Context, time.Time, string, *Viewer) ([]Record, error) {
	return nil, nil
}
func (c *captureStore) Events(context.Context, EventFilter, *Viewer, int, *time.Time) ([]Record, error) {
	return nil, nil
}
func (c *captureStore) Holds(context.Context, time.Time) (map[string]Record, error) { return nil, nil }
func (c *captureStore) Definitions(context.Context) ([]definitions.Row, error)      { return nil, nil }
func (c *captureStore) Ping(context.Context) error                                  { return nil }

func TestTheCatalogIsReadAsPythonReadsIt(t *testing.T) {
	var targets []string
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targets = append(targets, r.URL.RequestURI())
		auth = append(auth, r.Header.Get("Authorization"))
		if strings.Contains(r.URL.Path, "/by-name/user/") {
			_, _ = w.Write([]byte(`{"relations":[{"type":"memberOf","targetRef":"group:default/team-skyport"},{"type":"ownerOf","targetRef":"component:default/x"},{"type":"memberOf","targetRef":"user:default/zzz"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"gate-api"}},{"metadata":{"name":"Flight-API"}},{"nometadata":1}]}`))
	}))
	defer srv.Close()
	b := newTestBackstage(t, srv.URL)
	apps, err := b.Apps(context.Background(), "user:jfillman")
	if err != nil || len(apps) != 2 || !apps["gate-api"] || !apps["flight-api"] {
		t.Fatalf("%v %v", apps, err)
	}
	if len(targets) != 2 || !strings.HasPrefix(targets[0], "/api/catalog/entities/by-name/user/default/jfillman") ||
		!strings.Contains(targets[1], "spec.owner%3Duser%3Adefault%2Fjfillman") || !strings.Contains(targets[1], "spec.owner%3Dgroup%3Adefault%2Fteam-skyport") ||
		!strings.Contains(targets[1], "limit=500") {
		t.Fatalf("%v", targets)
	}
	for _, a := range auth {
		if a != "Bearer catalog-token-xyz" {
			t.Fatalf("authorization %q", a)
		}
	}
	// Not a person, or not a name an entity could have: no request at all, and no groups.
	for _, p := range []string{"ns/sa", "user:../x", "user:", "user:a b", "x", "user:" + strings.Repeat("a", 300)} {
		n := len(targets)
		g, err := b.Groups(context.Background(), p)
		a, err2 := b.Apps(context.Background(), p)
		if err != nil || err2 != nil || len(g) != 0 || len(a) != 0 || len(targets) != n {
			t.Errorf("%q: %v %v %v %v %d", p, g, a, err, err2, len(targets)-n)
		}
	}
}

func TestACatalogAnswerIsCachedAFailureBrieflyAndAnUnknownPersonIsNotAFailure(t *testing.T) {
	var hits atomic.Int64
	status := atomic.Int64{}
	status.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if s := int(status.Load()); s != 200 {
			http.Error(w, "x", s)
			return
		}
		_, _ = w.Write([]byte(`{"relations":[]}`))
	}))
	defer srv.Close()
	b := newTestBackstage(t, srv.URL)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	b.Now = func() time.Time { return now }
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = b.Groups(ctx, "user:a")
	}
	if hits.Load() != 1 {
		t.Fatalf("%d requests for 5 reads", hits.Load())
	}
	status.Store(404) // the catalog does not know the person: an answer
	if g, err := b.Groups(ctx, "user:ghost"); err != nil || len(g) != 0 {
		t.Fatalf("%v %v", g, err)
	}
	status.Store(500)
	for i := 0; i < 5; i++ {
		if _, err := b.Groups(ctx, "user:c"); err == nil {
			t.Fatal("a 500 is not an answer")
		}
	}
	h := hits.Load()
	now = now.Add(11 * time.Second) // the failure's own, shorter lifetime
	_, _ = b.Groups(ctx, "user:c")
	if hits.Load() != h+1 {
		t.Fatalf("a failure was not retried after FailTTL: %d", hits.Load()-h)
	}
	for _, s := range []int{401, 403, 502} {
		status.Store(int64(s))
		now = now.Add(time.Minute)
		if _, err := b.Groups(ctx, "user:d"); err == nil {
			t.Fatalf("%d is an answer", s)
		}
	}
}

func TestConcurrentReadsOfOnePersonAskTheCatalogOnce(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"relations":[]}`))
	}))
	defer srv.Close()
	b := newTestBackstage(t, srv.URL)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = b.Groups(context.Background(), "user:a") }()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("%d requests", hits.Load())
	}
}

// The token goes to the configured host only: a redirect is not followed, and an answer that is not JSON of the shape is a failure.
func TestTheCatalogTokenIsNotSentWhereItWasNotMeantAndBadAnswersFail(t *testing.T) {
	var leaked atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer srv.Close()
	b := newTestBackstage(t, srv.URL)
	if _, err := b.Groups(context.Background(), "user:a"); err == nil || leaked.Load() != 0 {
		t.Fatalf("%v leaked=%d", err, leaked.Load())
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<html>`)) }))
	defer bad.Close()
	if _, err := newTestBackstage(t, bad.URL).Groups(context.Background(), "user:a"); err == nil {
		t.Fatal("html is not an answer")
	}
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(strings.Repeat("x", 5<<20))) }))
	defer big.Close()
	if _, err := newTestBackstage(t, big.URL).Groups(context.Background(), "user:a"); err == nil {
		t.Fatal("5 MiB is not an answer")
	}
	// No token file: an error, and no request.
	nb := NewBackstage(srv.URL, filepath.Join(t.TempDir(), "absent"))
	if _, err := nb.Groups(context.Background(), "user:a"); err == nil {
		t.Fatal("no token")
	}
}

// DispatchDirectory (what the inbox and the person controls read): an error is no group, which is the same fail-closed rule.
func TestTheInboxsDirectoryFailsClosed(t *testing.T) {
	b := NewBackstage("http://127.0.0.1:1", filepath.Join(t.TempDir(), "absent"))
	if g := (DispatchDirectory{B: b}).Groups(context.Background(), "user:a"); len(g) != 0 {
		t.Fatalf("%v", g)
	}
}
