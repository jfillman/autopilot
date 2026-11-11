package main

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// setting is `name`'s value, else the deprecated `legacy` name's (recorded in used as legacy -> name), else "". The
// gateway's own settings are AUTOPILOT_*; the CLEARANCE_* names they replaced (R-10a) still work for one release, the
// new name winning when both are set. An empty variable is an unset one. Settings that address the Clearance product
// (CLEARANCE_CORE_URL, CLEARANCE_CORE_TOKEN_FILE) keep their names and do not come through here.
func setting(env func(string) string, used map[string]string, name, legacy string) string {
	if v := env(name); v != "" {
		return v
	}
	if legacy != "" {
		if v := env(legacy); v != "" {
			used[legacy] = name
			return v
		}
	}
	return ""
}

func settingOr(env func(string) string, used map[string]string, name, legacy, d string) string {
	if v := setting(env, used, name, legacy); v != "" {
		return v
	}
	return d
}

// deprecationWarning is the single line logged at start for the legacy names in use ("" when none is).
func deprecationWarning(used map[string]string) string {
	if len(used) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(used))
	for old, nu := range used {
		pairs = append(pairs, fmt.Sprintf("%s -> %s", old, nu))
	}
	sort.Strings(pairs)
	return "deprecated settings in use, renamed in this release (the old names stop working in the next): " + strings.Join(pairs, "; ")
}

func warnDeprecated(log *slog.Logger, used map[string]string) {
	if m := deprecationWarning(used); m != "" {
		log.Warn(m)
	}
}
