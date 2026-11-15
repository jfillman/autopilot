package airframe

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/pyrepr"
)

// The planner (airframe_plan.py): an AppSpec becomes an ordered set of repo changes, each with its gate, its dependencies and what
// it will do. A pure function: no network, no clock, the same input gives the same output. Values are the ordered value model
// (*definitions.OMap, int64, float64, string, bool, nil, []any); the spec the caller passes is never changed or kept.

const (
	chartDefaultPort = 8080
	envsDir          = "glidepath/envs"
	baseFile         = "glidepath/base.yaml"
)

var (
	stackKind        = map[string]string{"nodejs": "NodeJSApplication", "springboot": "SpringBootApplication", "go": "GoApplication", "python": "PythonApplication"}
	pipelineStages   = map[string]bool{"build": true, "test": true, "deploy": true, "release": true}
	legacyDeployKeys = []string{"lowerEnvironments", "upperEnvironments", "promotionOrder"} // removed from the schema 2026-10-07
)

// SpecError is a spec the planner refuses.
type SpecError struct{ Msg string }

func (e *SpecError) Error() string { return e.Msg }

// NeedsInput is an intent that is ambiguous in a way a default should not paper over.
type NeedsInput struct {
	Question string
	Options  []string
}

func (e *NeedsInput) Error() string { return e.Question }

// Features is what the current Airframe can do. False means not yet.
type Features struct {
	RolloutGuard bool
	BaseLayer    bool
	ReleaseSplit bool
}

// FileChange is one file of a step.
type FileChange struct {
	Path    string
	Op      string // create | merge-patch
	Content any    // text for create (or a mapping), a mapping for merge-patch
	Before  any    // merge-patch: the content it applies to
}

// Step is one step of a change set.
type Step struct {
	ID        string
	Title     string
	Repo      string
	Gate      string // auto-merge | human-merge | wait | verify
	Risk      string // T0 | T1 | T2
	DependsOn []string
	Files     []FileChange
	WaitFor   *string
	Effects   []string
	Checks    []string
}

// ChangeSet is the plan.
type ChangeSet struct {
	App         string
	Steps       []*Step
	Assumptions []string
	Warnings    []string
}

