package airframe

import (
	"context"
	"fmt"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// airframe.verify (airframe_tools.AirframeTools.verify): an XR's sidecar verify checks, made from the XR alone. A step that needs a
// Secret value, a ConfigMap, a port-forward, exec or a cloud CLI is skipped with the reason.

var skipReason = map[string]string{
	"secretKeys":             "reads the values of Secret {where}; Clearance never reads Secrets",
	"configMapKeys":          "reads ConfigMap {where}; ConfigMaps are outside Clearance's read allowlist",
	"tcp":                    "needs a port-forward to svc/{where}; Clearance never port-forwards",
	"http":                   "needs an HTTP request to {where}, through a port-forward for an in-cluster Service; Clearance makes neither",
	"oauthClientCredentials": "mints a token from the client secret in Secret {where}; Clearance never reads Secrets",
	"cloudCli":               "runs the {where} CLI with cloud credentials; Clearance has none",
}

type stepFailed string

func cond(xr map[string]any, ctype any) map[string]any {
	for _, c := range conditionsOf(xr) {
		if stPyEq(c["type"], ctype) {
			return c
		}
	}
	return nil
}

func runCondition(run *definitions.OMap, xr map[string]any) (string, *stepFailed) {
	want := any("True")
	if v, ok := run.M["status"]; ok {
		want = v
	}
	ct := run.M["type"]
	c := cond(xr, ct)
	if c == nil {
		f := stepFailed(fmt.Sprintf("the XR has no %s condition", stPyStr(ct)))
		return "", &f
	}
	detail := fmt.Sprintf("%s=%s reason=%s", stPyStr(ct), stPyStr(c["status"]), stPyStr(c["reason"]))
	gen, obs := stAsMap(xr["metadata"])["generation"], c["observedGeneration"]
	if o, ok := stNum(obs); ok {
		if g, ok := stNum(gen); ok && o < g {
			detail += fmt.Sprintf(" (observedGeneration %s < generation %s: not yet reconciled against the current spec)", stPyStr(obs), stPyStr(gen))
		}
	}
	reasonOK := true
	if rs := run.M["reasons"]; stTruthy(rs) {
		reasonOK = false
		for _, r := range stAsList(rs) {
			reasonOK = reasonOK || stPyEq(c["reason"], r)
		}
	}
	if !stPyEq(c["status"], want) || !reasonOK {
		if stTruthy(c["message"]) {
			detail += "; message: " + stPyStr(stClip(c["message"], 300))
		}
		f := stepFailed(detail)
		return "", &f
	}
	return detail, nil
}

func runXRField(run *definitions.OMap, xr map[string]any) (string, *stepFailed) {
	path, _ := run.M["path"].(string)
	v := Dig(xr, path)
	var ok bool
	var expected any
	if eq, has := run.M["equals"]; has {
		ok, expected = stPyEq(v, eq), eq
	} else {
		for _, e := range stAsList(run.M["in"]) {
			ok = ok || stPyEq(v, e)
		}
		expected = run.M["in"]
	}
	if !ok {
		f := stepFailed(fmt.Sprintf("%s is %s, expected %s", path, stPyRepr(v), stPyRepr(stNorm(expected))))
		return "", &f
	}
	return fmt.Sprintf("%s=%s", path, stPyRepr(v)), nil
}

func checkSynced(sc *definitions.OMap, xr map[string]any) map[string]any {
	res := map[string]any{"id": "xr-synced", "kind": "condition", "check": "The XR is Synced: Crossplane reconciled it with the current spec."}
	c := cond(xr, "Synced")
	set := func(kv ...any) map[string]any {
		out := map[string]any{}
		for k, v := range res {
			out[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	if c == nil {
		return set("status", "fail", "detail", "the XR has no Synced condition yet")
	}
	if c["status"] == "True" {
		return set("status", "pass", "detail", "Synced=True")
	}
	msg := stOr(c["message"], "")
	if k := knownFailure(sc, "Synced", msg); k != nil {
		return set("status", "fail", "knownFailure", k.id, "detail",
			fmt.Sprintf("Synced=False, known failure %s: %s Hint: %s", k.id, k.meaning, k.hint))
	}
	return set("status", "fail", "detail", fmt.Sprintf("Synced=False (%s): %s", stPyStr(c["reason"]), stPyStr(stClip(msg, 300))))
}

// where is airframe_tools._where: the names a skipped step would have used, rendered from the XR (connection-detail names, never values).
func where(run *definitions.OMap, xr map[string]any) (string, map[string]any) {
	r := func(tpl any) any {
		v, err := Render(tpl, xr)
		if u, ok := err.(*Unresolved); ok {
			return "<" + u.Key + " not published>"
		}
		return v
	}
	nsT, ok := run.M["namespace"]
	if !ok {
		nsT = "{namespace}"
	}
	ns := r(nsT)
	k, _ := run.M["kind"].(string)
	switch k {
	case "secretKeys", "oauthClientCredentials":
		names := map[string]any{"secret": r(run.M["secret"]), "namespace": ns}
		if keys, has := run.M["keys"]; has {
			names["keys"] = stNorm(keys)
		}
		return stPyStr(ns) + "/" + stPyStr(names["secret"]), names
	case "configMapKeys":
		cm := r(run.M["configMap"])
		return stPyStr(ns) + "/" + stPyStr(cm), map[string]any{"configMap": cm, "namespace": ns, "keys": stNorm(run.M["keys"])}
	case "tcp":
		svc := r(run.M["service"])
		return fmt.Sprintf("%s:%s in %s", stPyStr(svc), stPyStr(stNorm(run.M["port"])), stPyStr(ns)), map[string]any{"service": svc, "port": stNorm(run.M["port"]), "namespace": ns}
	case "http":
		var target string
		if stTruthy(run.M["url"]) {
			target = stPyStr(r(run.M["url"]))
		} else {
			p := any("/")
			if v, has := run.M["path"]; has {
				p = v
			}
			target = fmt.Sprintf("svc/%s:%s%s in %s", stPyStr(r(run.M["service"])), stPyStr(stNorm(run.M["port"])), stPyStr(p), stPyStr(ns))
		}
		return target, map[string]any{"target": target}
	case "cloudCli":
		return stPyStr(run.M["cli"]), map[string]any{"cli": run.M["cli"]}
	}
	return "", map[string]any{}
}

// resourceStep makes a `resource` step only when the resource is one that may be read at all (an XR, an Application or a
// Rollout) and lies in this app's own namespaces; otherwise it is skipped, never attempted.
func (s *State) resourceStep(ctx context.Context, allowed map[string]bool, run *definitions.OMap, xr map[string]any) (map[string]any, error) {
	resource, _ := run.M["resource"].(string)
	plural, group, _ := strings.Cut(resource, ".")
	name, nerr := Render(run.M["name"], xr)
	nsT, ok := run.M["namespace"]
	if !ok {
		nsT = "{namespace}"
	}
	ns, serr := Render(nsT, xr)
	for _, err := range []error{nerr, serr} {
		if u, ok := err.(*Unresolved); ok {
			return map[string]any{"status": "fail", "detail": fmt.Sprintf("cannot resolve {%s}: the XR has not published it", u.Key)}, nil
		}
	}
	nameS, nameOK := name.(string)
	nsS, nsOK := ns.(string)
	clusterScoped := stTruthy(run.M["clusterScoped"])
	var path string
	switch {
	case group == "catalog.hangar.io" && !clusterScoped:
		path = stXrPath(plural, nsS, nameS)
	case group == "argoproj.io" && (plural == "applications" || plural == "rollouts") && !clusterScoped:
		path = stArgoPath(plural, nsS, nameS)
	default:
		return map[string]any{"status": "skipped", "names": map[string]any{"resource": resource, "name": name},
			"reason": fmt.Sprintf("reads %s %s, which is outside Clearance's read allowlist (Airframe XRs, Argo CD Applications and Rollouts)", resource, stPyStr(name))}, nil
	}
	// The namespace and name come from the XR's spec, which a tenant writes: a step reads only inside this app's own namespaces.
	if !nameOK || !nsOK || !allowed[nsS] {
		return map[string]any{"status": "skipped", "names": map[string]any{"resource": resource, "name": name},
			"reason": fmt.Sprintf("reads %s %s outside the app's own namespaces", resource, stPyStr(name))}, nil
	}
	obj, err := s.get(ctx, path)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return map[string]any{"status": "fail", "detail": fmt.Sprintf("%s %s does not exist", resource, nameS)}, nil
	}
	parts := []string{fmt.Sprintf("%s %s exists", resource, nameS)}
	if c := omapOf(run.M["condition"]); run.M["condition"] != nil && stTruthy(run.M["condition"]) {
		got := cond(obj, c.M["type"])
		want := any("True")
		if v, ok := c.M["status"]; ok {
			want = v
		}
		if got == nil || !stPyEq(got["status"], want) {
			st := "absent"
			if got != nil {
				st = stPyStr(got["status"])
			}
			return map[string]any{"status": "fail", "detail": fmt.Sprintf("%s %s: %s is %s", resource, nameS, stPyStr(c.M["type"]), st)}, nil
		}
		parts = append(parts, stPyStr(got["type"])+"="+stPyStr(got["status"]))
	}
	if stTruthy(run.M["field"]) {
		field, _ := run.M["field"].(string)
		v := Dig(obj, field)
		if eq, has := run.M["equals"]; has && !stPyEq(v, eq) {
			return map[string]any{"status": "fail", "detail": fmt.Sprintf("%s %s: %s is %s, expected %s", resource, nameS, field, stPyRepr(v), stPyRepr(stNorm(eq)))}, nil
		}
		parts = append(parts, field+"="+stPyRepr(v))
	}
	return map[string]any{"status": "pass", "detail": strings.Join(parts, ", ")}, nil
}

// Verify is airframe.verify: an XR's sidecar verify checks, made from its status alone.
func (s *State) Verify(ctx context.Context, app, kind, name string, env *string) (any, error) {
	sc, xrd := FindKind(kind)
	if sc == nil || len(xrd.Keys) == 0 {
		return stAnswer("AF-KIND-404", fmt.Sprintf("no verify contract for kind %s", stPyRepr(kind)), "airframe.capabilities lists every kind with its verify checks."), nil
	}
	plural, _ := xrd.M["plural"].(string)
	named := env != nil && *env != ""
	var xr map[string]any
	var allowed map[string]bool // the namespaces a `resource` step may read: the XR's own and its app's cicd namespace
	knownEnv := !named
	for _, t := range stTypes {
		ns := t + "-" + app + "-cicd"
		if named {
			// the environment is one the platform registered for the app, and its namespace the one it assigned (see place)
			pl, err := s.place(ctx, app, *env, t, false)
			if err != nil {
				return nil, err
			}
			if !pl.known || pl.ns == "" {
				continue
			}
			knownEnv, ns = true, pl.ns
		}
		if !named {
			ok, oerr := s.ownedCicd(ctx, ns, app)
			if oerr != nil {
				return nil, oerr
			}
			if !ok {
				continue
			}
		}
		var err error
		if xr, err = s.get(ctx, stXrPath(plural, ns, name)); err != nil {
			return nil, err
		}
		if xr != nil {
			allowed = map[string]bool{ns: true}
			cicd := stCicdNamespace(t, app)
			if ok, oerr := s.ownedCicd(ctx, cicd, app); oerr != nil {
				return nil, oerr
			} else if ok {
				allowed[cicd] = true
			}
			break
		}
	}
	if !knownEnv {
		return stUnknownEnv(app, *env), nil
	}
	if xr == nil {
		var ws []string
		for _, t := range stTypes {
			e := "cicd"
			if named {
				e = *env
			}
			ws = append(ws, t+"-"+app+"-"+e)
		}
		return stAnswer("AF-XR-404", fmt.Sprintf("no %s %s in %s", stPyStr(sc.M["kind"]), stPyRepr(name), strings.Join(ws, " or ")),
			"airframe.status lists the app's XRs; a Ground environment's components are in <type>-<app>-<env>."), nil
	}
	steps := []any{checkSynced(sc, xr)}
	for _, st := range stAsList(sc.M["verify"]) {
		step := omapOf(st)
		run := omapOf(step.M["run"])
		rk, _ := run.M["kind"].(string)
		res := map[string]any{"id": step.M["id"], "kind": run.M["kind"], "check": step.M["check"]}
		put := func(m map[string]any) {
			for k, v := range m {
				res[k] = v
			}
		}
		switch {
		case !WhenMatches(step.M["when"], xr):
			put(map[string]any{"status": "skipped", "reason": fmt.Sprintf("when %s does not match this XR", stPyRepr(step.M["when"]))})
		case rk == "manual":
			put(map[string]any{"status": "manual", "reason": "no tool can make this check; the sentence says what a human looks at"})
		case rk == "condition" || rk == "xrField":
			var detail string
			var fail *stepFailed
			if rk == "condition" {
				detail, fail = runCondition(run, xr)
			} else {
				detail, fail = runXRField(run, xr)
			}
			if fail != nil {
				put(map[string]any{"status": "fail", "detail": string(*fail)})
			} else {
				put(map[string]any{"status": "pass", "detail": detail})
			}
		case rk == "resource":
			r, err := s.resourceStep(ctx, allowed, run, xr)
			if err != nil {
				return nil, err
			}
			put(r)
		default:
			w, names := where(run, xr)
			reason, ok := skipReason[rk]
			if !ok {
				reason = "a check of a kind this gateway does not make: {where}"
			}
			put(map[string]any{"status": "skipped", "reason": strings.ReplaceAll(reason, "{where}", w), "names": names})
		}
		steps = append(steps, res)
	}
	counts := map[string]any{}
	for _, k := range []string{"pass", "fail", "skipped", "manual"} {
		var n int64
		for _, x := range steps {
			if x.(map[string]any)["status"] == k {
				n++
			}
		}
		counts[k] = n
	}
	return map[string]any{"kind": sc.M["kind"], "namespace": stAsMap(xr["metadata"])["namespace"], "name": name,
		"passed": counts["fail"] == int64(0), "counts": counts, "steps": steps}, nil
}
