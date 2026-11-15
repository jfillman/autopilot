package backends

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/backends/fakes"
)

// The second independent review of #109: each test below was written to fail on b0fba3d.

func gitHubWithBase(t *testing.T, files map[string]string) (*fakes.GitHub, *fakeEnv) {
	gh := fakes.NewGitHub("o/gate-api")
	t.Cleanup(gh.Close)
	gh.SeedFiles("o/gate-api", files)
	return gh, newFakeEnvWith(t, gh, nil, nil, "s-1", openDef("o/*"), allTools(), "T3")
}

func filesArg(paths ...string) []any {
	var out []any
	for _, p := range paths {
		out = append(out, map[string]any{"path": p, "content": "x"})
	}
	return out
}

func noWrites(t *testing.T, gh *fakes.GitHub) {
	t.Helper()
	for _, c := range gh.Calls() {
		if c.Method == "POST" && !strings.HasSuffix(c.Target, "agent-installation-token") {
			t.Fatalf("a write went out: %s", c.Target)
		}
	}
}

// 1. A tree written over a protected FILE deletes the file.
func TestAFileIsNeverWrittenUnderAProtectedFileOrAnyFile(t *testing.T) {
	gh, e := gitHubWithBase(t, map[string]string{"CODEOWNERS": "x", "docs/CODEOWNERS": "y", "release.yaml": "r", "a/release.yaml": "r", "src/app.py": "x", "docs/readme.md": "r"})
	for _, p := range []string{"CODEOWNERS/x", "docs/CODEOWNERS/x", "release.yaml/x", "a/release.yaml/x", "codeowners/x", "a/Release.yaml/y/z"} {
		a := prArgs("agent/t-1")
		a["files"] = filesArg(p)
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-PATH")
	}
	// under a file that is not protected: the base is read, whatever the deny list says
	for _, p := range []string{"src/app.py/x", "docs/readme.md/y/z"} {
		a := prArgs("agent/t-2")
		a["files"] = filesArg(p)
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-PATH")
	}
	noWrites(t, gh)
	// a new file next to them is fine
	a := prArgs("agent/t-3")
	a["files"] = filesArg("src/new.py", "docs/new.md")
	if _, err := e.call("repo.pr.open", a); err != nil {
		t.Fatal(err)
	}
}

func TestTheReadsOfTheBaseAreBoundedAndFailClosed(t *testing.T) {
	seed := map[string]string{}
	var paths []string
	for i := 0; i < 50; i++ {
		seed[fmt.Sprintf("p%d/a/b/c/d/e/f/g/h/old.txt", i)] = "x"
		paths = append(paths, fmt.Sprintf("p%d/a/b/c/d/e/f/g/h/new.txt", i))
	}
	gh, e := gitHubWithBase(t, seed)
	a := prArgs("agent/t-1")
	a["files"] = filesArg(paths...)
	_, err := e.call("repo.pr.open", a)
	refusedWith(t, err, "GH-FILES")
	noWrites(t, gh)
	reads := 0
	for _, c := range gh.Calls() {
		if strings.Contains(c.Target, "/contents/") {
			reads++
		}
	}
	if reads > maxBaseReads+1 {
		t.Fatalf("%d reads of the base", reads)
	}
}

