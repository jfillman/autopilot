package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// ---- helpers: keys are generated here, at test time; nothing is committed ----

var (
	testRSA  = mustRSA()
	otherRSA = mustRSA()
	testEC   = mustEC()
)

func mustRSA() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}

func mustEC() *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return k
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jwkRSA(kid string, k *rsa.PublicKey, extra map[string]any) map[string]any {
	m := map[string]any{"kty": "RSA", "n": b64u(k.N.Bytes()), "e": b64u(big.NewInt(int64(k.E)).Bytes())}
	if kid != "" {
		m["kid"] = kid
	}
	for a, b := range extra {
		m[a] = b
	}
	return m
}

func jwkEC(kid string, k *ecdsa.PublicKey) map[string]any {
	pad := func(b *big.Int) []byte { return b.FillBytes(make([]byte, 32)) }
	return map[string]any{"kty": "EC", "crv": "P-256", "kid": kid, "x": b64u(pad(k.X)), "y": b64u(pad(k.Y))}
}

// jwksServer serves a JWKS document and counts the fetches.
type jwksServer struct {
	*httptest.Server
	hits atomic.Int64
	keys atomic.Value // []map[string]any
}

func newJWKSServer(t *testing.T, keys ...map[string]any) *jwksServer {
	s := &jwksServer{}
	s.keys.Store(keys)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": s.keys.Load()})
	}))
	t.Cleanup(s.Close)
	return s
}

type sign func(signing string) []byte

func signRS256(k *rsa.PrivateKey) sign {
	return func(s string) []byte {
		h := sha256.Sum256([]byte(s))
		sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, h[:])
		if err != nil {
			panic(err)
		}
		return sig
	}
}

