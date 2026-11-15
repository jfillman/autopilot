package airframe

import (
	"errors"
	"sort"
	"strings"
	"unicode"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/pyrepr"
)

// GenericFailure is one of airframe-verify's catalog-wide Synced failures (airframe_tools.GENERIC_FAILURES).
type GenericFailure struct{ ID, Condition, Match, Meaning, Hint string }

// GenericFailures are tried after each kind's own knownFailures.
var GenericFailures = []GenericFailure{
	{ID: "AdmissionWebhookUnavailable", Condition: "Synced", Match: `failed calling webhook`,
		Meaning: "An admission webhook that one of the composed resources passes through did not answer; Crossplane could not apply it.",
		Hint:    "Find the webhook's Service in the message and check the operator behind it. The XR re-syncs once it answers."},
}

func omapOf(v any) *definitions.OMap {
	if o, ok := v.(*definitions.OMap); ok {
		return o
	}
	return &definitions.OMap{M: map[string]any{}}
}

func strOf(v any) string { s, _ := v.(string); return s }

// every is {**components, **targets, **kinds}: a later group wins on a repeated key, and the first place of a key stays.
func every() *definitions.OMap {
	out := &definitions.OMap{M: map[string]any{}}
	c := Contract()
	for _, g := range []string{"components", "targets", "kinds"} {
		src := omapOf(c.M[g])
		for _, k := range src.Keys {
			out.Set(k, src.M[k])
		}
	}
	return out
}

// FindKind is airframe_tools.find_kind: a kind's sidecar and XRD summary by Kind, lowercase key or plural. (nil, nil) when
// there is none; the XRD is an empty object when the contract has none for the kind.
func FindKind(name string) (sc, xrd *definitions.OMap) {
	low := strings.ToLower(name)
	all := every()
	xrds := omapOf(Contract().M["xrds"])
	for _, key := range all.Keys {
		s := omapOf(all.M[key])
		kind := strOf(s.M["kind"])
		x := omapOf(xrds.M[kind])
		if low == key || low == strings.ToLower(kind) || low == strings.ToLower(strOf(x.M["plural"])) {
			return s, x
		}
	}
	return nil, nil
}

// -- AF-* rules (airframe_tools.AF_RULES) ---------------------------------------------------------------------------------------

// AFRules are airframe-validate's rules; the bundle does not carry them. The Python test fails when the table and the ids
// airframe-validate can emit differ; this copy is held equal to Python's by the fixtures.
var afRuleIDs = []string{
	"AF-SCHEMA-001",
	"AF-SCHEMA-002",
	"AF-RENDER-001",
	"AF-COMP-001",
	"AF-COMP-002",
	"AF-COMP-003",
	"AF-SVC-001",
	"AF-CLUSTER-001",
	"AF-ENV-001",
	"AF-SECRET-001",
	"AF-OWNER-001",
	"AF-XR-001",
	"AF-XR-002",
	"AF-XR-003",
	"AF-XR-004",
}