// Step is ChangeSet.step: the step with this id, or nil.
func (cs *ChangeSet) Step(id string) *Step {
	for _, s := range cs.Steps {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func plStrList(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

// AsMap is dataclasses.asdict(cs): plain maps and slices, ready for JSON.
func (cs *ChangeSet) AsMap() map[string]any {
	steps := make([]any, len(cs.Steps))
	for i, s := range cs.Steps {
		files := make([]any, len(s.Files))
		for j, f := range s.Files {
			files[j] = map[string]any{"path": f.Path, "op": f.Op, "content": definitions.Plain(plCopyV(f.Content)), "before": definitions.Plain(plCopyV(f.Before))}
		}
		var wait any
		if s.WaitFor != nil {
			wait = *s.WaitFor
		}
		steps[i] = map[string]any{"id": s.ID, "title": s.Title, "repo": s.Repo, "gate": s.Gate, "risk": s.Risk,
			"depends_on": plStrList(s.DependsOn), "files": files, "wait_for": wait, "effects": plStrList(s.Effects), "checks": plStrList(s.Checks)}
	}
	return map[string]any{"app": cs.App, "steps": steps, "assumptions": plStrList(cs.Assumptions), "warnings": plStrList(cs.Warnings)}
}

// -- values -----------------------------------------------------------------------------------------------------------------------

func plNewO() *definitions.OMap { return &definitions.OMap{M: map[string]any{}} }

func plMk(kv ...any) *definitions.OMap {
	o := plNewO()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// copyV is copy.deepcopy.
func plCopyV(v any) any {
	switch x := v.(type) {
	case *definitions.OMap:
		o := plNewO()
		for _, k := range x.Keys {
			o.Set(k, plCopyV(x.M[k]))
		}
		return o
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plCopyV(e)
		}
		return out
	}
	return v
}

func plAsMap(v any) (*definitions.OMap, bool) { o, ok := v.(*definitions.OMap); return o, ok }

// getO is m.get(k) when it is a mapping, else an empty one (Python's `x.get(k) or {}`).
func plGetO(m *definitions.OMap, k string) *definitions.OMap {
	if m == nil {
		return plNewO()
	}
	if o, ok := m.M[k].(*definitions.OMap); ok {
		return o
	}
	return plNewO()
}

func plTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case *definitions.OMap:
		return len(x.Keys) > 0
	}
	return true
}

func plNumOf(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// pyEq is Python's ==: mappings ignore key order, lists do not, and 1 == 1.0 == True.
func plPyEq(a, b any) bool {
	if x, ok := plNumOf(a); ok {
		y, ok2 := plNumOf(b)
		return ok2 && x == y
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !plPyEq(x[i], y[i]) {
				return false
			}
		}
		return true
	case *definitions.OMap:
		y, ok := b.(*definitions.OMap)
		if !ok || len(x.Keys) != len(y.Keys) {
			return false
		}
		for _, k := range x.Keys {
			v, has := y.M[k]
			if !has || !plPyEq(x.M[k], v) {
				return false
			}
		}
		return true
	}
	return false
}

// text is str(v) / f"{v}": a string as it is, anything else as its repr.
func plText(v any) string { return pyrepr.Text(v) }

// setdefault is dict.setdefault for a value that must be a mapping later: it returns the stored value.
func plSetdefault(m *definitions.OMap, k string, d any) any {
	if v, ok := m.M[k]; ok {
		return v
	}
	m.Set(k, d)
	return d
}

// merge is the deep merge, b wins; lists are replaced, not concatenated.
func plMerge(a, b *definitions.OMap) *definitions.OMap {
	out := plCopyV(a).(*definitions.OMap)
	for _, k := range b.Keys {
		v := b.M[k]
		if vm, ok := v.(*definitions.OMap); ok {
			if om, ok := out.M[k].(*definitions.OMap); ok {
				out.Set(k, plMerge(om, vm))
				continue
			}
		}
		out.Set(k, plCopyV(v))
	}
	return out
}

func plEnvConfig(spec *definitions.OMap, env string) *definitions.OMap {
	var cfg *definitions.OMap = plNewO()
	if c, ok := spec.M["config"].(*definitions.OMap); ok {
		cfg = c
	}
	over := plNewO()
	if o, ok := spec.M["overrides"].(*definitions.OMap); ok {
		if e, ok := o.M[env].(*definitions.OMap); ok {
			over = e
		}
	}
	return plMerge(cfg, over)
}

var (
	schemaOnce sync.Once
	theSchema  *definitions.Schema
	chartKeys  *definitions.OMap
)

func plSchema() *definitions.Schema {
	schemaOnce.Do(func() {
		s, err := definitions.NewSchema(appSpecJSON)
		if err != nil {
			panic("the AppSpec schema: " + err.Error())
		}
		theSchema = s
		root := mustOMap(appSpecJSON, "the AppSpec schema")
		chartKeys = plGetO(plGetO(plGetO(root, "$defs"), "chartValues"), "properties")
	})
	return theSchema
}

func plRisk(key string) string {
	plSchema()
	if r, ok := plGetO(chartKeys, key).M["x-hangar-risk"].(string); ok {
		return r
	}
	return "unknown"
}

func routeHosts(c *definitions.OMap) []string {
	var hosts []string
	if plTruthy(plGetO(c, "httpRoute").M["enabled"]) {
		if l, ok := c.M["httpRoute"].(*definitions.OMap).M["hostnames"].([]any); ok {
			for _, h := range l {
				hosts = append(hosts, plText(h))
			}
		}
	}
	if plTruthy(plGetO(c, "ingress").M["enabled"]) {
		if h := c.M["ingress"].(*definitions.OMap).M["host"]; plTruthy(h) {
			hosts = append(hosts, plText(h))
		}
	}
	return hosts
}

func envChecks(env string, c *definitions.OMap) []string {
	var out []string
	if l, ok := c.M["env"].([]any); ok && len(l) > 0 {
		parts := make([]string, 0, len(l))
		for _, e := range l {
			em := plGetO(&definitions.OMap{M: map[string]any{"e": e}, Keys: []string{"e"}}, "e")
			if _, has := em.M["value"]; has {
				parts = append(parts, plText(em.M["name"])+"="+plText(em.M["value"]))
			} else {
				parts = append(parts, plText(em.M["name"]))
			}
		}
		out = append(out, env+": rendered config contains "+strings.Join(parts, ", "))
	}
	r := plGetO(c, "rollout")
	if plTruthy(r.M["steps"]) {
		out = append(out, env+": rollout steps == "+plPyJSON(r.M["steps"]))
	}
	if v, has := r.M["replicas"]; has {
		out = append(out, env+": Rollout spec.replicas == "+plText(v))
	}
	if ps, ok := r.M["ports"].([]any); ok && len(ps) > 0 {
		parts := make([]string, len(ps))
		for i, p := range ps {
			pm := plGetO(&definitions.OMap{M: map[string]any{"p": p}, Keys: []string{"p"}}, "p")
			parts[i] = plText(pm.M["name"]) + ":" + plText(pm.M["containerPort"])
		}
		out = append(out, env+": Service ports == "+strings.Join(parts, ", "))
	}
	a := plGetO(c, "autoscaling")
	if plTruthy(a.M["enabled"]) {
		mn, mx := any(int64(2)), any(int64(10))
		if v, ok := a.M["min"]; ok {
			mn = v
		}
		if v, ok := a.M["max"]; ok {
			mx = v
		}
		out = append(out, fmt.Sprintf("%s: HorizontalPodAutoscaler targets the Rollout (min %s, max %s)", env, plText(mn), plText(mx)))
	}
	if plTruthy(plGetO(c, "podDisruptionBudget").M["enabled"]) {
		out = append(out, env+": PodDisruptionBudget exists for the Rollout's pods")
	}
	for _, h := range routeHosts(c) {
		out = append(out, env+": HTTPRoute for "+h+" is Accepted by its Gateway")
	}
	return out
}

// pyJSON is json.dumps(v, separators=(",", ":")): compact, ensure_ascii, keys in order.
func plPyJSON(v any) string {
	var b strings.Builder
	var w func(v any)
	w = func(v any) {
		switch x := v.(type) {
		case nil:
			b.WriteString("null")
		case bool:
			if x {
				b.WriteString("true")
			} else {
				b.WriteString("false")
			}
		case int64:
			b.WriteString(strconv.FormatInt(x, 10))
		case float64:
			switch {
			case math.IsNaN(x):
				b.WriteString("NaN")
			case math.IsInf(x, 1):
				b.WriteString("Infinity")
			case math.IsInf(x, -1):
				b.WriteString("-Infinity")
			default:
				b.WriteString(pyrepr.Float(x))
			}
		case string:
			b.WriteString(plPyJSONString(x))
		case []any:
			b.WriteByte('[')
			for i, e := range x {
				if i > 0 {
					b.WriteByte(',')
				}
				w(e)
			}
			b.WriteByte(']')
		case *definitions.OMap:
			b.WriteByte('{')
			for i, k := range x.Keys {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(plPyJSONString(k))
				b.WriteByte(':')
				w(x.M[k])
			}
			b.WriteByte('}')
		default:
			b.WriteString(plPyJSONString(fmt.Sprint(x)))
		}
	}
	w(v)
	return b.String()
}

func plPyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20 || (r >= 0x7f && r < 0x10000):
			if r == 0x7f {
				b.WriteByte(0x7f)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		case r >= 0x10000:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// -- XRs --------------------------------------------------------------------------------------------------------------------------

// XRAnnotations is xr_annotations: what the Backstage plugin reads to find an XR request's file.
func XRAnnotations(app, filename string, reg Registry) *definitions.OMap {
	owner := reg.Owner
	src := "{" + `"pushToGit":true,"gitBranch":"main","gitRepo":` + plPyJSONString("github.com?owner="+owner+"&repo="+reg.TenantsRepo) +
		`,"gitLayout":"custom","basePath":""}`
	return plMk("terasky.backstage.io/source-info", src,
		"terasky.backstage.io/add-to-catalog", "true",
		"terasky.backstage.io/owner", "group:default/"+owner,
		"terasky.backstage.io/system", "app-"+app+"-cicd",
		"terasky.backstage.io/source-file-url", "https://github.com/"+owner+"/"+reg.TenantsRepo+"/blob/main/tenants/"+app+"/xr-requests/"+filename)
}

func appXR(spec *definitions.OMap, reg Registry) (string, *definitions.OMap) {
	app, stack := plText(spec.M["app"]), plText(spec.M["stack"])
	kind := stackKind[stack]
	opts := plGetO(spec, "stackOptions")
	get := func(k string, d any) any {
		if v, ok := opts.M[k]; ok {
			return v
		}
		return d
	}
	desc := any(app)
	if v, ok := spec.M["description"]; ok {
		desc = v
	}
	body := plMk("devCluster", reg.DevCluster, "visibility", get("visibility", "private"), "description", desc)
	if stack == "python" {
		body.Set("pythonVersion", get("pythonVersion", "3.12"))
		body.Set("packageManager", get("packageManager", "pip"))
		body.Set("port", get("port", int64(8080)))
	} else {
		body.Set("port", get("port", int64(8080)))
	}
	fn := app + ".yaml"
	return fn, plMk("apiVersion", "catalog.hangar.io/v1alpha1", "kind", kind,
		"metadata", plMk("annotations", XRAnnotations(app, fn, reg), "name", app, "namespace", "app-"+app+"-cicd"), "spec", body)
}

func envXR(app, cluster, env string, reg Registry) (string, *definitions.OMap) {
	name := app + "-" + cluster + "-" + env
	fn := name + ".yaml"
	return fn, plMk("apiVersion", "catalog.hangar.io/v1alpha1", "kind", "ApplicationEnvironment",
		"metadata", plMk("annotations", XRAnnotations(app, fn, reg), "name", name, "namespace", "app-"+app+"-cicd"),
		"spec", plMk("appName", app, "cluster", cluster, "configMapGenerator", false, "env", env))
}

type flightEnv struct{ Name, Cluster string }

func resolveFlight(spec *definitions.OMap, reg Registry) ([]flightEnv, []string, error) {
	var assumptions []string
	var out []flightEnv
	flight, _ := plGetO(spec, "environments").M["flight"].([]any)
	for _, x := range flight {
		e := plGetO(&definitions.OMap{M: map[string]any{"e": x}, Keys: []string{"e"}}, "e")
		name := plText(e.M["name"])
		cluster, has := e.M["cluster"]
		var c string
		if !has || cluster == nil {
			switch len(reg.UpperClusters) {
			case 1:
				c = reg.UpperClusters[0]
				assumptions = append(assumptions, fmt.Sprintf("flight environment '%s' has no cluster; the only upper cluster is %s, so it goes there", name, c))
			case 0:
				return nil, nil, &SpecError{"no upper cluster is registered; a flight environment cannot be created"}
			default:
				return nil, nil, &NeedsInput{fmt.Sprintf("Which upper cluster should flight environment '%s' use?", name), append([]string{}, reg.UpperClusters...)}
			}
		} else {
			c = plText(cluster)
		}
		found := false
		for _, u := range reg.UpperClusters {
			found = found || u == c
		}
		if !found {
			return nil, nil, &SpecError{fmt.Sprintf("%s is not a registered upper cluster (%s)", pyrepr.Str(c), strings.Join(reg.UpperClusters, ", "))}
		}
		out = append(out, flightEnv{name, c})
	}
	return out, assumptions, nil
}

// after is _after: the index just past the last element matching pred (0 when none does).
func plAfter(l []any, pred func(any) bool) int {
	at := 0
	for i, e := range l {
		if pred(e) {
			at = i + 1
		}
	}
	return at
}

func plInsertAt(l []any, i int, v any) []any {
	l = append(l, nil)
	copy(l[i+1:], l[i:])
	l[i] = v
	return l
}

func plIsDict(v any) (*definitions.OMap, bool) { return plAsMap(v) }

func plHasName(envs []any) map[string]bool {
	names := map[string]bool{}
	for _, e := range envs {
		if m, ok := plIsDict(e); ok {
			if n, ok := m.M["name"].(string); ok {
				names[n] = true
			}
		}
	}
	return names
}

// mapOf is the mapping a setdefault returned, or a SpecError naming what it was in the cicd.yaml instead (Python raises there).
func plMapOf(v any, what string) (*definitions.OMap, error) {
	if o, ok := v.(*definitions.OMap); ok {
		return o, nil
	}
	return nil, &SpecError{"cicd.yaml: " + what + " is not a mapping"}
}

func plListOf(v any, what string) ([]any, error) {
	if l, ok := v.([]any); ok {
		return l, nil
	}
	return nil, &SpecError{"cicd.yaml: " + what + " is not a list"}
}

// groundCICD is the part of the environment declaration an agent may write: Ground environments and their deploy steps, added
// to what the file already has, nothing removed or rewritten.
func groundCICD(base *definitions.OMap, ground []string) (*definitions.OMap, error) {
	out := plCopyV(base).(*definitions.OMap)
	dep, err := plMapOf(plSetdefault(out, "deploy", plNewO()), "deploy")
	if err != nil {
		return nil, err
	}
	envs, err := plListOf(plSetdefault(dep, "environments", []any{plMk("name", "dev", "tier", "ground")}), "deploy.environments")
	if err != nil {
		return nil, err
	}
	names := plHasName(envs)
	for _, g := range ground {
		if !names[g] {
			envs = plInsertAt(envs, plAfter(envs, func(e any) bool { m, ok := plIsDict(e); return ok && m.M["tier"] == "ground" }), plMk("name", g, "tier", "ground"))
			names[g] = true
		}
	}
	dep.Set("environments", envs)
	pipes, err := plMapOf(plSetdefault(out, "pipelines", plNewO()), "pipelines")
	if err != nil {
		return nil, err
	}
	ci, err := plMapOf(plSetdefault(pipes, "ci", plMk("trigger", plMk("source", "git", "event", "push", "branch", "main"))), "pipelines.ci")
	if err != nil {
		return nil, err
	}
	steps, err := plListOf(plSetdefault(ci, "steps", []any{}), "pipelines.ci.steps")
	if err != nil {
		return nil, err
	}
	stage := func(s any, want string) bool { m, ok := plIsDict(s); return ok && m.M["stage"] == want }
	any1 := false
	for _, s := range steps {
		any1 = any1 || stage(s, "build")
	}
	if !any1 {
		steps = plInsertAt(steps, 0, plMk("stage", "build"))
	}
	for _, g := range ground {
		have := false
		for _, s := range steps {
			m, ok := plIsDict(s)
			have = have || (ok && m.M["stage"] == "deploy" && m.M["env"] == g)
		}
		if !have {
			at := plAfter(steps, func(s any) bool { return stage(s, "build") || stage(s, "deploy") })
			steps = plInsertAt(steps, at, plMk("stage", "deploy", "env", g))
		}
	}
	ci.Set("steps", steps)
	return out, nil
}

// flightCICD is what only a human may write: each Flight environment, the release step into the first one, and dropping the
// pre-ADR-0019 deploy keys.
func flightCICD(doc *definitions.OMap, flight []flightEnv) (*definitions.OMap, error) {
	out := plCopyV(doc).(*definitions.OMap)
	dep, err := plMapOf(plSetdefault(out, "deploy", plNewO()), "deploy")
	if err != nil {
		return nil, err
	}
	for _, k := range legacyDeployKeys {
		delete(dep.M, k)
		for i, kk := range dep.Keys {
			if kk == k {
				dep.Keys = append(dep.Keys[:i], dep.Keys[i+1:]...)
				break
			}
		}
	}
	envs, err := plListOf(plSetdefault(dep, "environments", []any{}), "deploy.environments")
	if err != nil {
		return nil, err
	}
	have := plHasName(envs)
	for _, e := range flight {
		if !have[e.Name] {
			envs = append(envs, plMk("name", e.Name, "tier", "flight", "cluster", e.Cluster))
		}
	}
	dep.Set("environments", envs)
	if len(flight) > 0 {
		pipes, err := plMapOf(plSetdefault(out, "pipelines", plNewO()), "pipelines")
		if err != nil {
			return nil, err
		}
		ci, err := plMapOf(plSetdefault(pipes, "ci", plNewO()), "pipelines.ci")
		if err != nil {
			return nil, err
		}
		steps, err := plListOf(plSetdefault(ci, "steps", []any{}), "pipelines.ci.steps")
		if err != nil {
			return nil, err
		}
		rel := false
		for _, s := range steps {
			m, ok := plIsDict(s)
			rel = rel || (ok && m.M["stage"] == "release")
		}
		if !rel {
			steps = append(steps, plMk("stage", "release", "env", flight[0].Name))
		}
		ci.Set("steps", steps)
	}
	return out, nil
}

// cicdPatch is a merge patch carrying deploy and pipelines whole (lists replace in RFC 7386, so they are sent entire); nulls
// remove the legacy keys legacyFrom still has.
func cicdPatch(doc *definitions.OMap, legacyFrom *definitions.OMap) *definitions.OMap {
	dep := plNewO()
	if d, ok := doc.M["deploy"].(*definitions.OMap); ok {
		for _, k := range d.Keys {
			dep.Set(k, d.M[k])
		}
	}
	for _, k := range legacyDeployKeys {
		if _, has := plGetO(legacyFrom, "deploy").M[k]; has {
			dep.Set(k, nil)
		}
	}
	var pipes any = plNewO()
	if plTruthy(doc.M["pipelines"]) {
		pipes = doc.M["pipelines"]
	}
	return plMk("deploy", dep, "pipelines", pipes)
}

func plSortedCopy(l []string) []string {
	out := append([]string{}, l...)
	sort.Strings(out)
	return out
}

// Plan is plan(spec, reg) with today's Features.
func Plan(spec any, reg Registry, baseCICD any) (*ChangeSet, error) {
	return PlanWith(spec, reg, Features{RolloutGuard: true}, baseCICD)
}

// PlanWith is plan(spec, reg, features, base_cicd). baseCICD is nil (none given) or a mapping.
func PlanWith(specIn any, reg Registry, features Features, baseCICD any) (*ChangeSet, error) {
	sch := plSchema()
	if best := sch.Best(specIn); best != nil {
		parts := make([]string, 0, len(best.Path))
		for _, p := range best.Path {
			parts = append(parts, fmt.Sprint(p))
		}
		path := strings.Join(parts, "/")
		if path == "" {
			path = "<root>"
		}
		return nil, &SpecError{path + ": " + best.Message}
	}
	spec := plCopyV(specIn).(*definitions.OMap)
	var base *definitions.OMap
	if baseCICD != nil {
		b, ok := baseCICD.(*definitions.OMap)
		if !ok {
			return nil, &SpecError{"baseCicd is not a mapping"}
		}
		base = plCopyV(b).(*definitions.OMap)
	}
	app, owner := plText(spec.M["app"]), reg.Owner
	envsM := plGetO(spec, "environments")
	var ground []string
	for _, g := range envsM.M["ground"].([]any) {
		ground = append(ground, plText(g))
	}
	flight, assumptions, err := resolveFlight(spec, reg)
	if err != nil {
		return nil, err
	}
	warnings := []string{}
	allEnvs := append([]string{}, ground...)
	for _, f := range flight {
		allEnvs = append(allEnvs, f.Name)
	}
	seen := map[string]bool{}
	for _, e := range allEnvs {
		if seen[e] {
			return nil, &SpecError{"an environment name is used twice across ground and flight"}
		}
		seen[e] = true
	}
	if ov, ok := spec.M["overrides"].(*definitions.OMap); ok {
		for _, k := range ov.Keys {
			if !seen[k] {
				return nil, &SpecError{fmt.Sprintf("overrides/%s: no environment named %s (%s)", k, pyrepr.Str(k), strings.Join(allEnvs, ", "))}
			}
		}
	}

	port, hasPort := plGetO(spec, "stackOptions").M["port"]
	if hasPort && port != nil {
		cfg, err := plMapOf(plSetdefault(spec, "config", plNewO()), "config")
		if err != nil {
			return nil, err
		}
		rollout := plGetO(cfg, "rollout")
		if _, has := rollout.M["ports"]; !has {
			nr := plCopyV(rollout).(*definitions.OMap)
			nr.Set("ports", []any{plMk("name", "http", "containerPort", port)})
			cfg.Set("rollout", nr)
			if !plPyEq(port, int64(chartDefaultPort)) {
				assumptions = append(assumptions, fmt.Sprintf("stackOptions.port %s: the chart's rollout.ports is set to http:%s (its default is %d), so the Service reaches the scaffolded server", plText(port), plText(port), chartDefaultPort))
			}
		}
	}

	hostOrder := []string{}
	hosts := map[string][]string{}
	for _, e := range allEnvs {
		c := plEnvConfig(spec, e)
		errs := sch.ErrorsAt("#/$defs/chartValues", c)
		if len(errs) > 0 {
			definitions.SortByPath(errs)
			parts := make([]string, 0, len(errs[0].Path))
			for _, p := range errs[0].Path {
				parts = append(parts, fmt.Sprint(p))
			}
			path := strings.Join(parts, "/")
			if path == "" {
				path = "<root>"
			}
			return nil, &SpecError{fmt.Sprintf("%s (config merged with overrides.%s): %s: %s", e, e, path, errs[0].Message)}
		}
		if hasPort && port != nil {
			ok := false
			if ps, isL := plGetO(c, "rollout").M["ports"].([]any); isL {
				for _, p := range ps {
					pm := plGetO(&definitions.OMap{M: map[string]any{"p": p}, Keys: []string{"p"}}, "p")
					ok = ok || plPyEq(pm.M["containerPort"], port)
				}
			}
			if !ok {
				return nil, &SpecError{fmt.Sprintf("%s: rollout.ports does not include stackOptions.port %s; the Service would miss the scaffolded server", e, plText(port))}
			}
		}
		for _, h := range routeHosts(c) {
			if _, has := hosts[h]; !has {
				hostOrder = append(hostOrder, h)
			}
			hosts[h] = append(hosts[h], e)
		}
	}
	for _, h := range hostOrder {
		if envs := hosts[h]; len(envs) > 1 {
			return nil, &NeedsInput{fmt.Sprintf("Hostname %s would be served by %s; a hostname belongs to one environment. Which one is it for? (Put it under overrides.<env>.)", h, strings.Join(envs, ", ")), envs}
		}
	}

	for _, e := range allEnvs {
		if pipelineStages[e] {
			warnings = append(warnings, fmt.Sprintf("environment '%s' has the same name as a pipeline stage; cicd.yaml will read `stage: %s, env: %s`. Legal, but easy to misread.", e, e, e))
		}
	}
	cfg := plGetO(spec, "config")
	if sc, ok := plGetO(cfg, "rollout").M["steps"].([]any); ok && len(sc) == 1 {
		if m, ok := plIsDict(sc[0]); ok && plPyEq(m.M["setWeight"], int64(100)) {
			warnings = append(warnings, "a single 100% step promotes immediately: it is a valid canary but has no gradual ramp")
		}
	}
	type floorKey struct{ replicas, floor string }
	var floorOrder []floorKey
	floors := map[floorKey][]string{}
	for _, e := range allEnvs {
		c := plEnvConfig(spec, e)
		replicas, hasRep := plGetO(c, "rollout").M["replicas"]
		a := plGetO(c, "autoscaling")
		if plTruthy(a.M["enabled"]) && hasRep && replicas != nil {
			mn := any(int64(2))
			if v, ok := a.M["min"]; ok {
				mn = v
			}
			x, ok1 := plNumOf(mn)
			y, ok2 := plNumOf(replicas)
			if ok1 && ok2 && x < y {
				k := floorKey{plText(replicas), plText(mn)}
				if _, has := floors[k]; !has {
					floorOrder = append(floorOrder, k)
				}
				floors[k] = append(floors[k], e)
			}
		}
	}
	for _, k := range floorOrder {
		warnings = append(warnings, fmt.Sprintf("%s: autoscaling is on and rollout.replicas is %s, but the HPA owns the count and may scale down to autoscaling.min %s; set autoscaling.min: %s if %s is a floor",
			strings.Join(floors[k], ", "), k.replicas, k.floor, k.replicas, k.replicas))
	}
	critSet := map[string]bool{}
	for _, e := range allEnvs {
		for _, k := range plEnvConfig(spec, e).Keys {
			if plRisk(k) == "critical" {
				critSet[k] = true
			}
		}
	}
	var critical []string
	for k := range critSet {
		critical = append(critical, k)
	}
	sort.Strings(critical)
	if len(critical) > 0 {
		warnings = append(warnings, strings.Join(critical, ", ")+" is critical-risk: the app-repo step needs a human merge")
	}

	var cs []*Step
	// 1. tenants repo: the app XR and every flight environment, in one PR
	var files []FileChange
	fn, xr := appXR(spec, reg)
	files = append(files, FileChange{Path: "tenants/" + app + "/xr-requests/" + fn, Op: "create", Content: xr})
	for _, e := range flight {
		fn2, xr2 := envXR(app, e.Cluster, e.Name, reg)
		files = append(files, FileChange{Path: "tenants/" + app + "/xr-requests/" + fn2, Op: "create", Content: xr2})
	}
	kind := stackKind[plText(spec.M["stack"])]
	effects := []string{fmt.Sprintf("creates the source repo %s/%s and the deploy repo %s/gitops-%s", owner, app, owner, app),
		"onboards the CI/CD pipeline", "commits per-app SecretStore XRs (the compositions do this themselves)"}
	for _, e := range flight {
		effects = append(effects, fmt.Sprintf("creates flight environment %s on %s", e.Name, e.Cluster))
	}
	cs = append(cs, &Step{ID: "tenants", Title: "Create " + app + " and its flight environments", Repo: reg.TenantsRepo, Gate: "human-merge", Risk: "T2", Files: files, Effects: effects})
	wf := fmt.Sprintf("%s/%s CicdOnboarded=True and repos %s, gitops-%s exist", kind, app, app, app)
	cs = append(cs, &Step{ID: "repos-ready", Title: "Wait for the repos and onboarding", Repo: reg.TenantsRepo, Gate: "wait", Risk: "T0", DependsOn: []string{"tenants"}, WaitFor: &wf})

	// 2. app repo: cicd.yaml + ground environments
	groundFile := func(env string) *definitions.OMap {
		c := plEnvConfig(spec, env)
		d := plMk("envName", env)
		src := c
		if features.BaseLayer {
			src = plGetO(plGetO(spec, "overrides"), env)
		}
		for _, k := range src.Keys {
			d.Set(k, src.M[k])
		}
		return d
	}
	baseDoc := base
	if baseDoc == nil {
		baseDoc = plNewO()
		assumptions = append(assumptions, "no current cicd.yaml was given: the cicd.yaml patches assume the scaffold's (dev only); read the app's cicd.yaml and plan again before opening the pull request")
	}
	var legacy []string
	for _, k := range legacyDeployKeys {
		if _, has := plGetO(baseDoc, "deploy").M[k]; has {
			legacy = append(legacy, k)
		}
	}
	var groundDoc *definitions.OMap
	if len(legacy) > 0 {
		groundDoc = baseDoc
	} else if groundDoc, err = groundCICD(baseDoc, ground); err != nil {
		return nil, err
	}
	gd, err := groundCICD(baseDoc, ground)
	if err != nil {
		return nil, err
	}
	finalDoc, err := flightCICD(gd, flight)
	if err != nil {
		return nil, err
	}
	if len(legacy) > 0 {
		warnings = append(warnings, fmt.Sprintf("cicd.yaml still carries %s (pre-ADR-0019): removing them is a human change, so the whole cicd.yaml change is in the human-merge cicd-flight step", strings.Join(legacy, ", ")))
	}
	var appFiles []FileChange
	if !plPyEq(groundDoc, baseDoc) {
		appFiles = append(appFiles, FileChange{Path: "cicd.yaml", Op: "merge-patch", Content: cicdPatch(groundDoc, nil), Before: plCopyV(baseDoc)})
	}
	if features.BaseLayer {
		appFiles = append(appFiles, FileChange{Path: baseFile, Op: "create", Content: plCopyV(cfg)})
	}
	for _, g := range ground {
		appFiles = append(appFiles, FileChange{Path: envsDir + "/" + g + ".yaml", Op: "create", Content: groundFile(g)})
	}
	gate, rk := "auto-merge", "T1"
	if len(critical) > 0 {
		gate, rk = "human-merge", "T2"
	}
	var appEffects []string
	for _, g := range ground {
		appEffects = append(appEffects, fmt.Sprintf("creates namespace app-%s-%s for each ground environment", app, g))
	}
	appEffects = append(appEffects, "changing cicd.yaml regenerates .tekton/ in a follow-up PR")
	cicdCheck := "cicd.yaml validates against glidepath's cicd.schema.json"
	cs = append(cs, &Step{ID: "app-repo", Title: "Declare ground environments " + strings.Join(ground, ", "), Repo: owner + "/" + app, Gate: gate, Risk: rk,
		DependsOn: []string{"repos-ready"}, Files: appFiles, Effects: appEffects, Checks: []string{cicdCheck}})
	if !plPyEq(finalDoc, groundDoc) {
		title := "Retire the pre-ADR-0019 deploy keys"
		var fx []string
		for _, e := range flight {
			fx = append(fx, fmt.Sprintf("declares flight environment %s on %s", e.Name, e.Cluster))
		}
		if len(flight) > 0 {
			title = "Declare the flight environments in cicd.yaml"
			fx = append(fx, "adds the release step into "+flight[0].Name)
		}
		fx = append(fx, "Flight environments and their clusters are human-only cicd.yaml fields (GP-SCOPE-003, GP-SCOPE-001)")
		cs = append(cs, &Step{ID: "cicd-flight", Title: title, Repo: owner + "/" + app, Gate: "human-merge", Risk: "T2", DependsOn: []string{"tenants", "app-repo"},
			Files:   []FileChange{{Path: "cicd.yaml", Op: "merge-patch", Content: cicdPatch(finalDoc, groundDoc), Before: plCopyV(groundDoc)}},
			Effects: fx, Checks: []string{cicdCheck}})
	}
	rw := "the resync PR opened by the platform"
	cs = append(cs, &Step{ID: "tekton-resync", Title: "Merge the regenerated .tekton/ PR", Repo: owner + "/" + app, Gate: "auto-merge", Risk: "T1", DependsOn: []string{"app-repo"}, WaitFor: &rw})

	// 3. gitops repo: flight environment config
	if len(flight) > 0 {
		var gfiles []FileChange
		if features.BaseLayer {
			gfiles = append(gfiles, FileChange{Path: "base/values.yaml", Op: "create", Content: plCopyV(cfg)})
		}
		for _, e := range flight {
			var patch any
			if features.BaseLayer {
				patch = plGetO(spec, "overrides").M[e.Name]
			} else {
				patch = plEnvConfig(spec, e.Name)
			}
			if plTruthy(patch) {
				gfiles = append(gfiles, FileChange{Path: e.Cluster + "/" + e.Name + "/values.yaml", Op: "merge-patch", Content: plCopyV(patch)})
			}
		}
		if len(gfiles) > 0 {
			names := make([]string, len(flight))
			for i, e := range flight {
				names[i] = e.Name
			}
			g := "each environment's values.yaml exists (the composition bootstraps it)"
			cs = append(cs, &Step{ID: "gitops", Title: "Configure the flight environments", Repo: owner + "/gitops-" + app, Gate: "human-merge", Risk: "T2",
				DependsOn: []string{"tenants", "repos-ready"}, Files: gfiles, WaitFor: &g,
				Effects: []string{"changes what runs in " + strings.Join(names, ", ") + " after the next release"}})
		}
	}

	// 4. verify
	checks := []string{fmt.Sprintf("%s/%s Ready and CicdOnboarded", kind, app)}
	for _, e := range flight {
		checks = append(checks, fmt.Sprintf("ApplicationEnvironment %s-%s-%s Ready", app, e.Cluster, e.Name))
	}
	for _, g := range ground {
		checks = append(checks, fmt.Sprintf("namespace app-%s-%s exists and its Application is Synced", app, g))
	}
	for _, env := range allEnvs {
		checks = append(checks, envChecks(env, plEnvConfig(spec, env))...)
	}
	deps := make([]string, len(cs))
	for i, s := range cs {
		deps[i] = s.ID
	}
	cs = append(cs, &Step{ID: "verify", Title: "Verify", Repo: "", Gate: "verify", Risk: "T0", DependsOn: deps, Checks: checks})
	return &ChangeSet{App: app, Steps: cs, Assumptions: assumptions, Warnings: warnings}, nil
}