func signES256(k *ecdsa.PrivateKey) sign {
	return func(s string) []byte {
		h := sha256.Sum256([]byte(s))
		r, ss, err := ecdsa.Sign(rand.Reader, k, h[:])
		if err != nil {
			panic(err)
		}
		return append(r.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
	}
}

func mint(header, claims map[string]any, sg sign) string {
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	in := b64u(hb) + "." + b64u(cb)
	var sig []byte
	if sg != nil {
		sig = sg(in)
	}
	return in + "." + b64u(sig)
}

var fixedNow = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func fixedClock() time.Time { return fixedNow }

const (
	iss = "https://dex.example/dex"
	aud = "tower"
)

func goodClaims(extra map[string]any) map[string]any {
	n := fixedNow.Unix()
	c := map[string]any{"iss": iss, "aud": aud, "sub": "abc", "iat": n - 10, "exp": n + 3600, "email": "ada@example.com", "email_verified": true}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func rsHeader(kid string) map[string]any {
	return map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
}

func newOIDC(t *testing.T, srv *jwksServer) *OIDC {
	return &OIDC{Issuer: iss, Audience: aud, Keys: &JWKS{URL: srv.URL, Now: fixedClock}, TierCeiling: 1, Allowed: []string{"*@example.com"}, Now: fixedClock}
}

func mustRefuse(t *testing.T, err error) {
	t.Helper()
	var ae *core.AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("want *core.AuthError, got %T %v", err, err)
	}
	if ae.Msg != "token rejected" {
		t.Fatalf("the refusal carries detail: %q", ae.Msg)
	}
}

// ---- the people's OIDC verifier ----

func TestOIDC(t *testing.T) {
	srv := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil), jwkEC("e1", &testEC.PublicKey))
	n := fixedNow.Unix()
	rs := signRS256(testRSA)
	cases := []struct {
		name  string
		token string
		want  string // principal id; "" = refused
	}{
		{"valid RS256", mint(rsHeader("k1"), goodClaims(nil), rs), "user:ada@example.com"},
		{"valid ES256", mint(map[string]any{"alg": "ES256", "kid": "e1"}, goodClaims(nil), signES256(testEC)), "user:ada@example.com"},
		{"aud as a list holding it", mint(rsHeader("k1"), goodClaims(map[string]any{"aud": []string{"x", aud}}), rs), "user:ada@example.com"},
		{"email_verified absent is fine", mint(rsHeader("k1"), goodClaims(map[string]any{"email_verified": nil}), rs), "user:ada@example.com"},
		{"just inside leeway after exp", mint(rsHeader("k1"), goodClaims(map[string]any{"exp": n - 29}), rs), "user:ada@example.com"},
		{"iat inside leeway", mint(rsHeader("k1"), goodClaims(map[string]any{"iat": n + 29}), rs), "user:ada@example.com"},
		{"wrong issuer", mint(rsHeader("k1"), goodClaims(map[string]any{"iss": iss + "x"}), rs), ""},
		{"issuer with trailing slash", mint(rsHeader("k1"), goodClaims(map[string]any{"iss": iss + "/"}), rs), ""},
		{"wrong audience", mint(rsHeader("k1"), goodClaims(map[string]any{"aud": "other"}), rs), ""},
		{"aud list without it", mint(rsHeader("k1"), goodClaims(map[string]any{"aud": []string{"a", "b"}}), rs), ""},
		{"aud list with a non-string", mint(rsHeader("k1"), goodClaims(map[string]any{"aud": []any{aud, 1}}), rs), ""},
		{"expired", mint(rsHeader("k1"), goodClaims(map[string]any{"exp": n - 31}), rs), ""},
		{"exp at the leeway edge", mint(rsHeader("k1"), goodClaims(map[string]any{"exp": n - 30}), rs), ""},
		{"not yet valid (nbf)", mint(rsHeader("k1"), goodClaims(map[string]any{"nbf": n + 31}), rs), ""},
		{"not yet valid (iat)", mint(rsHeader("k1"), goodClaims(map[string]any{"iat": n + 31}), rs), ""},
		{"exp is a string", mint(rsHeader("k1"), goodClaims(map[string]any{"exp": "9999999999"}), rs), ""},
		{"no exp", mint(rsHeader("k1"), goodClaims(map[string]any{"exp": nil}), rs), ""},
		{"no iat", mint(rsHeader("k1"), goodClaims(map[string]any{"iat": nil}), rs), ""},
		{"no sub", mint(rsHeader("k1"), goodClaims(map[string]any{"sub": nil}), rs), ""},
		{"no aud", mint(rsHeader("k1"), goodClaims(map[string]any{"aud": nil}), rs), ""},
		{"no iss", mint(rsHeader("k1"), goodClaims(map[string]any{"iss": nil}), rs), ""},
		{"bad signature (another key)", mint(rsHeader("k1"), goodClaims(nil), signRS256(otherRSA)), ""},
		{"tampered payload", func() string {
			p := strings.Split(mint(rsHeader("k1"), goodClaims(nil), rs), ".")
			p[1] = b64u([]byte(`{"iss":"` + iss + `","aud":"tower","sub":"abc","iat":1,"exp":9999999999,"email":"root@example.com"}`))
			return strings.Join(p, ".")
		}(), ""},
		{"alg none", mint(map[string]any{"alg": "none", "kid": "k1"}, goodClaims(nil), nil), ""},
		{"alg None", mint(map[string]any{"alg": "None", "kid": "k1"}, goodClaims(nil), nil), ""},
		{"alg missing", mint(map[string]any{"kid": "k1"}, goodClaims(nil), rs), ""},
		{"HS256 keyed with the public key's DER", func() string {
			der, _ := x509.MarshalPKIXPublicKey(&testRSA.PublicKey)
			return mint(map[string]any{"alg": "HS256", "kid": "k1"}, goodClaims(nil), func(s string) []byte {
				m := hmac.New(sha256.New, der)
				m.Write([]byte(s))
				return m.Sum(nil)
			})
		}(), ""},
		{"HS256 keyed with the modulus", mint(map[string]any{"alg": "HS256", "kid": "k1"}, goodClaims(nil), func(s string) []byte {
			m := hmac.New(sha256.New, testRSA.N.Bytes())
			m.Write([]byte(s))
			return m.Sum(nil)
		}), ""},
		{"ES256 header on an RSA key", mint(map[string]any{"alg": "ES256", "kid": "k1"}, goodClaims(nil), signES256(testEC)), ""},
		{"RS256 header on an EC key", mint(map[string]any{"alg": "RS256", "kid": "e1"}, goodClaims(nil), rs), ""},
		{"RS384 is not accepted", mint(map[string]any{"alg": "RS384", "kid": "k1"}, goodClaims(nil), rs), ""},
		{"PS256 is not accepted", mint(map[string]any{"alg": "PS256", "kid": "k1"}, goodClaims(nil), rs), ""},
		{"crit header", mint(map[string]any{"alg": "RS256", "kid": "k1", "crit": []string{"exp"}}, goodClaims(nil), rs), ""},
		{"unknown kid", mint(rsHeader("nope"), goodClaims(nil), rs), ""},
		{"no kid when the key has one", mint(map[string]any{"alg": "RS256"}, goodClaims(nil), rs), ""},
		{"oversized", mint(rsHeader("k1"), goodClaims(map[string]any{"pad": strings.Repeat("a", MaxTokenBytes)}), rs), ""},
		{"not a JWT", "abc", ""},
		{"two parts", "a.b", ""},
		{"four parts", "a.b.c.d", ""},
		{"empty", "", ""},
		{"padded base64", strings.Replace(mint(rsHeader("k1"), goodClaims(nil), rs), ".", "=.", 1), ""},
		{"payload is not an object", func() string {
			h := b64u([]byte(`{"alg":"RS256","kid":"k1"}`))
			in := h + "." + b64u([]byte(`[1]`))
			return in + "." + b64u(rs(in))
		}(), ""},
		{"email not verified", mint(rsHeader("k1"), goodClaims(map[string]any{"email_verified": false}), rs), ""},
		{"email not allowed", mint(rsHeader("k1"), goodClaims(map[string]any{"email": "eve@evil.test"}), rs), ""},
		{"no email", mint(rsHeader("k1"), goodClaims(map[string]any{"email": nil}), rs), ""},
		{"email is not a string", mint(rsHeader("k1"), goodClaims(map[string]any{"email": 5}), rs), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := newOIDC(t, srv)
			p, err := o.Authenticate(context.Background(), c.token)
			if c.want == "" {
				if p != nil {
					t.Fatalf("accepted as %+v", p)
				}
				mustRefuse(t, err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.ID != c.want || p.Kind != "user" || p.TierCeiling != 1 || len(p.Attrs) != 0 {
				t.Fatalf("principal %+v", p)
			}
		})
	}
}

