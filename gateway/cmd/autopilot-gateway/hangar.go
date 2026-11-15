package main

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/airframe"
	"github.com/jfillman/autopilot/gateway/internal/backends"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// hangarSettings are the settings of the S5b tools (the Glidepath, repository and Argo CD sync tools), as Python's
// config_from_env reads them, and the three Go adds (below).
type hangarSettings struct {
	glidepathURL, glidepathTokenFile    string
	glidepathOnBehalfOf                 bool
	glidepathAllowHTTP, brokerAllowHTTP bool // GLIDEPATH_API_ALLOW_HTTP, BROKER_ALLOW_HTTP (Go only, off): a bearer token over plain http

	githubPRs                 bool
	agentTokenFile, brokerURL string
	githubOwners              []string

	argoSync bool

	// The Airframe tools (A-9 S5c): AIRFRAME_ENABLED, the fleet the planner knows, and xr.request.
	airframeEnabled     bool
	airframeCluster     string
	airframeUpper       []string
	airframeTenantsRepo string
	airframeOwner       string
	xrRequest           bool
	xrPolicy            airframe.XRPolicy
	airframeNote        string // why the Airframe tools are off when the default would have turned them on
}

const (
	// glidepathTokenDefault is the service account token the kubelet projects (Python: SA_DIR / "token").
	glidepathTokenDefault = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	// agentTokenDefault is the projected token the token broker authenticates (Python: AGENT_TOKEN_FILE).
	agentTokenDefault = "/var/run/clearance/agent-installation-token"
)

