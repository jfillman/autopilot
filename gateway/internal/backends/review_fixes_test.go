package backends

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/backends/fakes"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// The independent review of #109: each test below was written to fail on b3151ca.

func openDef(repos ...string) *definitions.Definition {
	reg := tools.Default()
	return &definitions.Definition{Name: "fixture-agent", Kind: "task", ReposAllow: repos, DenyPaths: reg.BaselineDenyPaths,
		RawProfile: json.RawMessage(`{"glidepath":{"apps":["gate-*"]}}`)}
}

func brokerBodies(gh *fakes.GitHub) []map[string]string {
	var out []map[string]string
	for _, c := range gh.Calls() {
		if strings.HasSuffix(c.Target, "/agent-installation-token") {
			m := map[string]string{}
			_ = json.Unmarshal(c.Body, &m)
			out = append(out, m)
		}
	}
	return out
}

// 1. A name is one name whatever its case: GitHub resolves them without it, so the checks must too.
func TestEveryCaseVariantOfAReleaseRepositoryIsTheReleaseRepository(t *testing.T) {
	gh := fakes.NewGitHub("o/gitops-gate-api", "o/gate-api", "o/gitops-infra-gate-api")
	defer gh.Close()
	gpf := fakes.NewGlidepath()
	defer gpf.Close()
	e := newFakeEnvWith(t, gh, gpf, nil, "s-1", openDef("o/*"), allTools(), "T3")
	for _, repo := range []string{"o/GITOPS-gate-api", "o/GitOps-gate-api", "O/gitops-gate-api", "O/GITOPS-GATE-API", "o/gitOPS-gate-api"} {
		a := prArgs("agent/t-1")
		a["repo"] = repo
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "R012")
	}
	for _, repo := range []string{"o/GITOPS-infra-gate-api", "O/GitOps-Infra-Gate-Api"} {
		_, err := e.call("glidepath.cicd.propose", map[string]any{"app": "gate-api", "repo": repo, "patch": map[string]any{"a": 1}, "title": "x"})
		refusedWith(t, err, "R012")
	}
	if len(gh.Calls()) != 0 || len(gpf.Calls()) != 0 {
		t.Fatalf("a variant reached an upstream: %d %d", len(gh.Calls()), len(gpf.Calls()))
	}
	// the one canonical, lower case value is what is checked, asked of the broker, put in the URL and answered
	a := prArgs("agent/t-1")
	a["repo"], a["title"] = "O/GATE-API", "t"
	out, err := e.call("repo.pr.open", a)
	if err != nil {
		t.Fatal(err)
	}
	b := brokerBodies(gh)
	if len(b) != 1 || b[0]["owner"] != "o" || b[0]["repo"] != "gate-api" || b[0]["app"] != "gate-api" {
		t.Fatalf("the broker is asked about %v", b)
	}
	for _, c := range gh.Calls() {
		if strings.Contains(c.Target, "GATE") || strings.Contains(c.Target, "/O/") {
			t.Fatalf("a request with the agent's case: %s", c.Target)
		}
	}
	if _ = out; len(gh.PullRequests("o/gate-api")) != 1 {
		t.Fatal("no pull request")
	}
	// the allow list matches without case, too
	e2 := newFakeEnvWith(t, gh, gpf, nil, "s-1", openDef("O/Gate-*"), allTools(), "T3")
	a = prArgs("agent/t-2")
	a["repo"] = "o/GATE-api"
	if _, err := e2.call("repo.pr.open", a); err != nil {
		t.Fatal(err)
	}
	// a name that is another spelling of a repository is refused, not trimmed
	for _, repo := range []string{"o/gate-api.git", "o/gate-api.GIT", "o/gate-api.Git"} {
		a := prArgs("agent/t-3")
		a["repo"] = repo
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-REPO")
		_, err = e.call("repo.file.get", map[string]any{"repo": repo, "path": "a"})
		refusedWith(t, err, "GH-REPO")
	}
	if _, err := e.call("repo.file.get", map[string]any{"repo": "O/Gate-API", "path": "a"}); err != nil {
		t.Fatal(err)
	}
	last := gh.Calls()[len(gh.Calls())-1]
	if !strings.HasPrefix(last.Target, "/repos/o/gate-api/") {
		t.Fatalf("%s", last.Target)
	}
}

