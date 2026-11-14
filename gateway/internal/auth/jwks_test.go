package auth

import (
	"context"
	"crypto"
	"crypto/sha256"
	"encoding/hex"
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
	o := &OIDC{Issuer: iss, Audience: aud, Keys: &JWKS{URL: srv.URL, Now: c.now}, Allowed: []string{"*"}, Now: c.now}
	tokenK2 := func() string {
		return mint(rsHeader("k2"), goodClaims(map[string]any{"iat": c.now().Unix() - 10, "exp": c.now().Unix() + 3600}), signRS256(otherRSA))
	}
	if _, err := o.Authenticate(context.Background(), tokenK2()); err != nil {
		t.Fatal(err)
	}
	srv.keys.Store([]map[string]any{jwkRSA("k1", &testRSA.PublicKey, nil)}) // the issuer withdraws k2
	c.advance(9 * time.Minute)
	if _, err := o.Authenticate(context.Background(), tokenK2()); err != nil || srv.hits.Load() != 1 {
		t.Fatalf("inside ten minutes the old set stands: %v %d", err, srv.hits.Load())
	}
	c.advance(2 * time.Minute) // 11 minutes: the verified request still uses the old key, the refetch runs behind it
	if _, err := o.Authenticate(context.Background(), tokenK2()); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, o.Keys)
	if srv.hits.Load() != 2 {
		t.Fatalf("%d fetches, want the one age refetch", srv.hits.Load())
	}
	if _, err := o.Authenticate(context.Background(), tokenK2()); err == nil {
		t.Fatal("a withdrawn key is still trusted after the unconditional refresh")
	}
	k1 := mint(rsHeader("k1"), goodClaims(map[string]any{"iat": c.now().Unix() - 10, "exp": c.now().Unix() + 3600}), signRS256(testRSA))
	if _, err := o.Authenticate(context.Background(), k1); err != nil {
		t.Fatal(err)
	}
}

// An unauthenticated caller cannot drive the age refetch: known-kid lookups and bad signatures never start it.
func TestAgeRefetchNeedsAVerifiedToken(t *testing.T) {
	c := newClock()
	srv := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil))
	o := &OIDC{Issuer: iss, Audience: aud, Keys: &JWKS{URL: srv.URL, Now: c.now}, Allowed: []string{"*"}, Now: c.now}
	mk := func(sg sign) string {
		return mint(rsHeader("k1"), goodClaims(map[string]any{"iat": c.now().Unix() - 10, "exp": c.now().Unix() + 3600}), sg)
	}
	if _, err := o.Authenticate(context.Background(), mk(signRS256(testRSA))); err != nil {
		t.Fatal(err)
	}
	c.advance(30 * time.Minute)
	for i := 0; i < 100; i++ {
		_, _ = o.Authenticate(context.Background(), mk(signRS256(otherRSA))) // known kid, bad signature
		_, _ = o.Keys.Key(context.Background(), "k1", true)
	}
	waitIdle(t, o.Keys)
	if srv.hits.Load() != 1 {
		t.Fatalf("an unauthenticated caller caused %d fetches", srv.hits.Load())
	}
	if _, err := o.Authenticate(context.Background(), mk(signRS256(testRSA))); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, o.Keys)
	if srv.hits.Load() != 2 {
		t.Fatalf("a verified token did not trigger the age refetch (%d fetches)", srv.hits.Load())
	}
}

