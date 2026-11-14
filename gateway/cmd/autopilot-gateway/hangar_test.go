package main

import (
	"strings"
	"testing"
)

// The S5b settings are carried now, with Python's names and defaults; a bad value stops the start, and what the Go gateway does
// not carry yet (the pinned definitions and xr.request that GITHUB_PR_ENABLED turns on in Python) must be said to be off.

func TestHangarSettingsDefaultsAndValues(t *testing.T) {
	c, err := configFromEnv(envOf(base(nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := c.hangar
	if h.glidepathURL != "" || h.githubPRs || h.argoSync || h.glidepathOnBehalfOf || h.glidepathTokenFile != "/var/run/secrets/kubernetes.io/serviceaccount/token" ||
		h.agentTokenFile != "/var/run/clearance/agent-installation-token" {
		t.Fatalf("the defaults: everything off, Python's token files: %+v", h)
	}
	c, err = configFromEnv(envOf(base(map[string]string{
		"GLIDEPATH_API_URL": "http://glidepath-api.platform.svc:8080/", "GLIDEPATH_TOKEN_FILE": "/gp", "GLIDEPATH_ON_BEHALF_OF": "1",
		"GITHUB_PR_ENABLED": "yes", "AGENT_XRS_ENABLED": "0", "XR_REQUEST_ENABLED": "0", "AGENT_TOKEN_FILE": "/agent",
		"BROKER_URL": "http://broker.platform.svc:8080", "GITHUB_OWNERS": "acme, other", "ARGO_SYNC_ENABLED": "true"})))
	if err != nil {
		t.Fatal(err)
	}
	h = c.hangar
	if h.glidepathURL != "http://glidepath-api.platform.svc:8080/" || h.glidepathTokenFile != "/gp" || !h.glidepathOnBehalfOf || !h.githubPRs || h.agentTokenFile != "/agent" ||
		h.brokerURL != "http://broker.platform.svc:8080" || len(h.githubOwners) != 2 || h.githubOwners[1] != "other" || !h.argoSync {
		t.Fatalf("%+v", h)
	}
}

func TestBadHangarSettingsStopTheStart(t *testing.T) {
	on := map[string]string{"GITHUB_PR_ENABLED": "1", "AGENT_XRS_ENABLED": "0", "XR_REQUEST_ENABLED": "0", "BROKER_URL": "http://b:8080", "GITHUB_OWNERS": "acme"}
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"a bad glidepath URL", map[string]string{"GLIDEPATH_API_URL": "ftp://x"}, "GLIDEPATH_API_URL"},
		{"credentials in the glidepath URL", map[string]string{"GLIDEPATH_API_URL": "http://u:p@x"}, "GLIDEPATH_API_URL"},
		{"a bad flag", map[string]string{"GITHUB_PR_ENABLED": "maybe"}, "GITHUB_PR_ENABLED must be 0 or 1"},
		{"a bad on-behalf-of flag", map[string]string{"GLIDEPATH_ON_BEHALF_OF": "2"}, "GLIDEPATH_ON_BEHALF_OF must be 0 or 1"},
		{"GitHub with the pinned definitions left on", map[string]string{"GITHUB_PR_ENABLED": "1", "BROKER_URL": "http://b", "GITHUB_OWNERS": "acme"}, "AGENT_XRS_ENABLED=0"},
		{"GitHub with xr.request left on", map[string]string{"GITHUB_PR_ENABLED": "1", "AGENT_XRS_ENABLED": "0", "BROKER_URL": "http://b", "GITHUB_OWNERS": "acme"}, "XR_REQUEST_ENABLED=0"},
		{"the pinned definitions asked for", map[string]string{"AGENT_XRS_ENABLED": "1"}, "not carried"},
		{"xr.request asked for", map[string]string{"XR_REQUEST_ENABLED": "1"}, "not carried"},
		{"GitHub with no broker (there is no lab default)", merge(on, map[string]string{"BROKER_URL": ""}), "BROKER_URL"},
		{"GitHub with a bad broker", merge(on, map[string]string{"BROKER_URL": "gopher://b"}), "BROKER_URL"},
		{"GitHub with no owners", merge(on, map[string]string{"GITHUB_OWNERS": ""}), "GITHUB_OWNERS"},
		{"GitHub with a bad owner", merge(on, map[string]string{"GITHUB_OWNERS": "a/b"}), "GITHUB_OWNERS"},
		{"a sync with Argo CD reads off", map[string]string{"ARGO_SYNC_ENABLED": "1", "ARGO_ENABLED": "0"}, "ARGO_SYNC_ENABLED needs ARGO_ENABLED"},
		{"events still need the trigger bridge", map[string]string{"EVENTS_ENABLED": "1", "GLIDEPATH_API_URL": "http://g"}, "EVENTS_ENABLED"},
	}
	for _, c := range cases {
		_, err := configFromEnv(envOf(base(c.env)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want a refusal naming %q, got %v", c.name, c.want, err)
		}
	}
}

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