// 2. Ownership is the gateway's trailer block, not a line anybody can write.
func TestOwnershipIsTheGatewaysTrailerBlockAndATitleCannotForgeIt(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	for _, ti := range []string{"Hangar-Session: s-1", "hangar-session: s-1", "Hangar-Agent: fixture-agent", "Hangar-Merge: person", " Hangar-Task: t-1"} {
		a := prArgs("agent/t-1")
		a["title"] = ti
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-TITLE")
	}
	files := map[string]string{"src/app.py": "print('fixed')\n"}
	meta := map[string]string{"session": "s-1", "agent": "fixture-agent", "task": "t-abc123", "run": "r-1"}
	good := "t\n\nHangar-Agent: fixture-agent\nHangar-Run: r-1\nHangar-Session: s-1\nHangar-Task: t-abc123"
	for branch, msg := range map[string]string{
		"agent/forged-subject": "Hangar-Session: s-1\n\nHangar-Agent: fixture-agent",
		"agent/extra-trailer":  good + "\nHangar-Merge: person",
		"agent/missing":        "t\n\nHangar-Agent: fixture-agent\nHangar-Session: s-1",
		"agent/no-blank":       "t\nHangar-Agent: fixture-agent\nHangar-Run: r-1\nHangar-Session: s-1\nHangar-Task: t-abc123",
		"agent/other-session":  strings.Replace(good, "s-1", "s-2", 1),
		"agent/body-line":      "t\n\nHangar-Agent: fixture-agent\nHangar-Run: r-1\nHangar-Session: s-1\nHangar-Task: t-abc123\n\nHangar-Session: s-1",
		"agent/twice":          good + "\nHangar-Session: s-1",
	} {
		gh.SeedBranch("o/gate-api", branch, files, msg)
		in := OpenPRInput{Repo: "o/gate-api", Branch: branch, Title: "t", Files: []File{{Path: "src/app.py", Content: files["src/app.py"]}}, Meta: meta}
		_, err := e.git.EnsurePR(context.Background(), in)
		refusedWith(t, err, "APPLY-BRANCH-FOREIGN")
	}
	gh.SeedBranch("o/gate-api", "agent/mine", files, good)
	in := OpenPRInput{Repo: "o/gate-api", Branch: "agent/mine", Title: "t", Files: []File{{Path: "src/app.py", Content: files["src/app.py"]}}, Meta: meta}
	if _, err := e.git.EnsurePR(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	// the PR found for a branch is resumed only when its head is this session's and its files are the ones asked for
	gh.SeedBranch("o/gate-api", "agent/pr-foreign", files, "someone's")
	gh.SeedPR("o/gate-api", "agent/pr-foreign")
	gh.SeedBranch("o/gate-api", "agent/pr-diverged", map[string]string{"src/app.py": "other\n"}, good)
	gh.SeedPR("o/gate-api", "agent/pr-diverged")
	for branch, rule := range map[string]string{"agent/pr-foreign": "APPLY-BRANCH-FOREIGN", "agent/pr-diverged": "APPLY-BRANCH-DIVERGED"} {
		in.Branch = branch
		_, err := e.git.EnsurePR(context.Background(), in)
		refusedWith(t, err, rule)
	}
}

func TestADeterministicBranchNameIsUniqueToItsSession(t *testing.T) {
	a, b := branchFor("t-1", "s-1"), branchFor("t-1", "s-2")
	if a == b || a != branchFor("t-1", "s-1") {
		t.Fatalf("%q %q", a, b)
	}
	if !ValidNewBranch(a, "main") || !ValidNewBranch(branchFor("", "s-9"), "main") || !ValidNewBranch(branchFor("---", "s-9"), "main") {
		t.Fatalf("%q", a)
	}
	if len(a) > 80 {
		t.Fatalf("%q", a)
	}
}

// 3. The cicd.yaml check is asked about the default branch; the lab's plain http is a switch, not a default.
func TestTheCICDCheckIsAskedAboutTheDefaultBranch(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	gh.SetDefaultBranch("o/gate-api", "stable")
	gpf := fakes.NewGlidepath()
	defer gpf.Close()
	e := newFakeEnv(t, gh, gpf, nil, "s-1")
	a := prArgs("agent/t-1")
	a["files"] = []any{map[string]any{"path": "cicd.yaml", "content": "a: 1\n"}}
	e.call("repo.pr.open", a)
	found := false
	for _, c := range gpf.Calls() {
		if strings.HasSuffix(c.Target, "/cicd/validations") {
			var b map[string]any
			_ = json.Unmarshal(c.Body, &b)
			found = true
			if b["baseRef"] != "stable" {
				t.Fatalf("the check was asked about %v", b["baseRef"])
			}
		}
	}
	if !found {
		t.Fatal("no check")
	}
}

func TestPlainHTTPForTheAPIsIsRefusedWithoutItsSwitch(t *testing.T) {
	tok := writeTemp(t, "a-token-value-1")
	o := GitHubOptions{API: "http://api.example", BrokerURL: "https://broker.example", TokenFile: tok, Owners: []string{"o"}, AllowHTTP: true}
	if _, err := NewGitHub(o); err == nil {
		t.Fatal("BROKER_ALLOW_HTTP let the GitHub API URL be plain http")
	}
	o.AllowHTTPAPI = true
	if _, err := NewGitHub(o); err != nil {
		t.Fatal(err)
	}
	k := &KubeReader{Server: "http://k8s.example", TokenFile: tok, Timeout: time.Second}
	if _, err := NewArgoSync(k, []string{"argocd"}); err == nil {
		t.Fatal("a plain http Kubernetes API URL was taken")
	}
	k.AllowHTTP = true
	if _, err := NewArgoSync(k, []string{"argocd"}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewArgoSync(nil, []string{"argocd"}); err != nil {
		t.Fatal(err)
	}
}

func TestAnApplicationWithNoAppLabelIsNotSynced(t *testing.T) {
	path := "/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications/gate-api-dev"
	obj := appObject("gate-api-lower")
	delete(obj["metadata"].(map[string]any), "labels")
	kube := fakes.NewKube(map[string]map[string]any{path: obj})
	defer kube.Close()
	e := newFakeEnvWith(t, nil, nil, kube, "s-1", openDef("o/*"), allTools(), "T3")
	_, err := e.call("argo.sync.lower", map[string]any{"app": "gate-api-dev", "project": "gate-api-lower"})
	refusedWith(t, err, "ARGO-UNLABELLED")
	if kube.PatchCount() != 0 {
		t.Fatal("patched")
	}
}

// 4. The closed list is ten requests, with their methods (S5c added the list of a pull request's files).
func TestTheClosedListOfGitHubRequestsIsExactlyTen(t *testing.T) {
	want := []string{"GET /repos/{owner}/{repo}/git/ref/heads/{branch}", "GET /repos/{owner}/{repo}/git/commits/{sha}", "POST /repos/{owner}/{repo}/git/trees",
		"POST /repos/{owner}/{repo}/git/commits", "POST /repos/{owner}/{repo}/git/refs", "POST /repos/{owner}/{repo}/pulls",
		"GET /repos/{owner}/{repo}/pulls?head={owner}:{branch}&state={all|open}&per_page={5|10}", "GET /repos/{owner}/{repo}/contents/{path}?ref={ref}",
		"GET /repos/{owner}/{repo}", "GET /repos/{owner}/{repo}/pulls/{number}/files?per_page=100"}
	if len(AllowedGitHubRequests) != 10 {
		t.Fatalf("%d entries", len(AllowedGitHubRequests))
	}
	for i, w := range want {
		if AllowedGitHubRequests[i] != w {
			t.Errorf("entry %d is %q, want %q", i, AllowedGitHubRequests[i], w)
		}
	}
}

func TestAPullRequestTitleNotifiesNobody(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	a := prArgs("agent/t-1")
	a["title"] = "@alice please fix #3"
	if _, err := e.call("repo.pr.open", a); err != nil {
		t.Fatal(err)
	}
	if got := gh.PullRequests("o/gate-api")[0].Title; got != "@\u200balice please fix\u200b #\u200b3" {
		t.Fatalf("%q", got)
	}
}
