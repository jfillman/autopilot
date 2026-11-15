package backends

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/jfillman/autopilot/gateway/internal/airframe"
	"github.com/jfillman/autopilot/gateway/internal/backends/fakes"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// What an agent cannot do with the Airframe tools, whatever it sends (A-9 S5c). Each test fails if the check it names is
// removed: the scope is the gateway's own, decided before any request exists.

const (
	tenantsRepo = "o/gitops-tenants"
	xrTool      = "xr.request"
)

// stubKube is the cluster the state tools read: it holds nothing and counts what it was asked.
type stubKube struct {
	mu    sync.Mutex
	calls int
}

func (k *stubKube) Get(context.Context, string) (any, bool, error) {
	k.mu.Lock()
	k.calls++
	k.mu.Unlock()
	return nil, false, nil
}
func (k *stubKube) List(context.Context, string, string) ([]map[string]any, error) {
	k.mu.Lock()
	k.calls++
	k.mu.Unlock()
	return nil, nil
}
func (k *stubKube) count() int { k.mu.Lock(); defer k.mu.Unlock(); return k.calls }

func afDef(repos []string, profile string, deny ...string) *definitions.Definition {
	reg := tools.Default()
	return &definitions.Definition{Name: "fixture-agent", Kind: "task", ReposAllow: repos, DenyPaths: append(append([]string{}, reg.BaselineDenyPaths...), deny...),
		RawProfile: json.RawMessage(profile)}
}

const afProfile = `{"glidepath":{"apps":["*"]},"airframe":{"apps":["flight-api","parachute"],"proposeFlight":true}}`

type afEnv struct {
	*fakeEnv
	kube *stubKube
}