var afRules = map[string][4]string{ // mode, severity, description, hint
	"AF-SCHEMA-001":  {"values, xr", "error", "An unknown key: the strict schema (values.schema.json, or the XRD with additionalProperties:false) has no such field.", "Fix the spelling (the problem's hint names the closest real key) or remove the key."},
	"AF-SCHEMA-002":  {"values, xr", "error", "A wrong type or value for a field, or the file is not valid YAML.", "See the field's schema in values.schema.json (or the XRD)."},
	"AF-RENDER-001":  {"values", "error", "helm template failed on the airframe-application chart with this file (a chart render guard, or values the chart rejects).", "Fix the value the message names, then validate again."},
	"AF-COMP-001":    {"values", "error", "A components[] entry has an unknown component type.", "Use one of the component types airframe.capabilities lists under components."},
	"AF-COMP-002":    {"values", "error", "An env[].fromComponent reference names a components[] entry, or an output of it, that does not exist.", "components[].name must match exactly; the output must be one the component's sidecar declares."},
	"AF-COMP-003":    {"values", "warning", "A hand-written valueFrom.secretKeyRef or configMapKeyRef matches a real component's own output name.", "Use fromComponent: {name, output} instead of the derived Secret or ConfigMap name."},
	"AF-SVC-001":     {"values", "warning", "An env value hand-writes another app's in-cluster Service URL (<svc>.<namespace>.svc[.cluster.local]), relying on the platform's namespace convention instead of letting the platform resolve it.", "Use fromService: {app, port} (scheme, type, env, service as needed); a platform service outside the <type>-<app>-<env> namespaces can stay a value."},
	"AF-CLUSTER-001": {"values", "error", "devClusterName is set; it was removed (2026-10-16) and the chart compares no cluster names.", "Delete it; the platform sets clusterType and cluster from the cluster registry."},
	"AF-ENV-001":     {"values", "error", "envName is a pipeline stage name (build, test, deploy, release), not an environment.", "Name the environment (dev, staging, prod, a PR number)."},
	"AF-SECRET-001":  {"values", "error", "A literal in env or configMaps looks like a credential (an AWS key id, a GitHub or Slack token, an API key, a private key, a JWT).", "Secrets are references (valueFrom, fromComponent, secrets: from Infisical), never values."},
	"AF-OWNER-001":   {"values", "error", "A release-owned key (release, releaseTracking, the deprecated rollout.image) in a human-owned file, or anything else in a release file (*.release.yaml, release.yaml).", "Leave release keys to the release pipeline's release file."},
	"AF-XR-001":      {"xr", "error", "An XR request's apiVersion is not catalog.hangar.io/v1alpha1 (catalog.idp.io was renamed on 2026-10-01).", "Set apiVersion: catalog.hangar.io/v1alpha1."},
	"AF-XR-002":      {"xr", "error", "An XR request names an unknown kind.", "Use a kind airframe.capabilities lists (the problem's hint names the closest one)."},
	"AF-XR-003":      {"xr", "error", "An XR request has no metadata.name, is not a mapping, or the file holds no document.", "One XR per document, with apiVersion, kind, metadata.name and spec."},
	"AF-XR-004":      {"xr", "warning", "An XR request sets no metadata.namespace.", "Fine under the xr-requests Application (it supplies app-<app>-cicd); set it if applied by hand."},
}

// PyError is an exception the Python tools raise for a malformed argument (a KeyError for an unknown section): the call is an
// error, never an answer for the agent.
type PyError struct{ Class, Msg string }

func (e *PyError) Error() string { return e.Msg }

// Kind is the Python class name.
func (e *PyError) Kind() string { return e.Class }

// Answer is airframe_tools._answer: a refusal about the question, not the caller.
func Answer(rule, message, hint string) *definitions.OMap {
	return plMk("error", plMk("rule", rule, "message", message, "hint", hint))
}

// -- helpers over the contract ----------------------------------------------------------------------------------------------------

func oget(o *definitions.OMap, k string) any {
	if o == nil {
		return nil
	}
	return o.M[k]
}

// pyFields is " ".join(s.split()) with Python's white space.
func pyFields(s string) string {
	isSpace := func(r rune) bool {
		return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
	}
	return strings.Join(strings.FieldsFunc(s, isSpace), " ")
}

// line is airframe_tools._line: the first sentence of a schema description, at most n characters.
func line(t string, n int) string {
	t = pyFields(t)
	first := t
	if i := strings.Index(t, ". "); i >= 0 {
		first = t[:i]
	}
	first = strings.TrimRight(first, ".")
	r := []rune(first)
	if len(r) <= n {
		return first
	}
	return strings.TrimRight(string(r[:n-1]), " \t\n\r\x0b\x0c") + "..."
}

func keysOf(o *definitions.OMap) []string {
	if o == nil {
		return nil
	}
	return append([]string{}, o.Keys...)
}

