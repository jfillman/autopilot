package backends

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/backends/fakes"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// What a write does when the answer is lost, or the process is gone, between the remote write and the outcome record. Each
// test runs a fake upstream that does the write and then drops the connection, and holds that the call never makes a
// second pull request, a second run or a second sync, and never says "failed" about a write that may have happened without
// saying it may have.

type fakeEnv struct {
	h    map[string]dispatch.Handler
	sess *session.Session
	git  *GitHub
	gp   *Glidepath
}

func newFakeEnv(t *testing.T, gh *fakes.GitHub, gpf *fakes.Glidepath, kube *fakes.Kube, sessionID string) *fakeEnv {
	t.Helper()
	reg := tools.Default()
	def, names := fixtureDefinition(reg)
	return newFakeEnvWith(t, gh, gpf, kube, sessionID, def, names, "T2")
}

// newFakeEnvWith is newFakeEnv for an agent with the definition, the granted tools and the tier ceiling it is given.
func newFakeEnvWith(t *testing.T, gh *fakes.GitHub, gpf *fakes.Glidepath, kube *fakes.Kube, sessionID string, def *definitions.Definition, names []string, ceiling string) *fakeEnv {
	t.Helper()
	dir := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := HangarConfig{Definition: func(a string) *definitions.Definition {
		if a != def.Name {
			return nil
		}
		return def
	}, Tools: tools.Default()}
	e := &fakeEnv{}
	if gh != nil {
		g, err := NewGitHub(GitHubOptions{API: gh.Srv.URL, BrokerURL: gh.BrokerURL(), TokenFile: write("agent", "agent-token-value"), Owners: []string{"o"}, AllowHTTP: true, AllowHTTPAPI: true, Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		g.retry.base, g.retry.sleep = time.Millisecond, func(context.Context, time.Duration) bool { return true }
		cfg.GitHub, e.git = g, g
	}
	if gpf != nil {
		g, err := NewGlidepath(gpf.Srv.URL, write("gp", "gp-token-value-1"), true, 5*time.Second, true)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Glidepath, e.gp = g, g
	}
	if kube != nil {
		k := &KubeReader{Server: kube.Srv.URL, TokenFile: write("kube", "kube-token-value"), Timeout: 5 * time.Second}
		k.hc = client(nil, time.Second, 4)
		k.AllowHTTP = true
		as, err := NewArgoSync(k, []string{"argocd-apps"})
		if err != nil {
			t.Fatal(err)
		}
		cfg.ArgoSync = as
	}
	e.h = hangarHandlers(cfg)
	run := "r-1"
	e.sess = fixtureSession(map[string]any{"id": sessionID, "principal": "user:jfillman", "agent": "fixture-agent", "task": "t-abc123", "run": run}, names)
	e.sess.Limits.TierCeiling = ceiling
	return e
}

func (e *fakeEnv) call(tool string, a map[string]any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return e.h[tool](ctx, e.sess, a)
}

func prArgs(branch string) map[string]any {
	return map[string]any{"repo": "o/gate-api", "branch": branch, "title": "Fix the probe",
		"files": []any{map[string]any{"path": "src/app.py", "content": "print('fixed')\n"}}}
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %v", v)
	}
	return m
}

func TestALostAnswerToTheRefIsRecoveredAndOpensOnePullRequest(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	gh.DropAfter = func(key string, n int) bool { return key == "POST /repos/o/gate-api/git/refs" && n == 1 }
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	out, err := e.call("repo.pr.open", prArgs("agent/t-1"))
	if err != nil {
		t.Fatalf("the branch is ours and was made: the call goes on: %v", err)
	}
	if m := asMap(t, out); m["branch"] != "agent/t-1" || m["pr"] != int64(1) {
		t.Fatalf("%v", out)
	}
	if n := len(gh.PullRequests("o/gate-api")); n != 1 {
		t.Fatalf("%d pull requests", n)
	}
}

func TestALostAnswerToThePullRequestIsRecoveredNotRepeated(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	gh.DropAfter = func(key string, n int) bool { return key == "POST /repos/o/gate-api/pulls" && n == 1 }
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	out, err := e.call("repo.pr.open", prArgs("agent/t-2"))
	if err != nil {
		t.Fatalf("the pull request exists, one look finds it: %v", err)
	}
	if asMap(t, out)["pr"] != int64(1) || len(gh.PullRequests("o/gate-api")) != 1 {
		t.Fatalf("%v / %d pull requests", out, len(gh.PullRequests("o/gate-api")))
	}
	posts := 0
	for _, c := range gh.Calls() {
		if c.Method == "POST" && strings.HasSuffix(c.Target, "/pulls") {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("the pull request was asked for %d times", posts)
	}
}

func TestAnAnswerLostTwiceSaysTheChangeMayHaveBeenMadeAndAReplayOpensNoSecondPullRequest(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	gh.DropAfter = func(key string, n int) bool {
		return n == 1 && (key == "POST /repos/o/gate-api/pulls" || key == "GET /repos/o/gate-api/pulls")
	}
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	_, err := e.call("repo.pr.open", prArgs("agent/t-3"))
	if err == nil {
		t.Fatal("neither answer came")
	}
	if got := dispatch.BackendError(err); !strings.Contains(got, "may have been made") {
		t.Fatalf("the agent is told it failed outright: %q", got)
	}
	if got := dispatch.ErrorResult(err); !strings.Contains(got, "may have been made") || !strings.HasPrefix(got, "error:GitHubError: ") {
		t.Fatalf("the outcome says it failed outright: %q", got)
	}
	if len(gh.PullRequests("o/gate-api")) != 1 {
		t.Fatalf("the write happened: %d pull requests", len(gh.PullRequests("o/gate-api")))
	}
	// The agent asks again, the same call: the branch is there, the answer is a refusal and the count stays 1.
	_, err = e.call("repo.pr.open", prArgs("agent/t-3"))
	var den *core.Denied
	if !errors.As(err, &den) || den.Rule != "GH-BRANCH-EXISTS" {
		t.Fatalf("a replay: %v", err)
	}
	if len(gh.PullRequests("o/gate-api")) != 1 {
		t.Fatalf("a replay opened another: %d", len(gh.PullRequests("o/gate-api")))
	}
}

func TestALostAnswerToTheTreeLeavesNothingBehindAndAReplayWorks(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	gh.DropAfter = func(key string, n int) bool { return key == "POST /repos/o/gate-api/git/trees" && n == 1 }
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	_, err := e.call("repo.pr.open", prArgs("agent/t-4"))
	if err == nil || !strings.Contains(dispatch.ErrorResult(err), "may have been made") {
		t.Fatalf("%v", err)
	}
	if b := gh.Branches("o/gate-api"); len(b) != 1 || len(gh.PullRequests("o/gate-api")) != 0 {
		t.Fatalf("a tree is not visible: branches %v, pull requests %d", b, len(gh.PullRequests("o/gate-api")))
	}
	if _, err := e.call("repo.pr.open", prArgs("agent/t-4")); err != nil {
		t.Fatal(err)
	}
	if len(gh.PullRequests("o/gate-api")) != 1 {
		t.Fatal("one expected")
	}
}

func TestAReplayOfAFinishedCallOpensNoSecondPullRequest(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	if _, err := e.call("repo.pr.open", prArgs("agent/t-5")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, err := e.call("repo.pr.open", prArgs("agent/t-5"))
		var den *core.Denied
		if !errors.As(err, &den) || den.Rule != "GH-BRANCH-EXISTS" {
			t.Fatalf("replay %d: %v", i, err)
		}
	}
	if len(gh.PullRequests("o/gate-api")) != 1 {
		t.Fatal("one expected")
	}
	// the branch the agent did not create is never touched: main is where it was
	if gh.Branch("o/gate-api", "main") == "" {
		t.Fatal("main is gone")
	}
}

func TestEnsurePRResumesABranchLeftWithoutItsPullRequest(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	// the call died between the ref and the pull request: the branch holds the files, no pull request exists
	gh.SeedBranch("o/gate-api", "agent/step-1", map[string]string{"src/app.py": "print('fixed')\n"}, "step\n\nHangar-Agent: fixture-agent\nHangar-Session: s-1")
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	in := OpenPRInput{Repo: "o/gate-api", Branch: "agent/step-1", Title: "Step 1", Files: []File{{Path: "src/app.py", Content: "print('fixed')\n"}}, Meta: map[string]string{"agent": "fixture-agent", "session": "s-1"}}
	ctx := context.Background()
	// open_pr refuses (the branch exists) and opens nothing
	if _, err := e.git.OpenPR(ctx, in); err == nil {
		t.Fatal("open_pr takes an existing branch")
	}
	if len(gh.PullRequests("o/gate-api")) != 0 {
		t.Fatal("open_pr opened one")
	}
	first, err := e.git.EnsurePR(ctx, in)
	if err != nil || first["resumed"] != true {
		t.Fatalf("%v %v", first, err)
	}
	second, err := e.git.EnsurePR(ctx, in)
	if err != nil || second["resumed"] != true || second["pr"] != first["pr"] {
		t.Fatalf("%v %v", second, err)
	}
	if len(gh.PullRequests("o/gate-api")) != 1 {
		t.Fatalf("%d pull requests", len(gh.PullRequests("o/gate-api")))
	}
	// a branch whose files are not the ones asked for is a person's to look at: nothing is pushed to it
	gh.SeedBranch("o/gate-api", "agent/step-2", map[string]string{"src/app.py": "other\n"}, "step\n\nHangar-Agent: fixture-agent\nHangar-Session: s-1")
	in.Branch = "agent/step-2"
	_, err = e.git.EnsurePR(ctx, in)
	var den *core.Denied
	if !errors.As(err, &den) || den.Rule != "APPLY-BRANCH-DIVERGED" {
		t.Fatalf("%v", err)
	}
	for _, c := range gh.Calls() {
		if c.Method != "GET" && c.Method != "POST" {
			t.Fatalf("a %s", c.Method)
		}
	}
}

func TestARerunWhoseAnswerIsLostIsAskedAgainWithTheSameKeyAndStartsOneRun(t *testing.T) {
	gpf := fakes.NewGlidepath()
	defer gpf.Close()
	gpf.DropAfter = func(key string, n int) bool { return strings.HasSuffix(key, "/rerun") && n == 1 }
	e := newFakeEnv(t, nil, gpf, nil, "s-1")
	a := map[string]any{"app": "gate-api", "pipelinerun": "gate-api-run-1"}
	_, err := e.call("glidepath.run.rerun", a)
	if err == nil || !strings.Contains(dispatch.BackendError(err), "may have been made") || !strings.Contains(dispatch.ErrorResult(err), "may have been made") {
		t.Fatalf("a lost answer is not a failure: %v", err)
	}
	if !strings.HasPrefix(dispatch.ErrorResult(err), "error:GlidepathError: ") {
		t.Fatal(dispatch.ErrorResult(err))
	}
	out, err := e.call("glidepath.run.rerun", a)
	if err != nil || asMap(t, out)["replayed"] != true {
		t.Fatalf("%v %v", out, err)
	}
	if gpf.StartedCount() != 1 {
		t.Fatalf("%d runs started", gpf.StartedCount())
	}
	keys := map[string]bool{}
	for _, c := range gpf.Calls() {
		if k := c.Header.Get("Idempotency-Key"); k != "" {
			keys[k] = true
		}
	}
	if len(keys) != 1 {
		t.Fatalf("the two asks carry %d keys", len(keys))
	}
	// another session is another key: its own run
	e2 := newFakeEnv(t, nil, gpf, nil, "s-2")
	if _, err := e2.call("glidepath.run.rerun", a); err != nil || gpf.StartedCount() != 2 {
		t.Fatalf("%v %d", err, gpf.StartedCount())
	}
}

func appObject(project string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": "gate-api-dev", "namespace": "argocd-apps", "resourceVersion": "42", "labels": map[string]any{"hangar.io/app": "gate-api"}},
		"spec": map[string]any{"project": project}, "status": map[string]any{}}
}

func TestASyncWhoseAnswerIsLostIsFoundByItsOwnSessionAndNotRepeated(t *testing.T) {
	path := "/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications/gate-api-dev"
	kube := fakes.NewKube(map[string]map[string]any{path: appObject("gate-api-lower")})
	defer kube.Close()
	kube.DropAfter = func(key string, n int) bool { return key == "PATCH "+path && n == 1 }
	e := newFakeEnv(t, nil, nil, kube, "s-1")
	a := map[string]any{"app": "gate-api-dev", "project": "gate-api-lower"}
	_, err := e.call("argo.sync.lower", a)
	if err == nil || !strings.Contains(dispatch.BackendError(err), "may have been requested") || !strings.Contains(dispatch.ErrorResult(err), "error:ArgoSyncError: ") {
		t.Fatalf("a lost answer is not a failure: %v / %q", err, dispatch.ErrorResult(err))
	}
	if kube.PatchCount() != 1 {
		t.Fatalf("the write happened: %d", kube.PatchCount())
	}
	out, err := e.call("argo.sync.lower", a)
	if err != nil || asMap(t, out)["replayed"] != true || asMap(t, out)["requested"] != true {
		t.Fatalf("%v %v", out, err)
	}
	if kube.PatchCount() != 1 {
		t.Fatalf("a second sync: %d", kube.PatchCount())
	}
	// someone else's session meets the operation in flight as a busy Application
	e2 := newFakeEnv(t, nil, nil, kube, "s-2")
	_, err = e2.call("argo.sync.lower", a)
	var den *core.Denied
	if !errors.As(err, &den) || den.Rule != "ARGO-BUSY" {
		t.Fatalf("%v", err)
	}
	if kube.PatchCount() != 1 {
		t.Fatal("a patch went through a busy Application")
	}
}

// A call whose own time runs out after the write is reported as the write finished (dispatch.Gateway.KeepAnswer), not as a
// timeout: this is what the registered list is for.
func TestEveryRemoteWriteIsOnTheKeepAnswerList(t *testing.T) {
	reg := tools.Default()
	in := map[string]bool{}
	for _, n := range RemoteWrites {
		in[n] = true
	}
	for _, s := range reg.All() {
		write := s.Tier >= tools.T1 && (strings.HasPrefix(s.Name, "repo.pr.") || strings.HasPrefix(s.Name, "glidepath.") || s.Name == "argo.sync.lower") && s.Name != "xr.request" && s.Name != "airframe.apply"
		if write && !in[s.Name] {
			t.Errorf("%s writes to another system and is not on the list", s.Name)
		}
	}
}
