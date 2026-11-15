package main

import (
	"strconv"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/backends"
)

// readSettings are the settings of the S5a read backends (catalog.read, app.api.get, argo.app.get), as Python's
// config_from_env reads them.
type readSettings struct {
	backstageURL, backstageTokenFile string
	backstageAllowHTTP               bool // BACKSTAGE_ALLOW_HTTP (Go only, default off): the URL may be plain http
	appAPIServices                   map[string]string
	argoNamespaces                   []string // empty: argo.app.get is off (ARGO_ENABLED=0)
}

// readBackends reads BACKSTAGE_URL and BACKSTAGE_TOKEN_FILE (catalog.read), APP_API_SERVICES (app.api.get, a JSON object
// {service: base URL}) and ARGO_ENABLED (default on) with ARGO_APP_NAMESPACES (default argocd) for argo.app.get. Each is
// checked here, so a bad one stops the start rather than a call.
func readBackends(env func(string) string) (readSettings, error) {
	var r readSettings
	var err error
	if u := env("BACKSTAGE_URL"); u != "" {
		if _, err := backends.NewCatalog(u, "", 0); err != nil {
			return r, configError(err.Error())
		}
		// The gateway's token for the catalog goes to this URL on every call: https, unless the operator says plain http in a setting of its
		// own (nothing here treats a host name as special).
		if r.backstageAllowHTTP, err = strictFlag(env, "BACKSTAGE_ALLOW_HTTP", false); err != nil {
			return r, err
		}
		if !strings.HasPrefix(u, "https://") && !r.backstageAllowHTTP {
			return r, configError("BACKSTAGE_URL must be an https URL: the catalog's bearer token is sent to it (a plain http URL needs BACKSTAGE_ALLOW_HTTP=1)")
		}
		r.backstageURL = u
	}
	r.backstageTokenFile = getenv(env, "BACKSTAGE_TOKEN_FILE", backends.BackstageTokenFileDefault)
	svcs, err := backends.ParseAppAPIServices(env("APP_API_SERVICES"))
	if err != nil {
		return r, configError(err.Error())
	}
	if len(svcs) > 0 {
		if _, err := backends.NewAppAPIs(svcs, 0); err != nil {
			return r, configError(err.Error())
		}
		r.appAPIServices = svcs
	}
	on := true
	switch strings.ToLower(env("ARGO_ENABLED")) {
	case "", "1", "true", "yes", "on":
	case "0", "false", "no", "off":
		on = false
	default:
		return r, configError("ARGO_ENABLED must be 0 or 1, not " + strconv.Quote(env("ARGO_ENABLED")))
	}
	ns, err := backends.ParseNamespaces(getenv(env, "ARGO_APP_NAMESPACES", "argocd"))
	if err != nil {
		return r, configError(err.Error())
	}
	if on && len(ns) == 0 {
		return r, configError("ARGO_APP_NAMESPACES names no namespace (or set ARGO_ENABLED=0)")
	}
	if on {
		r.argoNamespaces = ns
	}
	return r, nil
}
