package main

import (
	"context"
	"strings"
	"testing"
)

func TestAuthConfigIsPythonsConfig(t *testing.T) {
	oidc := map[string]string{"AUTOPILOT_OIDC_ISSUER": "https://dex.example/dex/", "AUTOPILOT_OIDC_AUDIENCE": "tower", "AUTOPILOT_OIDC_ALLOWED_USERS": " a@x , b@y,,"}
	for name, c := range map[string]struct {
		env  map[string]string
		want string // "" = accepted
	}{
		"nothing set":                      {nil, ""},
		"issuer without audience":          {map[string]string{"AUTOPILOT_OIDC_ISSUER": "https://dex"}, "set together"},
		"audience without issuer":          {map[string]string{"AUTOPILOT_OIDC_AUDIENCE": "c"}, "set together"},
		"issuer needs allowed":             {map[string]string{"AUTOPILOT_OIDC_ISSUER": "https://dex", "AUTOPILOT_OIDC_AUDIENCE": "c"}, "needs AUTOPILOT_OIDC_ALLOWED_USERS"},
		"tower needs cluster":              {map[string]string{"TOWER_ISSUER": "https://t", "TOWER_SERVICE_ACCOUNTS": "a/b"}, "needs TOWER_CLUSTER"},
		"tower needs accounts":             {map[string]string{"TOWER_ISSUER": "https://t", "TOWER_CLUSTER": "c"}, "needs TOWER_CLUSTER and TOWER_SERVICE_ACCOUNTS"},
		"tower cluster alone is ignored":   {map[string]string{"TOWER_CLUSTER": "c"}, ""},
		"retired name":                     {map[string]string{"CLEARANCE_OIDC_ISSUER": "https://dex"}, "CLEARANCE_OIDC_ISSUER -> AUTOPILOT_OIDC_ISSUER"},
		"plain http, public host":          {map[string]string{"AUTOPILOT_OIDC_ISSUER": "http://dex.example", "AUTOPILOT_OIDC_AUDIENCE": "c", "AUTOPILOT_OIDC_ALLOWED_USERS": "*"}, "must be https"},
		"plain http, .local host, default": {map[string]string{"AUTOPILOT_OIDC_ISSUER": "http://dex.hangar-dev.local/dex", "AUTOPILOT_OIDC_AUDIENCE": "c", "AUTOPILOT_OIDC_ALLOWED_USERS": "*"}, "AUTOPILOT_JWKS_ALLOW_HTTP"},
		"plain http, opted in":             {map[string]string{"AUTOPILOT_OIDC_ISSUER": "http://dex.hangar-dev.local/dex", "AUTOPILOT_OIDC_AUDIENCE": "c", "AUTOPILOT_OIDC_ALLOWED_USERS": "*", "AUTOPILOT_JWKS_ALLOW_HTTP": "1"}, ""},
		"plain http, loopback":             {map[string]string{"AUTOPILOT_OIDC_ISSUER": "http://127.0.0.1:5556/dex", "AUTOPILOT_OIDC_AUDIENCE": "c", "AUTOPILOT_OIDC_ALLOWED_USERS": "*"}, ""},
		"tower over plain http, default":   {map[string]string{"TOWER_ISSUER": "http://t.example", "TOWER_CLUSTER": "c", "TOWER_SERVICE_ACCOUNTS": "a/b"}, "TOWER_JWKS_URL"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := configFromEnv(envOf(base(c.env)))
			if err == nil && (c.env["AUTOPILOT_OIDC_ISSUER"] != "" || c.env["TOWER_ISSUER"] != "") {
				_, _, err = cfg.auth.build("clearance", nil)
			}
			if c.want == "" && err != nil {
				t.Fatal(err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}

	cfg, err := configFromEnv(envOf(base(oidc)))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.auth
	if a.oidcIssuer != "https://dex.example/dex" || a.oidcJWKS != "https://dex.example/dex/keys" || a.oidcUserClaim != "email" ||
		strings.Join(a.oidcAllowed, "|") != "a@x|b@y" {
		t.Fatalf("%+v", a)
	}
	if cfg.issuerURL != "https://dex.example/dex" {
		t.Fatalf("the metadata names the OIDC issuer as the authorization server, got %q", cfg.issuerURL)
	}
	cfg, _ = configFromEnv(envOf(base(map[string]string{"AUTOPILOT_RESOURCE_URL": "https://r/mcp"})))
	if cfg.issuerURL != "https://r/mcp" {
		t.Fatalf("without an issuer the authorization server is this server, got %q", cfg.issuerURL)
	}

	cfg, err = configFromEnv(envOf(base(map[string]string{"TOWER_ISSUER": "https://hangarplatform.dev/oidc/hangar-prod/", "TOWER_CLUSTER": "hangar-prod",
		"TOWER_SERVICE_ACCOUNTS": "app-backstage-prod/backstage"})))
	if err != nil {
		t.Fatal(err)
	}
	if a = cfg.auth; a.towerIssuer != "https://hangarplatform.dev/oidc/hangar-prod" || a.towerJWKS != "https://hangarplatform.dev/oidc/hangar-prod/jwks.json" {
		t.Fatalf("%+v", a)
	}
}

// With nothing configured the chain still refuses a token it has no verifier for, and never reaches a JWKS.
func TestBuiltChainWithoutOIDCRefusesNonWorkloadTokens(t *testing.T) {
	cfg, _ := configFromEnv(envOf(base(nil)))
	chain, _, err := cfg.auth.build("clearance", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chain.Authenticate(context.Background(), "not-a-jwt"); err == nil {
		t.Fatal("accepted")
	}
}
