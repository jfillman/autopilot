package auth

import (
	"context"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// FuzzTokenParsingAndRouting feeds arbitrary bearer tokens through the routing authenticator and every verifier. The
// keys are a JWKS that publishes nothing and fails, so NO token can verify: any acceptance, and any panic, is a bug.
// Every refusal must be the one opaque error.
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
