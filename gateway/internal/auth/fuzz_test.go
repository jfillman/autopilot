package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// FuzzTokenParsingAndRouting feeds arbitrary bearer tokens through the routing authenticator and every verifier. It
// covers the parsing and routing path ONLY: the key set is a JWKS that never answers, so no token can verify and
// signature checking, claim checks and principal construction are never reached. Any acceptance, panic or
// non-opaque error is a bug. FuzzMutatedValidTokens and FuzzCheckClaims cover the rest.
func FuzzTokenParsingAndRouting(f *testing.F) {
	valid := mint(rsHeader("k1"), goodClaims(nil), signRS256(testRSA))
	for _, s := range []string{"", ".", "..", "a.b.c", valid, valid + ".x", valid[:len(valid)/2],
		mint(map[string]any{"alg": "none"}, goodClaims(nil), nil),
		mint(map[string]any{"alg": "HS256", "kid": "k1", "crit": []string{"x"}}, goodClaims(nil), func(string) []byte { return []byte("x") }),
		"eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJodHRwczovL2RleC5leGFtcGxlL2RleCJ9.AAAA",
		"eyJhbGciOiJSUzI1NiIsImtpZCI6bnVsbH0.W10.AA", "e30.e30.e30"} {
		f.Add(s)
	}
	keys := &JWKS{URL: "http://127.0.0.1:1/none", Now: fixedClock}
	people := &OIDC{Issuer: iss, Audience: aud, Keys: keys, Allowed: []string{"*"}, Now: fixedClock}
	tower := &Foreign{Name: "c", Issuer: "https://t/oidc", Audience: aud, Keys: keys, Allowed: []string{"*"}, Now: fixedClock}
	gh := &GitHubActions{Audience: aud, Keys: keys, Allowed: []string{"*"}, Now: fixedClock}
	r := &Router{OIDC: map[string]dispatch.Authenticator{iss: people, "https://t/oidc": tower, GitHubActionsIssuer: gh}}
	f.Fuzz(func(t *testing.T, token string) {
		for _, a := range []dispatch.Authenticator{r, people, tower, gh} {
			p, err := a.Authenticate(context.Background(), token)
			if p != nil || err == nil {
				t.Fatalf("accepted %q as %+v", token, p)
			}
			mustRefuse(t, err)
		}
		_ = CanonicalSub(token)
		_ = fnmatchCase(token, token)
	})
}

// decoded is a token's header and claims as plain JSON values, for comparing two tokens by meaning rather than bytes.
func decoded(token string) (h, c map[string]any, ok bool) {
	p := strings.Split(token, ".")
	if len(p) != 3 {
		return nil, nil, false
	}
	hb, e1 := b64(p[0])
	cb, e2 := b64(p[1])
	if e1 != nil || e2 != nil || json.Unmarshal(hb, &h) != nil || json.Unmarshal(cb, &c) != nil {
		return nil, nil, false
	}
	return h, c, true
}

// FuzzMutatedValidTokens starts from validly signed tokens (RSA and EC keys made at test time, served by an httptest
// JWKS) and lets the fuzzer mutate their bytes. A mutated token may be accepted only if its decoded header and claims
// equal those of one of the originals it was derived from (so the principal is the original's); anything else
// accepted, including a different principal for a changed claim, fails.
func FuzzMutatedValidTokens(f *testing.F) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwkRSA("k1", &testRSA.PublicKey, nil), jwkEC("e1", &testEC.PublicKey)}})
	}))
	f.Cleanup(srv.Close)
	keys := &JWKS{URL: srv.URL, Now: fixedClock}
	if err := keys.Refresh(context.Background()); err != nil {
		f.Fatal(err)
	}
	people := &OIDC{Issuer: iss, Audience: aud, Keys: keys, Allowed: []string{"*@example.com"}, TierCeiling: 1, Now: fixedClock}
	tower := &Foreign{Name: "c", Issuer: "https://t/oidc", Audience: aud, Keys: keys, Allowed: []string{"ns/*"}, TierCeiling: 1, Now: fixedClock}
	gh := &GitHubActions{Audience: aud, Keys: keys, Allowed: []string{"repo:acme/*"}, TierCeiling: 1, Now: fixedClock}
	auths := []dispatch.Authenticator{people, tower, gh}
	rs, es := signRS256(testRSA), signES256(testEC)
	n := fixedNow.Unix()
	seeds := []string{
		mint(rsHeader("k1"), goodClaims(nil), rs),
		mint(map[string]any{"alg": "ES256", "kid": "e1"}, goodClaims(nil), es),
		mint(rsHeader("k1"), goodClaims(map[string]any{"iss": "https://t/oidc", "sub": "system:serviceaccount:ns:sa", "exp": n + 600}), rs),
		mint(rsHeader("k1"), goodClaims(map[string]any{"iss": GitHubActionsIssuer, "sub": "repo:acme/shop:ref:x", "repository": "Acme/Shop", "ref": "x"}), rs),
	}
	type original struct {
		h, c map[string]any
		p    []*dispatch.Principal // per authenticator, nil when refused
	}
	var origs []original
	for _, s := range seeds {
		h, c, _ := decoded(s)
		o := original{h: h, c: c}
		accepted := false
		for _, a := range auths {
			p, err := a.Authenticate(context.Background(), s)
			if err != nil {
				p = nil
			}
			accepted = accepted || p != nil
			o.p = append(o.p, p)
		}
		if !accepted {
			f.Fatalf("a seed no authenticator accepts: %s", s)
		}
		origs = append(origs, o)
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, token string) {
		for i, a := range auths {
			p, err := a.Authenticate(context.Background(), token)
			if err != nil {
				mustRefuse(t, err)
				continue
			}
			h, c, ok := decoded(token)
			if !ok {
				t.Fatalf("authenticator %d accepted a token that does not decode: %q", i, token)
			}
			match := false
			for _, o := range origs {
				if o.p[i] != nil && reflect.DeepEqual(o.h, h) && reflect.DeepEqual(o.c, c) && reflect.DeepEqual(o.p[i], p) {
					match = true
				}
			}
			if !match {
				t.Fatalf("authenticator %d accepted a mutated token as %+v: %q", i, p, token)
			}
		}
	})
}

