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

// JWKS is an issuer's signing keys, fetched from its JWKS URL and refreshed when a token names a key not seen yet,
// at most once per MinRefresh (a minute, as the Python gateway does), so a stream of bad kids cannot make the gateway
// hammer the issuer: nothing a caller sends triggers a fetch more often than that, and concurrent requests share the
// one refresh. A failed fetch counts as an attempt (the next try is a minute away), as in Python.
type JWKS struct {
	URL        string
	MinRefresh time.Duration // default one minute
	HTTP       *http.Client  // tests; nil builds one (see newJWKSClient)
	Now        func() time.Time

	refresh sync.Mutex // one refresh at a time
	mu      sync.RWMutex
	keys    map[string]crypto.PublicKey // by kid
	noKid   []crypto.PublicKey          // keys the issuer published without a kid
	fetched time.Time
	tried   bool
}

// NewJWKS returns a key set for a JWKS URL, or an error when the URL may not be fetched (see checkJWKSURL).
func NewJWKS(rawURL string) (*JWKS, error) {
	if err := checkJWKSURL(rawURL); err != nil {
		return nil, err
	}
	return &JWKS{URL: rawURL}, nil
}

// checkJWKSURL: https, with one named exception. The Python gateway fetches any http(s) URL; the lab's Dex is served
// over plain http on a `.local` name (tools/hangar_login.py), so http is accepted for loopback and `.local` hosts
// only. Any other http URL is refused at start, with this message.
func checkJWKSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return errors.New(raw + " is not a usable JWKS URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if labHost(u.Hostname()) {
			return nil
		}
	}
	return errors.New("JWKS URL " + raw + " must be https (plain http only for loopback and .local lab hosts)")
}

func labHost(h string) bool {
	h = strings.ToLower(h)
	if h == "localhost" || strings.HasSuffix(h, ".local") {
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

func (j *JWKS) minRefresh() time.Duration {
	if j.MinRefresh > 0 {
		return j.MinRefresh
	}
	return time.Minute
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

// Key returns the key for a token's kid (absent hasKid matches a key published without one, as in Python), refreshing
// the set first when the kid is unknown and the last attempt is old enough. It never returns detail.
func (j *JWKS) Key(ctx context.Context, kid string, hasKid bool) (crypto.PublicKey, error) {
	if k, ok := j.lookup(kid, hasKid); ok {
		return k, nil
	}
	j.refresh.Lock()
	// Another request may have refreshed while this one waited, or the attempt may be too recent.
	if k, ok := j.lookup(kid, hasKid); ok {
		j.refresh.Unlock()
		return k, nil
	}
	j.mu.RLock()
	due := !j.tried || j.now().Sub(j.fetched) >= j.minRefresh()
	j.mu.RUnlock()
	if due {
		j.mu.Lock()
		j.tried, j.fetched = true, j.now()
		j.mu.Unlock()
		if keys, noKid, err := j.fetch(ctx); err == nil {
			j.mu.Lock()
			j.keys, j.noKid = keys, noKid
			j.mu.Unlock()
		}
	}
	j.refresh.Unlock()
	if k, ok := j.lookup(kid, hasKid); ok {
		return k, nil
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
