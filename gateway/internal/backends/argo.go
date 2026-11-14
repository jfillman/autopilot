package backends

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/core"
)

// argo.app.get (reads.KubeArgo.get_app and app_summary): an Argo CD Application read through the Kubernetes API as the
// gateway's own identity. What comes back is a summary (source, destination, sync, health, the last operation, the
// conditions and the resources that are not synced or not healthy), never spec.source's Helm values, which can carry
// anything a chart was given: only the fields named below are copied out of the object. Given an app's name instead of an
// Application's, it lists that app's Applications by the hangar.io/app label Hangar's tenant-onboarding stamps on each.

const (
	appLabel     = "hangar.io/app"
	envLabel     = "hangar.io/env"
	maxApps      = 50
	maxResources = 50
)

var labelValue = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)

// Argo is the argo.app.get backend.
type Argo struct {
	Kube       *KubeReader
	Namespaces []string
}

// ParseNamespaces reads ARGO_APP_NAMESPACES (comma separated). Each must be a DNS label: it goes into a path.
func ParseNamespaces(raw string) ([]string, error) {
	var out []string
	for _, n := range strings.Split(raw, ",") {
		if n = strings.TrimSpace(n); n != "" {
			if !nsPattern.MatchString(n) {
				return nil, fmt.Errorf("ARGO_APP_NAMESPACES: %s is not a namespace name (a DNS label)", pyStrRepr(n))
			}
			out = append(out, n)
		}
	}
	return out, nil
}

func appBrief(o map[string]any) map[string]any {
	meta, st := obj(o["metadata"]), obj(o["status"])
	labels := obj(meta["labels"])
	return map[string]any{"app": meta["name"], "namespace": meta["namespace"], "env": labels[envLabel],
		"role": labels["hangar.io/subcomponent"], "project": obj(o["spec"])["project"],
		"sync": obj(st["sync"])["status"], "health": obj(st["health"])["status"]}
}

func pick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

func appSummary(o map[string]any) map[string]any {
	meta, spec, st := obj(o["metadata"]), obj(o["spec"]), obj(o["status"])
	var sources []any
	if s, _ := spec["sources"].([]any); len(s) > 0 {
		sources = s
	} else if pyTruthy(spec["source"]) {
		sources = []any{spec["source"]}
	}
	srcs := []any{}
	for _, s := range sources {
		srcs = append(srcs, pick(obj(s), "repoURL", "path", "chart", "targetRevision"))
	}
	op := obj(st["operationState"])
	var operation any
	if len(op) > 0 {
		operation = map[string]any{"phase": op["phase"], "message": clip(op["message"], MaxMessage), "startedAt": op["startedAt"],
			"finishedAt": op["finishedAt"]}
	}
	conds := []any{}
	cs, _ := st["conditions"].([]any)
	for _, c := range cs {
		cm := obj(c)
		conds = append(conds, map[string]any{"type": cm["type"], "message": clip(cm["message"], MaxMessage)})
	}
	var bad []map[string]any
	rs, _ := st["resources"].([]any)
	for _, r := range rs {
		rm := obj(r)
		hs := obj(rm["health"])["status"]
		if (rm["status"] != nil && rm["status"] != "Synced") || (hs != nil && hs != "Healthy") {
			bad = append(bad, rm)
		}
	}
	shown := []any{}
	for i, rm := range bad {
		if i >= maxResources {
			break
		}
		h := obj(rm["health"])
		shown = append(shown, map[string]any{"kind": rm["kind"], "namespace": rm["namespace"], "name": rm["name"],
			"status": rm["status"], "health": h["status"], "message": clip(h["message"], MaxMessage)})
	}
	sync, health := obj(st["sync"]), obj(st["health"])
	return map[string]any{
		"app": meta["name"], "namespace": meta["namespace"], "project": spec["project"],
		"sources":     srcs,
		"destination": pick(obj(spec["destination"]), "server", "name", "namespace"),
		"sync":        map[string]any{"status": sync["status"], "revision": sync["revision"], "revisions": sync["revisions"]},
		"health":      map[string]any{"status": health["status"], "message": clip(health["message"], MaxMessage)},
		"operation":   operation, "conditions": conds,
		"unhealthyResources":          shown,
		"unhealthyResourcesTruncated": len(bad) > maxResources,
		"reconciledAt":                st["reconciledAt"],
	}
}

// Get answers argo.app.get: an Application by its name, else, when no Application has that name, an app's Applications by
// the label tenant-onboarding stamps on them, never by guessing a name.
func (a *Argo) Get(ctx context.Context, app any) (any, error) {
	name, ok := app.(string)
	if !ok || !segPattern.MatchString(name) {
		return nil, &core.Denied{Rule: "READ-ARGO-NAME", Message: pyRepr(app) + " is not an Application name",
			Hint: "Pass the Argo CD Application's name (e.g. flight-api-dev) or an app's catalog component name (e.g. flight-api)."}
	}
	for _, ns := range a.Namespaces {
		o, found, err := a.Kube.Get(ctx, ApplicationsPath(ns, name), nil)
		if err != nil {
			return nil, err
		}
		if found {
			return appSummary(obj(o)), nil
		}
	}
	var found []map[string]any
	if labelValue.MatchString(name) {
		for _, ns := range a.Namespaces {
			items, err := a.Kube.List(ctx, ApplicationsPath(ns, ""), appLabel+"="+name)
			if err != nil {
				return nil, err
			}
			found = append(found, items...)
		}
	}
	searched := make([]any, len(a.Namespaces))
	for i, n := range a.Namespaces {
		searched[i] = n
	}
	if len(found) == 0 {
		return map[string]any{"app": name, "found": false, "searched": searched,
			"hint": "No Application has that name, and none is labelled as that app's. Applications are named <app>-<env> (e.g. flight-api-dev); catalog.read gives an app's component name."}, nil
	}
	briefs := make([]map[string]any, len(found))
	for i, o := range found {
		briefs[i] = appBrief(o)
	}
	sort.SliceStable(briefs, func(i, j int) bool {
		ei, ej := briefs[i]["env"], briefs[j]["env"]
		if (ei == nil) != (ej == nil) {
			return ej == nil
		}
		if s1, s2 := pyStrOrEmpty(ei), pyStrOrEmpty(ej); s1 != s2 {
			return s1 < s2
		}
		return pyStrOrEmpty(briefs[i]["app"]) < pyStrOrEmpty(briefs[j]["app"])
	})
	shown := make([]any, 0, maxApps)
	for i, b := range briefs {
		if i >= maxApps {
			break
		}
		shown = append(shown, b)
	}
	return map[string]any{"app": name, "found": false, "resolvedBy": "label " + appLabel + "=" + name, "applications": shown,
		"truncated": len(briefs) > maxApps,
		"hint":      "These are the app's Applications; an environment's is <app>-<env>. Call argo.app.get with one of these names for its sync, health, last operation and unhealthy resources."}, nil
}

func pyStrOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	return pyStr(v)
}