func TestOIDCUserClaimAndAllowList(t *testing.T) {
	srv := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil))
	rs := signRS256(testRSA)
	tok := mint(rsHeader("k1"), goodClaims(map[string]any{"preferred_username": "ada", "email_verified": false}), rs)
	// A non-email claim is not subject to email_verified.
	o := newOIDC(t, srv)
	o.UserClaim, o.Allowed = "preferred_username", []string{"a?a"}
	if p, err := o.Authenticate(context.Background(), tok); err != nil || p.ID != "user:ada" {
		t.Fatalf("%v %+v", p, err)
	}
	o.Allowed = nil // empty allows nobody
	_, err := o.Authenticate(context.Background(), tok)
	mustRefuse(t, err)
	o.Allowed = []string{"b*"}
	_, err = o.Authenticate(context.Background(), tok)
	mustRefuse(t, err)
}

// ---- JWKS: refresh rate limit, fetch hardening ----

func TestJWKSUnknownKidRefreshIsRateLimited(t *testing.T) {
	srv := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil))
	now := fixedNow
	j := &JWKS{URL: srv.URL, Now: func() time.Time { return now }}
	o := &OIDC{Issuer: iss, Audience: aud, Keys: j, Allowed: []string{"*"}, Now: fixedClock}
	bad := mint(rsHeader("rotated"), goodClaims(nil), signRS256(testRSA))
	for i := 0; i < 50; i++ {
		_, err := o.Authenticate(context.Background(), bad)
		mustRefuse(t, err)
	}
	if h := srv.hits.Load(); h != 1 {
		t.Fatalf("50 requests with an unknown kid fetched the JWKS %d times, want 1", h)
	}
	// The issuer rotates its key; inside the minute the new kid is still refused, after it the set is refetched.
	srv.keys.Store([]map[string]any{jwkRSA("rotated", &testRSA.PublicKey, nil)})
	now = now.Add(59 * time.Second)
	if _, err := o.Authenticate(context.Background(), bad); err == nil {
		t.Fatal("refreshed inside the minimum interval")
	}
	now = now.Add(2 * time.Second)
	if _, err := o.Authenticate(context.Background(), bad); err != nil {
		t.Fatalf("not refreshed after the interval: %v", err)
	}
	if h := srv.hits.Load(); h != 2 {
		t.Fatalf("fetches %d, want 2", h)
	}
	// A known kid never fetches.
	if _, err := o.Authenticate(context.Background(), bad); err != nil || srv.hits.Load() != 2 {
		t.Fatalf("a known kid fetched: %v %d", err, srv.hits.Load())
	}
}

