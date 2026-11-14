package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	jwksTimeout   = 10 * time.Second
	jwksMaxBytes  = 1 << 20 // a JWKS of a few keys is a few KB; 1 MiB is generous
	jwksMaxRedirs = 3
	minRSABits    = 2048
)

// JWKS is an issuer's signing keys, fetched from its JWKS URL and refreshed when a token names a key not seen yet.
//
//   - An unknown kid refetches at most once per MinRefresh (a minute) after a successful fetch, however many requests ask,
//     and after a FAILED fetch at most once per FailRetry: 5 s, doubling per consecutive failure to a cap of 60 s, reset
//     by a success, so a blip at the issuer does not lock sign-in out for a minute and a long outage is not hammered.
//     The attempt is recorded when the fetch ENDS. An answer with no usable key is a failed fetch: the old keys stay.
//   - The fetch runs detached from the request that triggered it (its own 10 s timeout), so a caller that names an
//     unknown kid and hangs up cannot cancel the refresh for everyone else; callers wait for it only as long as their
//     own context lives, and share the one fetch.
//   - Every MaxAge (10 minutes) the key set is refetched, so a key the issuer withdrew stops being trusted. Only a token
//     whose signature has VERIFIED triggers it (NoteVerified), so an unauthenticated caller cannot drive it; that request
//     still uses the old keys and the refetch runs behind it. Until that
//     refetch, and for as long as the issuer cannot be reached, a withdrawn key stays trusted, as in Python.
type JWKS struct {
	URL          string
	MinRefresh   time.Duration // after a successful fetch, between refetches for an unknown kid; default one minute
	FailRetry    time.Duration // after a failed fetch, doubling per consecutive failure up to MaxFailRetry; default 5 s
	MaxFailRetry time.Duration // default 60 s
	MaxAge       time.Duration // an unconditional refetch this long after the last success; default 10 minutes
	HTTP         *http.Client  // tests; nil builds one (see newJWKSClient)
	Now          func() time.Time

	mu       sync.RWMutex
	keys     map[string]crypto.PublicKey                                                    // by kid
	noKid    []crypto.PublicKey                                                             // keys the issuer published without a kid
	attempts bool                                                                           // a fetch has ended
	lastOK   bool                                                                           // the last fetch to end succeeded
	last     time.Time                                                                      // when it ended
	lastGood time.Time                                                                      // when the last successful fetch ended
	failures int                                                                            // consecutive failed fetches
	fetchFn  func(context.Context) (map[string]crypto.PublicKey, []crypto.PublicKey, error) // tests
	inflight *flight
}

// flight is one fetch in progress, shared by everyone who needs it.
type flight struct {
	done chan struct{}
	err  error
}

// NewJWKS returns a key set for a JWKS URL, or an error when the URL may not be fetched (see checkJWKSURL).
func NewJWKS(rawURL string, allowHTTP bool) (*JWKS, error) {
	if err := checkJWKSURL(rawURL, allowHTTP); err != nil {
		return nil, err
	}
	return &JWKS{URL: rawURL}, nil
}

// checkJWKSURL: https, or http for a loopback host (local runs and tests), or http for any host only when the operator
// opted in (AUTOPILOT_JWKS_ALLOW_HTTP). The Python gateway fetches any http(s) URL; a lab that serves its issuer over
// plain http says so in its configuration, the product does not guess from a host name.
func checkJWKSURL(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return errors.New(raw + " is not a usable JWKS URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowHTTP || LoopbackHost(u.Hostname()) {
			return nil
		}
	}
	return errors.New("JWKS URL " + raw + " must be https (AUTOPILOT_JWKS_ALLOW_HTTP=1 allows plain http for a lab issuer)")
}

// LoopbackHost is localhost or a loopback address.
func LoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (j *JWKS) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (j *JWKS) lookup(kid string, hasKid bool) (crypto.PublicKey, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if hasKid {
		k, ok := j.keys[kid]
		return k, ok
	}
	if len(j.noKid) > 0 {
		return j.noKid[len(j.noKid)-1], true
	}
	return nil, false
}

// start returns the fetch in progress, or begins one when the last attempt is old enough for interval (after a
// failure: for FailRetry), or returns nil when it is too soon.
func (j *JWKS) start(ctx context.Context, interval time.Duration) *flight {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.inflight != nil {
		return j.inflight
	}
	age := j.now().Sub(j.last)
	if j.attempts && ((j.lastOK && age < interval) || (!j.lastOK && age < j.backoff())) {
		return nil
	}
	f := &flight{done: make(chan struct{})}
	j.inflight = f
	go j.run(context.WithoutCancel(ctx), f)
	return f
}