// 2. A blob written at a path replaces the tree the base has there.
func TestAFileIsNeverWrittenWhereTheBaseHasADirectory(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	gh.SeedFiles("o/gate-api", map[string]string{"deploy/prod/values.yaml": "a", "deploy/prod/release.yaml": "r", "charts/x/Chart.yaml": "c", "apps/a/AppProject.yaml": "p", "src/app.py": "x"})
	def := openDef("o/*")
	def.DenyPaths = append(append([]string{}, def.DenyPaths...), "deploy/prod/values.yaml")
	e := newFakeEnvWith(t, gh, nil, nil, "s-1", def, allTools(), "T3")
	for _, p := range []string{"deploy", "deploy/prod", "charts", "charts/x", "apps", "apps/a", "DEPLOY", "Deploy/Prod"} {
		a := prArgs("agent/t-1")
		a["files"] = []any{map[string]any{"path": p, "content": "x"}}
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-PATH")
	}
	for _, c := range gh.Calls() {
		if c.Method == "POST" && !strings.HasSuffix(c.Target, "agent-installation-token") {
			t.Fatalf("a write went out: %s", c.Target)
		}
	}
	// names that mean another name on a file system
	for _, p := range []string{"a/b ", "a /b", "a/b.", "a/.github.", "CODEOW~1", "x/GITHUB~1/y", "a/.tekton ", "a:b", "x/.github::$DATA"} {
		a := prArgs("agent/t-2")
		a["files"] = []any{map[string]any{"path": p, "content": "x"}}
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-PATH", "GH-FILES")
	}
	// a new file in a directory the base has is fine, and so is a file over a file
	a := prArgs("agent/t-3")
	a["files"] = []any{map[string]any{"path": "deploy/prod/new.yaml", "content": "x"}, map[string]any{"path": "src/app.py", "content": "y"}}
	if _, err := e.call("repo.pr.open", a); err != nil {
		t.Fatal(err)
	}
}

// 3. The base is the default branch; the head is the gateway's own namespace.
func TestTheBaseIsTheDefaultBranchAndTheHeadIsTheGatewaysOwn(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	gh.SeedBranch("o/gate-api", "release/1", nil, "x")
	for _, base := range []string{"release/1", "gh-pages", "agent/other", "refs/heads/main", "MAIN"} {
		a := prArgs("agent/t-1")
		a["base"] = base
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-BASE", "GH-BRANCH")
	}
	for _, head := range []string{"feature/x", "x", "alice/fix", "AGENT/x", "agents/x", "agent", "agent/", "release/1"} {
		_, err := e.call("repo.pr.open", prArgs(head))
		refusedWith(t, err, "GH-BRANCH")
	}
	if _, err := e.call("repo.pr.open", prArgs("agent/t-ok")); err != nil {
		t.Fatal(err)
	}
	// the base ref given to the cicd.yaml check is a ref, and bounded
	gpf := fakes.NewGlidepath()
	defer gpf.Close()
	e = newFakeEnv(t, gh, gpf, nil, "s-1")
	a := prArgs("agent/t-9")
	a["files"] = []any{map[string]any{"path": "cicd.yaml", "content": "a: 1\n"}}
	for _, base := range []any{"a b", "../x", strings.Repeat("a", 300), 5, "x\n"} {
		a["base"] = base
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-BRANCH")
	}
	if len(gpf.Calls()) != 0 {
		t.Fatalf("a bad base was forwarded: %d", len(gpf.Calls()))
	}
}

func TestAResumedBranchMustBeThisSessionsOwn(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	files := map[string]string{"src/app.py": "print('fixed')\n"}
	gh.SeedBranch("o/gate-api", "agent/theirs", files, "someone's branch")
	gh.SeedBranch("o/gate-api", "agent/other-session", files, "x\n\nHangar-Session: s-2")
	gh.SeedBranch("o/gate-api", "agent/mine", files, "x\n\nHangar-Session: s-1")
	for branch, ok := range map[string]bool{"agent/theirs": false, "agent/other-session": false, "agent/mine": true} {
		in := OpenPRInput{Repo: "o/gate-api", Branch: branch, Title: "t", Files: []File{{Path: "src/app.py", Content: files["src/app.py"]}},
			Meta: map[string]string{"session": "s-1", "agent": "fixture-agent"}}
		_, err := e.git.EnsurePR(context.Background(), in)
		if ok != (err == nil) {
			t.Fatalf("%s: %v", branch, err)
		}
	}
	// with no session to check against nothing is resumed
	in := OpenPRInput{Repo: "o/gate-api", Branch: "agent/mine", Title: "t", Files: []File{{Path: "src/app.py", Content: files["src/app.py"]}}}
	if _, err := e.git.EnsurePR(context.Background(), in); err == nil {
		t.Fatal("resumed with no session")
	}
}