func flagEnv(env func(string) string, k string, d bool) (bool, error) {
	switch strings.ToLower(env(k)) {
	case "":
		return d, nil
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, configError(k + " must be 0 or 1, not " + strconv.Quote(env(k)))
}

// hangarFromEnv reads GLIDEPATH_API_URL, GLIDEPATH_TOKEN_FILE and GLIDEPATH_ON_BEHALF_OF (the glidepath.* tools),
// GITHUB_PR_ENABLED with AGENT_TOKEN_FILE and BROKER_URL (the repo.* tools and glidepath.cicd.propose) and ARGO_SYNC_ENABLED
// (argo.sync.lower). Each is checked here, so a bad one stops the start rather than a call.
//
// What the Go gateway adds, each stricter than Python and each said in docs/admin/go-gateway.md:
//   - BROKER_URL has no default (Python's names a lab Service) and GITHUB_PR_ENABLED needs it;
//   - GITHUB_OWNERS (Go only) names the GitHub owners the pull requests go to: no argument chooses one;
//   - GITHUB_PR_ENABLED defaults the pinned definitions (AGENT_XRS_ENABLED, carried since S4: definitions.go) and xr.request
//     (XR_REQUEST_ENABLED) on in Python; the Go gateway does not carry xr.request yet, so it needs that said to be 0 rather than
//     start without it silently;
//   - ARGO_SYNC_ENABLED needs ARGO_ENABLED (Python ignores it when Argo CD reads are off).
func hangarFromEnv(env func(string) string, argoEnabled bool) (hangarSettings, error) {
	var h hangarSettings
	var err error
	if h.glidepathAllowHTTP, err = flagEnv(env, "GLIDEPATH_API_ALLOW_HTTP", false); err != nil {
		return h, err
	}
	if h.brokerAllowHTTP, err = flagEnv(env, "BROKER_ALLOW_HTTP", false); err != nil {
		return h, err
	}
	if u := env("GLIDEPATH_API_URL"); u != "" {
		if _, err = backends.NewGlidepath(u, "x", false, 0, h.glidepathAllowHTTP); err != nil {
			return h, configError(err.Error())
		}
		h.glidepathURL = u
	}
	h.glidepathTokenFile = getenv(env, "GLIDEPATH_TOKEN_FILE", glidepathTokenDefault)
	if h.glidepathOnBehalfOf, err = flagEnv(env, "GLIDEPATH_ON_BEHALF_OF", false); err != nil {
		return h, err
	}
	if h.githubPRs, err = flagEnv(env, "GITHUB_PR_ENABLED", false); err != nil {
		return h, err
	}
	h.agentTokenFile = getenv(env, "AGENT_TOKEN_FILE", agentTokenDefault)
	if h.githubPRs {
		h.brokerURL = env("BROKER_URL")
		for _, o := range strings.Split(env("GITHUB_OWNERS"), ",") {
			if o = strings.TrimSpace(o); o != "" {
				h.githubOwners = append(h.githubOwners, o)
			}
		}
	}
	// One build of the GitHub backend checks the broker, the owners and the tenants repository together. The GitHub settings are
	// reported first, then the Airframe ones.
	aerr := airframeFromEnv(env, &h)
	if h.githubPRs {
		if _, err = backends.NewGitHub(h.githubOptions()); err != nil {
			return h, configError("GITHUB_PR_ENABLED: " + err.Error())
		}
	}
	if aerr != nil {
		return h, aerr
	}
	if h.argoSync, err = flagEnv(env, "ARGO_SYNC_ENABLED", false); err != nil {
		return h, err
	}
	if h.argoSync && !argoEnabled {
		return h, configError("ARGO_SYNC_ENABLED needs ARGO_ENABLED (and ARGO_APP_NAMESPACES): argo.sync.lower reads the Application it patches")
	}
	return h, nil
}

// hangarConfig builds the backends the settings switch on.
func hangarConfig(h hangarSettings, argoNamespaces []string, reg *tools.Registry, def backends.DefinitionOf) (backends.HangarConfig, error) {
	c := backends.HangarConfig{Definition: def, Tools: reg}
	var err error
	// A credential too short to be one is refused at the start, not skipped when an answer is cleaned (a file that is not there
	// yet is the call's own error).
	for _, f := range []struct {
		on   bool
		file string
	}{{h.glidepathURL != "", h.glidepathTokenFile}, {h.githubPRs, h.agentTokenFile}} {
		if !f.on {
			continue
		}
		if b, rerr := os.ReadFile(f.file); rerr == nil && len(strings.TrimSpace(string(b))) < backends.MinToken {
			return c, fmt.Errorf("%s holds a token shorter than %d characters: not a credential", f.file, backends.MinToken)
		}
	}
	if h.glidepathAllowHTTP || h.brokerAllowHTTP {
		slog.Warn("a bearer token is sent over plain http (GLIDEPATH_API_ALLOW_HTTP or BROKER_ALLOW_HTTP is on): anyone on the path can read it",
			"glidepath", h.glidepathAllowHTTP, "broker", h.brokerAllowHTTP)
	}
	if h.glidepathURL != "" {
		if c.Glidepath, err = backends.NewGlidepath(h.glidepathURL, h.glidepathTokenFile, h.glidepathOnBehalfOf, 0, h.glidepathAllowHTTP); err != nil {
			return c, err
		}
	}
	if h.githubPRs {
		if c.GitHub, err = backends.NewGitHub(h.githubOptions()); err != nil {
			return c, err
		}
	}
	if h.airframeEnabled || h.xrRequest {
		ac := &backends.AirframeConfig{Enabled: h.airframeEnabled, XRRequests: h.xrRequest,
			Registry: airframe.Registry{DevCluster: h.airframeCluster, UpperClusters: h.airframeUpper, TenantsRepo: h.airframeTenantsRepo, Owner: h.airframeOwner, XR: h.xrPolicy}}
		if h.airframeEnabled {
			kr := backends.NewKubeReader()
			if err := kr.Validate(); err != nil {
				return c, err
			}
			ac.State = &airframe.State{Kube: backends.StateReader{KubeReader: kr}, Cluster: h.airframeCluster, ArgoNamespace: airframe.ArgoNamespace}
		}
		c.Airframe = ac
	}
	if h.argoSync {
		if c.ArgoSync, err = backends.NewArgoSync(nil, argoNamespaces); err != nil {
			return c, err
		}
	}
	return c, nil
}

// githubOptions is the GitHub backend's options: the broker, the owners and (when something writes there) the tenants repository,
// the one repository the broker is asked about with no app.
func (h hangarSettings) githubOptions() backends.GitHubOptions {
	o := backends.GitHubOptions{BrokerURL: h.brokerURL, TokenFile: h.agentTokenFile, Owners: h.githubOwners, AllowHTTP: h.brokerAllowHTTP}
	if (h.airframeEnabled || h.xrRequest) && h.airframeOwner != "" {
		o.TenantsRepo = h.airframeOwner + "/" + h.airframeTenantsRepo
	}
	return o
}

var (
	clusterName = regexp.MustCompile(`\A[A-Za-z0-9][A-Za-z0-9._-]{0,62}\z`)
	repoName    = regexp.MustCompile(`\A[A-Za-z0-9._-]{1,100}\z`)
)

// airframeFromEnv reads the Airframe tools' settings (Python's, with their defaults) and the one the Go gateway adds:
//
//   - AIRFRAME_ENABLED (default on), AIRFRAME_CLUSTER (hangar-dev), AIRFRAME_UPPER_CLUSTERS (hangar-prod) and AIRFRAME_TENANTS_REPO
//     (gitops-hangar-dev-tenants) are Python's. The planner, airframe.status/describe/verify and airframe.apply need them.
//   - XR_REQUEST_ENABLED defaults to GITHUB_PR_ENABLED, as in Python, and needs it: an XR request is a pull request opened as the
//     agents' App. (S5b required it to be said to be 0 while the Go gateway had no xr.request; that rule is gone.)
//   - AIRFRAME_OWNER (Go only) is the GitHub owner of the tenants repository and of the apps' repositories the planner names (Python
//     assumes one lab user). It must be one of GITHUB_OWNERS; with a single owner there it is that one, otherwise it is required
//     when anything Airframe is on.
func airframeFromEnv(env func(string) string, h *hangarSettings) error {
	var err error
	if h.airframeEnabled, err = flagEnv(env, "AIRFRAME_ENABLED", true); err != nil {
		return err
	}
	if h.xrRequest, err = flagEnv(env, "XR_REQUEST_ENABLED", h.githubPRs); err != nil {
		return err
	}
	if h.xrRequest && !h.githubPRs {
		return configError("XR_REQUEST_ENABLED needs GITHUB_PR_ENABLED=1 (an XR request is a pull request opened as the agents' App)")
	}
	h.airframeCluster = getenv(env, "AIRFRAME_CLUSTER", "hangar-dev")
	h.airframeTenantsRepo = getenv(env, "AIRFRAME_TENANTS_REPO", "gitops-hangar-dev-tenants")
	for _, c := range strings.Split(getenv(env, "AIRFRAME_UPPER_CLUSTERS", "hangar-prod"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			h.airframeUpper = append(h.airframeUpper, c)
		}
	}
	if !h.airframeEnabled && !h.xrRequest {
		return nil
	}
	if err = xrPolicyFromEnv(env, &h.xrPolicy); err != nil {
		return err
	}
	for k, v := range map[string]string{"AIRFRAME_CLUSTER": h.airframeCluster} {
		if !clusterName.MatchString(v) {
			return configError(k + " is a cluster name (letters, digits, `.`, `_`, `-`), not " + strconv.Quote(v))
		}
	}
	for _, c := range h.airframeUpper {
		if !clusterName.MatchString(c) {
			return configError("AIRFRAME_UPPER_CLUSTERS names " + strconv.Quote(c) + ", which is not a cluster name")
		}
	}
	if !repoName.MatchString(h.airframeTenantsRepo) || h.airframeTenantsRepo == "." || h.airframeTenantsRepo == ".." {
		return configError("AIRFRAME_TENANTS_REPO is a repository name (no owner), not " + strconv.Quote(h.airframeTenantsRepo))
	}
	h.airframeOwner = env("AIRFRAME_OWNER")
	var owners []string
	for _, o := range strings.Split(env("GITHUB_OWNERS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			owners = append(owners, o)
		}
	}
	if h.airframeOwner == "" && len(owners) == 1 {
		h.airframeOwner = owners[0]
	}
	if h.airframeOwner == "" && !h.xrRequest && env("AIRFRAME_ENABLED") == "" {
		// Python turns the Airframe tools on by default. They need an owner this gateway does not guess: with none named and the
		// setting left at its default they are off, said at start; asked for by name, or with xr.request on, they refuse to start.
		h.airframeEnabled = false
		h.airframeNote = "the airframe.* tools are off: AIRFRAME_OWNER is not set and GITHUB_OWNERS does not name exactly one owner (AIRFRAME_ENABLED=0 silences this)"
		return nil
	}
	if h.airframeOwner == "" {
		return configError("the Airframe tools need AIRFRAME_OWNER, the GitHub owner of the tenants repository and the apps' repositories (or GITHUB_OWNERS with one entry); AIRFRAME_ENABLED=0 and XR_REQUEST_ENABLED=0 turn them off")
	}
	if h.githubPRs {
		found := false
		for _, o := range owners {
			found = found || strings.EqualFold(o, h.airframeOwner)
		}
		if !found {
			return configError("AIRFRAME_OWNER " + strconv.Quote(h.airframeOwner) + " is not one of GITHUB_OWNERS")
		}
	}
	return nil
}

var (
	regionName   = regexp.MustCompile(`\A[a-z]{2}(-[a-z]+)+-[0-9]\z`)
	vpcCIDR      = regexp.MustCompile(`\A10\.([0-9]|[1-9][0-9]|1[0-9][0-9]|2[0-4][0-9]|25[0-5])\.0\.0/16\z`)
	slackChannel = regexp.MustCompile(`\A[#A-Za-z0-9._-]{1,80}\z`)
)

// xrPolicyFromEnv reads the bounds the platform puts on what xr.request may ask for (Go only; Python bounds nothing). Each has a safe
// default, and a value above the XRD's own maximum stops the start:
//
//	XR_REGIONS (us-east-1), XR_VPC_CIDRS (none: the platform's own network), XR_SLACK_CHANNELS (none), XR_DB_SIZES (small,medium),
//	XR_MAX_DB_STORAGE_GIB (64, at most 256), XR_MAX_CONSUMERS (3, at most 10), XR_ALLOW_UNPROTECTED_DB (0: protect: false is refused)
//	XR_ALLOW_EMPTY_ON_DELETE (0: emptyOnDelete: true is refused) and XR_SLO_LABELS (job, service, route, method, code, status_code, le: the only
//	labels an SLO's matchers may select on, never one that names a namespace).
func xrPolicyFromEnv(env func(string) string, p *airframe.XRPolicy) error {
	listOf := func(k string, ok *regexp.Regexp) ([]string, error) {
		var out []string
		for _, v := range strings.Split(env(k), ",") {
			if v = strings.TrimSpace(v); v != "" {
				if ok != nil && !ok.MatchString(v) {
					return nil, configError(k + " names " + strconv.Quote(v) + ", which is not an accepted value")
				}
				out = append(out, v)
			}
		}
		return out, nil
	}
	var err error
	if p.Regions, err = listOf("XR_REGIONS", regionName); err != nil {
		return err
	}
	if p.VPCCidrs, err = listOf("XR_VPC_CIDRS", vpcCIDR); err != nil {
		return err
	}
	if p.SlackChannels, err = listOf("XR_SLACK_CHANNELS", slackChannel); err != nil {
		return err
	}
	if p.SLOLabels, err = listOf("XR_SLO_LABELS", regexp.MustCompile(`\A[a-zA-Z_][a-zA-Z0-9_]{0,63}\z`)); err != nil {
		return err
	}
	for _, l := range p.SLOLabels {
		if strings.HasPrefix(l, "__") || strings.Contains(strings.ToLower(l), "namespace") {
			return configError("XR_SLO_LABELS names " + strconv.Quote(l) + ": a label that names a namespace is the gateway's to write, never an agent's")
		}
	}
	if p.DBSizes, err = listOf("XR_DB_SIZES", nil); err != nil {
		return err
	}
	for _, sz := range p.DBSizes {
		if sz != "small" && sz != "medium" && sz != "large" {
			return configError("XR_DB_SIZES names " + strconv.Quote(sz) + ": small, medium or large")
		}
	}
	for k, dst := range map[string]*int64{"XR_MAX_DB_STORAGE_GIB": &p.MaxStorageGiB} {
		if v := env(k); v != "" {
			n, perr := strconv.ParseInt(v, 10, 64)
			if perr != nil || n < 32 || n > 256 {
				return configError(k + " is a whole number from 32 to 256 (the XRD's own largest), not " + strconv.Quote(v))
			}
			*dst = n
		}
	}
	if v := env("XR_MAX_CONSUMERS"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 || n > 10 {
			return configError("XR_MAX_CONSUMERS is a whole number from 1 to 10, not " + strconv.Quote(v))
		}
		p.MaxConsumers = n
	}
	if p.AllowUnprotectedDB, err = flagEnv(env, "XR_ALLOW_UNPROTECTED_DB", false); err != nil {
		return err
	}
	if p.AllowEmptyOnDelete, err = flagEnv(env, "XR_ALLOW_EMPTY_ON_DELETE", false); err != nil {
		return err
	}
	return nil
}