// FuzzCheckClaims runs checkClaims on arbitrary payloads and checks an acceptance against a plain restatement of the
// rules: required claims present and not null, iss and aud exact, sub a string, exp ahead and iat and nbf not ahead
// of now by more than the leeway.
func FuzzCheckClaims(f *testing.F) {
	n := fixedNow.Unix()
	for _, s := range []string{
		`{"iss":"i","aud":"a","sub":"s","iat":1,"exp":99999999999}`,
		`{"iss":"i","aud":["x","a"],"sub":"s","iat":1.5e9,"exp":1e30,"nbf":-5}`,
		`{"iss":"i","aud":"","sub":"s","iat":1,"exp":99999999999}`,
		`{"iss":"i","aud":"a","sub":"s","iat":"1","exp":99999999999}`,
		`{"iss":"i","aud":"a","sub":"s","iat":1,"exp":99999999999,"nbf":null}`,
		`{"iss":"i","aud":[1,"a"],"sub":"s","iat":1,"exp":99999999999}`,
		`{"iss":"i","aud":"a","sub":"s","iat":1,"exp":` + json.Number(strings.Repeat("9", 400)).String() + `}`,
	} {
		f.Add(s, "i", "a")
	}
	_ = n
	f.Fuzz(func(t *testing.T, payload, issuer, audience string) {
		dec := json.NewDecoder(bytes.NewReader([]byte(payload)))
		dec.UseNumber()
		var c map[string]any
		if dec.Decode(&c) != nil || c == nil {
			return
		}
		err := checkClaims(c, issuer, audience, 30*time.Second, fixedNow)
		if err != nil {
			mustRefuse(t, err)
			return
		}
		num := func(k string) float64 {
			js, ok := c[k].(json.Number)
			if !ok {
				t.Fatalf("accepted with %s not a number: %v", k, c[k])
			}
			v, e := js.Float64()
			if e != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("accepted with %s unreadable: %v", k, c[k])
			}
			return v
		}
		now := float64(fixedNow.Unix())
		// checkClaims truncates a time toward zero like Python's int(); so does the oracle.
		if math.Trunc(num("exp")) <= now-30 || math.Trunc(num("iat")) > now+30 {
			t.Fatalf("accepted outside the time window: %s", payload)
		}
		if v, ok := c["nbf"]; ok && v != nil && math.Trunc(num("nbf")) > now+30 {
			t.Fatalf("accepted before nbf: %s", payload)
		}
		if s, ok := c["iss"].(string); !ok || s != issuer {
			t.Fatalf("accepted with another issuer: %s", payload)
		}
		if _, ok := c["sub"].(string); !ok {
			t.Fatalf("accepted with no string sub: %s", payload)
		}
		if audience == "" {
			t.Fatalf("accepted with an empty configured audience: %s", payload)
		}
		okAud := false
		switch a := c["aud"].(type) {
		case string:
			okAud = a == audience
		case []any:
			for _, e := range a {
				s, isStr := e.(string)
				if !isStr {
					t.Fatalf("accepted an aud list with a non-string: %s", payload)
				}
				okAud = okAud || s == audience
			}
		}
		if !okAud {
			t.Fatalf("accepted without the audience: %s", payload)
		}
	})
}

// FuzzNumericDate: a time claim is read as a finite whole number of seconds or not at all, and never panics.
func FuzzNumericDate(f *testing.F) {
	for _, s := range []string{"0", "-1", "1.9", "1e400", "1e30", "-0", "NaN", "", "9" + strings.Repeat("9", 400)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, ok := numericDate(json.Number(s))
		if ok && (math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v)) {
			t.Fatalf("%q -> %v", s, v)
		}
		_ = audienceMatches(s, s)
		_ = audienceMatches([]any{s, 1}, s)
	})
}
