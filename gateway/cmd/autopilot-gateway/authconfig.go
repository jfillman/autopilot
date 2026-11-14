package main

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/auth"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// authConfig is what the Python gateway reads to build its authenticator (serve.py config_from_env and build_auth):
// the people's OIDC issuer (Dex) and Tower's foreign-cluster issuer. Both are off unless their issuer is set.
type authConfig struct {
	oidcIssuer, oidcAudience, oidcJWKS, oidcUserClaim string
	oidcAllowed                                       []string
	towerCluster, towerIssuer, towerJWKS              string
	towerAccounts                                     []string
	allowHTTP                                         bool // AUTOPILOT_JWKS_ALLOW_HTTP: a JWKS URL may be plain http (a lab issuer)
}

func csv(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// authConfigFromEnv applies the Python gateway's rules and defaults, with its messages.
func authConfigFromEnv(env func(string) string) (authConfig, error) {
	var a authConfig
	a.allowHTTP = boolEnv(env, "AUTOPILOT_JWKS_ALLOW_HTTP", false)
	issuer, audience := env("AUTOPILOT_OIDC_ISSUER"), env("AUTOPILOT_OIDC_AUDIENCE")
	if (issuer != "") != (audience != "") {
		return a, configError("AUTOPILOT_OIDC_ISSUER and AUTOPILOT_OIDC_AUDIENCE are set together or not at all")
	}
	if issuer != "" {
		issuer = strings.TrimRight(issuer, "/")
	}
	a.oidcAllowed = csv(env("AUTOPILOT_OIDC_ALLOWED_USERS"))
	if issuer != "" && len(a.oidcAllowed) == 0 {
		return a, configError("AUTOPILOT_OIDC_ISSUER needs AUTOPILOT_OIDC_ALLOWED_USERS: an issuer that federates GitHub signs in anyone with a GitHub account")
	}
	a.oidcIssuer, a.oidcAudience = issuer, audience
	if issuer != "" {
		a.oidcJWKS = getenv(env, "AUTOPILOT_OIDC_JWKS_URL", issuer+"/keys")
	}
	a.oidcUserClaim = getenv(env, "AUTOPILOT_OIDC_USER_CLAIM", "email")
	tower := strings.TrimRight(env("TOWER_ISSUER"), "/")
	cluster, sas := env("TOWER_CLUSTER"), csv(env("TOWER_SERVICE_ACCOUNTS"))
	if tower != "" && (cluster == "" || len(sas) == 0) {
		return a, configError("TOWER_ISSUER needs TOWER_CLUSTER and TOWER_SERVICE_ACCOUNTS (namespace/name globs): the issuer signs a token for every pod of that cluster")
	}
	if tower != "" {
		a.towerCluster, a.towerIssuer, a.towerAccounts = cluster, tower, sas
		a.towerJWKS = getenv(env, "TOWER_JWKS_URL", tower+"/jwks.json")
	}
	return a, nil
}

// build returns the authenticator the Python gateway's build_auth builds: a short cache over a router that sends a
// token to the verifier its (unverified) issuer names, and every other token to the in-cluster workload verifier.
// foreignAudience is AUTOPILOT_AUDIENCE, the audience of Tower's tokens. The second result starts, in the background,
// one fetch of each configured key set; a failed fetch is a warning, never fatal (the first token that needs the keys
// tries again).
func (a authConfig) build(foreignAudience string, workload dispatch.Authenticator) (dispatch.Authenticator, func(), error) {
	byIssuer := map[string]dispatch.Authenticator{}
	var sets []*auth.JWKS
	keyset := func(setting, raw string) (*auth.JWKS, error) {
		keys, err := auth.NewJWKS(raw, a.allowHTTP)
		if err != nil {
			return nil, configError(setting + ": " + err.Error())
		}
		if u, _ := url.Parse(raw); u != nil && u.Scheme == "http" && !loopback(u.Hostname()) {
			slog.Warn("a JWKS is fetched over plain http (AUTOPILOT_JWKS_ALLOW_HTTP is on): its keys can be replaced by anyone on the path", "host", u.Host, "setting", setting)
		}
		sets = append(sets, keys)
		return keys, nil
	}
	if a.oidcIssuer != "" {
		keys, err := keyset("AUTOPILOT_OIDC_JWKS_URL", a.oidcJWKS)
		if err != nil {
			return nil, nil, err
		}
		byIssuer[a.oidcIssuer] = &auth.OIDC{Issuer: a.oidcIssuer, Audience: a.oidcAudience, Keys: keys, UserClaim: a.oidcUserClaim,
			TierCeiling: 1, Allowed: a.oidcAllowed}
	}
	if a.towerIssuer != "" {
		keys, err := keyset("TOWER_JWKS_URL", a.towerJWKS)
		if err != nil {
			return nil, nil, err
		}
		byIssuer[a.towerIssuer] = &auth.Foreign{Name: a.towerCluster, Issuer: a.towerIssuer, Audience: foreignAudience, Keys: keys,
			Allowed: a.towerAccounts, TierCeiling: 1}
	}
	prefetch := func() {
		for _, k := range sets {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if err := k.Refresh(ctx); err != nil {
					u, _ := url.Parse(k.URL)
					slog.Warn("the first fetch of a JWKS failed; the first token that needs it tries again", "host", u.Host)
				}
			}()
		}
	}
	return &auth.Cache{Inner: &auth.Router{OIDC: byIssuer, Workload: workload}, TTL: time.Minute}, prefetch, nil
}

func loopback(h string) bool { return auth.LoopbackHost(h) }
