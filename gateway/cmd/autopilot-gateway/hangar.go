package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

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
//   - GITHUB_PR_ENABLED defaults the pinned definitions (AGENT_XRS_ENABLED) and xr.request (XR_REQUEST_ENABLED) on in Python;
//     the Go gateway carries neither yet, so it needs both said to be 0 rather than start without them silently;
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
	xrs, err := flagEnv(env, "AGENT_XRS_ENABLED", false)
	if err != nil {
		return h, err
	}
	xrReq, err := flagEnv(env, "XR_REQUEST_ENABLED", false)
	if err != nil {
		return h, err
	}
	if xrs || xrReq {
		return h, configError("AGENT_XRS_ENABLED and XR_REQUEST_ENABLED: not carried by the Go gateway yet (docs/admin/go-gateway.md); unset them, or keep running the Python gateway")
	}
	h.agentTokenFile = getenv(env, "AGENT_TOKEN_FILE", agentTokenDefault)
	if h.githubPRs {
		if env("AGENT_XRS_ENABLED") == "" || env("XR_REQUEST_ENABLED") == "" {
			return h, configError("GITHUB_PR_ENABLED turns the pinned definitions and xr.request on in the Python gateway, and the Go gateway does not carry them yet: " +
				"set AGENT_XRS_ENABLED=0 and XR_REQUEST_ENABLED=0 to say you know (docs/admin/go-gateway.md)")
		}
		h.brokerURL = env("BROKER_URL")
		for _, o := range strings.Split(env("GITHUB_OWNERS"), ",") {
			if o = strings.TrimSpace(o); o != "" {
				h.githubOwners = append(h.githubOwners, o)
			}
		}
		if _, err = backends.NewGitHub(backends.GitHubOptions{BrokerURL: h.brokerURL, TokenFile: h.agentTokenFile, Owners: h.githubOwners, AllowHTTP: h.brokerAllowHTTP}); err != nil {
			return h, configError("GITHUB_PR_ENABLED: " + err.Error())
		}
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
		if c.GitHub, err = backends.NewGitHub(backends.GitHubOptions{BrokerURL: h.brokerURL, TokenFile: h.agentTokenFile, Owners: h.githubOwners, AllowHTTP: h.brokerAllowHTTP}); err != nil {
			return c, err
		}
	}
	if h.argoSync {
		if c.ArgoSync, err = backends.NewArgoSync(nil, argoNamespaces); err != nil {
			return c, err
		}
	}
	return c, nil
}
