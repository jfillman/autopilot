package main

import (
	"slices"
	"strings"
	"testing"
)

// The S5a settings are carried now (they are no longer refused at start), with Python's names and defaults; a bad value
// stops the start, and what is still not carried is still refused.
func TestReadBackendSettings(t *testing.T) {
	c, err := configFromEnv(envOf(base(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.reads.argoNamespaces, []string{"argocd"}) || c.reads.backstageURL != "" || c.reads.appAPIServices != nil ||
		c.reads.backstageTokenFile != "/var/run/clearance/backstage-token" {
		t.Fatalf("the defaults: argo on in argocd, the rest off: %+v", c.reads)
	}
	c, err = configFromEnv(envOf(base(map[string]string{
		"BACKSTAGE_URL": "http://backstage.backstage.svc:7007", "BACKSTAGE_TOKEN_FILE": "/tok",
		"APP_API_SERVICES": `{"flight-api":"http://flight-api.app.svc:8080"}`, "ARGO_APP_NAMESPACES": "argocd, argocd-apps"})))
	if err != nil {
		t.Fatal(err)
	}
	if c.reads.backstageURL != "http://backstage.backstage.svc:7007" || c.reads.backstageTokenFile != "/tok" ||
		c.reads.appAPIServices["flight-api"] != "http://flight-api.app.svc:8080" || !slices.Equal(c.reads.argoNamespaces, []string{"argocd", "argocd-apps"}) {
		t.Fatalf("%+v", c.reads)
	}
	c, err = configFromEnv(envOf(base(map[string]string{"ARGO_ENABLED": "0", "ARGO_APP_NAMESPACES": ""})))
	if err != nil || len(c.reads.argoNamespaces) != 0 {
		t.Fatalf("ARGO_ENABLED=0: %+v %v", c, err)
	}
}

func TestBadReadBackendSettingsStopTheStart(t *testing.T) {
	for env, want := range map[string]string{
		"BACKSTAGE_URL=ftp://h":                    "BACKSTAGE_URL",
		"BACKSTAGE_URL=http://u:p@h":               "BACKSTAGE_URL",
		"BACKSTAGE_URL=http://h?x=1":               "BACKSTAGE_URL",
		"APP_API_SERVICES=[]":                      "APP_API_SERVICES is a JSON object",
		"APP_API_SERVICES=nonsense":                "APP_API_SERVICES is a JSON object",
		`APP_API_SERVICES={"Bad_Name":"http://h"}`: "APP_API_SERVICES",
		`APP_API_SERVICES={"a":"http://u:p@h"}`:    "APP_API_SERVICES",
		`APP_API_SERVICES={"a":"ftp://h"}`:         "APP_API_SERVICES",
		`APP_API_SERVICES={"a":5}`:                 "APP_API_SERVICES",
		"ARGO_ENABLED=maybe":                       "ARGO_ENABLED must be 0 or 1",
		"ARGO_APP_NAMESPACES=a/b":                  "ARGO_APP_NAMESPACES",
		"ARGO_APP_NAMESPACES=../x":                 "ARGO_APP_NAMESPACES",
		"ARGO_APP_NAMESPACES=,":                    "ARGO_APP_NAMESPACES names no namespace",
	} {
		k, v, _ := strings.Cut(env, "=")
		if _, err := configFromEnv(envOf(base(map[string]string{k: v}))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want a refusal naming %q, got %v", env, want, err)
		}
	}
}

func TestWhatIsStillNotCarriedIsStillRefused(t *testing.T) {
	for _, k := range []string{"GITHUB_PR_ENABLED", "AGENT_XRS_ENABLED", "EVENTS_ENABLED", "GLIDEPATH_API_URL", "ARGO_SYNC_ENABLED", "AUTOPILOT_EPHEMERAL"} {
		if _, err := configFromEnv(envOf(base(map[string]string{k: "1"}))); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s must still stop the start: %v", k, err)
		}
	}
	for _, k := range []string{"BACKSTAGE_URL", "APP_API_SERVICES"} {
		if slices.Contains(unported, k) {
			t.Errorf("%s is carried now", k)
		}
	}
}