func TestJWKSConcurrentUnknownKidsShareOneFetch(t *testing.T) {
	srv := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil))
	j := &JWKS{URL: srv.URL, Now: fixedClock}
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			_, _ = j.Key(context.Background(), "nope", true)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	if h := srv.hits.Load(); h != 1 {
		t.Fatalf("%d fetches", h)
	}
}

func TestJWKSDown(t *testing.T) {
	srv := newJWKSServer(t)
	srv.Close() // connection refused
	o := &OIDC{Issuer: iss, Audience: aud, Keys: &JWKS{URL: srv.URL, Now: fixedClock}, Allowed: []string{"*"}, Now: fixedClock}
	_, err := o.Authenticate(context.Background(), mint(rsHeader("k1"), goodClaims(nil), signRS256(testRSA)))
	mustRefuse(t, err) // an AuthError, never a different error or a pass
}

func TestJWKSBadResponses(t *testing.T) {
	big := strings.Repeat("a", jwksMaxBytes+10)
	for name, h := range map[string]http.HandlerFunc{
		"500":      func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 500) },
		"not json": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>")) },
		"too large": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"keys":[],"pad":"` + big + `"}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(h)
			defer s.Close()
			j := &JWKS{URL: s.URL, Now: fixedClock}
			if _, err := j.Key(context.Background(), "k1", true); err == nil {
				t.Fatal("a key from a bad response")
			}
		})
	}
}

func TestJWKSRedirectToAnotherHostIsRefused(t *testing.T) {
	other := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil)) // a different host:port
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer redir.Close()
	j := &JWKS{URL: redir.URL, Now: fixedClock}
	if _, err := j.Key(context.Background(), "k1", true); err == nil {
		t.Fatal("followed a redirect to another host")
	}
	if other.hits.Load() != 0 {
		t.Fatal("the other host was contacted")
	}
}

func TestJWKSRedirectOnTheSameHostIsFollowed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/keys", http.StatusFound) })
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwkRSA("k1", &testRSA.PublicKey, nil)}})
	})
	s := httptest.NewServer(mux)
	defer s.Close()
	j := &JWKS{URL: s.URL + "/old", Now: fixedClock}
	if _, err := j.Key(context.Background(), "k1", true); err != nil {
		t.Fatal(err)
	}
}

