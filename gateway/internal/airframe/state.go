package airframe

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// airframe.status, airframe.describe and airframe.verify (airframe_tools.py): what an app's Airframe XRs, its Argo CD
// Applications and its Rollouts say, read through a Reader that can only GET what the closed list allows (backends.KubeReader).
//
// The arguments are an agent's: every namespace and name is built from them the way Python builds it and percent-escaped, and the
// Reader refuses a path that is not a valid DNS name or not on the list (AF-K8S-PATH) before any request exists. What the cluster
// answers is data (a tenant writes an XR's spec, an Application's resources): it is read without a panic whatever its shape.

// Reader is the Kubernetes reads the state tools make. backends.KubeReader is the real one.
type Reader interface {
	// Get is the object at path; found is false for a 404.
	Get(ctx context.Context, path string) (obj any, found bool, err error)
	// List is every item of the list at path (labelSelector may be empty), following continue (bounded).
	List(ctx context.Context, path, labelSelector string) ([]map[string]any, error)
}

// State is the cluster-state tools.
type State struct {
	Kube Reader
	// Cluster is the cluster this gateway reads (a Flight environment on another one is named, not read).
	Cluster string
	// ArgoNamespace is where Ground environments' Applications are (Python: argocd-apps).
	ArgoNamespace string
	// Workers bounds the parallel list reads (Python: 8).
	Workers int
}

const (
	// ArgoNamespace is glidepath-app templates/argocd/lower-envs.yaml's.
	ArgoNamespace = "argocd-apps"
	// maxListItems bounds one kind's list in an answer.
	maxListItems = 1000
)

// types is glidepath-app.envNamespace: <type>-<app>-<env>.
var stTypes = []string{"app", "infra"}

var stLabelValue = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)

func stAnswer(rule, message, hint string) map[string]any {
	return map[string]any{"error": map[string]any{"rule": rule, "message": message, "hint": hint}}
}

func stClip(v any, n int) any {
	if s, ok := v.(string); ok {
		if r := []rune(s); len(r) > n {
			return string(r[:n])
		}
	}
	return v
}

// -- reading ------------------------------------------------------------------------------------------------------------

func (s *State) get(ctx context.Context, path string) (map[string]any, error) {
	o, found, err := s.Kube.Get(ctx, path)
	if err != nil || !found {
		return nil, err
	}
	m, _ := stNorm(o).(map[string]any)
	return m, nil // an answer that is not an object is no object
}

func (s *State) list(ctx context.Context, path, selector string) ([]map[string]any, error) {
	items, err := s.Kube.List(ctx, path, selector)
	if err != nil {
		return nil, err
	}
	if len(items) > maxListItems {
		items = items[:maxListItems]
	}
	out := make([]map[string]any, len(items))
	for i, it := range items {
		out[i], _ = stNorm(it).(map[string]any)
	}
	return out, nil
}

