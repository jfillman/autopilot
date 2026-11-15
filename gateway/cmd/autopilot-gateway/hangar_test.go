package main

import (
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// The S5b settings are carried now, with Python's names and defaults; a bad value stops the start. S5c carries xr.request and the
// Airframe tools: XR_REQUEST_ENABLED defaults to GITHUB_PR_ENABLED as in Python, and the S5b rule that it be said to be 0 is gone.

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
		"GLIDEPATH_API_URL": "http://glidepath-api.platform.svc:8080/", "GLIDEPATH_API_ALLOW_HTTP": "1", "BROKER_ALLOW_HTTP": "true",
		"GLIDEPATH_TOKEN_FILE": "/gp", "GLIDEPATH_ON_BEHALF_OF": "1",
		"GITHUB_PR_ENABLED": "yes", "AGENT_XRS_ENABLED": "0", "AGENT_TOKEN_FILE": "/agent",
		"BROKER_URL": "http://broker.platform.svc:8080", "GITHUB_OWNERS": "acme, other", "AIRFRAME_OWNER": "acme", "ARGO_SYNC_ENABLED": "true"})))
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
	on := map[string]string{"GITHUB_PR_ENABLED": "1", "AGENT_XRS_ENABLED": "0", "BROKER_URL": "https://b:8080", "GITHUB_OWNERS": "acme"}
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"plain http for glidepath-api, which carries a bearer token", map[string]string{"GLIDEPATH_API_URL": "http://glidepath-api:8080"}, "GLIDEPATH_API_URL must be an https URL"},
		{"plain http for the broker, which carries a bearer token", merge(on, map[string]string{"BROKER_URL": "http://b:8080"}), "BROKER_URL must be an https URL"},
		{"a bad plain-http flag", map[string]string{"BROKER_ALLOW_HTTP": "x"}, "BROKER_ALLOW_HTTP must be 0 or 1"},
		{"a bad glidepath URL", map[string]string{"GLIDEPATH_API_URL": "ftp://x"}, "GLIDEPATH_API_URL"},
		{"credentials in the glidepath URL", map[string]string{"GLIDEPATH_API_URL": "http://u:p@x"}, "GLIDEPATH_API_URL"},
		{"a bad flag", map[string]string{"GITHUB_PR_ENABLED": "maybe"}, "GITHUB_PR_ENABLED must be 0 or 1"},
		{"a bad on-behalf-of flag", map[string]string{"GLIDEPATH_ON_BEHALF_OF": "2"}, "GLIDEPATH_ON_BEHALF_OF must be 0 or 1"},
		{"GitHub with the pinned definitions left on and no namespaces to read them from", map[string]string{"GITHUB_PR_ENABLED": "1", "BROKER_URL": "https://b", "GITHUB_OWNERS": "acme"}, "AGENT_XRS_NAMESPACES"},
		{"xr.request without GitHub", map[string]string{"XR_REQUEST_ENABLED": "1"}, "XR_REQUEST_ENABLED needs GITHUB_PR_ENABLED=1"},
		{"GitHub with several owners and no Airframe owner", map[string]string{"GITHUB_PR_ENABLED": "1", "AGENT_XRS_ENABLED": "0", "BROKER_URL": "https://b", "GITHUB_OWNERS": "acme,other"}, "AIRFRAME_OWNER"},
		{"an Airframe owner that is not one of the GitHub owners", map[string]string{"GITHUB_PR_ENABLED": "1", "AGENT_XRS_ENABLED": "0", "BROKER_URL": "https://b", "GITHUB_OWNERS": "acme", "AIRFRAME_OWNER": "evil"}, "not one of GITHUB_OWNERS"},
		{"a tenants repository with an owner in it", map[string]string{"GITHUB_PR_ENABLED": "1", "AGENT_XRS_ENABLED": "0", "BROKER_URL": "https://b", "GITHUB_OWNERS": "acme", "AIRFRAME_TENANTS_REPO": "evil/repo"}, "AIRFRAME_TENANTS_REPO"},
		{"a bad cluster name", map[string]string{"GITHUB_PR_ENABLED": "1", "AGENT_XRS_ENABLED": "0", "BROKER_URL": "https://b", "GITHUB_OWNERS": "acme", "AIRFRAME_CLUSTER": "a b"}, "AIRFRAME_CLUSTER"},
		{"Airframe asked for with no owner", map[string]string{"AIRFRAME_ENABLED": "1"}, "AIRFRAME_OWNER"},
		{"the pinned definitions asked for without GitHub", map[string]string{"AGENT_XRS_ENABLED": "1"}, "AGENT_XRS_ENABLED needs GITHUB_PR_ENABLED"},
		{"GitHub with no broker (there is no lab default)", merge(on, map[string]string{"BROKER_URL": ""}), "BROKER_URL"},
		{"GitHub with a bad broker", merge(on, map[string]string{"BROKER_URL": "gopher://b"}), "BROKER_URL"},
		{"GitHub with no owners", merge(on, map[string]string{"GITHUB_OWNERS": ""}), "GITHUB_OWNERS"},
		{"GitHub with a bad owner", merge(on, map[string]string{"GITHUB_OWNERS": "a/b"}), "GITHUB_OWNERS"},
		{"a sync with Argo CD reads off", map[string]string{"ARGO_SYNC_ENABLED": "1", "ARGO_ENABLED": "0"}, "ARGO_SYNC_ENABLED needs ARGO_ENABLED"},
		{"events need glidepath-api's URL", map[string]string{"EVENTS_ENABLED": "1"}, "needs GLIDEPATH_API_URL"},
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