func TestJWKSURLRules(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://dex.example/keys":             true,
		"http://dex.hangar-dev.local/dex/keys": true,
		"http://localhost:5556/keys":           true,
		"http://127.0.0.1:5556/keys":           true,
		"http://[::1]:5556/keys":               true,
		"http://dex.example/keys":              false,
		"http://10.0.0.5/keys":                 false,
		"http://localhost.evil.test/keys":      false,
		"ftp://dex.example/keys":               false,
		"file:///etc/passwd":                   false,
		"https://user:pw@dex.example/keys":     false,
		"":                                     false,
	} {
		_, err := NewJWKS(u)
		if (err == nil) != ok {
			t.Errorf("%q: ok=%v err=%v", u, ok, err)
		}
	}
}

func TestJWKSSkipsKeysItCannotUse(t *testing.T) {
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	srv := newJWKSServer(t,
		map[string]any{"kty": "oct", "kid": "h", "k": "AAAA"},
		map[string]any{"kty": "OKP", "kid": "o", "crv": "Ed25519", "x": "AAAA"},
		jwkRSA("small", &small.PublicKey, nil),
		jwkRSA("enc", &testRSA.PublicKey, map[string]any{"use": "enc"}),
		jwkRSA("k1", &testRSA.PublicKey, nil))
	j := &JWKS{URL: srv.URL, Now: fixedClock}
	for _, kid := range []string{"h", "o", "small", "enc"} {
		if _, err := j.Key(context.Background(), kid, true); err == nil {
			t.Errorf("kid %s was usable", kid)
		}
	}
	j.Now = func() time.Time { return fixedNow.Add(time.Hour) }
	if _, err := j.Key(context.Background(), "k1", true); err != nil {
		t.Fatalf("a good key beside bad ones: %v", err)
	}
}

func TestJWKSKeyWithoutKid(t *testing.T) {
	srv := newJWKSServer(t, jwkRSA("", &testRSA.PublicKey, nil))
	o := newOIDC(t, srv)
	// A token with no kid matches a key published without one (as in Python); a token naming a kid does not.
	if _, err := o.Authenticate(context.Background(), mint(map[string]any{"alg": "RS256"}, goodClaims(nil), signRS256(testRSA))); err != nil {
		t.Fatal(err)
	}
	_, err := o.Authenticate(context.Background(), mint(rsHeader("k1"), goodClaims(nil), signRS256(testRSA)))
	mustRefuse(t, err)
}

// ---- Tower / foreign cluster ----

func TestForeign(t *testing.T) {
	srv := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil))
	rs := signRS256(testRSA)
	sa := func(sub string) string { return mint(rsHeader("k1"), goodClaims(map[string]any{"sub": sub}), rs) }
	f := &Foreign{Name: "hangar-prod", Issuer: iss, Audience: aud, Keys: &JWKS{URL: srv.URL, Now: fixedClock},
		Allowed: []string{"app-backstage-prod/backstage", "tower-*/*"}, TierCeiling: 1, Now: fixedClock}
	cases := []struct{ name, token, want string }{
		{"allowed account", sa("system:serviceaccount:app-backstage-prod:backstage"), "hangar-prod/app-backstage-prod/backstage"},
		{"glob account", sa("system:serviceaccount:tower-x:anything"), "hangar-prod/tower-x/anything"},
		{"account not listed", sa("system:serviceaccount:kube-system:coredns"), ""},
		{"not a service account", sa("someone"), ""},
		{"user subject", sa("system:serviceaccount:a"), ""},
		{"too many colons", sa("system:serviceaccount:a:b:c"), ""},
		{"wrong audience", mint(rsHeader("k1"), goodClaims(map[string]any{"sub": "system:serviceaccount:app-backstage-prod:backstage", "aud": "x"}), rs), ""},
		{"wrong issuer", mint(rsHeader("k1"), goodClaims(map[string]any{"sub": "system:serviceaccount:app-backstage-prod:backstage", "iss": "https://evil"}), rs), ""},
		{"expired", mint(rsHeader("k1"), goodClaims(map[string]any{"sub": "system:serviceaccount:app-backstage-prod:backstage", "exp": fixedNow.Unix() - 60}), rs), ""},
		{"bad signature", mint(rsHeader("k1"), goodClaims(map[string]any{"sub": "system:serviceaccount:app-backstage-prod:backstage"}), signRS256(otherRSA)), ""},
		{"alg none", mint(map[string]any{"alg": "none", "kid": "k1"}, goodClaims(map[string]any{"sub": "system:serviceaccount:app-backstage-prod:backstage"}), nil), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := f.Authenticate(context.Background(), c.token)
			if c.want == "" {
				mustRefuse(t, err)
				return
			}
			if err != nil || p.ID != c.want || p.Kind != "workload" || p.TierCeiling != 1 {
				t.Fatalf("%+v %v", p, err)
			}
		})
	}
	// No allow-list admits nobody.
	f.Allowed = nil
	_, err := f.Authenticate(context.Background(), sa("system:serviceaccount:app-backstage-prod:backstage"))
	mustRefuse(t, err)
}

