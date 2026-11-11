package main

import (
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func base(extra map[string]string) map[string]string {
	m := map[string]string{"GATEWAY_DSN": "postgres://x", "CLEARANCE_CORE_URL": "http://core:8080"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestNewNameRead(t *testing.T) {
	c, err := configFromEnv(envOf(base(map[string]string{"AUTOPILOT_AGENTS_DIR": "/a", "AUTOPILOT_AUDIENCE": "aud", "AUTOPILOT_EXPIRE_SECONDS": "15", "AUTOPILOT_RESOURCE_URL": "https://r/mcp"})))
	if err != nil {
		t.Fatal(err)
	}
	if c.agentsDir != "/a" || c.audience != "aud" || c.expireEvery.Seconds() != 15 || c.resourceURL != "https://r/mcp" {
		t.Fatalf("new names not read: %+v", c)
	}
	if w := deprecationWarning(c.deprecated); w != "" {
		t.Fatalf("unexpected warning %q", w)
	}
}

func TestLegacyAliasReadAndWarned(t *testing.T) {
	c, err := configFromEnv(envOf(base(map[string]string{"CLEARANCE_AGENTS_DIR": "/old", "CLEARANCE_EXPIRE_SECONDS": "9"})))
	if err != nil {
		t.Fatal(err)
	}
	if c.agentsDir != "/old" || c.expireEvery.Seconds() != 9 {
		t.Fatalf("legacy not read: %+v", c)
	}
	w := deprecationWarning(c.deprecated)
	if !strings.Contains(w, "CLEARANCE_AGENTS_DIR -> AUTOPILOT_AGENTS_DIR") || !strings.Contains(w, "CLEARANCE_EXPIRE_SECONDS -> AUTOPILOT_EXPIRE_SECONDS") {
		t.Fatalf("warning %q", w)
	}
}

func TestNewNameWinsOverLegacy(t *testing.T) {
	c, err := configFromEnv(envOf(base(map[string]string{"AUTOPILOT_AUDIENCE": "new", "CLEARANCE_AUDIENCE": "old"})))
	if err != nil {
		t.Fatal(err)
	}
	if c.audience != "new" {
		t.Fatalf("audience %q", c.audience)
	}
	if len(c.deprecated) != 0 {
		t.Fatalf("legacy ignored, so not warned: %v", c.deprecated)
	}
}

func TestClientNamesKept(t *testing.T) {
	if _, err := configFromEnv(envOf(map[string]string{"GATEWAY_DSN": "x", "AUTOPILOT_CORE_URL": "http://core"})); err == nil || !strings.Contains(err.Error(), "CLEARANCE_CORE_URL is required") {
		t.Fatalf("CLEARANCE_CORE_URL is still the core's address: %v", err)
	}
}

func TestUnportedRefusedUnderBothNames(t *testing.T) {
	for _, k := range []string{"AUTOPILOT_EPHEMERAL", "CLEARANCE_EPHEMERAL", "AUTOPILOT_OIDC_ISSUER", "CLEARANCE_OIDC_ISSUER"} {
		if _, err := configFromEnv(envOf(base(map[string]string{k: "1"}))); err == nil || !strings.Contains(err.Error(), k) {
			t.Fatalf("%s: %v", k, err)
		}
	}
}
