package backends

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/airframe"
)

// The reads of the airframe.* tools: the allowlist, held to a list written out separately.

var kubeOracle = regexp.MustCompile(`^/apis/(catalog\.hangar\.io/v1alpha1/namespaces/[a-z0-9][a-z0-9-]*/[a-z]+(/[a-z0-9][a-z0-9.-]*)?|argoproj\.io/v1alpha1/namespaces/[a-z0-9][a-z0-9-]*/(applications|rollouts)(/[a-z0-9][a-z0-9.-]*)?)$`)

func TestTheKubeAllowlistIsTheClosedListOfXRsRolloutsAndApplications(t *testing.T) {
	ok := []string{
		XRPath("managedpostgresqls", "app-shop-dev", ""), XRPath("pythonapplications", "app-shop-cicd", "shop"), XRPath("redis", "infra-x-dev", "r"),
		XRPath("agents", "app-x-cicd", ""), RolloutsPath("app-shop-dev", ""), RolloutsPath("app-shop-dev", "shop"),
		ApplicationsPath("argocd-apps", ""), ApplicationsPath("argocd-apps", "shop-dev"),
	}
	for _, p := range ok {
		if !pathAllowed(p) {
			t.Errorf("%s must be allowed", p)
		}
	}
	no := []string{
		XRPath("agents", "app-x-cicd", "x"),              // an Agent XR is listed by the loader, never read by name
		XRPath("secrets", "app-x-dev", ""),               // not an XR kind of the contract
		XRPath("compositions", "app-x-dev", ""),          //
		XRPath("xrds", "app-x-dev", "x"),                 //
		XRPath("pythonapplications", "App-x", "x"),       // upper case
		XRPath("pythonapplications", "a/b", "x"),         //
		XRPath("pythonapplications", "app-x-dev", "a/b"), //
		XRPath("pythonapplications", "app-x-dev", "x/status"),
		"/apis/catalog.hangar.io/v1alpha1/pythonapplications", // cluster-wide
		"/apis/catalog.hangar.io/v1alpha1/namespaces/a/pythonapplications/x/status",
		"/apis/catalog.hangar.io/v1/namespaces/a/pythonapplications",
		"/apis/argoproj.io/v1alpha1/rollouts", "/apis/argoproj.io/v1alpha1/applications",
		"/apis/argoproj.io/v1alpha1/namespaces/a/rollouts/x/status", "/apis/argoproj.io/v1alpha1/namespaces/a/analysisruns/x",
		"/apis/argoproj.io/v1alpha1/namespaces/a/appprojects/x", "/apis/argoproj.io/v1alpha1/namespaces/a/applicationsets",
		"/api/v1/namespaces/a/secrets/x", "/api/v1/namespaces/a/configmaps", "/api/v1/namespaces/a/pods/x/exec",
		"/apis/rbac.authorization.k8s.io/v1/clusterroles", "/apis/external-secrets.io/v1beta1/namespaces/a/externalsecrets",
	}
	for _, p := range no {
		if pathAllowed(p) {
			t.Errorf("%s must not be allowed", p)
		}
	}
	for _, l := range AllowedKubeGETs {
		if !strings.HasPrefix(l, "GET /apis/") {
			t.Errorf("%s", l)
		}
	}
	if len(xrPlurals) < 20 || xrPlurals["secrets"] || !xrPlurals["agents"] {
		t.Fatalf("the plurals are %v", xrPlurals)
	}
}

func FuzzKubeAllowlist(f *testing.F) {
	for _, s := range []string{"managedpostgresqls", "agents", "rollouts", "../x", "a%2fb", "A", "a b", ""} {
		f.Add(s, s, s)
	}
	f.Fuzz(func(t *testing.T, plural, ns, name string) {
		for _, p := range []string{XRPath(plural, ns, name), XRPath(plural, ns, ""), RolloutsPath(ns, name), RolloutsPath(ns, ""), ApplicationsPath(ns, name),
			"/apis/catalog.hangar.io/v1alpha1/namespaces/" + ns + "/" + plural + "/" + name, "/apis/argoproj.io/v1alpha1/namespaces/" + ns + "/" + plural} {
			if !pathAllowed(p) {
				continue
			}
			u, err := url.Parse("https://kubernetes.default.svc" + p)
			if err != nil || u.Host != "kubernetes.default.svc" || u.Path != p || u.RawQuery != "" || u.Fragment != "" || !kubeOracle.MatchString(p) ||
				strings.ContainsAny(p, "?#%\\ \n\x00") || strings.Contains(p, "..") && strings.Contains(p, "/..") {
				t.Fatalf("%q was allowed: %q", p, p)
			}
		}
	})
}

type auditReader struct {
	mu   sync.Mutex
	bad  []string
	seen int
}

func (a *auditReader) note(p string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen++
	if pathAllowed(p) && !kubeOracle.MatchString(p) {
		a.bad = append(a.bad, p)
	}
}

func (a *auditReader) Get(ctx context.Context, p string) (any, bool, error) {
	a.note(p)
	return nil, false, nil
}
func (a *auditReader) List(ctx context.Context, p, sel string) ([]map[string]any, error) {
	a.note(p)
	return nil, nil
}

func FuzzAirframeToolPaths(f *testing.F) {
	for _, s := range []string{"shop", "a/b", "A", "..", "x%2fy", "é", ""} {
		f.Add(s, s, s)
	}
	f.Fuzz(func(t *testing.T, app, env, name string) {
		a := &auditReader{}
		s := &airframe.State{Kube: a, Cluster: "c"}
		ctx := context.Background()
		_, _ = s.Status(ctx, app, &env)
		_, _ = s.Describe(ctx, app, env)
		_, _ = s.Verify(ctx, app, "PythonApplication", name, &env)
		_, _ = s.Verify(ctx, app, "Redis", name, nil)
		if len(a.bad) > 0 {
			t.Fatalf("paths the allowlist accepts that are not on the list: %q", a.bad)
		}
	})
}