// With the issuer down, a flood of bad-signature known-kid tokens and unknown-kid tokens for ten simulated minutes
// causes a bounded number of fetches: after the first failure 5 s, then 10, 20, 40, 60 s apart, and an unknown kid only
// after a minute on a good set.
func TestFloodWhileDownIsBounded(t *testing.T) {
	c := newClock()
	var down atomic.Bool
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if down.Load() {
			http.Error(w, "down", 503)
			return
		}
		_, _ = w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"k1","n":"` + b64u(testRSA.N.Bytes()) + `","e":"AQAB"}]}`))
	}))
	defer srv.Close()
	o := &OIDC{Issuer: iss, Audience: aud, Keys: &JWKS{URL: srv.URL, Now: c.now}, Allowed: []string{"*"}, Now: c.now}
	mk := func(kid string, sg sign) string {
		return mint(rsHeader(kid), goodClaims(map[string]any{"iat": c.now().Unix() - 10, "exp": c.now().Unix() + 3600}), sg)
	}
	if _, err := o.Authenticate(context.Background(), mk("k1", signRS256(testRSA))); err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	hits.Store(0)
	for s := 0; s < 600; s++ {
		c.advance(time.Second)
		_, _ = o.Authenticate(context.Background(), mk("k1", signRS256(otherRSA)))
		_, _ = o.Authenticate(context.Background(), mk("nope", signRS256(otherRSA)))
		_, _ = o.Authenticate(context.Background(), mk("nope2", signRS256(otherRSA)))
	}
	// Fetches at about 60 (first unknown kid on a good set), then 5, 10, 20, 40, 60, 60, ... after each failure.
	const bound = 14
	t.Logf("%d fetches in 10 simulated minutes while down", hits.Load())
	if h := hits.Load(); h > bound || h < 5 {
		t.Fatalf("%d fetches in 10 simulated minutes while down, want between 5 and %d", h, bound)
	}
	// A valid token for the key held still works throughout, and a recovery resets the backoff.
	if _, err := o.Authenticate(context.Background(), mk("k1", signRS256(testRSA))); err != nil {
		t.Fatal(err)
	}
	down.Store(false)
	c.advance(61 * time.Second)
	_, _ = o.Authenticate(context.Background(), mk("nope", signRS256(otherRSA)))
	o.Keys.mu.RLock()
	f := o.Keys.failures
	o.Keys.mu.RUnlock()
	if f != 0 {
		t.Fatalf("failures not reset by a success: %d", f)
	}
}

func TestPanickingFetchDoesNotKillOrStickTheKeySet(t *testing.T) {
	var calls atomic.Int64
	c := newClock()
	j := &JWKS{URL: "http://127.0.0.1:1/x", Now: c.now}
	j.fetchFn = func(context.Context) (map[string]crypto.PublicKey, []crypto.PublicKey, error) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		return map[string]crypto.PublicKey{"k1": &testRSA.PublicKey}, nil, nil
	}
	if _, err := j.Key(context.Background(), "k1", true); err == nil {
		t.Fatal("a panicked fetch gave a key")
	}
	waitIdle(t, j)
	j.mu.RLock()
	failed := !j.lastOK
	j.mu.RUnlock()
	if !failed {
		t.Fatal("a panic was not recorded as a failed fetch")
	}
	c.advance(6 * time.Second)
	if _, err := j.Key(context.Background(), "k1", true); err != nil {
		t.Fatalf("the key set stayed stuck after a panic: %v", err)
	}
}

func TestCacheReinsertOfAnExistingKeyEvictsNothing(t *testing.T) {
	now := fixedNow
	inner := &spyAuth{p: &dispatch.Principal{ID: "u"}}
	c := &Cache{Inner: inner, TTL: time.Hour, Max: 4, Now: func() time.Time { return now }}
	var toks []string
	for i := 0; i < 4; i++ {
		toks = append(toks, mint(map[string]any{"alg": "RS256"}, map[string]any{"exp": now.Unix() + 100 + int64(i)}, func(string) []byte { return nil }))
		if _, err := c.Authenticate(context.Background(), toks[i]); err != nil {
			t.Fatal(err)
		}
	}
	// Force the existing entry to be looked up as a miss (expired in the cache) and re-inserted at the cap.
	key := sha256Hex(toks[3])
	c.mu.Lock()
	e := c.m[key]
	e.expires = now.Add(-time.Second)
	c.m[key] = e
	c.mu.Unlock()
	if _, err := c.Authenticate(context.Background(), toks[3]); err != nil {
		t.Fatal(err)
	}
	if len(c.m) != 4 {
		t.Fatalf("%d entries: re-inserting an existing key evicted others", len(c.m))
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

// waitIdle waits (with a deadline, so a regression fails instead of hanging) until no fetch is in flight.
func waitIdle(t *testing.T, j *JWKS) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j.mu.RLock()
		busy := j.inflight != nil
		j.mu.RUnlock()
		if !busy {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("a fetch is still in flight after 5 s")
}

// A 200 answer with no usable keys must not replace a good key set.
func TestEmptyAnswerKeepsTheGoodKeys(t *testing.T) {
	c := newClock()
	srv := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil))
	o := &OIDC{Issuer: iss, Audience: aud, Keys: &JWKS{URL: srv.URL, Now: c.now}, Allowed: []string{"*"}, Now: c.now}
	good := func() string {
		return mint(rsHeader("k1"), goodClaims(map[string]any{"iat": c.now().Unix() - 10, "exp": c.now().Unix() + 3600}), signRS256(testRSA))
	}
	if _, err := o.Authenticate(context.Background(), good()); err != nil {
		t.Fatal(err)
	}
	for _, empty := range [][]map[string]any{{}, {{"kty": "oct", "kid": "x", "k": "AAAA"}}} {
		srv.keys.Store(empty)
		c.advance(11 * time.Minute)
		if _, err := o.Authenticate(context.Background(), good()); err != nil { // notices the age, refetches behind it
			t.Fatal(err)
		}
		waitIdle(t, o.Keys)
		if _, err := o.Authenticate(context.Background(), good()); err != nil {
			t.Fatalf("an empty answer replaced the good key set: %v", err)
		}
	}
	// And the very first fetch returning nothing is a failure, not an empty key set that counts as fetched.
	j := &JWKS{URL: newJWKSServer(t).URL, Now: c.now}
	if err := j.Refresh(context.Background()); err == nil {
		t.Fatal("an empty first answer counted as success")
	}
}

func sha256Hex(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
