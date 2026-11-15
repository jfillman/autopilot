package backends

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/backends/fakes"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// -- scope: the gateway decides again what the core decided ------------------------------------------------------------

func refusedWith(t *testing.T, err error, rules ...string) *core.Denied {
	t.Helper()
	var den *core.Denied
	if !errors.As(err, &den) {
		t.Fatalf("want a refusal %v, got %v", rules, err)
	}
	if len(rules) > 0 {
		ok := false
		for _, r := range rules {
			ok = ok || den.Rule == r
		}
		if !ok {
			t.Fatalf("want a refusal %v, got %s: %s", rules, den.Rule, den.Message)
		}
	}
	return den
}

func narrowDefinition() *definitions.Definition {
	reg := tools.Default()
	return &definitions.Definition{Name: "fixture-agent", Kind: "task", ReposAllow: []string{"o/gate-*", "o/gitops-gate-api"},
		DenyPaths:  append(append([]string{}, reg.BaselineDenyPaths...), "secret/**"),
		RawProfile: json.RawMessage(`{"glidepath":{"apps":["gate-*"]}}`)}
}

func allTools() []string { return tools.Default().Names() }

func without(names []string, drop ...string) []string {
	var out []string
	for _, n := range names {
		keep := true
		for _, d := range drop {
			keep = keep && n != d
		}
		if keep {
			out = append(out, n)
		}
	}
	return out
}

func TestEveryWriteIsRefusedOutsideTheSessionsGrantAndTheDefinitionsScope(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api", "o/gitops-gate-api", "o/other", "p/gate-api")
	defer gh.Close()
	gpf := fakes.NewGlidepath()
	defer gpf.Close()
	path := "/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications/gate-api-dev"
	kube := fakes.NewKube(map[string]map[string]any{path: appObject("gate-api-lower")})
	defer kube.Close()
	touched := func() int { return len(gh.Calls()) + len(gpf.Calls()) + len(kube.Calls()) }
	run := func(def *definitions.Definition, grant []string, ceiling string) *fakeEnv {
		return newFakeEnvWith(t, gh, gpf, kube, "s-1", def, grant, ceiling)
	}
	open := prArgs("agent/t-1")
	cicd := map[string]any{"app": "gate-api", "repo": "o/gate-api", "patch": map[string]any{"a": 1}, "title": "x"}
	rerun := map[string]any{"app": "gate-api", "pipelinerun": "gate-api-run-1"}
	promote := map[string]any{"app": "gate-api", "env": "prod", "cluster": "c", "sourceEnv": "stage", "sourceCluster": "c0"}
	sync := map[string]any{"app": "gate-api-dev", "project": "gate-api-lower"}
	writes := []struct {
		tool string
		args map[string]any
	}{{"repo.pr.open", open}, {"repo.pr.open_upper", prArgs("agent/t-2")}, {"glidepath.cicd.propose", cicd}, {"glidepath.run.rerun", rerun},
		{"glidepath.run.cancel", rerun}, {"glidepath.promote", promote}, {"argo.sync.lower", sync}}

	t.Run("a tool the session was not granted", func(t *testing.T) {
		for _, w := range writes {
			e := run(narrowDefinition(), without(allTools(), w.tool), "T3")
			_, err := e.call(w.tool, w.args)
			refusedWith(t, err, "R006")
		}
		if touched() != 0 {
			t.Fatalf("%d requests went out", touched())
		}
	})
	t.Run("a tool above the session's ceiling", func(t *testing.T) {
		for _, w := range writes {
			spec, _ := tools.Default().Get(w.tool)
			if spec.Tier < tools.T1 {
				continue
			}
			ceiling := "T0"
			e := run(narrowDefinition(), allTools(), ceiling)
			_, err := e.call(w.tool, w.args)
			refusedWith(t, err, "R007")
		}
		// a propose-only tool is for a ceiling of T1 and above, and not above T2 tools that are not propose-only
		e := run(narrowDefinition(), allTools(), "T1")
		if _, err := e.call("glidepath.promote", promote); err != nil {
			var den *core.Denied
			if errors.As(err, &den) && den.Rule == "R007" {
				t.Fatalf("a propose-only tool is allowed to T1: %v", err)
			}
		}
		if touched() > 3 {
			t.Fatalf("%d requests went out", touched())
		}
	})
	before := touched()
	t.Run("an agent with no definition", func(t *testing.T) {
		e := run(narrowDefinition(), allTools(), "T3")
		e.sess.Agent = "ghost"
		for _, w := range writes {
			_, err := e.call(w.tool, w.args)
			refusedWith(t, err, "R003")
		}
	})
	t.Run("a repository outside repos.allow", func(t *testing.T) {
		e := run(narrowDefinition(), allTools(), "T3")
		for _, repo := range []string{"o/other", "p/gate-api", "o/gate", "o/gate-api/x", "o/", "gate-api"} {
			a := prArgs("agent/t-3")
			a["repo"] = repo
			_, err := e.call("repo.pr.open", a)
			refusedWith(t, err, "R014")
			_, err = e.call("repo.pr.open_upper", a)
			refusedWith(t, err, "R014")
			c := map[string]any{"app": "gate-api", "repo": repo, "patch": map[string]any{"a": 1}, "title": "x"}
			_, err = e.call("glidepath.cicd.propose", c)
			refusedWith(t, err, "R014")
			_, err = e.call("repo.file.get", map[string]any{"repo": repo, "path": "a"})
			refusedWith(t, err, "R014")
		}
	})
	t.Run("a tool for lower environments on a release repository", func(t *testing.T) {
		e := run(narrowDefinition(), allTools(), "T3")
		a := prArgs("agent/t-4")
		a["repo"] = "o/gitops-gate-api"
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "R012")
		// whatever extra argument says the target is lower: the name decides
		a["env"], a["project"] = "dev", "gate-api-lower"
		_, err = e.call("repo.pr.open", a)
		refusedWith(t, err, "R012")
		c := map[string]any{"app": "gate-api", "repo": "o/gitops-gate-api", "patch": map[string]any{"a": 1}, "title": "x"}
		_, err = e.call("glidepath.cicd.propose", c)
		refusedWith(t, err, "R012")
	})
	t.Run("a glidepath app outside profile.glidepath.apps", func(t *testing.T) {
		e := run(narrowDefinition(), allTools(), "T3")
		for _, app := range []string{"other", "x-gate", "gate", "", "GATE-api"} {
			for tool, a := range map[string]map[string]any{
				"glidepath.run.rerun": {"app": app, "pipelinerun": "x-1"}, "glidepath.run.cancel": {"app": app, "pipelinerun": "x-1"},
				"glidepath.runs.list": {"app": app}, "glidepath.promote": {"app": app, "env": "prod", "cluster": "c", "sourceEnv": "s", "sourceCluster": "c"},
				"glidepath.cicd.propose": {"app": app, "repo": "o/gate-api", "patch": map[string]any{"a": 1}, "title": "x"}} {
				_, err := e.call(tool, a)
				refusedWith(t, err, "R022")
			}
		}
	})
	t.Run("a protected path of the definition's own", func(t *testing.T) {
		e := run(narrowDefinition(), allTools(), "T3")
		a := prArgs("agent/t-5")
		a["files"] = []any{map[string]any{"path": "secret/key.txt", "content": "x"}}
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-PATH")
	})
	if touched() != before {
		t.Fatalf("a refused call sent %d requests", touched()-before)
	}
	t.Run("what is allowed still works", func(t *testing.T) {
		e := run(narrowDefinition(), allTools(), "T2")
		if _, err := e.call("repo.pr.open", prArgs("agent/t-ok")); err != nil {
			t.Fatal(err)
		}
		if _, err := e.call("glidepath.run.rerun", rerun); err != nil {
			t.Fatal(err)
		}
		if _, err := e.call("argo.sync.lower", sync); err != nil {
			t.Fatal(err)
		}
	})
}

