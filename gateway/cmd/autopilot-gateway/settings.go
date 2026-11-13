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
// asked for (an audience, a delegate list) would be a silent change. CLEARANCE_EPHEMERAL and CLEARANCE_OIDC_ISSUER are
// refused already, with their AUTOPILOT_ twins, as features this slice does not carry (serve.go, unported).
var retired = map[string]string{
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