// 5. Text that goes to people.
func TestTextForAPullRequestNotifiesNobodyAndLinksNothing(t *testing.T) {
	for in, want := range map[string]string{
		"`@alice": "`@​alice", "see #12": "see #​12", "o/r#7": "o/r#​7", "Fixes #3": "F​ixes #​3",
		"closes o/r#4": "c​loses o/r#​4", "RESOLVED #1": "R​ESOLVED #​1", "a<!-- hidden -->b": "ab", "x <!-- open": "x ",
		"<img src=x onerror=1>": "&lt;img src=x onerror=1>", "![i](http://e/x.png)": "!​[i](http://e/x.png)", "### heading": "### heading",
		"mail a@b.c": "mail a@b.c", "plain": "plain",
	} {
		if got := defang(in); got != want {
			t.Errorf("defang(%q) = %q, want %q", in, got, want)
		}
	}
	// the size cap is of the text as it goes out
	long := strings.Repeat("@a ", 30000)
	if got := []rune(defang(long)); len(got) > 65000 {
		t.Errorf("%d runes", len(got))
	}
}

// 4. A bearer token does not cross a network in the clear unless the deployment said so; a short credential is no credential.
func TestPlainHTTPNeedsItsSetting(t *testing.T) {
	tok := writeTemp(t, "a-token-value-1")
	if _, err := NewGlidepath("http://glidepath-api.platform.svc:8080", tok, false, 0, false); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("%v", err)
	}
	if _, err := NewGlidepath("http://glidepath-api.platform.svc:8080", tok, false, 0, true); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGlidepath("https://glidepath-api.example", tok, false, 0, false); err != nil {
		t.Fatal(err)
	}
	o := GitHubOptions{BrokerURL: "http://broker.platform.svc:8080", TokenFile: tok, Owners: []string{"o"}}
	if _, err := NewGitHub(o); err == nil || !strings.Contains(err.Error(), "BROKER_URL") {
		t.Fatalf("%v", err)
	}
	o.API = "http://api.example"
	o.BrokerURL = "https://broker.example"
	if _, err := NewGitHub(o); err == nil {
		t.Fatal("a plain http API URL was taken")
	}
	o.AllowHTTP = true
	if _, err := NewGitHub(o); err != nil {
		t.Fatal(err)
	}
}

func TestACredentialTooShortIsNotSentAndRedactionTakesOutAnyToken(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	gp, err := NewGlidepath(gh.Srv.URL, writeTemp(t, "short"), false, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gp.Contract(context.Background(), "p"); err == nil || len(gh.Calls()) != 0 {
		t.Fatalf("a short token was sent: %v", err)
	}
	if got := string(redact([]byte("a b a"), "a")); got != "[redacted] b [redacted]" {
		t.Fatalf("%q", got)
	}
}

// 6. argo.sync.lower is in scope for the agent's definition, as the other tools are.
func TestAnApplicationOutsideTheAgentsAppsIsNotSynced(t *testing.T) {
	path := "/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications/other-dev"
	obj := appObject("other-lower")
	obj["metadata"].(map[string]any)["name"] = "other-dev"
	own := "/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications/gate-api-dev"
	labelled := appObject("gate-api-lower")
	labelled["metadata"].(map[string]any)["labels"] = map[string]any{"hangar.io/app": "other"}
	kube := fakes.NewKube(map[string]map[string]any{path: obj, own: appObject("gate-api-lower"),
		"/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications/odd-dev": labelled})
	defer kube.Close()
	e := newFakeEnvWith(t, nil, nil, kube, "s-1", openDef("o/*"), allTools(), "T3")
	_, err := e.call("argo.sync.lower", map[string]any{"app": "other-dev", "project": "other-lower"})
	refusedWith(t, err, "R022")
	if len(kube.Calls()) != 0 {
		t.Fatalf("an app outside the agent's reached the API server: %d", len(kube.Calls()))
	}
	_, err = e.call("argo.sync.lower", map[string]any{"app": "odd-dev", "project": "gate-api-lower"})
	refusedWith(t, err, "R022")
	if kube.PatchCount() != 0 {
		t.Fatal("patched")
	}
	if _, err := e.call("argo.sync.lower", map[string]any{"app": "gate-api-dev", "project": "gate-api-lower"}); err != nil {
		t.Fatal(err)
	}
}