// -- names and paths an agent writes -----------------------------------------------------------------------------------

func TestRepositoryWriteAttacksAreRefusedBeforeAnyRequest(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api", "o/gitops-gate-api")
	defer gh.Close()
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	branches := []string{"refs/heads/x", "refs/tags/v1", "refs/pull/1/head", "x..y", "x@{1}", "-x", "--force", "-", ".hidden", "a/.b", "x.lock", "a.lock/b", "a//b",
		"x ", " x", "x\n", "x\r", "x\x00", "x~1", "x^", "x:y", "x?y", "x*", "x[", "x\\y", "heads/../main", "HEAD", "main", "MAIN", "Main", "master", "develop",
		"production", "release", "gh-pages", "origin/../main", "ｍａｉｎ", "ma​in", "a/", "/a", "a.", "x y", "x%2fy", "x%00", "x y",
		"x;rm", "x$(id)", "x`id`", "x'y", "x\"y", "x<y", "x>y", "x|y", "x&y", "x#y", "é", strings.Repeat("a", 201)}
	for _, b := range branches {
		a := prArgs(b)
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-BRANCH")
	}
	for _, b := range []string{"../main", "refs/../x", "-x", "x..", "a b", "x\n", "", "x@{u}", "x:y"} {
		a := prArgs("agent/ok")
		a["base"] = b
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-BRANCH")
	}
	files := []string{".github/workflows/x.yml", ".GitHub/workflows/x.yml", ".tekton/x.yaml", "a/.tekton/x.yaml", "a/.TEKTON/x.yaml", "CODEOWNERS", "docs/CODEOWNERS",
		"codeowners", "docs/CodeOwners", "release.yaml", "a/release.yaml", "a/Release.yaml", "x.release.yaml", "a/x.release.yaml", "AppProject.yaml", "a/appproject-x.yaml",
		"a/AppProject-prod.yaml", "a/./.github/x", "a/../.github/x", "a/../../x", "../x", "..", ".", "", "/etc/passwd", "a\\b", "a\x00b", "a\nb", "a/", "a//b",
		".git/config", "a/.git/hooks/x", "a/.GIT/x", "x/.git", ".github", ".GITHUB", ".tekton", "a/.tekton", "A/.TEKTON"}
	for _, p := range files {
		a := prArgs("agent/ok")
		a["files"] = []any{map[string]any{"path": p, "content": "x"}}
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-PATH", "GH-FILES")
	}
	titles := []string{"", "  ", "x\nHangar-Merge: person", "x\r\nHangar-Task: t-9", "x\x00", "x\x1b[31m", strings.Repeat("t", 201), "\n", "a\u0085b"}
	for _, ti := range titles {
		a := prArgs("agent/ok")
		a["title"] = ti
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-TITLE")
	}
	repos := []string{"evil/gate-api", "o/gate-api/../x", "o/..", "o/.", "o/gate api", "o/gate-api\n", "o\n/gate-api", "o/gate-api?x=1", "o/gate-api#x", "o/gate%2Fapi",
		"o/gate-api;", "o/gate\x00api", "o\\gate-api", "gate-api", "", "/o/gate-api", "o//gate-api", "@o/gate-api", "o/gate-api ", "o /gate-api"}
	for _, r := range repos {
		a := prArgs("agent/ok")
		a["repo"] = r
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "R014", "GH-REPO", "GH-OWNER")
		_, err = e.call("repo.file.get", map[string]any{"repo": r, "path": "a"})
		refusedWith(t, err, "R014", "GH-REPO", "GH-OWNER")
	}
	for _, p := range []string{"../x", "a/../../b", "/abs", "a\\b", "a\x00b", "a/./b", "a//b", "..", "a/../b"} {
		_, err := e.call("repo.file.get", map[string]any{"repo": "o/gate-api", "path": p})
		refusedWith(t, err, "GH-PATH")
	}
	for _, r := range []string{"../x", "-x", "a b", "x\n", "x@{u}", "refs/..", ""} {
		_, err := e.call("repo.file.get", map[string]any{"repo": "o/gate-api", "path": "a", "ref": r})
		refusedWith(t, err, "GH-REF")
	}
	if n := len(gh.Calls()); n != 0 {
		t.Fatalf("%d of the refused calls reached GitHub", n)
	}
}