// xrPlurals is the sorted set of plurals of the contract's catalog.hangar.io kinds.
func xrPluralList() []string {
	set := map[string]bool{}
	xrds := omapOf(Contract().M["xrds"])
	for _, k := range xrds.Keys {
		x := omapOf(xrds.M[k])
		if strOf(x.M["group"]) == "catalog.hangar.io" {
			if pl := strOf(x.M["plural"]); pl != "" {
				set[pl] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

var pluralList = xrPluralList()

func stXrPath(plural, namespace, name string) string {
	p := "/apis/catalog.hangar.io/v1alpha1/namespaces/" + stEsc(namespace) + "/" + stEsc(plural)
	if name != "" {
		p += "/" + stEsc(name)
	}
	return p
}

func stNamespacePath(namespace string) string { return "/api/v1/namespaces/" + stEsc(namespace) }

// owned says a namespace is the app's: the Namespace object carries the platform's own label hangar.io/app, exactly the app. Nothing is
// listed or read in a namespace before this says yes; a Namespace that is missing, unlabelled or labelled for another app is not the
// app's, and an answer that is not either (the API server failing) is an error, never a yes.
func (s *State) owned(ctx context.Context, ns, app string) (bool, error) {
	o, err := s.get(ctx, stNamespacePath(ns))
	if err != nil || o == nil {
		return false, err
	}
	l, _ := stAsMap(stAsMap(o["metadata"])["labels"])["hangar.io/app"].(string)
	return l != "" && l == app, nil
}

// AppNamespaces are the exact namespaces the platform assigned to one of the app's environments on this cluster (an Application the
// platform labelled for it is destined for them, and each carries the app's own label): what an SLO's matchers are forced into.
func (s *State) AppNamespaces(ctx context.Context, app, env string) ([]string, error) {
	var out []string
	for _, t := range stTypes {
		pl, err := s.place(ctx, app, env, t, false)
		if err != nil {
			return nil, err
		}
		if pl.ns != "" {
			out = append(out, pl.ns)
		}
	}
	return out, nil
}

func stArgoPath(resource, namespace, name string) string {
	p := "/apis/argoproj.io/v1alpha1/namespaces/" + stEsc(namespace) + "/" + stEsc(resource)
	if name != "" {
		p += "/" + stEsc(name)
	}
	return p
}

// esc is urllib.parse.quote(s, safe=""): everything but letters, digits and _.-~ is percent-escaped.
func stEsc(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (s *State) workers() int {
	switch {
	case s.Workers < 1:
		return 8
	case s.Workers > 16:
		return 16
	}
	return s.Workers
}

// xrsIn is every XR of every kind in a namespace, sorted by (kind, name): one list read per kind, in parallel. Every list is
// read before an error is returned, as Python's map does; the first failing kind (in plural order) is the error.
func (s *State) xrsIn(ctx context.Context, namespace string) ([]map[string]any, error) {
	lists := make([][]map[string]any, len(pluralList))
	errs := make([]error, len(pluralList))
	sem := make(chan struct{}, s.workers())
	var wg sync.WaitGroup
	for i, pl := range pluralList {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					errs[i] = fmt.Errorf("reading %s failed", pl)
				}
			}()
			lists[i], errs[i] = s.list(ctx, stXrPath(pl, namespace, ""), "")
		}()
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}
	var out []map[string]any
	for _, l := range lists {
		for _, o := range l {
			if o != nil {
				out = append(out, o)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ki, kj := stPyStr(out[i]["kind"]), stPyStr(out[j]["kind"])
		if out[i]["kind"] == nil {
			ki = ""
		}
		if out[j]["kind"] == nil {
			kj = ""
		}
		if ki != kj {
			return ki < kj
		}
		return stMdName(out[i]) < stMdName(out[j])
	})
	return out, nil
}

func stMdName(o map[string]any) string {
	n, _ := stAsMap(o["metadata"])["name"].(string)
	return n
}

func (s *State) cicd(ctx context.Context, app string) (t, ns string, xrs []map[string]any, err error) {
	for _, t = range stTypes {
		ns = t + "-" + app + "-cicd"
		if ok, oerr := s.owned(ctx, ns, app); oerr != nil || !ok {
			if oerr != nil {
				return "", "", nil, oerr
			}
			continue
		}
		if xrs, err = s.xrsIn(ctx, ns); err != nil {
			return "", "", nil, err
		}
		if len(xrs) > 0 {
			return t, ns, xrs, nil
		}
	}
	return "", "", nil, nil
}

func stNoApp(app string) any {
	return stAnswer("AF-APP-404", fmt.Sprintf("no Airframe XRs for %s in app-%s-cicd or infra-%s-cicd", stPyRepr(app), app, app),
		"Check the app name; an app exists once its tenants-repo XR request is synced.")
}

// envXRs is _env_xrs: the cicd XRs (with only this environment's ApplicationEnvironments when env is named), the environment's
// own XRs, and the ApplicationEnvironments that are this environment (a Flight environment).
func (s *State) envXRs(ctx context.Context, app string, env *string) (t, cicdNS string, cicd, envXRs, flight []map[string]any, pl *envPlace, found bool, err error) {
	t, cicdNS, all, err := s.cicd(ctx, app)
	if err != nil || all == nil {
		return "", "", nil, nil, nil, nil, false, err
	}
	named := env != nil && *env != ""
	for _, x := range all {
		isAppEnv := x["kind"] == "ApplicationEnvironment"
		isEnv := isAppEnv && env != nil && stPyEq(stAsMap(x["spec"])["env"], *env) && stPyEq(stAsMap(x["spec"])["appName"], app)
		if isEnv {
			flight = append(flight, x)
		}
		if !isAppEnv || env == nil || isEnv {
			cicd = append(cicd, x)
		}
	}
	if named {
		// The environment is one the app has: an ApplicationEnvironment of its own, or an Application the platform labelled for it.
		// The namespace read is the one that Application's destination names, which must be the one the platform assigns to
		// (app, env): never a name joined from the argument, which another app's namespace can equal (gate + api-prod is gate-api + prod).
		if pl, err = s.place(ctx, app, *env, t, len(flight) > 0); err != nil {
			return "", "", nil, nil, nil, nil, false, err
		}
		if pl.known && pl.ns != "" {
			if envXRs, err = s.xrsIn(ctx, pl.ns); err != nil {
				return "", "", nil, nil, nil, nil, false, err
			}
		}
	}
	return t, cicdNS, cicd, envXRs, flight, pl, true, nil
}

var stEnvLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// envPlace is where the platform put one of an app's environments on this cluster.
type envPlace struct {
	known bool             // the app has this environment (a flight ApplicationEnvironment, or a labelled Application)
	ns    string           // its namespace, when an Application of it says so and it is the one the platform assigns; else ""
	apps  []map[string]any // the Applications that carry both the app's and the environment's label
}

// place decides whether env is one of the app's environments and which namespace is its own. Every Application it keeps carries
// hangar.io/app == app and hangar.io/env == env (the platform's labels, which a tenant's manifests do not set on an Application).
func (s *State) place(ctx context.Context, app, env, t string, flight bool) (*envPlace, error) {
	p := &envPlace{}
	if !stEnvLabel.MatchString(env) || !stEnvLabel.MatchString(app) {
		return p, nil
	}
	apps, err := s.applications(ctx, app, env, t)
	if err != nil {
		return nil, err
	}
	for _, a := range apps {
		labels := stAsMap(stAsMap(a["metadata"])["labels"])
		if labels["hangar.io/app"] != app || labels["hangar.io/env"] != env {
			continue
		}
		p.apps = append(p.apps, a)
		if dest, _ := stAsMap(stAsMap(a["spec"])["destination"])["namespace"].(string); dest == t+"-"+app+"-"+env && p.ns == "" {
			ok, oerr := s.owned(ctx, dest, app)
			if oerr != nil {
				return nil, oerr
			}
			if ok {
				p.ns = dest
			}
		}
	}
	p.known = flight || len(p.apps) > 0
	return p, nil
}

func stUnknownEnv(app, env string) any {
	return stAnswer("AF-ENV-404", fmt.Sprintf("%s has no environment %s on this cluster", app, stPyRepr(env)),
		"airframe.status {app} lists the app's environments; an environment is one the platform registered for the app.")
}

// -- normalising XR state -----------------------------------------------------------------------------------------------

func conditionsOf(xr map[string]any) []map[string]any {
	var out []map[string]any
	for _, c := range stAsList(stAsMap(xr["status"])["conditions"]) {
		if m := stAsMap(c); m != nil {
			out = append(out, m)
		}
	}
	return out
}

func reasonMeaning(sc *definitions.OMap, ctype any, reason any) any {
	if sc == nil {
		return nil
	}
	for _, c := range stAsList(sc.M["conditions"]) {
		co := omapOf(c)
		if !stPyEq(co.M["type"], ctype) {
			continue
		}
		for _, r := range stAsList(co.M["reasons"]) {
			ro := omapOf(r)
			if stPyEq(ro.M["reason"], reason) {
				return ro.M["meaning"]
			}
		}
	}
	return nil
}

type failure struct{ id, condition, match, meaning, hint string }

var rxCache sync.Map

func searchRE(pattern, text string) bool {
	v, ok := rxCache.Load(pattern)
	if !ok {
		re, err := regexp.Compile(pattern)
		if err != nil {
			re = nil
		}
		v, _ = rxCache.LoadOrStore(pattern, re)
	}
	re, _ := v.(*regexp.Regexp)
	return re != nil && re.MatchString(text)
}

func knownFailure(sc *definitions.OMap, ctype any, message any) *failure {
	msg, _ := message.(string)
	var all []failure
	if sc != nil {
		for _, k := range stAsList(sc.M["knownFailures"]) {
			ko := omapOf(k)
			all = append(all, failure{strOf(ko.M["id"]), strOf(ko.M["condition"]), strOf(ko.M["match"]), strOf(ko.M["meaning"]), strOf(ko.M["hint"])})
		}
	}
	for _, g := range GenericFailures {
		all = append(all, failure{g.ID, g.Condition, g.Match, g.Meaning, g.Hint})
	}
	for i := range all {
		if ct, ok := ctype.(string); ok && all[i].condition == ct && searchRE(all[i].match, msg) {
			return &all[i]
		}
	}
	return nil
}

func kindSidecar(kind any) (*definitions.OMap, *definitions.OMap) {
	name, ok := kind.(string)
	if !ok {
		return nil, nil
	}
	return FindKind(name)
}

// Normalise is airframe_tools.normalise: one XR's conditions as the agent needs them.
func Normalise(xr map[string]any) map[string]any {
	md := stAsMap(xr["metadata"])
	sc, _ := kindSidecar(stOr(xr["kind"], ""))
	gen := md["generation"]
	conds := conditionsOf(xr)
	var syncedObs any
	for _, c := range conds {
		if c["type"] == "Synced" {
			if c["status"] == "True" {
				syncedObs = c["observedGeneration"]
			}
			break
		}
	}
	out := []map[string]any{}
	for _, c := range conds {
		ctype, status, reason := c["type"], c["status"], c["reason"]
		msg := stOr(c["message"], "")
		obs, source := c["observedGeneration"], "condition"
		if obs == nil && syncedObs != nil && ctype != "Synced" && ctype != "Ready" {
			obs, source = syncedObs, "Synced"
		}
		n := map[string]any{"type": ctype, "status": status, "reason": reason, "message": stClip(msg, 300),
			"observedGeneration": obs, "stale": stStale(obs, gen)}
		if source != "condition" {
			n["observedGenerationFrom"] = source
		}
		meaning := reasonMeaning(sc, ctype, reason)
		if stTruthy(meaning) {
			n["meaning"] = meaning
		}
		if status != "True" {
			if k := knownFailure(sc, ctype, msg); k != nil {
				n["knownFailure"], n["hint"] = k.id, k.meaning+" "+k.hint
			} else if stTruthy(meaning) {
				n["hint"] = meaning
			} else if ctype == "Ready" {
				n["hint"] = "A composed resource is not ready yet: see the kind's own conditions and this message."
			} else if ctype == "Synced" {
				n["hint"] = "Crossplane could not reconcile the XR with its current spec; the message says why."
			} else {
				n["hint"] = "Not a reason the Airframe contract lists for this kind; read the message."
			}
		}
		out = append(out, n)
	}
	var staleTypes, notTrue []any
	ready, synced := false, false
	for _, c := range out {
		if c["stale"] == true {
			staleTypes = append(staleTypes, c["type"])
		}
		if c["status"] != "True" {
			notTrue = append(notTrue, c["type"])
		}
		ready = ready || c["type"] == "Ready" && c["status"] == "True"
		synced = synced || c["type"] == "Synced" && c["status"] == "True"
	}
	cs := make([]any, len(out))
	for i, c := range out {
		cs[i] = c
	}
	if staleTypes == nil {
		staleTypes = []any{}
	}
	if notTrue == nil {
		notTrue = []any{}
	}
	return map[string]any{"kind": xr["kind"], "name": md["name"], "namespace": md["namespace"], "generation": gen,
		"ready": ready, "synced": synced, "stale": len(staleTypes) > 0, "staleConditions": staleTypes, "notTrue": notTrue, "conditions": cs}
}

// stale is `obs < gen` when both are numbers, else None.
func stStale(obs, gen any) any {
	o, ok1 := stNum(obs)
	g, ok2 := stNum(gen)
	if ok1 && ok2 {
		return o < g
	}
	return nil
}

func specSummary(xr map[string]any) map[string]any {
	spec := map[string]any{}
	for k, v := range stAsMap(xr["spec"]) {
		spec[k] = v
	}
	refs := stAsList(stAsMap(spec["crossplane"])["resourceRefs"])
	delete(spec, "crossplane")
	return map[string]any{"spec": spec, "composedResources": int64(len(refs))}
}

func xrRows(xrs []map[string]any) []any {
	out := make([]any, len(xrs))
	for i, x := range xrs {
		out[i] = Normalise(x)
	}
	return out
}

func stNames(xs []any, pick func(map[string]any) bool) []any {
	out := []any{}
	for _, x := range xs {
		m := x.(map[string]any)
		if pick(m) {
			out = append(out, stPyStr(m["kind"])+"/"+stPyStr(m["name"]))
		}
	}
	return out
}

func stCnt(xs []any, key string) int64 {
	var n int64
	for _, x := range xs {
		if x.(map[string]any)[key] == true {
			n++
		}
	}
	return n
}

func stNameOf(v any) string {
	if v == nil {
		return "None"
	}
	return stPyStr(v)
}

// Status is airframe.status: an app's (or one environment's) XR conditions.
func (s *State) Status(ctx context.Context, app string, env *string) (any, error) {
	t, cicdNS, cicd, envXRs, flight, pl, found, err := s.envXRs(ctx, app, env)
	if err != nil {
		return nil, err
	}
	if !found {
		return stNoApp(app), nil
	}
	if pl != nil && !pl.known {
		return stUnknownEnv(app, *env), nil
	}
	all := append(append([]map[string]any{}, cicd...), envXRs...)
	xrs := xrRows(all)
	var envv any
	namespaces := map[string]any{"cicd": cicdNS}
	named := env != nil && *env != ""
	if env != nil {
		envv = *env
	}
	if named {
		namespaces["env"] = t + "-" + app + "-" + *env
	}
	notReady := stNames(xrs, func(m map[string]any) bool { return m["ready"] != true })
	staleX := stNames(xrs, func(m map[string]any) bool { return m["stale"] == true })
	out := map[string]any{"app": app, "env": envv, "namespaces": namespaces, "xrs": xrs,
		"summary": map[string]any{"xrs": int64(len(xrs)), "ready": stCnt(xrs, "ready"), "notReady": notReady, "stale": staleX}}
	if named && len(envXRs) == 0 && len(flight) == 0 {
		out["notes"] = []any{fmt.Sprintf("no XRs in %s-%s-%s on this cluster and no ApplicationEnvironment for %s", t, app, *env, stPyRepr(*env))}
	}
	return out, nil
}

func (s *State) applications(ctx context.Context, app, env, t string) ([]map[string]any, error) {
	if !stLabelValue.MatchString(app) || !stLabelValue.MatchString(env) {
		return nil, nil // not a label value: no Application can carry it (and the selector is never built from it)
	}
	sel := "hangar.io/app=" + app + ",hangar.io/env=" + env
	argo := s.ArgoNamespace
	if argo == "" {
		argo = ArgoNamespace
	}
	var out []map[string]any
	seen := map[string]bool{}
	for _, ns := range []string{argo} {
		if seen[ns] {
			continue
		}
		seen[ns] = true
		items, err := s.list(ctx, stArgoPath("applications", ns, ""), sel)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

func appSummary(a map[string]any) map[string]any {
	st, sp := stAsMap(a["status"]), stAsMap(a["spec"])
	sync := stAsMap(st["sync"])
	sources := stAsList(sp["sources"])
	if !stTruthy(sp["sources"]) && stTruthy(sp["source"]) {
		sources = []any{sp["source"]}
	}
	revs := stAsList(sync["revisions"])
	if !stTruthy(sync["revisions"]) && stTruthy(sync["revision"]) {
		revs = []any{sync["revision"]}
	}
	rows := []any{}
	for i, src := range sources {
		sm := stAsMap(src)
		var rev any
		if i < len(revs) {
			rev = revs[i]
		}
		rows = append(rows, map[string]any{"repoURL": sm["repoURL"], "path": sm["path"], "targetRevision": sm["targetRevision"], "revision": rev})
	}
	op := stAsMap(st["operationState"])
	md := stAsMap(a["metadata"])
	return map[string]any{"name": md["name"], "namespace": md["namespace"], "project": sp["project"], "destination": sp["destination"],
		"sync":         map[string]any{"status": sync["status"], "revisions": rows},
		"health":       map[string]any{"status": stAsMap(st["health"])["status"], "message": stClip(stAsMap(st["health"])["message"], 300)},
		"operation":    map[string]any{"phase": op["phase"], "message": stClip(op["message"], 300), "finishedAt": op["finishedAt"]},
		"reconciledAt": st["reconciledAt"]}
}

var stPyInt = regexp.MustCompile(`^[+-]?[0-9]+(_[0-9]+)*$`)

// intOf is int(v) for an Argo Rollouts observedGeneration, which it writes as a string: nil for what int() would refuse.
func stIntOf(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case bool:
		if x {
			return int64(1)
		}
		return int64(0)
	case int64:
		return x
	case float64:
		if x != x || x > 9e18 || x < -9e18 {
			return nil
		}
		return int64(x)
	case string:
		t := strings.TrimSpace(x)
		if !stPyInt.MatchString(t) {
			return nil
		}
		n, err := strconv.ParseInt(strings.ReplaceAll(t, "_", ""), 10, 64)
		if err != nil {
			return nil
		}
		return n
	}
	return nil
}

func rolloutSummary(r map[string]any) map[string]any {
	st, sp, md := stAsMap(r["status"]), stAsMap(r["spec"]), stAsMap(r["metadata"])
	var images []any
	for _, c := range stAsList(stAsMap(stAsMap(stAsMap(sp["template"])["spec"]))["containers"]) {
		images = append(images, stAsMap(c)["image"])
	}
	if images == nil {
		images = []any{}
	}
	obs := stIntOf(st["observedGeneration"])
	// The strategy's name is the first key Python's dict holds; an object here has no order, so with two keys (never a real
	// Rollout) the smallest is named.
	var strategy any
	if sm := stAsMap(sp["strategy"]); len(sm) > 0 {
		ks := make([]string, 0, len(sm))
		for k := range sm {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		strategy = ks[0]
	}
	return map[string]any{"name": md["name"], "namespace": md["namespace"], "phase": st["phase"], "message": stClip(st["message"], 300),
		"replicas": sp["replicas"], "readyReplicas": st["readyReplicas"], "availableReplicas": st["availableReplicas"],
		"updatedReplicas": st["updatedReplicas"], "images": images, "strategy": strategy, "currentStepIndex": st["currentStepIndex"],
		"stableRS": st["stableRS"], "currentPodHash": st["currentPodHash"], "generation": md["generation"], "observedGeneration": obs,
		"stale": stStale(obs, md["generation"])}
}

// Describe is airframe.describe: one environment, its XRs, the Argo CD Application syncing it and its Rollout.
func (s *State) Describe(ctx context.Context, app, env string) (any, error) {
	e := env
	t, cicdNS, cicd, envXRs, flight, pl, found, err := s.envXRs(ctx, app, &e)
	if err != nil {
		return nil, err
	}
	if !found {
		return stNoApp(app), nil
	}
	if pl == nil || !pl.known {
		return stUnknownEnv(app, env), nil
	}
	envNS := pl.ns
	if envNS == "" {
		envNS = t + "-" + app + "-" + env // named in the answer only: nothing is read from it
	}
	xrs := []any{}
	for _, x := range append(append([]map[string]any{}, cicd...), envXRs...) {
		n := Normalise(x)
		row := map[string]any{"kind": n["kind"], "name": n["name"], "namespace": n["namespace"]}
		for k, v := range specSummary(x) {
			row[k] = v
		}
		for _, k := range []string{"ready", "synced", "stale", "notTrue"} {
			row[k] = n[k]
		}
		xrs = append(xrs, row)
	}
	notes := []any{}
	apps := pl.apps
	tier := "ground"
	if len(flight) > 0 {
		tier = "flight"
	} else {
		for _, a := range apps {
			if stAsMap(stAsMap(a["metadata"])["labels"])["hangar.io/ephemeral-env"] == "true" {
				tier = "ephemeral"
			}
		}
	}
	rollouts := []any{}
	for _, a := range apps {
		for _, res := range stAsList(stAsMap(a["status"])["resources"]) {
			rm := stAsMap(res)
			if rm["kind"] != "Rollout" || rm["group"] != "argoproj.io" {
				continue
			}
			ns, _ := rm["namespace"].(string)
			name, _ := rm["name"].(string)
			// An Application's resource list is written from a tenant's manifests: a Rollout is read only where this environment's
			// own workload runs.
			if name == "" || pl.ns == "" || ns != pl.ns {
				continue
			}
			r, err := s.get(ctx, stArgoPath("rollouts", ns, name))
			if err != nil {
				return nil, err
			}
			if r != nil {
				rollouts = append(rollouts, rolloutSummary(r))
			}
		}
	}
	if len(apps) == 0 {
		for _, f := range flight {
			if cl := stAsMap(f["spec"])["cluster"]; stTruthy(cl) && !stPyEq(cl, s.Cluster) {
				c := "one cluster"
				if s.Cluster != "" {
					c = s.Cluster
				}
				notes = append(notes, fmt.Sprintf("%s is a Flight environment on %s: its Application and Rollout run there, and this adapter reads %s only", env, stPyStr(cl), c))
			}
		}
		if len(flight) == 0 && pl.ns != "" {
			rs, err := s.list(ctx, stArgoPath("rollouts", envNS, ""), "")
			if err != nil {
				return nil, err
			}
			rollouts = []any{}
			for _, r := range rs {
				if r != nil {
					rollouts = append(rollouts, rolloutSummary(r))
				}
			}
			argo := s.ArgoNamespace
			if argo == "" {
				argo = ArgoNamespace
			}
			notes = append(notes, fmt.Sprintf("no Argo CD Application labelled hangar.io/app=%s, hangar.io/env=%s in %s or %s", app, env, argo, cicdNS))
		}
	}
	summaries := []any{}
	for _, a := range apps {
		summaries = append(summaries, appSummary(a))
	}
	out := map[string]any{"app": app, "env": env, "tier": tier, "namespaces": map[string]any{"cicd": cicdNS, "env": envNS}, "xrs": xrs,
		"applications": summaries, "rollouts": rollouts}
	if len(notes) > 0 {
		out["notes"] = notes
	}
	return out, nil
}
