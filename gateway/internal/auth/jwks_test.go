package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// clock is a test clock the background fetch can read while the test moves it.
type clock struct{ t atomic.Int64 }

func newClock() *clock { c := &clock{}; c.t.Store(fixedNow.UnixNano()); return c }

func (c *clock) now() time.Time          { return time.Unix(0, c.t.Load()) }
func (c *clock) advance(d time.Duration) { c.t.Add(int64(d)) }

func TestCancelledRequestsNeverStopTheRefreshForOthers(t *testing.T) {
	srv := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil))
	o := newOIDC(t, srv)
	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := o.Authenticate(ctx, mint(rsHeader("nope"), goodClaims(nil), signRS256(testRSA)))
		mustRefuse(t, err)
	}
	good := mint(rsHeader("k1"), goodClaims(nil), signRS256(testRSA))
	// The detached fetch the cancelled callers started may still be running; the honest caller shares it or starts one.
	if _, err := o.Authenticate(context.Background(), good); err != nil {
		t.Fatalf("refused after cancelled requests: %v", err)
	}
	if h := srv.hits.Load(); h != 1 {
		t.Fatalf("%d fetches", h)
	}
}

func TestWaiterReturnsWhenItsOwnContextEnds(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	defer slow.Close()
	defer close(release)
	j := &JWKS{URL: slow.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := j.Key(ctx, "k1", true)
	mustRefuse(t, err)
	if time.Since(start) > 2*time.Second {
		t.Fatal("the waiter outlived its own context")
	}
}

func TestFailedFetchRetriesSoonSuccessfulOneDoesNot(t *testing.T) {
	c := newClock()
	var up atomic.Bool
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !up.Load() {
			http.Error(w, "down", 503)
			return
		}
		_, _ = w.Write([]byte(`{"keys":[` + `{"kty":"RSA","kid":"k1","n":"` + b64u(testRSA.N.Bytes()) + `","e":"AQAB"}]}`))
	}))
	defer srv.Close()
	j := &JWKS{URL: srv.URL, Now: c.now}
	ask := func(kid string) error { _, err := j.Key(context.Background(), kid, true); return err }
	// Down: 20 requests inside 5 s fetch once; after 5 s one more try; once up, the next try succeeds.
	for i := 0; i < 20; i++ {
		_ = ask("k1")
	}
	if hits.Load() != 1 {
		t.Fatalf("%d fetches while down", hits.Load())
	}
	c.advance(4 * time.Second)
	_ = ask("k1")
	if hits.Load() != 1 {
		t.Fatal("retried inside the failure interval")
	}
	up.Store(true)
	c.advance(2 * time.Second)
	if err := ask("k1"); err != nil || hits.Load() != 2 {
		t.Fatalf("not retried after the failure interval: %v %d", err, hits.Load())
	}
	// Up: 50 unknown kids cause no fetch, and none until a minute has passed.
	for i := 0; i < 50; i++ {
		mustRefuse(t, ask("nope"))
	}
	c.advance(59 * time.Second)
	mustRefuse(t, ask("nope"))
	if hits.Load() != 2 {
		t.Fatalf("%d fetches inside the minute after a success", hits.Load())
	}
	c.advance(2 * time.Second)
	mustRefuse(t, ask("nope"))
	if hits.Load() != 3 {
		t.Fatalf("%d fetches after the minute", hits.Load())
	}
}

func TestFiftyUnknownKidsAfterASuccessCauseOneFetch(t *testing.T) {
	srv := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil))
	j := &JWKS{URL: srv.URL, Now: fixedClock}
	if _, err := j.Key(context.Background(), "k1", true); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = j.Key(context.Background(), "nope", true)
		}()
	}
	wg.Wait()
	if srv.hits.Load() != 1 {
		t.Fatalf("%d fetches", srv.hits.Load())
	}
}