func TestAnAgentCannotForgeTheTrailersOrMentionPeople(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	gpf := fakes.NewGlidepath()
	defer gpf.Close()
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	if _, err := e.call("repo.pr.open", prArgs("agent/t-1")); err != nil {
		t.Fatal(err)
	}
	var msg string
	for _, c := range gh.Calls() {
		if strings.HasSuffix(c.Target, "/git/commits") && c.Method == "POST" {
			var b struct{ Message string }
			_ = json.Unmarshal(c.Body, &b)
			msg = b.Message
		}
	}
	trailers := 0
	for _, l := range strings.Split(msg, "\n") {
		if strings.HasPrefix(l, "Hangar-") {
			trailers++
		}
	}
	if trailers != 4 || strings.Contains(msg, "Hangar-Merge") {
		t.Fatalf("the trailers are the session's own, four of them and no merge mark:\n%s", msg)
	}
	// the text a verdict or an agent hands the pull request notifies nobody
	for in, want := range map[string]string{
		"thanks @alice": "thanks @​alice", "@bob and @org/team": "@​bob and @​org/team", "mail a@b.c": "mail a@b.c",
		"`@code`": "`@\u200bcode`", "line\n@carol": "line\n@​carol", "@@x": "@@​x", "no mention": "no mention", "@": "@",
	} {
		if got := defang(in); got != want {
			t.Errorf("defang(%q) = %q, want %q", in, got, want)
		}
	}
}

// -- glidepath arguments -----------------------------------------------------------------------------------------------

type recTransport struct {
	mu   sync.Mutex
	urls []string
	hdrs []http.Header
	body string
	code int
}

