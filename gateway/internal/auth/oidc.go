package auth

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

const defaultLeeway = 30 * time.Second

// verified is the part every OIDC-style verifier shares: parse, pick the key by kid, check the signature for one of
// algs, then the claims. Any failure is the same opaque *core.AuthError.
func verified(ctx context.Context, token string, keys *JWKS, algs []string, issuer, audience string, leeway time.Duration, now time.Time) (map[string]any, error) {
	t, err := parseJWT(token)
	if err != nil {
		return nil, err
	}
	key, err := keys.Key(ctx, t.kid, t.hasKid)
	if err != nil {
		return nil, err
	}
	if err := t.verify(key, algs); err != nil {
		return nil, err
	}
	c, err := t.claims()
	if err != nil {
		return nil, err
	}
	if err := checkClaims(c, issuer, audience, leeway, now); err != nil {
		return nil, err
	}
	return c, nil
}

func nowOf(f func() time.Time) time.Time {
	if f != nil {
		return f()
	}
	return time.Now()
}

func leewayOf(l time.Duration) time.Duration {
	if l == 0 {
		return defaultLeeway
	}
	return l
}

// OIDC is a person's OIDC ID token (Dex in the lab): signed by the issuer (RS256 or ES256 only), for this audience,
// not expired. The principal is "user:" + the configured claim (email by default), kind "user".
//
// Allowed names who may sign in at all (globs on the claim's value): an issuer that federates a public identity
// provider authenticates anyone with an account there, so a valid token alone is not a grant. Empty allows nobody.
type OIDC struct {
	Issuer, Audience string
	Keys             *JWKS
	UserClaim        string // default "email"
	TierCeiling      int
	Leeway           time.Duration // default 30 s
	Allowed          []string
	Now              func() time.Time
}

// Authenticate implements dispatch.Authenticator.
func (o *OIDC) Authenticate(ctx context.Context, token string) (*dispatch.Principal, error) {
	c, err := verified(ctx, token, o.Keys, []string{algRS256, algES256}, o.Issuer, o.Audience, leewayOf(o.Leeway), nowOf(o.Now))
	if err != nil {
		return nil, err
	}
	claim := o.UserClaim
	if claim == "" {
		claim = "email"
	}
	who, ok := c[claim].(string)
	if !ok || who == "" {
		return nil, refuse("token rejected")
	}
	if claim == "email" {
		if v, isBool := c["email_verified"].(bool); isBool && !v {
			return nil, refuse("token rejected")
		}
	}
	if !matchAny(who, o.Allowed) {
		return nil, refuse("token rejected")
	}
	return &dispatch.Principal{ID: "user:" + who, Kind: "user", TierCeiling: o.TierCeiling}, nil
}

// Foreign is a service-account token from ANOTHER cluster, verified against that cluster's public issuer (its JWKS),
// since this cluster's TokenReview cannot. The principal is "<Name>/<namespace>/<serviceaccount>", a workload, and
// only the service accounts Allowed names get in (globs on "<namespace>/<serviceaccount>"): an issuer signs a token
// for every pod of that cluster. Used for delegates such as Tower's backend.
type Foreign struct {
	Name, Issuer, Audience string
	Keys                   *JWKS
	Allowed                []string
	TierCeiling            int
	Leeway                 time.Duration
	Now                    func() time.Time
}

// Authenticate implements dispatch.Authenticator.
func (f *Foreign) Authenticate(ctx context.Context, token string) (*dispatch.Principal, error) {
	c, err := verified(ctx, token, f.Keys, []string{algRS256, algES256}, f.Issuer, f.Audience, leewayOf(f.Leeway), nowOf(f.Now))
	if err != nil {
		return nil, err
	}
	sub, _ := c["sub"].(string)
	if !strings.HasPrefix(sub, saPrefix) || strings.Count(sub, ":") != 3 {
		return nil, refuse("token rejected")
	}
	nsSA := strings.Replace(strings.TrimPrefix(sub, saPrefix), ":", "/", 1)
	if !matchAny(nsSA, f.Allowed) {
		return nil, refuse("token rejected")
	}
	return &dispatch.Principal{ID: f.Name + "/" + nsSA, Kind: "workload", TierCeiling: f.TierCeiling}, nil
}

