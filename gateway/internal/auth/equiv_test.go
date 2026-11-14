package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// TestEquivalenceHelper is the Go half of tests/test_gateway_go_auth.py: it reads cases (tokens the Python test minted,
// the JWKS it signed them for) from AUTOPILOT_AUTH_EQUIV_IN, runs each through the Go authenticator, and writes the
// outcome to AUTOPILOT_AUTH_EQUIV_OUT for pytest to compare with the Python authenticator's. It does nothing otherwise.
func TestEquivalenceHelper(t *testing.T) {
	in, out := os.Getenv("AUTOPILOT_AUTH_EQUIV_IN"), os.Getenv("AUTOPILOT_AUTH_EQUIV_OUT")
	if in == "" || out == "" {
		t.Skip("run by tests/test_gateway_go_auth.py")
	}
	raw, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		JWKS  json.RawMessage `json:"jwks"`
		Globs [][2]string     `json:"globs"`
		Cases []struct {
			Kind     string   `json:"kind"` // oidc | foreign | github
			Token    string   `json:"token"`
			Issuer   string   `json:"issuer"`
			Audience string   `json:"audience"`
			Claim    string   `json:"claim"`
			Allowed  []string `json:"allowed"`
			Name     string   `json:"name"`
		} `json:"cases"`
		Subs []string `json:"subs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(doc.JWKS) }))
	defer srv.Close()
	type result struct {
		OK    bool              `json:"ok"`
		ID    string            `json:"id,omitempty"`
		Kind  string            `json:"kind,omitempty"`
		Tier  int               `json:"tier,omitempty"`
		Attrs map[string]string `json:"attrs,omitempty"`
	}
	res := struct {
		Cases []result `json:"cases"`
		Globs []bool   `json:"globs"`
		Subs  []string `json:"subs"`
	}{}
	for _, c := range doc.Cases {
		keys := &JWKS{URL: srv.URL}
		var r result
		switch c.Kind {
		case "oidc":
			p, e := (&OIDC{Issuer: c.Issuer, Audience: c.Audience, Keys: keys, UserClaim: c.Claim, TierCeiling: 1, Allowed: c.Allowed, Leeway: 30 * time.Second}).Authenticate(context.Background(), c.Token)
			if e == nil {
				r = result{true, p.ID, p.Kind, p.TierCeiling, p.Attrs}
			}
		case "foreign":
			p, e := (&Foreign{Name: c.Name, Issuer: c.Issuer, Audience: c.Audience, Keys: keys, Allowed: c.Allowed, TierCeiling: 1}).Authenticate(context.Background(), c.Token)
			if e == nil {
				r = result{true, p.ID, p.Kind, p.TierCeiling, p.Attrs}
			}
		case "github":
			p, e := (&GitHubActions{Audience: c.Audience, Keys: keys, Allowed: c.Allowed, TierCeiling: 1}).Authenticate(context.Background(), c.Token)
			if e == nil {
				r = result{true, p.ID, p.Kind, p.TierCeiling, p.Attrs}
			}
		default:
			t.Fatalf("kind %q", c.Kind)
		}
		res.Cases = append(res.Cases, r)
	}
	for _, g := range doc.Globs {
		res.Globs = append(res.Globs, fnmatchCase(g[0], g[1]))
	}
	for _, s := range doc.Subs {
		res.Subs = append(res.Subs, CanonicalSub(s))
	}
	b, _ := json.Marshal(res)
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