func (r *recTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.urls = append(r.urls, req.URL.String())
	r.hdrs = append(r.hdrs, req.Header.Clone())
	r.mu.Unlock()
	code := r.code
	if code == 0 {
		code = 200
	}
	body := r.body
	if body == "" {
		body = "{}"
	}
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func recordingGlidepath(t testing.TB) (*Glidepath, *recTransport) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(p, []byte("gp-token-value-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gp, err := NewGlidepath("https://glidepath.example/gp", p, true, time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	rt := &recTransport{}
	gp.hc.Transport, gp.streamHC.Transport = rt, rt
	gp.retry.retries = 0
	return gp, rt
}

// glidepathCalls is each way a tool reaches a URL, with the argument each takes, so one value can be tried in every place.
func glidepathCalls(gp *Glidepath, v string) map[string]func() (any, error) {
	ctx := context.Background()
	return map[string]func() (any, error){
		"runs":                 func() (any, error) { return gp.Runs(ctx, v, nil, nil, "p") },
		"run":                  func() (any, error) { return gp.Run(ctx, "gate-api", v, "p") },
		"run.app":              func() (any, error) { return gp.Run(ctx, v, "0b9c3a54-1d2e-4f6a-8b7c-9d0e1f2a3b4c", "p") },
		"log":                  func() (any, error) { return gp.Log(ctx, "gate-api", "0b9c3a54-1d2e-4f6a-8b7c-9d0e1f2a3b4c", v, "p") },
		"explain":              func() (any, error) { return gp.Explain(ctx, "gate-api", v, "p") },
		"releases":             func() (any, error) { return gp.Releases(ctx, "gate-api", v, "p") },
		"releaseRun":           func() (any, error) { return gp.ReleaseRun(ctx, "gate-api", v, "p") },
		"evidence":             func() (any, error) { return gp.Evidence(ctx, "gate-api", v, "p") },
		"testReport":           func() (any, error) { return gp.TestReport(ctx, "gate-api", v, "p") },
		"events.type":          func() (any, error) { return gp.Events(ctx, "gate-api", v, nil, nil, "p") },
		"events.since":         func() (any, error) { return gp.Events(ctx, "gate-api", nil, v, nil, "p") },
		"events.app":           func() (any, error) { return gp.Events(ctx, v, nil, nil, nil, "p") },
		"rollbackPlan":         func() (any, error) { return gp.RollbackPlan(ctx, "gate-api", v, "c", "p") },
		"rollbackPlan.cluster": func() (any, error) { return gp.RollbackPlan(ctx, "gate-api", "prod", v, "p") },
		"liveLog":              func() (any, error) { return gp.LiveLog(ctx, "gate-api", "t-1", v, "p") },
		"liveLog.tr":           func() (any, error) { return gp.LiveLog(ctx, "gate-api", v, "step-x", "p") },
		"rerun":                func() (any, error) { return gp.Rerun(ctx, "gate-api", v, "clearance.abc", "p") },
		"cancel":               func() (any, error) { return gp.Cancel(ctx, v, "x-1", "p") },
		"promote.env": func() (any, error) {
			return gp.Promote(ctx, "gate-api", v, map[string]any{"cluster": "c"}, "clearance.abc", false, "p")
		},
		"rollback.env": func() (any, error) {
			return gp.Rollback(ctx, "gate-api", v, map[string]any{"cluster": "c"}, "clearance.abc", false, "p")
		},
		"validate": func() (any, error) { return gp.ValidateCICD(ctx, v, map[string]any{"patch": map[string]any{}}, "p") },
		"watch":    func() (any, error) { return gp.Watch(ctx, "gate-api", v, nil, "p") },
		"live":     func() (any, error) { return gp.Live(ctx, v, "p") },
	}
}

var glidepathAttacks = []string{"..", ".", "../..", "a/b", "a%2fb", "a%2Fb", "%2e%2e", "a b", "a\tb", "a\nb", "a\r\nX-Evil: 1", "a?b=c", "a#b", "a;b", "a\\b", "a\x00b", "",
	"-a", "A", "gate-api/../x", "gate-api/", "/gate-api", "gate-api%00", "．．", "gate∕api", "café", strings.Repeat("a", 300), "a&image=x", "a=b&c=d",
	"@evil.example", "evil.example:443", "//evil.example", "http://evil.example/", "a@b", "a:b", "a\u0085b", "ghcr.io/o/x:1 evil"}

func TestGlidepathArgumentsNeverReachTheURLUnlessTheyMatchTheirPattern(t *testing.T) {
	for _, v := range glidepathAttacks {
		gp, rt := recordingGlidepath(t)
		for name, call := range glidepathCalls(gp, v) {
			rt.urls = nil
			_, err := call()
			if len(rt.urls) == 0 {
				var den *core.Denied
				if !errors.As(err, &den) {
					t.Errorf("%s(%q): refused with %v, not a refusal", name, v, err)
				}
				continue
			}
			// Accepted: the request is under the base, and the value is exactly one segment or one query value.
			u := rt.urls[0]
			if !strings.HasPrefix(u, "https://glidepath.example/gp/v1/") {
				t.Errorf("%s(%q): %s is not under the base", name, v, u)
			}
		}
	}
	// the numbers
	gp, rt := recordingGlidepath(t)
	for _, bad := range []any{json.Number("1.5"), json.Number("0"), json.Number("745"), json.Number("-1"), json.Number("1e2"), "24", true, 24.0, []any{1}, map[string]any{}} {
		rt.urls = nil
		_, err := gp.Runs(context.Background(), "gate-api", bad, nil, "p")
		refusedWith(t, err, "GP-ARG")
		if b, isNum := bad.(json.Number); !isNum || b != "745" {
			_, err = gp.Runs(context.Background(), "gate-api", nil, bad, "p")
			refusedWith(t, err, "GP-ARG")
		}
		if len(rt.urls) != 0 {
			t.Fatalf("%v reached the URL: %v", bad, rt.urls)
		}
	}
	for _, bad := range []any{json.Number("1001"), json.Number("0")} {
		if _, err := gp.Runs(context.Background(), "gate-api", nil, bad, "p"); err == nil {
			t.Fatalf("a limit of %v was sent", bad)
		}
	}
	for _, good := range []any{json.Number("1"), json.Number("744"), 24, int64(5)} {
		if _, err := gp.Runs(context.Background(), "gate-api", good, nil, "p"); err != nil {
			t.Fatalf("%v: %v", good, err)
		}
	}
}

func TestAGlidepathWriteCannotChooseItsKeyItsBodyOrItsHeaders(t *testing.T) {
	gp, rt := recordingGlidepath(t)
	ctx := context.Background()
	for _, k := range []string{"", "a b", "a\r\nX-Evil: 1", "a/b", strings.Repeat("a", 129), "é"} {
		if _, err := gp.Rerun(ctx, "gate-api", "x-1", k, "p"); err == nil {
			t.Fatalf("key %q was sent", k)
		}
	}
	if len(rt.urls) != 0 {
		t.Fatal("a bad key reached the network")
	}
	// the body of a promote or a rollback is built from matched fields only; a source that is half of a pair is refused
	for _, a := range []map[string]any{
		{"cluster": "c"}, {"cluster": "c", "sourceEnv": "s"}, {"cluster": "c", "sourceEnv": "s", "sourceCluster": "c", "sourceImageRepo": "x"},
		{"cluster": "c", "sourceImageTag": "1"}, {"cluster": "c\n", "sourceEnv": "s", "sourceCluster": "c"}, {"cluster": "c", "sourceEnv": "../x", "sourceCluster": "c"},
		{"cluster": "c", "sourceImageRepo": "Evil/Upper", "sourceImageTag": "1"}, {"sourceEnv": "s", "sourceCluster": "c"}, {"cluster": 5, "sourceEnv": "s", "sourceCluster": "c"}} {
		if _, err := PromoteBody(a); err == nil {
			t.Errorf("a promote of %v was accepted", a)
		}
	}
	for _, a := range []map[string]any{
		{"cluster": "c", "targetReleaseId": "r", "reason": "  "}, {"cluster": "c", "targetReleaseId": "r", "reason": strings.Repeat("r", 1025)},
		{"cluster": "c", "targetReleaseId": "r s", "reason": "x"}, {"cluster": "c", "targetReleaseId": "", "reason": "x"}, {"cluster": "c", "targetReleaseId": "r", "reason": 5}} {
		if _, err := RollbackBody(a); err == nil {
			t.Errorf("a rollback of %v was accepted", a)
		}
	}
	// no argument reaches the on-behalf-of header or a second header
	rt.urls, rt.hdrs = nil, nil
	if _, err := gp.Cancel(ctx, "gate-api", "x-1", "user:a\r\nX-Evil: 1"); err == nil {
		t.Fatal("a principal with a line break was sent")
	}
	if len(rt.urls) != 0 {
		t.Fatal("it reached the network")
	}
}

// -- the closed list of requests ---------------------------------------------------------------------------------------

var allowedGitHub = []*regexp.Regexp{
	regexp.MustCompile(`^POST /broker/agent-installation-token$`),
	regexp.MustCompile(`^GET /repos/o/[A-Za-z0-9._-]+/git/ref/heads/[A-Za-z0-9._/-]+$`),
	regexp.MustCompile(`^GET /repos/o/[A-Za-z0-9._-]+/git/commits/[0-9a-f]{40}$`),
	regexp.MustCompile(`^POST /repos/o/[A-Za-z0-9._-]+/git/trees$`),
	regexp.MustCompile(`^POST /repos/o/[A-Za-z0-9._-]+/git/commits$`),
	regexp.MustCompile(`^POST /repos/o/[A-Za-z0-9._-]+/git/refs$`),
	regexp.MustCompile(`^POST /repos/o/[A-Za-z0-9._-]+/pulls$`),
	regexp.MustCompile(`^GET /repos/o/[A-Za-z0-9._-]+/pulls$`),
	regexp.MustCompile(`^GET /repos/o/[A-Za-z0-9._-]+/pulls/[0-9]+/files$`),
	regexp.MustCompile(`^GET /repos/o/[A-Za-z0-9._-]+/contents/[A-Za-z0-9._%/-]*$`),
	regexp.MustCompile(`^GET /repos/o/[A-Za-z0-9._-]+$`),
}

func TestTheGitHubRequestsAreTheClosedList(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api", "o/gitops-gate-api")
	defer gh.Close()
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	e.call("repo.pr.open", prArgs("agent/t-1"))
	e.call("repo.pr.open", prArgs("agent/t-1"))
	e.call("repo.pr.open_upper", map[string]any{"repo": "o/gitops-gate-api", "branch": "agent/t-2", "title": "t", "files": []any{map[string]any{"path": "values.yaml", "content": "x"}}})
	e.call("repo.file.get", map[string]any{"repo": "o/gate-api", "path": "src/app.py", "ref": "agent/t-1"})
	e.call("repo.file.get", map[string]any{"repo": "o/gate-api"})
	ctx := context.Background()
	in := OpenPRInput{Repo: "o/gate-api", Branch: "agent/t-3", Title: "t", Base: "main", Files: []File{{Path: "a.txt", Content: "a"}}}
	e.git.EnsurePR(ctx, in)
	e.git.EnsurePR(ctx, in)
	e.git.FindPR(ctx, "o/gate-api", "agent/t-3")
	for _, c := range gh.Calls() {
		key := c.Method + " " + strings.SplitN(c.Target, "?", 2)[0]
		ok := false
		for _, re := range allowedGitHub {
			ok = ok || re.MatchString(key)
		}
		if !ok {
			t.Errorf("a request outside the closed list: %s", key)
		}
		if c.Method != "GET" && c.Method != "POST" {
			t.Errorf("method %s", c.Method)
		}
		if c.Method == "POST" && strings.HasSuffix(c.Target, "/git/refs") {
			var b struct{ Ref, Sha string }
			_ = json.Unmarshal(c.Body, &b)
			if !strings.HasPrefix(b.Ref, "refs/heads/") || b.Ref == "refs/heads/main" {
				t.Errorf("a ref outside the agent's own branches: %s", b.Ref)
			}
		}
	}
	// main was never moved
	if gh.Branch("o/gate-api", "main") != fakes.NewGitHub("o/gate-api").Branch("o/gate-api", "main") {
		t.Error("main moved")
	}
	// the only requests to the broker are for the repositories asked about
	if gh.Minted == 0 {
		t.Fatal("no token was asked for")
	}
}

func TestNoGitHubRequestExistsForAMergeAReviewADeleteOrAProtectionChange(t *testing.T) {
	for _, ep := range []endpoint{epGetRef, epGetCommit, epPostTree, epPostCommit, epPostRef, epPostPull, epListPulls, epGetContents, epGetRepo, epListPullFiles} {
		r := ghReq{ep: ep, owner: "o", name: "r", number: 3, branch: "agent/x", sha: strings.Repeat("a", 40), state: "all", perPage: 5, ref: "main", path: "a",
			body: refBody{Ref: "refs/heads/agent/x", Sha: strings.Repeat("a", 40)}}
		m, target, err := r.target()
		if err != nil {
			t.Fatalf("endpoint %d: %v", ep, err)
		}
		if m != http.MethodGet && m != http.MethodPost {
			t.Fatalf("method %s", m)
		}
		for _, bad := range []string{"/merge", "/merges", "/reviews", "/branches", "/protection", "/rulesets", "/dispatches", "/forks", "/collaborators", "/hooks", "/keys", "/actions", "/environments", "/labels", "/comments", "/issues"} {
			if strings.Contains(target, bad) {
				t.Fatalf("%s reaches %s", target, bad)
			}
		}
	}
	// a ref body that names anything but a new branch of the agent's is never sent
	for _, ref := range []string{"refs/heads/main", "refs/heads/master", "refs/tags/v1", "refs/heads/../x", "heads/x", "refs/heads/", "refs/heads/-x", "refs/heads/HEAD"} {
		_, _, err := ghReq{ep: epPostRef, owner: "o", name: "r", body: refBody{Ref: ref, Sha: strings.Repeat("a", 40)}}.target()
		if err == nil {
			t.Errorf("a ref body of %q was built", ref)
		}
	}
	if _, _, err := (ghReq{ep: epPostRef, owner: "o", name: "r", body: map[string]any{"ref": "refs/heads/x"}}).target(); err == nil {
		t.Error("a ref body of another type was built")
	}
}

// -- redirects and credentials -----------------------------------------------------------------------------------------

func TestAWriteBackendsRedirectIsNeverFollowedAndNoCredentialGoesToItsTarget(t *testing.T) {
	var hits atomic.Int64
	var leaked atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		leaked.Store(r.Header.Get("Authorization") + r.Header.Get("X-Glidepath-Authorization"))
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"token":"ghs_redirected","object":{"sha":"` + strings.Repeat("a", 40) + `"}}`))
	}))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirect.Close()
	dir := t.TempDir()
	tok := filepath.Join(dir, "tok")
	_ = os.WriteFile(tok, []byte("secret-token-value\n"), 0o600)
	gp, _ := NewGlidepath(redirect.URL, tok, true, 5*time.Second, true)
	if _, err := gp.Contract(context.Background(), "p"); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("glidepath: %v", err)
	}
	if _, err := gp.Rerun(context.Background(), "gate-api", "x-1", "clearance.k", "p"); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("glidepath write: %v", err)
	}
	g, _ := NewGitHub(GitHubOptions{API: redirect.URL, BrokerURL: redirect.URL + "/b", TokenFile: tok, Owners: []string{"o"}, AllowHTTP: true, AllowHTTPAPI: true})
	_, err := g.OpenPR(context.Background(), OpenPRInput{Repo: "o/gate-api", Branch: "agent/x", Title: "t", Base: "main", Files: []File{{Path: "a", Content: "a"}}})
	var ge *GitHubError
	if !errors.As(err, &ge) || ge.Status != 302 {
		t.Fatalf("the broker's redirect: %v", err)
	}
	k := &KubeReader{Server: redirect.URL, TokenFile: tok, Timeout: 5 * time.Second}
	k.hc = client(nil, time.Second, 4)
	k.AllowHTTP = true
	as, _ := NewArgoSync(k, []string{"argocd-apps"})
	_, err = as.Sync(context.Background(), "gate-api-dev", "gate-api-lower", map[string]string{"agent": "a"}, nil)
	if err == nil {
		t.Fatal("the Kubernetes redirect was followed")
	}
	if hits.Load() != 0 {
		t.Fatalf("a redirect target was reached %d times, with %q", hits.Load(), leaked.Load())
	}
}

func TestTokensAreReadOnEveryCallAndNeverEscape(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	gpf := fakes.NewGlidepath()
	defer gpf.Close()
	e := newFakeEnv(t, gh, gpf, nil, "s-1")
	// the token files are re-read: change them and the next call carries the new value
	gpFile := filepath.Join(t.TempDir(), "gp")
	_ = os.WriteFile(gpFile, []byte("first-token-value\n"), 0o600)
	gp, _ := NewGlidepath(gpf.Srv.URL, gpFile, false, 5*time.Second, true)
	_, _ = gp.Contract(context.Background(), "p")
	_ = os.WriteFile(gpFile, []byte("second-token-value\n"), 0o600)
	_, _ = gp.Contract(context.Background(), "p")
	calls := gpf.Calls()
	if got := calls[len(calls)-2].Header.Get("Authorization") + "," + calls[len(calls)-1].Header.Get("Authorization"); got != "Bearer first-token-value,Bearer second-token-value" {
		t.Fatalf("the token is not read per call: %s", got)
	}
	// a token is never in what an upstream's error says, even when the upstream echoes the credentials it was given
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = fmt.Fprintf(w, `{"message":"bad: %s","error":"bad: %s"}`, r.Header.Get("Authorization"), r.Header.Get("Authorization"))
	}))
	defer echo.Close()
	tokf := filepath.Join(t.TempDir(), "t")
	_ = os.WriteFile(tokf, []byte("projected-secret-token\n"), 0o600)
	g, _ := NewGitHub(GitHubOptions{API: echo.URL, BrokerURL: echo.URL + "/b", TokenFile: tokf, Owners: []string{"o"}, AllowHTTP: true, AllowHTTPAPI: true})
	_, err := g.OpenPR(context.Background(), OpenPRInput{Repo: "o/x", Branch: "agent/x", Title: "t", Base: "main", Files: []File{{Path: "a", Content: "a"}}})
	if err == nil || strings.Contains(dispatch.BackendError(err)+dispatch.ErrorResult(err)+err.Error(), "projected-secret-token") {
		t.Fatalf("the credential came back in %v", err)
	}
	gp2, _ := NewGlidepath(echo.URL, tokf, false, 5*time.Second, true)
	_, err = gp2.Contract(context.Background(), "p")
	if err != nil && strings.Contains(err.Error()+dispatch.ErrorResult(err), "projected-secret-token") {
		t.Fatalf("glidepath: %v", err)
	}
	// 403 with the credential in the text is a denial: it is read with the credential taken out
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = fmt.Fprintf(w, `{"rule":"GP-AUTHZ-001","error":"you are %s","hint":"%s"}`, r.Header.Get("Authorization"), r.Header.Get("X-Glidepath-Authorization"))
	}))
	defer deny.Close()
	gp3, _ := NewGlidepath(deny.URL, tokf, false, 5*time.Second, true)
	_, err = gp3.Contract(context.Background(), "p")
	den := refusedWith(t, err, "GP-AUTHZ-001")
	if strings.Contains(den.Message+den.Hint, "projected-secret-token") {
		t.Fatalf("a denial carries the credential: %+v", den)
	}
	// the installation token is only sent to the configured API host
	for _, c := range gh.Calls() {
		if strings.HasPrefix(c.Target, "/repos/") && c.Header.Get("Authorization") != "Bearer "+gh.Token {
			t.Fatalf("a request with the wrong credential: %s", c.Header.Get("Authorization"))
		}
	}
	_ = e
}

// -- limits ------------------------------------------------------------------------------------------------------------

func TestResponsesAreCappedWhileTheyAreRead(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := 500
		if strings.HasPrefix(r.URL.Path, "/ok") {
			code = 200
		}
		w.WriteHeader(code)
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for i := 0; i < 64; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer big.Close()
	hc := client(nil, time.Second, 2)
	rp, err := send(context.Background(), hc, newLimiter(1), 10*time.Second, "GET", big.URL+"/err", http.Header{}, nil, maxAPIOK, maxAPIErr)
	if err != nil || len(rp.body) != maxAPIErr {
		t.Fatalf("an error answer is read to %d bytes: %d %v", maxAPIErr, len(rp.body), err)
	}
	rp, err = send(context.Background(), hc, newLimiter(1), 10*time.Second, "GET", big.URL+"/ok", http.Header{}, nil, maxAPIOK, maxAPIErr)
	if err != nil || len(rp.body) != maxAPIOK {
		t.Fatalf("an answer is read to %d bytes: %d %v", maxAPIOK, len(rp.body), err)
	}
	// a glidepath answer over the cap is not JSON: a JSONDecodeError, nothing echoed
	gp, _ := NewGlidepath(big.URL+"/ok", writeTemp(t, "a-token-value-1"), false, 10*time.Second, true)
	_, err = gp.Contract(context.Background(), "p")
	if err == nil || dispatch.ErrorResult(err) != "error:JSONDecodeError" {
		t.Fatalf("%v", err)
	}
}

func writeTemp(t testing.TB, v string) string {
	p := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestABackendThatIsSlowHoldsOnlyItsOwnShareOfTheGatewaysCalls(t *testing.T) {
	var inflight, peak atomic.Int64
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inflight.Add(-1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("{}"))
	}))
	defer slow.Close()
	fast := fakes.NewGitHub("o/gate-api")
	defer fast.Close()
	gp, _ := NewGlidepath(slow.URL, writeTemp(t, "a-token-value-1"), false, 30*time.Second, true)
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, _ = gp.Contract(ctx, "p")
		}()
	}
	time.Sleep(300 * time.Millisecond)
	// the saturated backend does not stop another from answering
	g, _ := NewGitHub(GitHubOptions{API: fast.Srv.URL, BrokerURL: fast.BrokerURL(), TokenFile: writeTemp(t, "a-token-value-1"), Owners: []string{"o"}, AllowHTTP: true, AllowHTTPAPI: true, Timeout: 5 * time.Second})
	done := make(chan error, 1)
	go func() { _, err := g.ReadFile(context.Background(), "o/gate-api", "a", "main"); done <- err }()
	select {
	case err := <-done:
		_ = err
	case <-time.After(3 * time.Second):
		t.Fatal("GitHub waited behind glidepath-api")
	}
	if p := peak.Load(); p > 16 {
		t.Fatalf("%d requests in flight to one backend, the limit is 16", p)
	}
	close(release)
	wg.Wait()
}

func TestRetriesAreBoundedBackOffAndNeverRepeatAWrite(t *testing.T) {
	var gets, posts atomic.Int64
	status := atomic.Int64{}
	status.Store(503)
	hdr := atomic.Value{}
	hdr.Store(http.Header{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			gets.Add(1)
		} else {
			posts.Add(1)
		}
		for k, v := range hdr.Load().(http.Header) {
			w.Header()[k] = v
		}
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	var slept []time.Duration
	pol := retryPolicy{retries: 2, base: 10 * time.Millisecond, maxWait: 2 * time.Second, now: time.Now,
		sleep: func(_ context.Context, d time.Duration) bool { slept = append(slept, d); return true }}
	hc := client(nil, time.Second, 2)
	get := func() *reply {
		r, err := sendRead(context.Background(), pol, hc, newLimiter(1), 5*time.Second, srv.URL+"/x", http.Header{}, 1<<20, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := get(); r.status != 503 || gets.Load() != 3 || len(slept) != 2 || slept[1] != 2*slept[0] {
		t.Fatalf("a failing GET is tried 3 times with a growing wait: %d %d %v", r.status, gets.Load(), slept)
	}
	// a write is sent once
	if _, err := send(context.Background(), hc, newLimiter(1), 5*time.Second, "POST", srv.URL+"/x", http.Header{}, []byte("{}"), 1<<20, 1<<20); err != nil || posts.Load() != 1 {
		t.Fatalf("%v %d", err, posts.Load())
	}
	// Retry-After is respected, and a wait that is too long is the answer
	gets.Store(0)
	slept = nil
	hdr.Store(http.Header{"Retry-After": {"1"}})
	get()
	if len(slept) != 2 || slept[0] != time.Second {
		t.Fatalf("Retry-After is waited: %v", slept)
	}
	gets.Store(0)
	slept = nil
	hdr.Store(http.Header{"Retry-After": {"600"}})
	if r := get(); r.status != 503 || gets.Load() != 1 || len(slept) != 0 {
		t.Fatalf("a long wait is not sat out: %d %v", gets.Load(), slept)
	}
	// a 403 is a rate limit only when it says so
	gets.Store(0)
	status.Store(403)
	hdr.Store(http.Header{})
	get()
	if gets.Load() != 1 {
		t.Fatalf("a plain 403 is not retried: %d", gets.Load())
	}
	gets.Store(0)
	hdr.Store(http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(time.Now().Add(time.Second).Unix())}})
	get()
	if gets.Load() != 3 {
		t.Fatalf("a rate-limited 403 is retried: %d", gets.Load())
	}
	// a 404, a 422, a 500 are final
	for _, code := range []int64{404, 422, 500, 401} {
		gets.Store(0)
		status.Store(code)
		hdr.Store(http.Header{})
		get()
		if gets.Load() != 1 {
			t.Fatalf("%d is final: %d", code, gets.Load())
		}
	}
	// the call's own deadline limits the wait
	gets.Store(0)
	status.Store(503)
	hdr.Store(http.Header{"Retry-After": {"1"}})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := sendRead(ctx, pol, hc, newLimiter(1), 5*time.Second, srv.URL+"/x", http.Header{}, 1<<20, 1<<20); err != nil || gets.Load() != 1 {
		t.Fatalf("a wait past the deadline is not taken: %v %d", err, gets.Load())
	}
}

func TestABodyThatIsTooLargeIsRefusedBeforeItIsSent(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	gpf := fakes.NewGlidepath()
	defer gpf.Close()
	e := newFakeEnv(t, gh, gpf, nil, "s-1")
	a := prArgs("agent/big")
	var files []any
	for i := 0; i < 9; i++ {
		files = append(files, map[string]any{"path": fmt.Sprintf("f%d", i), "content": strings.Repeat("x", 1<<20)})
	}
	a["files"] = files
	_, err := e.call("repo.pr.open", a)
	refusedWith(t, err, "GH-FILES")
	_, err = e.call("glidepath.cicd.validate", map[string]any{"app": "gate-api", "patch": map[string]any{"k": strings.Repeat("x", 400<<10)}})
	refusedWith(t, err, "GP-ARG")
	if len(gh.Calls()) != 0 || len(gpf.Calls()) != 0 {
		t.Fatal("a request went out")
	}
}

// -- the names the validators accept are names git accepts -------------------------------------------------------------

// gitRefOK is git-check-ref-format's rules for a branch name, written out from its manual, independent of ValidRef.
func gitRefOK(s string) bool {
	if s == "" || strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") || strings.HasPrefix(s, "/") {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	for _, c := range strings.Split(s, "/") {
		if c == "" || strings.HasPrefix(c, ".") || strings.HasSuffix(c, ".lock") {
			return false
		}
	}
	return true
}

func TestTheRefValidatorsAgreeWithGitItself(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git to ask")
	}
	for _, s := range append(branchesToAsk(), "agent/x", "a", "a-b_c.d", "feature/x/y", "x.lock", "a.lock/b", "a..b", "-x", "a@{1}", "x.", "a//b") {
		out := exec.Command(git, "check-ref-format", "refs/heads/"+s).Run() == nil
		if ValidRef(s) && !out {
			t.Errorf("ValidRef accepts %q, git does not", s)
		}
		if ValidRef(s) && !gitRefOK(s) {
			t.Errorf("the oracle disagrees with ValidRef on %q", s)
		}
		if gitRefOK(s) != out && !strings.HasPrefix(s, "-") {
			t.Errorf("the oracle and git disagree on %q: oracle %v git %v", s, gitRefOK(s), out)
		}
	}
}

func branchesToAsk() []string {
	return []string{"refs/heads/x", "x..y", "x@{1}", ".hidden", "a/.b", "a//b", "x ", "x\n", "x~1", "x^", "x:y", "x?y", "x*", "x[", "x\\y", "@", "a/", "/a", "é", "x\x7f", "HEAD", "a b"}
}