// backoff is the wait after the last failed fetch: FailRetry, doubled per further consecutive failure, up to the cap.
func (j *JWKS) backoff() time.Duration {
	d, max := orDefault(j.FailRetry, 5*time.Second), orDefault(j.MaxFailRetry, time.Minute)
	for i := 1; i < j.failures && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// Host is the JWKS URL's host, for log lines (never the path or query).
func (j *JWKS) Host() string { return j.host() }

func (j *JWKS) host() string {
	u, _ := url.Parse(j.URL)
	if u == nil {
		return ""
	}
	return u.Host
}

var errFetchPanicked = errors.New("jwks: fetch panicked")

// run is one fetch. Whatever happens in it, including a panic, the in-flight state is cleared and the waiters are
// released, so the key set cannot be left stuck in flight and the process does not die.
func (j *JWKS) run(ctx context.Context, f *flight) {
	var keys map[string]crypto.PublicKey
	var noKid []crypto.PublicKey
	err := errFetchPanicked
	defer func() {
		if r := recover(); r != nil {
			slog.Error("a JWKS fetch panicked; treated as a failed fetch", "host", j.host())
			err = errFetchPanicked
		}
		j.mu.Lock()
		j.attempts, j.last, j.lastOK = true, j.now(), err == nil
		if err == nil {
			j.keys, j.noKid, j.lastGood, j.failures = keys, noKid, j.last, 0
		} else {
			j.failures++
		}
		j.inflight = nil
		j.mu.Unlock()
		f.err = err
		close(f.done)
	}()
	fetch := j.fetch
	if j.fetchFn != nil {
		fetch = j.fetchFn
	}
	keys, noKid, err = fetch(ctx)
	if err == nil && len(keys)+len(noKid) == 0 {
		// Nothing usable (an empty set, or every key skipped): keep the keys held, if any, and retry as after a failure.
		slog.Warn("a JWKS answered with no usable key; the keys held are kept", "host", j.host())
		err = errors.New("jwks: no usable key")
	}
}

// Refresh fetches the key set now unless a fetch is in progress (then it waits for that one), and returns the fetch's
// error. The gateway calls it once per issuer at start, in the background: a failure is not fatal, the first token
// that needs the keys tries again.
func (j *JWKS) Refresh(ctx context.Context) error {
	f := j.start(ctx, 0)
	if f == nil {
		return nil
	}
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NoteVerified is called when a token's signature has verified with this key set. If the set is MaxAge old it starts
// the unconditional refetch, behind that request (which keeps the old keys). Only a verified token may do this: a lookup
// of a known kid is free for anyone to make.
func (j *JWKS) NoteVerified(ctx context.Context) {
	j.mu.RLock()
	old := !j.lastGood.IsZero() && j.now().Sub(j.lastGood) >= orDefault(j.MaxAge, 10*time.Minute)
	j.mu.RUnlock()
	if old {
		j.start(ctx, orDefault(j.MaxAge, 10*time.Minute))
	}
}

// Key returns the key for a token's kid (absent hasKid matches a key published without one, as in Python). An unknown
// kid refetches the set first, within the limits above. It never returns detail.
func (j *JWKS) Key(ctx context.Context, kid string, hasKid bool) (crypto.PublicKey, error) {
	if k, ok := j.lookup(kid, hasKid); ok {
		return k, nil
	}
	if f := j.start(ctx, orDefault(j.MinRefresh, time.Minute)); f != nil {
		select {
		case <-f.done:
		case <-ctx.Done():
			return nil, refuse("token rejected")
		}
		if k, ok := j.lookup(kid, hasKid); ok {
			return k, nil
		}
	}
	return nil, refuse("token rejected")
}

func (j *JWKS) client() *http.Client {
	if j.HTTP != nil {
		return j.HTTP
	}
	return newJWKSClient()
}

// newJWKSClient: a timeout, and redirects only to the same host, same or better scheme, a few hops.
func newJWKSClient() *http.Client {
	return &http.Client{Timeout: jwksTimeout, CheckRedirect: sameHostRedirect}
}

func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > jwksMaxRedirs {
		return errors.New("too many redirects")
	}
	first := via[0].URL
	if req.URL.Host != first.Host || (first.Scheme == "https" && req.URL.Scheme != "https") {
		return errors.New("redirect to another host or scheme refused")
	}
	return nil
}

func (j *JWKS) fetch(ctx context.Context) (map[string]crypto.PublicKey, []crypto.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, jwksTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.URL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := j.client().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, errors.New("jwks: status " + resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jwksMaxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > jwksMaxBytes {
		return nil, nil, errors.New("jwks: response too large")
	}
	return parseJWKS(body)
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// parseJWKS reads the RSA and P-256 keys meant for signing. A key of another kind or size, or malformed, is skipped
// (Python's PyJWK refuses the whole set when one entry is unusable; one odd key must not take the sign-in down, and
// skipping can only remove keys, never add one). Entries with `use` other than "sig" are skipped, as in Python.
func parseJWKS(body []byte) (map[string]crypto.PublicKey, []crypto.PublicKey, error) {
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, nil, err
	}
	keys := map[string]crypto.PublicKey{}
	var noKid []crypto.PublicKey
	for _, raw := range doc.Keys {
		var k jwk
		if json.Unmarshal(raw, &k) != nil {
			continue
		}
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub := k.public()
		if pub == nil {
			continue
		}
		var hasKid struct {
			Kid *string `json:"kid"`
		}
		_ = json.Unmarshal(raw, &hasKid)
		if hasKid.Kid == nil {
			noKid = append(noKid, pub)
		} else {
			keys[k.Kid] = pub
		}
	}
	return keys, noKid, nil
}

func (k jwk) public() crypto.PublicKey {
	switch k.Kty {
	case "RSA":
		n, err1 := b64(k.N)
		e, err2 := b64(k.E)
		if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 || len(e) > 4 {
			return nil
		}
		nn := new(big.Int).SetBytes(n)
		ee := new(big.Int).SetBytes(e)
		if nn.BitLen() < minRSABits || ee.Sign() <= 0 || !ee.IsInt64() || ee.Int64() < 3 || ee.Int64()%2 == 0 {
			return nil
		}
		return &rsa.PublicKey{N: nn, E: int(ee.Int64())}
	case "EC":
		if k.Crv != "P-256" {
			return nil
		}
		x, err1 := b64(k.X)
		y, err2 := b64(k.Y)
		if err1 != nil || err2 != nil || len(x) != 32 || len(y) != 32 {
			return nil
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if _, err := pub.ECDH(); err != nil { // refuses a point that is not on the curve
			return nil
		}
		return pub
	}
	return nil
}
