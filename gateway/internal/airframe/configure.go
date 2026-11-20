package airframe

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// The pure half of airframe.configure (airframe_apply.py: configure_branch, canonical, _named, _merge_values, _chart_errors): what a
// change to an existing environment's chart values means. The backends package reads and writes the repository.

// EnvsDir is where an app repository keeps one file per Ground environment (airframe_plan.ENVS_DIR).
const EnvsDir = "glidepath/envs"

var (
	configureApp = regexp.MustCompile(`\A[a-z][a-z0-9-]{1,38}[a-z0-9]\z`)
	configureEnv = regexp.MustCompile(`\A[a-z0-9]([a-z0-9-]{0,40}[a-z0-9])?\z`)
)

// ConfigureAppName and ConfigureEnvName are the names configure accepts (APP_NAME and ENV_NAME).
func ConfigureAppName(s string) bool { return configureApp.MatchString(s) }
func ConfigureEnvName(s string) bool { return configureEnv.MatchString(s) }

// ConfigureBranch is configure_branch: derived from what the change says (the app, the environment, the canonical patch and whether it
// replaces), so a retry of the same call finds the PR the first one opened. values is the ordered value model with every mapping's keys
// sorted (FromArgs gives that), so the hash is json.dumps(sort_keys=True, separators=(",", ":")) of the same document.
func ConfigureBranch(app, env string, values any, replace bool) string {
	r := "false"
	if replace {
		r = "true"
	}
	doc := `{"env":` + plPyJSONString(env) + `,"replace":` + r + `,"values":` + plPyJSON(values) + `}`
	h := sha256.Sum256([]byte(doc))
	return "agent/airframe-" + app + "-configure-" + env + "-" + hex.EncodeToString(h[:])[:10]
}

// configureNamed is _named: a list of mappings that each have a unique string `name`.
func configureNamed(v any) ([]*definitions.OMap, bool) {
	l, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]*definitions.OMap, 0, len(l))
	seen := map[string]bool{}
	for _, e := range l {
		m, ok := e.(*definitions.OMap)
		if !ok {
			return nil, false
		}
		n, ok := m.M["name"].(string)
		if !ok || seen[n] {
			return nil, false
		}
		seen[n] = true
		out = append(out, m)
	}
	return out, true
}

// MergeValues is _merge_values: `have` with the patch `want` merged in. Mappings merge key by key; lists of named entries (every item a
// mapping with a unique string `name`) append the entries the file lacks, keep an identical one and conflict on one with the same name
// and other content; any other list or scalar is kept when equal and a conflict otherwise. A conflict is named in conflicts and the
// file's value kept, unless replace: then the patch's value wins and its path is named in replaced.
func MergeValues(have, want any, path string, conflicts, replaced *[]string, replace bool) any {
	hm, hok := have.(*definitions.OMap)
	wm, wok := want.(*definitions.OMap)
	if hok && wok {
		out := &definitions.OMap{M: map[string]any{}}
		for _, k := range hm.Keys {
			out.Set(k, hm.M[k])
		}
		for _, k := range wm.Keys {
			sub := k
			if path != "" {
				sub = path + "." + k
			}
			if hv, has := hm.M[k]; has {
				out.Set(k, MergeValues(hv, wm.M[k], sub, conflicts, replaced, replace))
			} else {
				out.Set(k, wm.M[k])
			}
		}
		return out
	}
	hl, hnamed := configureNamed(have)
	wl, wnamed := configureNamed(want)
	if hnamed && wnamed {
		out := make([]any, len(hl))
		index := map[string]int{}
		for i, e := range hl {
			out[i] = e
			index[e.M["name"].(string)] = i
		}
		for _, item := range wl {
			name := item.M["name"].(string)
			sub := path + "[" + name + "]"
			i, has := index[name]
			switch {
			case !has:
				out = append(out, item)
			case !plPyEq(out[i], item):
				if replace {
					out[i] = item
					*replaced = append(*replaced, sub)
				} else {
					*conflicts = append(*conflicts, sub)
				}
			}
		}
		return out
	}
	if !plPyEq(have, want) {
		if path == "" {
			path = "<root>"
		}
		if replace {
			*replaced = append(*replaced, path)
			return want
		}
		*conflicts = append(*conflicts, path)
	}
	return have
}

// ChartErrors is _chart_errors: chartValues' findings, "path: message" each, in jsonschema's order. For a whole file only its chart
// keys are checked (envName and anything that is not a human-owned chart key are not the patch's to check); a patch is checked as it is.
func ChartErrors(doc *definitions.OMap, wholeFile bool) []string {
	sch := plSchema()
	part := doc
	if wholeFile {
		part = plNewO()
		for _, k := range doc.Keys {
			if _, ok := chartKeys.M[k]; ok {
				part.Set(k, doc.M[k])
			}
		}
	}
	errs := sch.ErrorsAt("#/$defs/chartValues", part)
	definitions.SortByPath(errs)
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		parts := make([]string, 0, len(e.Path))
		for _, p := range e.Path {
			parts = append(parts, plText(p))
		}
		path := strings.Join(parts, "/")
		if path == "" {
			path = "<root>"
		}
		out = append(out, path+": "+e.Message)
	}
	return out
}

// CriticalKey says a chart key is critical-risk (a PR that sets one needs a person to merge it).
func CriticalKey(key string) bool { return plRisk(key) == "critical" }

// SortedStrings is sorted(l) for a list of strings.
func SortedStrings(l []string) []string {
	out := append([]string(nil), l...)
	sort.Strings(out)
	return out
}
