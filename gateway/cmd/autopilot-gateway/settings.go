package main

import (
	"fmt"
	"sort"
	"strings"
)

// The gateway's own settings are AUTOPILOT_*. R-10a renamed them from CLEARANCE_*, with the old names read as
// fallbacks for one release; R-10e removed the fallbacks. Settings that address the Clearance product
// (CLEARANCE_CORE_URL, CLEARANCE_CORE_TOKEN_FILE) keep their names.
//
// An old name that is still set is refused at start rather than ignored: running without the setting the operator
// asked for (an audience, a delegate list) would be a silent change. CLEARANCE_EPHEMERAL is refused already,
// with its AUTOPILOT_ twin, as a feature this slice does not carry (serve.go, unported).
var retired = map[string]string{
	"CLEARANCE_OIDC_ISSUER":        "AUTOPILOT_OIDC_ISSUER",
	"CLEARANCE_OIDC_AUDIENCE":      "AUTOPILOT_OIDC_AUDIENCE",
	"CLEARANCE_OIDC_ALLOWED_USERS": "AUTOPILOT_OIDC_ALLOWED_USERS",
	"CLEARANCE_OIDC_JWKS_URL":      "AUTOPILOT_OIDC_JWKS_URL",
	"CLEARANCE_OIDC_USER_CLAIM":    "AUTOPILOT_OIDC_USER_CLAIM",
	"CLEARANCE_AGENTS_DIR":         "AUTOPILOT_AGENTS_DIR",
	"CLEARANCE_AUDIENCE":           "AUTOPILOT_AUDIENCE",
	"CLEARANCE_RESOURCE_URL":       "AUTOPILOT_RESOURCE_URL",
	"CLEARANCE_ISSUER_URL":         "AUTOPILOT_ISSUER_URL",
	"CLEARANCE_EXPIRE_SECONDS":     "AUTOPILOT_EXPIRE_SECONDS",
	"CLEARANCE_DELEGATES":          "AUTOPILOT_DELEGATES",
	"CLEARANCE_ADMINS":             "AUTOPILOT_ADMINS",
	"CLEARANCE_GATEWAY_URL":        "AUTOPILOT_GATEWAY_URL",
}

// retiredSettings is the refusal for the retired names that are set ("" when none is).
func retiredSettings(env func(string) string) string {
	var bad []string
	for old, nu := range retired {
		if env(old) != "" {
			bad = append(bad, fmt.Sprintf("%s -> %s", old, nu))
		}
	}
	if len(bad) == 0 {
		return ""
	}
	sort.Strings(bad)
	return "retired settings (R-10e): " + strings.Join(bad, "; ") + ". Rename them: the old names are no longer read"
}

// coreOnly are the Clearance core's settings (Python: serve.CORE_ONLY, R-3b): the evidence DSN, the deployment gate's verifiers, provenance,
// the GitHub App and GitHub Actions callers belong to the core. A gateway configured with one is refused, as Python refuses it, rather than run
// without something the operator asked for.
var coreOnly = []string{"CLEARANCE_DSN", "CLEARANCE_VERIFIERS", "AGENT_LOGINS", "CLEARANCE_GITHUB_APP_ID", "CLEARANCE_GITHUB_APP_KEY_FILE",
	"CLEARANCE_GITHUB_APP_WEBHOOK_SECRET_FILE", "CLEARANCE_GITHUB_APP_COMMENTS", "CLEARANCE_GITHUB_OIDC_AUDIENCE", "CLEARANCE_GITHUB_OIDC_ALLOWED"}

// coreOnlySettings is the refusal for the core's settings that are set ("" when none is).
func coreOnlySettings(env func(string) string) string {
	var stale []string
	for _, k := range coreOnly {
		if env(k) != "" {
			stale = append(stale, k)
		}
	}
	if len(stale) == 0 {
		return ""
	}
	return strings.Join(stale, ", ") + ": the Clearance core's settings, not the gateway's (the gateway has no evidence DSN and serves no gate, provenance or GitHub App); set them on the core"
}
