package airframe

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// An agent scoped to the app `gate` must not read the namespaces of the app `gate-api`: `<type>-<app>-<env>` joined from an
// argument is the namespace of another app when the env is `api-prod`. These tests failed on the first version of the state tools
// (which built the namespace from the argument), and hold the namespace to the one the platform assigned to (app, env).

type recReader struct {
	mu    sync.Mutex
	lists map[string][]map[string]any // path -> items (the label selector is applied below)
	objs  map[string]map[string]any
	seen  []string
}

func (r *recReader) Get(_ context.Context, path string) (any, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, path)
	o, ok := r.objs[path]
	return o, ok, nil
}

func (r *recReader) List(_ context.Context, path, sel string) ([]map[string]any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, path+"?"+sel)
	var out []map[string]any
	for _, it := range r.lists[path] {
		ok := true
		if sel != "" {
			labels := stAsMap(stAsMap(it["metadata"])["labels"])
			for _, kv := range strings.Split(sel, ",") {
				k, v, _ := strings.Cut(kv, "=")
				ok = ok && labels[k] == v
			}
		}
		if ok {
			out = append(out, it)
		}
	}
	return out, nil
}

func (r *recReader) touched(sub string) bool {
	for _, p := range r.seen {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func tenantWorld() *recReader {
	xr := func(kind, ns, name string, spec map[string]any) map[string]any {
		return map[string]any{"apiVersion": "catalog.hangar.io/v1alpha1", "kind": kind, "metadata": map[string]any{"name": name, "namespace": ns}, "spec": spec,
			"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}
	}
	app := func(name, ns, a, env, dest string) map[string]any {
		return map[string]any{"kind": "Application", "metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{"hangar.io/app": a, "hangar.io/env": env}},
			"spec": map[string]any{"destination": map[string]any{"namespace": dest}}, "status": map[string]any{"resources": []any{
				map[string]any{"kind": "Rollout", "group": "argoproj.io", "namespace": dest, "name": "web"}}}}
	}
	r := &recReader{lists: map[string][]map[string]any{}, objs: map[string]map[string]any{}}
	add := func(path string, o map[string]any) { r.lists[path] = append(r.lists[path], o) }
	const cat = "/apis/catalog.hangar.io/v1alpha1/namespaces/"
	add(cat+"app-gate-cicd/pythonapplications", xr("PythonApplication", "app-gate-cicd", "gate", nil))
	add(cat+"app-gate-cicd/applicationenvironments", xr("ApplicationEnvironment", "app-gate-cicd", "gate-k-dev", map[string]any{"appName": "gate", "env": "dev", "cluster": "k"}))
	add(cat+"app-gate-api-cicd/pythonapplications", xr("PythonApplication", "app-gate-api-cicd", "gate-api", nil))
	add(cat+"app-gate-api-prod/redis", xr("Redis", "app-gate-api-prod", "secret-cache", nil))
	add(cat+"app-gate-api-cicd/redis", xr("Redis", "app-gate-api-cicd", "secret-cache", nil))
	add("/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications", app("gate-dev", "argocd-apps", "gate", "dev", "app-gate-dev"))
	add("/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications", app("gate-api-prod", "argocd-apps", "gate-api", "prod", "app-gate-api-prod"))
	add("/apis/argoproj.io/v1alpha1/namespaces/app-gate-api-prod/rollouts", map[string]any{"kind": "Rollout", "metadata": map[string]any{"name": "web", "namespace": "app-gate-api-prod"}})
	for ns, a := range map[string]string{"app-gate-cicd": "gate", "app-gate-dev": "gate", "app-gate-api-cicd": "gate-api", "app-gate-api-prod": "gate-api", "app-gate-pr-12": "gate"} {
		r.objs["/api/v1/namespaces/"+ns] = map[string]any{"kind": "Namespace", "metadata": map[string]any{"name": ns, "labels": map[string]any{"hangar.io/app": a}}}
	}
	r.objs["/api/v1/namespaces/app-gate-bare"] = map[string]any{"kind": "Namespace", "metadata": map[string]any{"name": "app-gate-bare"}}
	r.objs["/apis/catalog.hangar.io/v1alpha1/namespaces/app-gate-api-prod/redis/secret-cache"] = xr("Redis", "app-gate-api-prod", "secret-cache", nil)
	r.objs["/apis/catalog.hangar.io/v1alpha1/namespaces/app-gate-api-cicd/redis/secret-cache"] = xr("Redis", "app-gate-api-cicd", "secret-cache", nil)
	r.objs["/apis/argoproj.io/v1alpha1/namespaces/app-gate-api-prod/rollouts/web"] = map[string]any{"kind": "Rollout", "metadata": map[string]any{"name": "web"}}
	return r
}

func TestAnAppCannotReadAnotherAppsNamespaceByNamingAnEnvironmentThatJoinsToIt(t *testing.T) {
	for _, env := range []string{"api-prod", "api-cicd", "api", "prod", "cicd", "dev-x", "a--b", "API", "dev/..", ""} {
		for _, tool := range []string{"status", "describe", "verify"} {
			r := tenantWorld()
			s := &State{Kube: r, Cluster: "k"}
			var out any
			var err error
			e := env
			switch tool {
			case "status":
				out, err = s.Status(context.Background(), "gate", &e)
			case "describe":
				out, err = s.Describe(context.Background(), "gate", e)
			case "verify":
				out, err = s.Verify(context.Background(), "gate", "Redis", "secret-cache", &e)
			}
			if env == "" && tool != "describe" {
				continue // no environment named: the app's own cicd namespace, which is its own
			}
			if r.touched("app-gate-api-") || r.touched("infra-gate-api-") {
				t.Errorf("%s env %q read another app's namespace: %v", tool, env, r.seen)
			}
			if m := stAsMap(out); err != nil || stAsMap(m["error"])["rule"] != "AF-ENV-404" {
				t.Errorf("%s env %q: %v %v", tool, env, out, err)
			}
		}
	}
	// its own environment still works, and reads its own namespace only
	r := tenantWorld()
	s := &State{Kube: r, Cluster: "k"}
	dev := "dev"
	out, err := s.Status(context.Background(), "gate", &dev)
	if err != nil || stAsMap(out)["error"] != nil || !r.touched("namespaces/app-gate-dev/") {
		t.Fatalf("%v %v %v", out, err, r.seen)
	}
}

func TestAResourceStepReadsOnlyTheNamespacesTheAppOwns(t *testing.T) {
	// `gate`'s XR names `app-gate-api-prod` in a template: the prefix `app-gate-` matched it once
	run := omapOf(mustJSONValue(t, `{"kind":"resource","resource":"redis.catalog.hangar.io","name":"secret-cache","namespace":"app-gate-api-prod"}`))
	xr := xrOf(t, `{"metadata":{"name":"x","namespace":"app-gate-cicd"},"spec":{}}`)
	for _, ns := range []string{"app-gate-api-prod", "app-gate-api-cicd", "app-gate-", "app-gate-x", "infra-gate-dev"} {
		r := tenantWorld()
		s := &State{Kube: r}
		run.M["namespace"] = ns
		res, err := s.resourceStep(context.Background(), map[string]bool{"app-gate-cicd": true, "app-gate-dev": true}, run, xr)
		if err != nil || res["status"] != "skipped" || len(r.seen) != 0 {
			t.Errorf("%s: %v %v %v", ns, res, err, r.seen)
		}
	}
}

func mustJSONValue(t *testing.T, s string) any {
	t.Helper()
	v, err := definitions.DecodeJSON([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// One Application labelled for `gate` whose destination is gate-api's namespace (the namespace a tenant of `gate` cannot own): the
// Namespace says whose it is, and nothing is read in it.
func TestAnApplicationLabelledForAnAppCannotPointTheToolsAtAnotherAppsNamespace(t *testing.T) {
	r := tenantWorld()
	r.lists["/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications"] = append(r.lists["/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications"],
		map[string]any{"kind": "Application", "metadata": map[string]any{"name": "evil", "namespace": "argocd-apps", "labels": map[string]any{"hangar.io/app": "gate", "hangar.io/env": "api-prod"}},
			"spec": map[string]any{"destination": map[string]any{"namespace": "app-gate-api-prod"}}})
	s := &State{Kube: r, Cluster: "k"}
	env := "api-prod"
	if _, err := s.Status(context.Background(), "gate", &env); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Describe(context.Background(), "gate", env); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(context.Background(), "gate", "Redis", "secret-cache", &env); err != nil {
		t.Fatal(err)
	}
	for _, p := range r.seen {
		if strings.Contains(p, "namespaces/app-gate-api-prod/") && !strings.HasPrefix(p, "/api/v1/namespaces/") {
			t.Fatalf("read in gate-api's namespace: %s", p)
		}
	}
	if nss, err := s.AppNamespaces(context.Background(), "gate", "api-prod"); err != nil || len(nss) != 0 {
		t.Fatalf("%v %v", nss, err)
	}
}

func TestANamespaceWithoutTheAppsLabelIsNeverRead(t *testing.T) {
	r := tenantWorld()
	r.lists["/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications"] = append(r.lists["/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications"],
		map[string]any{"kind": "Application", "metadata": map[string]any{"name": "bare", "namespace": "argocd-apps", "labels": map[string]any{"hangar.io/app": "gate", "hangar.io/env": "bare"}},
			"spec": map[string]any{"destination": map[string]any{"namespace": "app-gate-bare"}}})
	s := &State{Kube: r, Cluster: "k"}
	env := "bare"
	out, err := s.Status(context.Background(), "gate", &env)
	if err != nil || r.touched("namespaces/app-gate-bare/") {
		t.Fatalf("%v %v %v", out, err, r.seen)
	}
}

func TestADashedEnvironmentIsReadWhenItsNamespaceCarriesTheAppsLabel(t *testing.T) {
	r := tenantWorld()
	r.lists["/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications"] = append(r.lists["/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications"],
		map[string]any{"kind": "Application", "metadata": map[string]any{"name": "pr", "namespace": "argocd-apps", "labels": map[string]any{"hangar.io/app": "gate", "hangar.io/env": "pr-12"}},
			"spec": map[string]any{"destination": map[string]any{"namespace": "app-gate-pr-12"}}})
	// an Application in the app's own cicd namespace (a tenant can write there) is not a source of environments
	r.lists["/apis/argoproj.io/v1alpha1/namespaces/app-gate-cicd/applications"] = []map[string]any{
		{"kind": "Application", "metadata": map[string]any{"name": "x", "labels": map[string]any{"hangar.io/app": "gate", "hangar.io/env": "api-prod"}},
			"spec": map[string]any{"destination": map[string]any{"namespace": "app-gate-api-prod"}}}}
	s := &State{Kube: r, Cluster: "k"}
	env := "pr-12"
	out, err := s.Status(context.Background(), "gate", &env)
	if err != nil || stAsMap(out)["error"] != nil || !r.touched("namespaces/app-gate-pr-12/") {
		t.Fatalf("%v %v %v", out, err, r.seen)
	}
	if r.touched("namespaces/app-gate-cicd/applications") {
		t.Fatalf("Applications were listed in a namespace a tenant writes: %v", r.seen)
	}
}