func TestWithdrawnKeyStopsBeingTrustedAfterTenMinutes(t *testing.T) {
	c := newClock()
	srv := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil), jwkRSA("k2", &otherRSA.PublicKey, nil))
	j := &JWKS{URL: srv.URL, Now: c.now}
	if _, err := j.Key(context.Background(), "k2", true); err != nil {
		t.Fatal(err)
	}
	srv.keys.Store([]map[string]any{jwkRSA("k1", &testRSA.PublicKey, nil)}) // the issuer withdraws k2
	c.advance(9 * time.Minute)
	if _, err := j.Key(context.Background(), "k2", true); err != nil || srv.hits.Load() != 1 {
		t.Fatalf("inside ten minutes the old set stands: %v %d", err, srv.hits.Load())
	}
	c.advance(2 * time.Minute) // 11 minutes: the request that notices still uses the old key, the refetch runs behind it
	_, _ = j.Key(context.Background(), "k2", true)
	deadline := time.Now().Add(5 * time.Second)
	for srv.hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for {
		j.mu.RLock()
		busy := j.inflight != nil
		j.mu.RUnlock()
		if !busy {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := j.Key(context.Background(), "k2", true); err == nil {
		t.Fatal("a withdrawn key is still trusted after the unconditional refresh")
	}
	if _, err := j.Key(context.Background(), "k1", true); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshFailureIsNotFatalAndDoesNotBlockLaterUse(t *testing.T) {
	c := newClock()
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", 503)
			return
		}
		_, _ = w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"k1","n":"` + b64u(testRSA.N.Bytes()) + `","e":"AQAB"}]}`))
	}))
	defer srv.Close()
	j := &JWKS{URL: srv.URL, Now: c.now}
	if err := j.Refresh(context.Background()); err == nil {
		t.Fatal("a failed prefetch reported success")
	}
	up.Store(true)
	c.advance(6 * time.Second)
	if _, err := j.Key(context.Background(), "k1", true); err != nil {
		t.Fatalf("the first real request after a failed prefetch: %v", err)
	}
	// A prefetch that finds the keys already loaded does not fetch again.
	if err := j.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCacheEvictsTheOldestQuarter(t *testing.T) {
	now := fixedNow
	inner := &spyAuth{p: &dispatch.Principal{ID: "u"}}
	c := &Cache{Inner: inner, TTL: time.Hour, Max: 8, Now: func() time.Time { return now }}
	tokens := make([]string, 0, 9)
	for i := 0; i < 9; i++ {
		// exp grows with i, so token 0 expires first
		tokens = append(tokens, mint(map[string]any{"alg": "RS256"}, map[string]any{"exp": now.Unix() + 100 + int64(i)}, func(string) []byte { return nil }))
	}
	for _, tk := range tokens[:8] {
		if _, err := c.Authenticate(context.Background(), tk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Authenticate(context.Background(), tokens[8]); err != nil { // over the cap: 2 (a quarter of 8) go
		t.Fatal(err)
	}
	if n := len(c.m); n != 7 {
		t.Fatalf("%d entries after eviction, want 7 (not an emptied map)", n)
	}
	before := inner.calls.Load()
	for _, tk := range tokens[2:] { // the newest seven are still cached
		_, _ = c.Authenticate(context.Background(), tk)
	}
	if inner.calls.Load() != before {
		t.Fatal("a recent entry was evicted")
	}
	_, _ = c.Authenticate(context.Background(), tokens[0])
	if inner.calls.Load() != before+1 {
		t.Fatal("the oldest entry was kept")
	}
}

func TestTokenWithAnythingButBase64urlAndDotsIsRefused(t *testing.T) {
	srv := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil))
	o := newOIDC(t, srv)
	good := mint(rsHeader("k1"), goodClaims(nil), signRS256(testRSA))
	if _, err := o.Authenticate(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	p := strings.Split(good, ".")
	for name, bad := range map[string]string{
		"LF in the signature": p[0] + "." + p[1] + "." + p[2][:10] + "\n" + p[2][10:],
		"CR in the signature": p[0] + "." + p[1] + "." + p[2][:10] + "\r" + p[2][10:],
		"CRLF in the payload": p[0] + "." + p[1][:5] + "\r\n" + p[1][5:] + "." + p[2],
		"space":               p[0] + "." + p[1] + " ." + p[2],
		"plus":                p[0] + "." + p[1] + "+." + p[2],
		"slash":               p[0] + "." + p[1] + "/." + p[2],
		"equals":              p[0] + "." + p[1] + "." + p[2] + "=",
		"NUL":                 p[0] + "." + p[1] + "." + p[2] + "\x00",
		"non-ASCII":           p[0] + "." + p[1] + "." + p[2] + "\u00e9",
		"leading space":       " " + good,
		"trailing newline":    good + "\n",
	} {
		_, err := o.Authenticate(context.Background(), bad)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		mustRefuse(t, err)
	}
}

func TestEmptyAudienceNeverMatches(t *testing.T) {
	for _, aud := range []any{"", []any{""}, []any{"", "x"}, "x", nil, 5} {
		if audienceMatches(aud, "") {
			t.Errorf("an empty configured audience matched %v", aud)
		}
	}
	if !audienceMatches("x", "x") || !audienceMatches([]any{"a", "x"}, "x") {
		t.Fatal("the normal match is gone")
	}
	// And an authenticator configured with no audience accepts nothing.
	srv := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil))
	o := newOIDC(t, srv)
	o.Audience = ""
	for _, a := range []any{"", []string{""}} {
		_, err := o.Authenticate(context.Background(), mint(rsHeader("k1"), goodClaims(map[string]any{"aud": a}), signRS256(testRSA)))
		mustRefuse(t, err)
	}
}