// GitHubActionsIssuer is the issuer of a GitHub Actions workflow's OIDC token.
const GitHubActionsIssuer = "https://token.actions.githubusercontent.com"

var immutableRepo = regexp.MustCompile(`^repo:([^/@:]+)(?:@[0-9]+)?/([^/@:]+)(?:@[0-9]+)?(:.*)?$`)

// CanonicalSub is a workflow token's `sub` by name: GitHub's immutable-ID form (`repo:owner@<id>/name@<id>:ref:...`,
// the default for newer repositories) without the `@<id>` parts, so allow-lists are written by name
// (`repo:acme/shop:*`). The ids stay on the principal's attributes.
func CanonicalSub(sub string) string {
	if m := immutableRepo.FindStringSubmatch(sub); m != nil {
		return "repo:" + m[1] + "/" + m[2] + m[3]
	}
	return sub
}

// GitHubActions is a workflow's OIDC ID token, signed by GitHub (RS256 only), for this audience, not expired. The
// principal is "github:" + the canonical `sub`, a workload; the verified `repository` claim (lowercased) and the
// token's other facts travel as attributes. Allowed (globs on the canonical sub; empty allows none) narrows which
// workflows get in at all.
//
// The Python gateway no longer wires this verifier (the Clearance core takes GitHub Actions callers since R-3b); it is
// ported because the brief and engine/auth.py carry it, and is exercised by tests, not by `serve`.
type GitHubActions struct {
	Audience    string
	Keys        *JWKS
	Allowed     []string // Python's default is {"repo:*"}; a caller passes it explicitly
	Issuer      string   // default GitHubActionsIssuer
	TierCeiling int
	Leeway      time.Duration
	Now         func() time.Time
}

// Authenticate implements dispatch.Authenticator.
func (g *GitHubActions) Authenticate(ctx context.Context, token string) (*dispatch.Principal, error) {
	iss := g.Issuer
	if iss == "" {
		iss = GitHubActionsIssuer
	}
	c, err := verified(ctx, token, g.Keys, []string{algRS256}, iss, g.Audience, leewayOf(g.Leeway), nowOf(g.Now))
	if err != nil {
		return nil, err
	}
	raw, _ := c["sub"].(string)
	repo, _ := c["repository"].(string)
	if !strings.HasPrefix(raw, "repo:") || repo == "" {
		return nil, refuse("token rejected")
	}
	sub := CanonicalSub(raw)
	if !matchAny(sub, g.Allowed) {
		return nil, refuse("token rejected")
	}
	attrs := map[string]string{"repository": strings.ToLower(repo), "sub": raw}
	for _, k := range []string{"repository_id", "repository_owner_id", "environment", "ref", "sha", "workflow_ref", "run_id", "actor"} {
		if s, ok := c[k].(string); ok {
			attrs[k] = s
		}
	}
	return &dispatch.Principal{ID: "github:" + sub, Kind: "workload", TierCeiling: g.TierCeiling, Attrs: attrs}, nil
}

// Router chooses the verifier by the token's UNVERIFIED issuer: a listed issuer's verifier, else the workload one.
// The choice is not trust: the chosen verifier checks everything, the issuer included. The issuer is compared exactly
// (the configured ones have their trailing slash removed at start, so a token whose iss ends in "/" is not routed to
// them, as in Python). A token that is not a JWT, or that no verifier is configured for, is refused.
type Router struct {
	OIDC     map[string]dispatch.Authenticator
	Workload dispatch.Authenticator // nil: no in-cluster verifier
}

// Authenticate implements dispatch.Authenticator.
func (r *Router) Authenticate(ctx context.Context, token string) (*dispatch.Principal, error) {
	t, err := parseJWT(token)
	if err != nil {
		return nil, err
	}
	c, err := t.claims()
	if err != nil {
		return nil, err
	}
	var v dispatch.Authenticator
	if iss, ok := c["iss"].(string); ok {
		v = r.OIDC[iss]
	}
	if v == nil {
		v = r.Workload
	}
	if v == nil {
		return nil, refuse("token rejected")
	}
	return v.Authenticate(ctx, token)
}