func listAny(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

// -- airframe.capabilities ---------------------------------------------------------------------------------------------------

var (
	// WorkedExample and ExampleAsked are the AppSpec section's complete worked example.
	exampleAsked = "Python app parachute, ground dev + flight prod, canary 50%/30s pause/100%, env URL=http://hangarplatform.dev OWNER=jfillman"
	chartShapes  = map[string]string{
		"env":        "list of {name, value} or {name, valueFrom}",
		"components": "list of attached components (see airframe.capabilities {section: components} for the types and their spec)",
		"secrets":    "references only, never values",
	}
)

func workedExample() *definitions.OMap {
	return plMk("apiVersion", "airframe/v1", "kind", "AppSpec", "app", "parachute", "stack", "python",
		"environments", plMk("ground", []any{"dev"}, "flight", []any{plMk("name", "prod")}),
		"config", plMk("env", []any{plMk("name", "URL", "value", "http://hangarplatform.dev"), plMk("name", "OWNER", "value", "jfillman")},
			"rollout", plMk("enabled", true, "strategy", "canary",
				"steps", []any{plMk("setWeight", int64(50)), plMk("pause", plMk("duration", "30s")), plMk("setWeight", int64(100))})))
}

func appSpecSection() *definitions.OMap {
	sch := mustOMap(appSpecJSON, "the AppSpec schema")
	c := Contract()
	props := plGetO(sch, "properties")
	chart := plGetO(plGetO(plGetO(sch, "$defs"), "chartValues"), "properties")
	fields := plNewO()
	for _, k := range props.Keys {
		v := plGetO(props, k)
		desc, _ := v.M["description"].(string)
		switch {
		case hasKey(v, "const"):
			fields.Set(k, "always "+pyrepr.Value(v.M["const"]))
		case hasKey(v, "enum"):
			var en []string
			for _, e := range v.M["enum"].([]any) {
				en = append(en, e.(string))
			}
			s := line(desc, 140)
			if desc != "" {
				s += ". "
			}
			fields.Set(k, s+"One of: "+strings.Join(en, ", "))
		default:
			s := line(desc, 140)
			if s == "" {
				typ := "value"
				if t, ok := v.M["type"]; ok {
					typ = plText(t)
				}
				var parts []string
				for _, a := range v.Keys {
					if a == "minLength" || a == "maxLength" || a == "pattern" {
						parts = append(parts, a+": "+plText(v.M[a]))
					}
				}
				s = typ + ", " + strings.Join(parts, ", ")
			}
			fields.Set(k, s)
		}
	}
	fields.Set("app", fields.M["app"].(string)+" (pattern "+plText(plGetO(props, "app").M["pattern"])+")")
	fields.Set("stackOptions", "optional: "+strings.Join(keysOf(plGetO(plGetO(props, "stackOptions"), "properties")), ", ")+" (port also sets rollout.ports)")
	fields.Set("environments", "{ground: [names], flight: [{name, cluster?}]}: ground = disposable environments on the dev cluster, flight = governed ones on an upper cluster; both keys are required")
	fields.Set("config", "chart values applied to EVERY environment (the keys under chartValues below); anything an environment's values file may hold. Not a place for secrets")
	fields.Set("overrides", "{<environment name>: chart values}: per-environment chart values deep-merged over config (lists replace, not append); every key must be an environment declared in environments")
	chartFields := plGetO(plGetO(c, "chart"), "fields")
	chartValues := plNewO()
	for _, k := range chart.Keys {
		v := plGetO(chart, k)
		desc, _ := v.M["description"].(string)
		if desc == "" {
			desc, _ = plGetO(chartFields, k).M["description"].(string)
		}
		keys := keysOf(plGetO(v, "properties"))
		shape := chartShapes[k]
		if shape == "" && len(keys) > 0 {
			shape = "object with keys: " + strings.Join(keys, ", ")
		}
		e := plMk("meaning", line(desc, 110))
		if shape != "" {
			e.Set("shape", shape)
		}
		chartValues.Set(k, e)
	}
	var required []any
	reqSet := map[string]bool{}
	if r, ok := sch.M["required"].([]any); ok {
		required = append(required, r...)
		for _, x := range r {
			reqSet[plText(x)] = true
		}
	}
	optional := []any{}
	for _, k := range props.Keys {
		if !reqSet[k] {
			optional = append(optional, k)
		}
	}
	var notSettable []string
	for _, k := range chartFields.Keys {
		if _, ok := chart.M[k]; !ok {
			notSettable = append(notSettable, k)
		}
	}
	sort.Strings(notSettable)
	if required == nil {
		required = []any{}
	}
	return plMk(
		"what", "The spec airframe.plan and airframe.apply take as `spec`. Send it whole and exactly as below; do not guess field names, and do not send a field this section does not list (the schema is strict).",
		"required", required, "optional", optional, "fields", fields,
		"mapping", plMk("config", "chart values for every environment", "overrides", "chart values for one environment, over config",
			"chartValues", "the keys allowed under config and under each overrides.<env>; each is the airframe-application chart's own top-level value, so config.rollout.* is the chart's rollout.*",
			"notSettable", "platform- and release-owned chart keys ("+strings.Join(notSettable, ", ")+"): not in an AppSpec, the platform sets them"),
		"chartValues", chartValues,
		"workedExample", plMk("asked", exampleAsked, "spec", workedExample()),
		"ifNotExpressible", "If what was asked has no field here, say so and ask; do not drop it from the spec.")
}

func hasKey(o *definitions.OMap, k string) bool { _, ok := o.M[k]; return ok }

// Capabilities is AirframeTools.capabilities(kind, section): the contract bundle, or one kind's part of it. An empty kind or
// section is "not given". An unknown section is a *PyError (Python's KeyError).
func Capabilities(kind, section string) (any, error) {
	c := Contract()
	if kind != "" {
		sc, xrd := FindKind(kind)
		if sc == nil {
			return Answer("AF-KIND-404", "no Airframe kind "+pyrepr.Str(kind), "Call airframe.capabilities with no arguments for the list."), nil
		}
		out := plMk("kind", sc.M["kind"], "xrd", xrd)
		for _, k := range sc.Keys {
			out.Set(k, sc.M[k])
		}
		return out, nil
	}
	if section == "appspec" {
		return plMk("appspec", appSpecSection()), nil
	}
	if section != "" {
		v, ok := c.M[section]
		if !ok {
			return nil, &PyError{"KeyError", pyrepr.Str(section)}
		}
		return plMk(section, v), nil
	}
	all := every()
	keys := plSortedCopy(all.Keys)
	xrds := plGetO(c, "xrds")
	kinds := make([]any, 0, len(keys))
	for _, k := range keys {
		sc := plGetO(all, k)
		x := plGetO(xrds, plText(sc.M["kind"]))
		var verify []any
		if l, ok := sc.M["verify"].([]any); ok {
			for _, v := range l {
				verify = append(verify, plGetO(&definitions.OMap{M: map[string]any{"v": v}, Keys: []string{"v"}}, "v").M["id"])
			}
		}
		if verify == nil {
			verify = []any{}
		}
		outputs := listAny(plSortedCopy(keysOf(plGetO(sc, "outputs"))))
		kinds = append(kinds, plMk("kind", sc.M["kind"], "role", oget(sc, "role"), "summary", oget(sc, "summary"),
			"apiVersion", oget(x, "apiVersion"), "plural", oget(x, "plural"), "outputs", outputs, "verify", verify))
	}
	chart := plGetO(c, "chart")
	fields := plNewO()
	cf := plGetO(chart, "fields")
	for _, k := range cf.Keys {
		f := plNewO()
		src := plGetO(cf, k)
		for _, kk := range src.Keys {
			if kk != "default" {
				f.Set(kk, src.M[kk])
			}
		}
		fields.Set(k, f)
	}
	return plMk("more", "airframe.capabilities {section: appspec} FIRST when writing an AppSpec for airframe.plan/apply (every field, how config/overrides map onto chart values, a worked example); {kind} for one kind's outputs, conditions and verify checks; {section: chart|xrds|components|targets|kinds|appspec} for a whole section. This default answer is large and may be clipped: narrow with section or kind",
		"contract", c.M["$schema"], "source", "airframe contract/airframe-contract.json (vendored)",
		"chart", plMk("name", chart.M["name"], "fields", fields), "kinds", kinds), nil
}

// Explain is AirframeTools.explain: what an AF-* rule id, a condition reason or a knownFailures id means. A rule that is not a
// string is looked up and not found, as in Python (a list or mapping there is an unhashable TypeError: a *PyError).
func Explain(rule any) (any, error) {
	switch rule.(type) {
	case []any, map[string]any, *definitions.OMap:
		return nil, &PyError{"TypeError", "unhashable type"}
	}
	if s, ok := rule.(string); ok {
		if r, ok := afRules[s]; ok {
			return plMk("rule", s, "mode", r[0], "severity", r[1], "description", r[2], "hint", r[3], "source", "airframe tools/airframe-validate"), nil
		}
	}
	var found []any
	if s, ok := rule.(string); ok {
		all := every()
		for _, k := range all.Keys {
			sc := plGetO(all, k)
			if l, ok := sc.M["knownFailures"].([]any); ok {
				for _, x := range l {
					kf := plGetO(&definitions.OMap{M: map[string]any{"x": x}, Keys: []string{"x"}}, "x")
					if kf.M["id"] == s {
						found = append(found, plMk("kind", sc.M["kind"], "knownFailure", s, "condition", kf.M["condition"], "description", kf.M["meaning"], "hint", kf.M["hint"]))
					}
				}
			}
			if l, ok := sc.M["conditions"].([]any); ok {
				for _, x := range l {
					cd := plGetO(&definitions.OMap{M: map[string]any{"x": x}, Keys: []string{"x"}}, "x")
					if rs, ok := cd.M["reasons"].([]any); ok {
						for _, y := range rs {
							r := plGetO(&definitions.OMap{M: map[string]any{"x": y}, Keys: []string{"x"}}, "x")
							if r.M["reason"] == s {
								found = append(found, plMk("kind", sc.M["kind"], "condition", cd.M["type"], "status", r.M["status"], "description", r.M["meaning"]))
							}
						}
					}
				}
			}
		}
		for _, k := range GenericFailures {
			if k.ID == s {
				found = append(found, plMk("kind", "*", "knownFailure", s, "condition", k.Condition, "description", k.Meaning, "hint", k.Hint))
			}
		}
	}
	if len(found) > 0 {
		return plMk("rule", rule, "matches", found, "source", "airframe contract/airframe-contract.json"), nil
	}
	ids := plSortedCopy(afRuleIDs)
	return Answer("AF-EXPLAIN-404", pyrepr.Value(rule)+" is not an AF-* rule, a condition reason or a knownFailures id Airframe declares",
		"AF-* rules: "+strings.Join(ids, ", ")), nil
}

// -- the planner as a tool ----------------------------------------------------------------------------------------------------------

// Changeset is AirframeTools.changeset: the planner's ChangeSet, or (nil, the answer to give instead): the AF-SPEC-* refusal or
// needsInput. spec and baseCICD are in the ordered value model (FromArgs); baseCICD is nil, a mapping, or the text of the app's
// cicd.yaml. An error is a value that is not usable (not an answer).
func Changeset(spec any, baseCICD any, reg Registry) (*ChangeSet, any) {
	var base any
	switch b := baseCICD.(type) {
	case nil:
	case string:
		m, err := definitions.ParseYAML([]byte(b))
		switch {
		case err == nil:
			base = m
		case errors.Is(err, definitions.ErrNotAMapping):
			return nil, Answer("AF-SPEC-002", "baseCicd is not a YAML mapping", "Pass the app's cicd.yaml as it is, or as a mapping.")
		default:
			var se *definitions.StrictError
			if errors.As(err, &se) {
				return nil, Answer("AF-SPEC-002", "baseCicd is not acceptable YAML: "+se.Msg, "Pass the app's cicd.yaml as it is, or as a mapping.")
			}
			return nil, Answer("AF-SPEC-002", "baseCicd does not parse as YAML", "Pass the app's cicd.yaml as it is, or as a mapping.")
		}
	case *definitions.OMap:
		base = b
	default:
		return nil, Answer("AF-SPEC-002", "baseCicd is not a YAML mapping", "Pass the app's cicd.yaml as it is, or as a mapping.")
	}
	cs, err := Plan(spec, reg, base)
	if err == nil {
		return cs, nil
	}
	var ni *NeedsInput
	if errors.As(err, &ni) {
		return nil, plMk("needsInput", plMk("question", ni.Question, "options", listAny(ni.Options)))
	}
	var se *SpecError
	if errors.As(err, &se) {
		return nil, Answer("AF-SPEC-001", se.Msg, "Fix the AppSpec at the path named; airframe.capabilities lists the chart's fields.")
	}
	return nil, Answer("AF-SPEC-001", err.Error(), "Fix the AppSpec at the path named; airframe.capabilities lists the chart's fields.")
}

// PlanAnswer is AirframeTools.plan: the change set as a plain mapping, or the answer instead.
func PlanAnswer(spec any, baseCICD any, reg Registry) any {
	cs, answer := Changeset(spec, baseCICD, reg)
	if cs == nil {
		return definitions.Plain(answer)
	}
	return cs.AsMap()
}