// ---- GitHub Actions ----

func TestGitHubActions(t *testing.T) {
	srv := newJWKSServer(t, jwkRSA("g1", &testRSA.PublicKey, nil), jwkEC("e1", &testEC.PublicKey))
	rs := signRS256(testRSA)
	gh := func(extra map[string]any) string {
		c := goodClaims(map[string]any{"iss": GitHubActionsIssuer, "sub": "repo:acme/shop:environment:prod", "repository": "Acme/Shop"})
		for k, v := range extra {
			if v == nil {
				delete(c, k)
			} else {
				c[k] = v
			}
		}
		return mint(rsHeader("g1"), c, rs)
	}
	g := &GitHubActions{Audience: aud, Keys: &JWKS{URL: srv.URL, Now: fixedClock}, Allowed: []string{"repo:*"}, TierCeiling: 1, Now: fixedClock}
	p, err := g.Authenticate(context.Background(), gh(map[string]any{"repository_id": "12", "repository_owner_id": "34", "ref": "refs/heads/main", "sha": "abc",
		"environment": "prod", "run_id": "9", "actor": "ada", "workflow_ref": "w", "job_workflow_ref": "ignored", "run_attempt": 3}))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"repository": "acme/shop", "sub": "repo:acme/shop:environment:prod", "repository_id": "12", "repository_owner_id": "34",
		"ref": "refs/heads/main", "sha": "abc", "environment": "prod", "run_id": "9", "actor": "ada", "workflow_ref": "w"}
	if p.ID != "github:repo:acme/shop:environment:prod" || p.Kind != "workload" || len(p.Attrs) != len(want) {
		t.Fatalf("%+v", p)
	}
	for k, v := range want {
		if p.Attrs[k] != v {
			t.Errorf("attr %s = %q, want %q", k, p.Attrs[k], v)
		}
	}
	// A non-string attribute is dropped, not stringified.
	p, err = g.Authenticate(context.Background(), gh(map[string]any{"repository_id": 12}))
	if err != nil || p.Attrs["repository_id"] != "" {
		t.Fatalf("%+v %v", p, err)
	}
	// The immutable-id form is canonicalised for the allow-list and the id; the raw sub stays an attribute.
	p, err = g.Authenticate(context.Background(), gh(map[string]any{"sub": "repo:acme@111/shop@222:ref:refs/heads/main"}))
	if err != nil || p.ID != "github:repo:acme/shop:ref:refs/heads/main" || p.Attrs["sub"] != "repo:acme@111/shop@222:ref:refs/heads/main" {
		t.Fatalf("%+v %v", p, err)
	}
	g.Allowed = []string{"repo:acme/shop:*"}
	if _, err = g.Authenticate(context.Background(), gh(map[string]any{"sub": "repo:acme@111/shop@222:ref:refs/heads/main"})); err != nil {
		t.Fatalf("globs are written by name: %v", err)
	}
	for name, token := range map[string]string{
		"another repository":     gh(map[string]any{"sub": "repo:evil/shop:ref:x"}),
		"a lookalike":            gh(map[string]any{"sub": "repo:acme/shop2:ref:x"}),
		"no repository claim":    gh(map[string]any{"repository": nil}),
		"empty repository claim": gh(map[string]any{"repository": ""}),
		"not a repo subject":     gh(map[string]any{"sub": "org:acme"}),
		"wrong issuer":           gh(map[string]any{"iss": "https://dex.example"}),
		"wrong audience":         gh(map[string]any{"aud": "x"}),
		"expired":                gh(map[string]any{"exp": fixedNow.Unix() - 3600}),
		"ES256 is not GitHub's":  mint(map[string]any{"alg": "ES256", "kid": "e1"}, goodClaims(map[string]any{"iss": GitHubActionsIssuer, "sub": "repo:acme/shop:x", "repository": "acme/shop"}), signES256(testEC)),
	} {
		if _, err := g.Authenticate(context.Background(), token); err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			mustRefuse(t, err)
		}
	}
	g.Allowed = nil
	_, err = g.Authenticate(context.Background(), gh(nil))
	mustRefuse(t, err)
}