func TestAShortCredentialStopsTheStart(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(f, []byte("short\n"), 0o600)
	_, err := hangarConfig(hangarSettings{glidepathURL: "https://g.example", glidepathTokenFile: f}, nil, tools.Default(), nil)
	if err == nil || !strings.Contains(err.Error(), "not a credential") {
		t.Fatalf("%v", err)
	}
	_ = os.WriteFile(f, []byte("a-token-long-enough\n"), 0o600)
	if _, err := hangarConfig(hangarSettings{glidepathURL: "https://g.example", glidepathTokenFile: f}, nil, tools.Default(), nil); err != nil {
		t.Fatal(err)
	}
}

// The new rule: GITHUB_PR_ENABLED turns xr.request on (and the Airframe tools, with an owner) as it does in Python.
func TestTheAirframeSettings(t *testing.T) {
	c, err := configFromEnv(envOf(base(merge(githubOn, map[string]string{"AGENT_XRS_ENABLED": "0", "XR_REQUEST_ENABLED": "", "AIRFRAME_CLUSTER": "dev-1", "AIRFRAME_UPPER_CLUSTERS": "up-1, up-2"}))))
	if err != nil {
		t.Fatal(err)
	}
	h := c.hangar
	if !h.xrRequest || !h.airframeEnabled || h.airframeOwner != "acme" || h.airframeCluster != "dev-1" || len(h.airframeUpper) != 2 || h.airframeTenantsRepo != "gitops-hangar-dev-tenants" {
		t.Fatalf("%+v", h)
	}
	// said to be off, they are off
	c, err = configFromEnv(envOf(base(merge(githubOn, map[string]string{"AGENT_XRS_ENABLED": "0", "XR_REQUEST_ENABLED": "0", "AIRFRAME_ENABLED": "0"}))))
	if err != nil || c.hangar.xrRequest || c.hangar.airframeEnabled {
		t.Fatalf("%v %+v", err, c.hangar)
	}
	// with no owner to name and the default left alone the tools are off, with the reason
	c, err = configFromEnv(envOf(base(nil)))
	if err != nil || c.hangar.airframeEnabled || c.hangar.airframeNote == "" {
		t.Fatalf("%v %+v", err, c.hangar)
	}
}

func TestTheXRBoundsHaveSafeDefaultsAndRefuseAboveTheXRDsMaximum(t *testing.T) {
	c, err := configFromEnv(envOf(base(merge(githubOn, map[string]string{"AGENT_XRS_ENABLED": "0"}))))
	if err != nil {
		t.Fatal(err)
	}
	p := c.hangar.xrPolicy
	if len(p.Regions) != 0 || len(p.VPCCidrs) != 0 || len(p.SlackChannels) != 0 || p.AllowUnprotectedDB || p.AllowEmptyOnDelete || p.MaxStorageGiB != 0 {
		t.Fatalf("the defaults leave everything to the platform's own: %+v", p)
	}
	c, err = configFromEnv(envOf(base(merge(githubOn, map[string]string{"AGENT_XRS_ENABLED": "0", "XR_REGIONS": "eu-west-1, us-east-1", "XR_VPC_CIDRS": "10.43.0.0/16",
		"XR_SLACK_CHANNELS": "#deploys", "XR_DB_SIZES": "small", "XR_MAX_DB_STORAGE_GIB": "128", "XR_MAX_CONSUMERS": "5", "XR_ALLOW_EMPTY_ON_DELETE": "1"}))))
	if err != nil {
		t.Fatal(err)
	}
	p = c.hangar.xrPolicy
	if len(p.Regions) != 2 || p.VPCCidrs[0] != "10.43.0.0/16" || p.MaxStorageGiB != 128 || p.MaxConsumers != 5 || !p.AllowEmptyOnDelete || p.AllowUnprotectedDB {
		t.Fatalf("%+v", p)
	}
	for k, v := range map[string]string{"XR_MAX_DB_STORAGE_GIB": "512", "XR_MAX_CONSUMERS": "11", "XR_DB_SIZES": "huge", "XR_REGIONS": "nowhere", "XR_VPC_CIDRS": "0.0.0.0/0",
		"XR_SLACK_CHANNELS": "a b", "XR_ALLOW_UNPROTECTED_DB": "maybe"} {
		if _, err := configFromEnv(envOf(base(merge(githubOn, map[string]string{"AGENT_XRS_ENABLED": "0", k: v})))); err == nil {
			t.Errorf("%s=%s was accepted", k, v)
		}
	}
}

// Only the onboarding agents have airframe.apply (which writes a new app's tenants directory); the agent that holds xr.request does not,
// so it cannot onboard an app: xr.request needs tenants/<app>/ to exist.
func TestOnlyTheOnboardingAgentsMayApplyAnAppSpec(t *testing.T) {
	reg := tools.Default()
	defs, err := definitions.LoadDir(filepath.Join("..", "..", "..", "agents"), reg)
	if err != nil {
		t.Fatal(err)
	}
	var applying []string
	for _, d := range defs {
		has := func(tool string) bool {
			for _, x := range d.Limits.Tools {
				if x == tool {
					return true
				}
			}
			return false
		}
		if has("airframe.apply") {
			applying = append(applying, d.Name)
		}
		if has("xr.request") && has("airframe.apply") {
			t.Errorf("%s holds both xr.request and airframe.apply", d.Name)
		}
	}
	if len(applying) == 0 || len(applying) > 2 {
		t.Fatalf("the agents that apply an AppSpec: %v (parachute and app-provisioner)", applying)
	}
}
