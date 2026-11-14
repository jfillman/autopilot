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
}

func TestRetiredNamesRefused(t *testing.T) {
	for old, nu := range retired {
		_, err := configFromEnv(envOf(base(map[string]string{old: "x"})))
		if err == nil || !strings.Contains(err.Error(), old+" -> "+nu) {
			t.Fatalf("%s: want a refusal naming %s, got %v", old, nu, err)
		}
	}
	if _, err := configFromEnv(envOf(base(map[string]string{"AUTOPILOT_AUDIENCE": "new"}))); err != nil {
		t.Fatalf("the new names are fine: %v", err)
	}
}

func TestCoreNamesAreNotRetired(t *testing.T) {
	for _, k := range []string{"CLEARANCE_CORE_URL", "CLEARANCE_CORE_TOKEN_FILE", "CLEARANCE_API_URL"} {
		if _, ok := retired[k]; ok {
			t.Fatalf("%s addresses the Clearance product and keeps its name", k)
		}
	}
}

func TestClientNamesKept(t *testing.T) {
	if _, err := configFromEnv(envOf(map[string]string{"GATEWAY_DSN": "x", "AUTOPILOT_CORE_URL": "http://core"})); err == nil || !strings.Contains(err.Error(), "CLEARANCE_CORE_URL is required") {
		t.Fatalf("CLEARANCE_CORE_URL is still the core's address: %v", err)
	}
}

func TestUnportedRefusedUnderBothNames(t *testing.T) {
	for _, k := range []string{"AUTOPILOT_EPHEMERAL", "CLEARANCE_EPHEMERAL"} {
		if _, err := configFromEnv(envOf(base(map[string]string{k: "1"}))); err == nil || !strings.Contains(err.Error(), k) {
			t.Fatalf("%s: %v", k, err)
		}
	}
}