func TestCanonicalSub(t *testing.T) {
	for in, want := range map[string]string{
		"repo:acme/shop:ref:refs/heads/main":     "repo:acme/shop:ref:refs/heads/main",
		"repo:acme@1/shop@2:ref:refs/heads/main": "repo:acme/shop:ref:refs/heads/main",
		"repo:acme@1/shop@2":                     "repo:acme/shop",
		"repo:acme/shop@2:environment:prod":      "repo:acme/shop:environment:prod",
		"repo:acme@x/shop":                       "repo:acme@x/shop", // not an id: left alone
		"org:acme":                               "org:acme",
		"repo:a/b/c:ref":                         "repo:a/b/c:ref",
	} {
		if got := CanonicalSub(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

// ---- routing ----

type spyAuth struct {
	calls atomic.Int64
	p     *dispatch.Principal
	err   error
}

func (s *spyAuth) Authenticate(context.Context, string) (*dispatch.Principal, error) {
	s.calls.Add(1)
	return s.p, s.err
}

func TestRouter(t *testing.T) {
	people, tower, workload := &spyAuth{p: &dispatch.Principal{ID: "people"}}, &spyAuth{p: &dispatch.Principal{ID: "tower"}}, &spyAuth{p: &dispatch.Principal{ID: "workload"}}
	r := &Router{OIDC: map[string]dispatch.Authenticator{"https://dex/dex": people, "https://t/oidc": tower}, Workload: workload}
	tok := func(c map[string]any) string {
		return mint(map[string]any{"alg": "RS256"}, c, func(string) []byte { return []byte("sig") })
	}
	for _, c := range []struct{ name, token, want string }{
		{"people issuer", tok(map[string]any{"iss": "https://dex/dex"}), "people"},
		{"tower issuer", tok(map[string]any{"iss": "https://t/oidc"}), "tower"},
		{"unlisted issuer goes to the workload verifier", tok(map[string]any{"iss": "https://kubernetes.default.svc"}), "workload"},
		{"trailing slash is not the listed issuer", tok(map[string]any{"iss": "https://dex/dex/"}), "workload"},
		{"different case is not the listed issuer", tok(map[string]any{"iss": "https://DEX/dex"}), "workload"},
		{"no iss", tok(map[string]any{"sub": "x"}), "workload"},
		{"iss not a string", tok(map[string]any{"iss": []string{"https://dex/dex"}}), "workload"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := r.Authenticate(context.Background(), c.token)
			if err != nil || p.ID != c.want {
				t.Fatalf("%+v %v", p, err)
			}
		})
	}
	for _, bad := range []string{"", "opaque-token", "a.b.c", "a.b", strings.Repeat("a", MaxTokenBytes+1)} {
		_, err := r.Authenticate(context.Background(), bad)
		mustRefuse(t, err)
	}
	if people.calls.Load() != 1 || tower.calls.Load() != 1 || workload.calls.Load() != 5 {
		t.Fatalf("calls: %d %d %d", people.calls.Load(), tower.calls.Load(), workload.calls.Load())
	}
	// No workload verifier: an unlisted issuer is refused.
	r.Workload = nil
	_, err := r.Authenticate(context.Background(), tok(map[string]any{"iss": "https://elsewhere"}))
	mustRefuse(t, err)
}

// ---- caching ----

func TestCacheNeverOutlivesExpAndNeverCachesFailures(t *testing.T) {
	now := fixedNow
	inner := &spyAuth{p: &dispatch.Principal{ID: "u"}}
	c := &Cache{Inner: inner, TTL: time.Minute, Now: func() time.Time { return now }}
	short := mint(map[string]any{"alg": "RS256"}, map[string]any{"exp": now.Unix() + 10}, func(string) []byte { return nil })
	long := mint(map[string]any{"alg": "RS256"}, map[string]any{"exp": now.Unix() + 3600}, func(string) []byte { return nil })
	call := func(tok string) {
		if _, err := c.Authenticate(context.Background(), tok); err != nil {
			t.Fatal(err)
		}
	}
	call(short)
	call(short)
	if inner.calls.Load() != 1 {
		t.Fatalf("a repeat was not served from the cache (%d)", inner.calls.Load())
	}
	now = now.Add(11 * time.Second) // past exp, inside the 60 s TTL
	call(short)
	if inner.calls.Load() != 2 {
		t.Fatal("a cached success outlived the token's exp")
	}
	call(long)
	now = now.Add(61 * time.Second)
	call(long)
	if inner.calls.Load() != 4 {
		t.Fatal("the TTL was not honoured")
	}
	inner.p, inner.err = nil, refuse("token rejected")
	for i := 0; i < 3; i++ {
		if _, err := c.Authenticate(context.Background(), "x.y.z"); err == nil {
			t.Fatal("a failure passed")
		}
	}
	if inner.calls.Load() != 7 {
		t.Fatalf("a failure was cached (%d)", inner.calls.Load())
	}
}

// ---- the whole chain, as serve builds it ----

func TestChainEndToEnd(t *testing.T) {
	srv := newJWKSServer(t, jwkRSA("k1", &testRSA.PublicKey, nil))
	workload := &spyAuth{p: &dispatch.Principal{ID: "ns/sa", Kind: "workload"}}
	chain := &Cache{TTL: time.Minute, Now: fixedClock, Inner: &Router{
		OIDC:     map[string]dispatch.Authenticator{iss: newOIDC(t, srv)},
		Workload: workload}}
	good := mint(rsHeader("k1"), goodClaims(nil), signRS256(testRSA))
	for i := 0; i < 3; i++ {
		if p, err := chain.Authenticate(context.Background(), good); err != nil || p.ID != "user:ada@example.com" {
			t.Fatalf("%+v %v", p, err)
		}
	}
	if workload.calls.Load() != 0 {
		t.Fatal("a people token reached the workload verifier")
	}
	// A forged token claiming the people's issuer is verified by the people's verifier, never trusted for its iss.
	forged := mint(rsHeader("k1"), goodClaims(nil), signRS256(otherRSA))
	_, err := chain.Authenticate(context.Background(), forged)
	mustRefuse(t, err)
	if workload.calls.Load() != 0 {
		t.Fatal("a refused people token fell through to the workload verifier")
	}
}