func newAFEnv(t *testing.T, gh *fakes.GitHub, gpf *fakes.Glidepath, def *definitions.Definition, names []string, ceiling, sessionID string, xr bool) *afEnv {
	t.Helper()
	dir := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	reg := tools.Default()
	kube := &stubKube{}
	cfg := HangarConfig{Definition: func(a string) *definitions.Definition {
		if a != def.Name {
			return nil
		}
		return def
	}, Tools: reg, Airframe: &AirframeConfig{Enabled: true, XRRequests: xr, Registry: airframeFixtureRegistry,
		State: &airframe.State{Kube: kube, Cluster: "dev-cluster", ArgoNamespace: airframe.ArgoNamespace}}}
	e := &fakeEnv{}
	g, err := NewGitHub(GitHubOptions{API: gh.Srv.URL, BrokerURL: gh.BrokerURL(), TokenFile: write("agent", "agent-token-value"), Owners: []string{"o"},
		TenantsRepo: tenantsRepo, AllowHTTP: true, AllowHTTPAPI: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	g.retry.base, g.retry.sleep = time.Millisecond, func(context.Context, time.Duration) bool { return true }
	cfg.GitHub, e.git = g, g
	if gpf != nil {
		gp, err := NewGlidepath(gpf.Srv.URL, write("gp", "gp-token-value-1"), true, 5*time.Second, true)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Glidepath, e.gp = gp, gp
	}
	e.h = hangarHandlers(cfg)
	run := "r-1"
	e.sess = fixtureSession(map[string]any{"id": sessionID, "principal": "user:jfillman", "agent": "fixture-agent", "task": "t-abc123", "run": run}, names)
	e.sess.Limits.TierCeiling = ceiling
	return &afEnv{fakeEnv: e, kube: kube}
}

func afGitHub() *fakes.GitHub {
	gh := fakes.NewGitHub(tenantsRepo, "o/parachute", "o/gitops-parachute", "o/payments")
	gh.SeedFiles(tenantsRepo, map[string]string{"tenants/flight-api/xr-requests/keep.yaml": "x\n", "tenants/parachute/xr-requests/keep.yaml": "x\n"})
	return gh
}

func xrArgs() map[string]any {
	return map[string]any{"app": "flight-api", "env": "dev", "kind": "MessageQueue", "name": "flight-api-orders", "spec": map[string]any{"lockSeconds": json.Number("60")}}
}

func appSpec(description string) map[string]any {
	var spec map[string]any
	d := json.NewDecoder(strings.NewReader(`{"apiVersion":"airframe/v1","kind":"AppSpec","app":"parachute","stack":"python","description":` + string(mustJSON(description)) +
		`,"environments":{"ground":["dev"],"flight":[{"name":"staging","cluster":"up-cluster"}]}}`))
	d.UseNumber()
	if err := d.Decode(&spec); err != nil {
		panic(err)
	}
	return spec
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// -- scope: before any request ---------------------------------------------------------------------------------------------

func TestEveryAirframeToolIsScopedBeforeAnyRequestExists(t *testing.T) {
	type tc struct {
		name, tool string
		args       map[string]any
		def        *definitions.Definition
		names      []string
		ceiling    string
		rules      []string
	}
	all := allTools()
	okDef := afDef([]string{"o/*"}, afProfile)
	spec := appSpec("a service")
	other := appSpec("a service")
	other["app"] = "payments"
	status := map[string]any{"app": "flight-api"}
	cases := []tc{
		{"xr.request, no definition", xrTool, xrArgs(), &definitions.Definition{Name: "someone-else"}, all, "T2", []string{"R003"}},
		{"xr.request, not in the grant", xrTool, xrArgs(), okDef, without(all, xrTool), "T2", []string{"R006"}},
		{"xr.request, tier above the ceiling", xrTool, xrArgs(), okDef, all, "T0", []string{"R007"}},
		{"xr.request, the app is not on profile.airframe.apps", xrTool, merge1(xrArgs(), "app", "payments"), okDef, all, "T2", []string{"R023"}},
		{"xr.request, an empty app", xrTool, merge1(xrArgs(), "app", ""), okDef, all, "T2", []string{"R023"}},
		{"xr.request, an app that is not a string", xrTool, merge1(xrArgs(), "app", 7), okDef, all, "T2", []string{"R023"}},
		{"xr.request, the tenants repository is not on repos.allow", xrTool, xrArgs(), afDef([]string{"o/parachute"}, afProfile), all, "T2", []string{"R014"}},
		{"xr.request, the definition has no airframe section", xrTool, xrArgs(), afDef([]string{"o/*"}, `{"glidepath":{"apps":["*"]}}`), all, "T2", []string{"R023"}},
		{"xr.request, a path the definition denies", xrTool, xrArgs(), afDef([]string{"o/*"}, afProfile, "tenants/**"), all, "T2", []string{"GH-PATH"}},
		{"airframe.apply, no definition", "airframe.apply", map[string]any{"spec": spec}, &definitions.Definition{Name: "someone-else"}, all, "T2", []string{"R003"}},
		{"airframe.apply, not in the grant", "airframe.apply", map[string]any{"spec": spec}, okDef, without(all, "airframe.apply"), "T2", []string{"R006"}},
		{"airframe.apply, tier above the ceiling", "airframe.apply", map[string]any{"spec": spec}, okDef, all, "T0", []string{"R007"}},
		{"airframe.apply, another app", "airframe.apply", map[string]any{"spec": other}, okDef, all, "T2", []string{"R023"}},
		{"airframe.apply, the gitops repository is not on repos.allow", "airframe.apply", map[string]any{"spec": spec}, afDef([]string{tenantsRepo, "o/parachute"}, afProfile), all, "T2", []string{"R014"}},
		{"airframe.apply, the tenants repository is not on repos.allow", "airframe.apply", map[string]any{"spec": spec}, afDef([]string{"o/parachute", "o/gitops-parachute"}, afProfile), all, "T2", []string{"R014"}},
		{"airframe.apply, proposeFlight for a definition that may not", "airframe.apply", map[string]any{"spec": spec, "proposeFlight": true},
			afDef([]string{"o/*"}, `{"airframe":{"apps":["*"]}}`), all, "T2", []string{"R025"}},
		{"airframe.apply, proposeFlight for a definition that says false", "airframe.apply", map[string]any{"spec": spec, "proposeFlight": true},
			afDef([]string{"o/*"}, `{"airframe":{"apps":["*"],"proposeFlight":false}}`), all, "T2", []string{"R025"}},
		{"airframe.plan, another app", "airframe.plan", map[string]any{"spec": other}, okDef, all, "T2", []string{"R023"}},
		{"airframe.status, another app", "airframe.status", merge1(status, "app", "payments"), okDef, all, "T2", []string{"R023"}},
		{"airframe.status, no app", "airframe.status", map[string]any{}, okDef, all, "T2", []string{"R023"}},
		{"airframe.status, a glob that is not the app", "airframe.status", merge1(status, "app", "flight-api-x"), okDef, all, "T2", []string{"R023"}},
		{"airframe.describe, another app", "airframe.describe", map[string]any{"app": "payments", "env": "dev"}, okDef, all, "T2", []string{"R023"}},
		{"airframe.verify, another app", "airframe.verify", map[string]any{"app": "payments", "kind": "MessageQueue", "name": "x"}, okDef, all, "T2", []string{"R023"}},
		{"airframe.capabilities, not in the grant", "airframe.capabilities", map[string]any{}, okDef, without(all, "airframe.capabilities"), "T2", []string{"R006"}},
		{"airframe.explain, not in the grant", "airframe.explain", map[string]any{"rule": "AF-XR-001"}, okDef, without(all, "airframe.explain"), "T2", []string{"R006"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gh := afGitHub()
			defer gh.Close()
			e := newAFEnv(t, gh, nil, c.def, c.names, c.ceiling, "s-1", true)
			// the definition's own name is the session's agent, except where the test says there is none
			_, err := e.call(c.tool, c.args)
			refusedWith(t, err, c.rules...)
			if n := len(gh.Calls()); n != 0 {
				t.Fatalf("%d requests were made for a call that is refused: %v", n, gh.Calls()[0].Target)
			}
			if e.kube.count() != 0 {
				t.Fatal("the cluster was read")
			}
		})
	}
}

func merge1(m map[string]any, k string, v any) map[string]any {
	out := map[string]any{}
	for a, b := range m {
		out[a] = b
	}
	out[k] = v
	return out
}

func TestThereIsNoValidatorAndXRRequestIsOffUnlessSaidSo(t *testing.T) {
	gh := afGitHub()
	defer gh.Close()
	e := newAFEnv(t, gh, nil, afDef([]string{"o/*"}, afProfile), allTools(), "T2", "s-1", false)
	if e.h["airframe.validate"] != nil {
		t.Fatal("airframe.validate has a handler: the image carries no airframe-validate")
	}
	if e.h["xr.request"] != nil {
		t.Fatal("xr.request has a handler with XR_REQUEST_ENABLED off")
	}
	if e.h["airframe.apply"] == nil || e.h["airframe.plan"] == nil || e.h["airframe.status"] == nil {
		t.Fatal("the tools that are on have no handler")
	}
}

// -- xr.request ------------------------------------------------------------------------------------------------------------

func TestAnXRRequestOpensOnePullRequestWithOneFileInTheAppsOwnDirectory(t *testing.T) {
	gh := afGitHub()
	defer gh.Close()
	e := newAFEnv(t, gh, nil, afDef([]string{"o/*"}, afProfile), allTools(), "T2", "s-1", true)
	out, err := e.call(xrTool, xrArgs())
	if err != nil {
		t.Fatal(err)
	}
	m := asMap(t, out)
	if m["path"] != "tenants/flight-api/xr-requests/flight-api-orders.yaml" || m["namespace"] != "app-flight-api-cicd" || m["kind"] != "MessageQueue" {
		t.Fatalf("%v", m)
	}
	prs := gh.PullRequests(tenantsRepo)
	if len(prs) != 1 || !strings.HasPrefix(prs[0].Head, "agent/") || prs[0].Base != "main" {
		t.Fatalf("%+v", prs)
	}
	// the tenants repository's token is asked for with no app; every other repository's with its app
	for _, b := range brokerBodies(gh) {
		if b["repo"] == "gitops-tenants" && b["app"] != "" {
			t.Fatalf("the tenants token was asked for as an app's: %v", b)
		}
	}
	files := gh.CommitFiles(tenantsRepo, prs[0].CommitSHA)
	if len(files) != 1 {
		t.Fatalf("the pull request carries %d files", len(files))
	}
	var doc map[string]any
	for path, content := range files {
		if path != "tenants/flight-api/xr-requests/flight-api-orders.yaml" {
			t.Fatalf("file %s", path)
		}
		dec := yaml.NewDecoder(strings.NewReader(content))
		n := 0
		for {
			var d map[string]any
			if err := dec.Decode(&d); err != nil {
				break
			}
			doc, n = d, n+1
		}
		if n != 1 {
			t.Fatalf("%d documents", n)
		}
	}
	md := doc["metadata"].(map[string]any)
	if doc["kind"] != "MessageQueue" || doc["apiVersion"] != "catalog.hangar.io/v1alpha1" || md["namespace"] != "app-flight-api-cicd" || md["name"] != "flight-api-orders" {
		t.Fatalf("%v", doc)
	}
	if gh.Branch(tenantsRepo, "main") != fakes.NewGitHub(tenantsRepo).Branch(tenantsRepo, "main") {
		t.Fatal("main moved")
	}
	assertClosedList(t, gh)
}

func assertClosedList(t *testing.T, gh *fakes.GitHub) {
	t.Helper()
	for _, c := range gh.Calls() {
		key := c.Method + " " + strings.SplitN(c.Target, "?", 2)[0]
		ok := false
		for _, re := range allowedGitHub {
			ok = ok || re.MatchString(key)
		}
		if !ok || (c.Method != "GET" && c.Method != "POST") {
			t.Errorf("a request outside the closed list: %s", key)
		}
	}
}

func TestAnXRRequestForAKindThatBecomesAuthorityOrPlatformIsRefusedWithNoRequest(t *testing.T) {
	gh := afGitHub()
	defer gh.Close()
	e := newAFEnv(t, gh, nil, afDef([]string{"o/*"}, afProfile), allTools(), "T2", "s-1", true)
	for _, kind := range []string{"Agent", "AgentRun", "AgentRepository", "SecretStore", "Secret", "ClusterRoleBinding", "Composition", "ProviderConfig", "Application", "AppProject", "Namespace"} {
		_, err := e.call(xrTool, merge1(xrArgs(), "kind", kind))
		refusedWith(t, err, "XR-KIND")
	}
	// and not by a name or an app that points at one, or a path that leaves the app's directory
	for _, n := range []string{"../agents/x", "flight-api-x/../../y", "agent-x", "flight-api-"} {
		_, err := e.call(xrTool, merge1(xrArgs(), "name", n))
		refusedWith(t, err, "XR-NAME")
	}
	if len(gh.Calls()) != 0 || len(gh.PullRequests(tenantsRepo)) != 0 {
		t.Fatal("something was sent")
	}
}

func TestAnXRRequestThatWasAnsweredIsNotMadeTwice(t *testing.T) {
	gh := afGitHub()
	defer gh.Close()
	e := newAFEnv(t, gh, nil, afDef([]string{"o/*"}, afProfile), allTools(), "T2", "s-1", true)
	first, err := e.call(xrTool, xrArgs())
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.call(xrTool, xrArgs())
	if err != nil {
		t.Fatal(err)
	}
	if asMap(t, second)["pr"] != asMap(t, first)["pr"] || asMap(t, second)["resumed"] != true || len(gh.PullRequests(tenantsRepo)) != 1 {
		t.Fatalf("%v then %v: %d pull requests", first, second, len(gh.PullRequests(tenantsRepo)))
	}
	if n := len(gh.Branches(tenantsRepo)); n != 2 { // main and the one
		t.Fatalf("branches %v", gh.Branches(tenantsRepo))
	}
}

func TestALostAnswerToAnXRRequestNeverMakesASecondPullRequestOrFile(t *testing.T) {
	for _, drop := range []string{"POST /repos/o/gitops-tenants/pulls", "POST /repos/o/gitops-tenants/git/refs", "POST /repos/o/gitops-tenants/git/commits", "POST /repos/o/gitops-tenants/git/trees"} {
		t.Run(drop, func(t *testing.T) {
			gh := afGitHub()
			defer gh.Close()
			gh.DropAfter = func(key string, n int) bool { return key == drop && n == 1 }
			e := newAFEnv(t, gh, nil, afDef([]string{"o/*"}, afProfile), allTools(), "T2", "s-1", true)
			_, err1 := e.call(xrTool, xrArgs())
			out, err := e.call(xrTool, xrArgs())
			if err != nil {
				t.Fatalf("the retry: %v (the first: %v)", err, err1)
			}
			if got := len(gh.PullRequests(tenantsRepo)); got != 1 {
				t.Fatalf("%d pull requests after a lost answer to %s: %v", got, drop, out)
			}
			// the one branch holds the one file; no second branch for the same request
			agentBranches := 0
			for _, b := range gh.Branches(tenantsRepo) {
				if strings.HasPrefix(b, "agent/") {
					agentBranches++
				}
			}
			if agentBranches != 1 {
				t.Fatalf("branches %v", gh.Branches(tenantsRepo))
			}
		})
	}
}

func TestABranchThatIsNotThisSessionsIsNeverBuiltOnForAnXRRequest(t *testing.T) {
	gh := afGitHub()
	defer gh.Close()
	e := newAFEnv(t, gh, nil, afDef([]string{"o/*"}, afProfile), allTools(), "T2", "s-1", true)
	built, err := airframe.BuildXR(airframe.XRRequest{App: "flight-api", Env: "dev", Kind: "MessageQueue", Name: "flight-api-orders",
		Spec: mustArgs(t, xrArgs()["spec"])}, airframeFixtureRegistry)
	if err != nil {
		t.Fatal(err)
	}
	branch := branchForXR(SessionFacts{ID: "s-1", Task: "t-abc123"}, "flight-api-orders")
	// someone else made the branch, with the right file in it
	gh.SeedBranch(tenantsRepo, branch, map[string]string{built.Path: built.Text}, "just a commit by a person")
	_, err = e.call(xrTool, xrArgs())
	refusedWith(t, err, "APPLY-BRANCH-FOREIGN")
	if len(gh.PullRequests(tenantsRepo)) != 0 {
		t.Fatal("a pull request was opened on a branch that is not this session's")
	}
	// another session's name is its own: it does not meet this one's branch at all
	if branchForXR(SessionFacts{ID: "s-2", Task: "t-abc123"}, "flight-api-orders") == branch {
		t.Fatal("two sessions name one branch")
	}
}

func mustArgs(t *testing.T, v any) any {
	t.Helper()
	out, err := airframe.FromArgs(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOnlyTheConfiguredTenantsRepositoryGetsATokenWithNoApp(t *testing.T) {
	gh := afGitHub()
	defer gh.Close()
	e := newAFEnv(t, gh, nil, afDef([]string{"o/*"}, afProfile), allTools(), "T2", "s-1", true)
	for _, repo := range []string{"parachute", "gitops-parachute", "payments", "gitops-tenants-other"} {
		_, err := e.git.mintFor(context.Background(), "o", repo, true)
		refusedWith(t, err, "XR-TENANTS-TOKEN")
	}
	if len(gh.Calls()) != 0 {
		t.Fatal("the broker was asked")
	}
	// and the tenants repository is never asked for as an app's
	if _, err := e.git.mintFor(context.Background(), "o", "gitops-tenants", false); err != nil {
		t.Fatal(err)
	}
	if b := brokerBodies(gh); len(b) != 1 || b[0]["app"] != "tenants" {
		// asked for as any repository is: by its name (the broker refuses it as no app's, which is why xr.request asks the other way)
		t.Fatalf("%v", b)
	}
	// a gateway with no tenants repository cannot ask at all
	g2, err := NewGitHub(GitHubOptions{API: gh.Srv.URL, BrokerURL: gh.BrokerURL(), TokenFile: e.git.tokenFile, Owners: []string{"o"}, AllowHTTP: true, AllowHTTPAPI: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g2.mintFor(context.Background(), "o", "gitops-tenants", true); err == nil {
		t.Fatal("a token for the tenants repository with none configured")
	}
	if _, err := NewGitHub(GitHubOptions{API: gh.Srv.URL, BrokerURL: gh.BrokerURL(), TokenFile: e.git.tokenFile, Owners: []string{"o"}, TenantsRepo: "evil/gitops-tenants", AllowHTTP: true, AllowHTTPAPI: true}); err == nil {
		t.Fatal("a tenants repository under an owner the gateway is not configured for")
	}
}

// -- airframe.apply --------------------------------------------------------------------------------------------------------

func TestAnApplyOpensTheTenantsPullRequestWithTheFilesThePlanMakesAndNoOther(t *testing.T) {
	gh := afGitHub()
	defer gh.Close()
	e := newAFEnv(t, gh, nil, afDef([]string{"o/*"}, afProfile), allTools(), "T2", "s-1", true)
	out, err := e.call("airframe.apply", map[string]any{"spec": appSpec("a service")})
	if err != nil {
		t.Fatal(err)
	}
	m := asMap(t, out)
	if m["state"] != "awaiting-merge" || m["repo"] != tenantsRepo || m["step"] != "tenants" || m["resumed"] != false {
		t.Fatalf("%v", m)
	}
	prs := gh.PullRequests(tenantsRepo)
	if len(prs) != 1 {
		t.Fatal("no pull request")
	}
	files := gh.CommitFiles(tenantsRepo, prs[0].CommitSHA)
	if len(files) != 2 {
		t.Fatalf("%d files: the app's XR and the one Flight environment's", len(files))
	}
	for path, content := range files {
		if !strings.HasPrefix(path, "tenants/parachute/xr-requests/") {
			t.Fatalf("a file outside the app's directory: %s", path)
		}
		var d map[string]any
		dec := yaml.NewDecoder(strings.NewReader(content))
		if err := dec.Decode(&d); err != nil {
			t.Fatal(err)
		}
		if k := d["kind"]; k != "PythonApplication" && k != "ApplicationEnvironment" {
			t.Fatalf("%s is a %v", path, k)
		}
		if d["metadata"].(map[string]any)["namespace"] != "app-parachute-cicd" {
			t.Fatalf("%s: %v", path, d["metadata"])
		}
		var extra map[string]any
		if dec.Decode(&extra) == nil {
			t.Fatalf("%s holds a second document", path)
		}
	}
	// the receipt names the files, from what GitHub holds, on a second call; no second pull request
	again, err := e.call("airframe.apply", map[string]any{"spec": appSpec("a service")})
	if err != nil {
		t.Fatal(err)
	}
	if am := asMap(t, again); am["resumed"] != true || am["pr"] != m["pr"] || len(gh.PullRequests(tenantsRepo)) != 1 {
		t.Fatalf("%v", am)
	}
	assertClosedList(t, gh)
}

func TestAnAppSpecDescriptionCannotMakeAnotherDocumentOrAnotherKind(t *testing.T) {
	opened := 0
	for _, d := range []string{"x\n---\nkind: Agent\nmetadata: {name: evil}", "&a x", "!!python/object:os.system x", "- kind: Agent", "a: b", "\"", "x\r\n---\r\nkind: Agent", " --- kind: Agent"} {
		gh := afGitHub()
		e := newAFEnv(t, gh, nil, afDef([]string{"o/*"}, afProfile), allTools(), "T2", "s-1", true)
		out, err := e.call("airframe.apply", map[string]any{"spec": appSpec(d)})
		if err != nil {
			gh.Close()
			continue // refused by the schema or the checks: fine
		}
		if asMap(t, out)["state"] != "awaiting-merge" {
			gh.Close()
			continue
		}
		opened++
		prs := gh.PullRequests(tenantsRepo)
		for path, content := range gh.CommitFiles(tenantsRepo, prs[0].CommitSHA) {
			dec := yaml.NewDecoder(strings.NewReader(content))
			n := 0
			for {
				var doc map[string]any
				if dec.Decode(&doc) != nil {
					break
				}
				n++
				if doc["kind"] == "Agent" {
					t.Fatalf("%q made an Agent in %s", d, path)
				}
			}
			if n != 1 {
				t.Fatalf("%q made %d documents in %s", d, n, path)
			}
			if err := airframe.ReadBackXR("# x\n"+content, &definitions.OMap{M: map[string]any{}}); err != nil && strings.Contains(err.Error(), "anchor") {
				t.Fatalf("%q put an anchor in %s", d, path)
			}
		}
		gh.Close()
	}
	if opened < 4 {
		t.Fatalf("only %d of the descriptions reached a pull request: the test proves nothing", opened)
	}
}

func TestAnApplyForARepositoryTheAgentMayNotWriteSendsNothing(t *testing.T) {
	gh := afGitHub()
	defer gh.Close()
	// the tenants repository is allowed, the others are not
	e := newAFEnv(t, gh, nil, afDef([]string{tenantsRepo}, afProfile), allTools(), "T2", "s-1", true)
	_, err := e.call("airframe.apply", map[string]any{"spec": appSpec("a service")})
	refusedWith(t, err, "R014")
	if len(gh.Calls()) != 0 {
		t.Fatal("a request was made")
	}
}

func TestALostAnswerToAnApplyNeverMakesASecondPullRequest(t *testing.T) {
	for _, drop := range []string{"POST /repos/o/gitops-tenants/pulls", "POST /repos/o/gitops-tenants/git/refs"} {
		gh := afGitHub()
		gh.DropAfter = func(key string, n int) bool { return key == drop && n == 1 }
		e := newAFEnv(t, gh, nil, afDef([]string{"o/*"}, afProfile), allTools(), "T2", "s-1", true)
		_, _ = e.call("airframe.apply", map[string]any{"spec": appSpec("a service")})
		out, err := e.call("airframe.apply", map[string]any{"spec": appSpec("a service")})
		if err != nil {
			t.Fatalf("%s: %v", drop, err)
		}
		if n := len(gh.PullRequests(tenantsRepo)); n != 1 {
			t.Fatalf("%s: %d pull requests: %v", drop, n, out)
		}
		gh.Close()
	}
}

func TestAnApplyOfAnotherSessionsBranchIsRefusedNotBuiltOn(t *testing.T) {
	gh := afGitHub()
	defer gh.Close()
	e := newAFEnv(t, gh, nil, afDef([]string{"o/*"}, afProfile), allTools(), "T2", "s-1", true)
	// the branch this session would use already holds a commit that is not its own, with an open pull request
	branch := branchForStep("parachute", "tenants", "s-1")
	gh.SeedBranch(tenantsRepo, branch, map[string]string{"tenants/parachute/xr-requests/parachute.yaml": "x: 1\n"}, "a person's commit")
	gh.SeedPR(tenantsRepo, branch)
	_, err := e.call("airframe.apply", map[string]any{"spec": appSpec("a service")})
	refusedWith(t, err, "APPLY-BRANCH-FOREIGN")
	if len(gh.PullRequests(tenantsRepo)) != 1 {
		t.Fatal("a second pull request")
	}
}

func TestStatusToolsReadOnlyWhatTheScopeAllows(t *testing.T) {
	gh := afGitHub()
	defer gh.Close()
	e := newAFEnv(t, gh, nil, afDef([]string{"o/*"}, afProfile), allTools(), "T2", "s-1", true)
	for _, bad := range []map[string]any{{"app": "flight-api/../x"}, {"app": "flight-api\n"}, {"app": 7}, {"app": []any{"flight-api"}}} {
		if _, err := e.call("airframe.status", bad); err == nil {
			t.Errorf("%v was answered", bad)
		}
	}
	if _, err := e.call("airframe.status", map[string]any{"app": "flight-api", "env": 3}); err == nil {
		t.Error("an env that is not a string was answered")
	}
	if _, err := e.call("airframe.status", map[string]any{"app": "flight-api"}); err != nil {
		t.Fatal(err)
	}
	if e.kube.count() == 0 {
		t.Fatal("nothing was read")
	}
	if len(gh.Calls()) != 0 {
		t.Fatal("a state read went to GitHub")
	}
}

func TestOnboardingAndPathReadsTakeNoAgentControlledURLPart(t *testing.T) {
	gh := afGitHub()
	defer gh.Close()
	e := newAFEnv(t, gh, nil, afDef([]string{"o/*"}, afProfile), allTools(), "T2", "s-1", true)
	ctx := context.Background()
	for _, repo := range []string{"o/../x", "o/a b", "o/x?y=1", "o/x#f", "evil/parachute", "o/gitops-parachute/../x", "o/%2e%2e", "o/"} {
		if _, err := e.git.OnboardingPRs(ctx, repo, false); err == nil {
			t.Errorf("onboarding read of %q", repo)
		}
		if _, err := e.git.RepoReady(ctx, repo, false); err == nil {
			t.Errorf("ready read of %q", repo)
		}
		if _, err := e.git.PRPaths(ctx, repo, 1, false); err == nil {
			t.Errorf("paths read of %q", repo)
		}
	}
	for _, n := range []int64{0, -1} {
		if _, err := e.git.PRPaths(ctx, "o/parachute", n, false); err == nil {
			t.Errorf("paths of pull request %d", n)
		}
	}
	if len(gh.Calls()) != 0 {
		t.Fatalf("a request was made for a bad name: %v", gh.Calls()[0].Target)
	}
	var d *core.Denied
	if _, err := e.git.ReadFileIn(ctx, "o/parachute", "../etc/passwd", false); !errors.As(err, &d) {
		t.Fatalf("%v", err)
	}
}

// the pull request files list is read as one page of at most 100, and what is read of it is bounded
func TestAPullRequestsFileListIsOnePageAndItsNamesAreBounded(t *testing.T) {
	var seen []string
	var body = func() []byte {
		var l []any
		for i := 0; i < 300; i++ {
			l = append(l, map[string]any{"filename": "f" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + strings.Repeat("0", 3)})
		}
		l = append(l, map[string]any{"filename": strings.Repeat("a", 5000)}, map[string]any{"filename": 5}, "x")
		b, _ := json.Marshal(l)
		return b
	}()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		if strings.HasSuffix(r.URL.Path, "/agent-installation-token") {
			_, _ = w.Write([]byte(`{"token":"ghs_faketoken"}`))
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	g, err := NewGitHub(GitHubOptions{API: srv.URL, BrokerURL: srv.URL + "/b", TokenFile: afTemp(t, "agent-token-value"), Owners: []string{"o"}, AllowHTTP: true, AllowHTTPAPI: true})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := g.PRPaths(context.Background(), "o/parachute", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) > 100 {
		t.Fatalf("%d paths", len(paths))
	}
	for _, p := range paths {
		if len(p) > 1024 {
			t.Fatal("a path of 5000 characters")
		}
	}
	if len(seen) != 2 || seen[1] != "GET /repos/o/parachute/pulls/5/files?per_page=100" {
		t.Fatalf("%v", seen)
	}
}

func afTemp(t *testing.T, v string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
