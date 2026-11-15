package backends

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/backends/fakes"
)

// The final review of #109: each test was written to fail on 0173f1c.

func TestTheCommitSubjectIsDefangedAndOwnershipStillHolds(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	title := "Fixes #123 and Fixes o/r#5 for @alice"
	meta := map[string]string{"agent": "fixture-agent", "session": "s-1"}
	in := OpenPRInput{Repo: "o/gate-api", Branch: "agent/t-1", Title: title, Files: []File{{Path: "a.txt", Content: "a"}}, Meta: meta}
	if _, err := e.git.OpenPR(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	var subject string
	for _, c := range gh.Calls() {
		if c.Method == "POST" && strings.HasSuffix(c.Target, "/git/commits") {
			var b struct{ Message string }
			_ = json.Unmarshal(c.Body, &b)
			subject = strings.SplitN(b.Message, "\n", 2)[0]
		}
	}
	if subject != defang(title) || subject == title {
		t.Fatalf("the commit subject is %q", subject)
	}
	out, err := e.git.EnsurePR(context.Background(), in)
	if err != nil || out["resumed"] != true {
		t.Fatalf("a replay is still this session's own: %v %v", out, err)
	}
}

func TestOnlyAFileOrADirectoryIsAnythingTheBaseHasAtAPath(t *testing.T) {
	gh, e := gitHubWithBase(t, map[string]string{"src/app.py": "x"})
	gh.SeedEntry("o/gate-api", "link", "symlink")
	gh.SeedEntry("o/gate-api", "vendor", "submodule")
	gh.SeedEntry("o/gate-api", "odd", "something-new")
	for _, p := range []string{"link", "link/x", "vendor", "vendor/x", "odd", "odd/y"} {
		a := prArgs("agent/t-1")
		a["files"] = filesArg(p)
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-PATH")
	}
	noWrites(t, gh)
	a := prArgs("agent/t-2")
	a["files"] = filesArg("src/app.py", "src/new.py")
	if _, err := e.call("repo.pr.open", a); err != nil {
		t.Fatal(err)
	}
}

func TestTextThatClosesAnIssueByAnyNameOrSpaceIsBroken(t *testing.T) {
	z := "​"
	for in, want := range map[string]string{
		"fixes GH-12": "fixes" + z + " GH-" + z + "12", "see GH-9": "see GH-" + z + "9", "Fixes #1": "Fixes" + z + " #" + z + "1",
		"closes GH-5": "closes" + z + " GH-" + z + "5", "resolved:　https://x/y": "resolved" + z + ":　https://x/y", "GH-x": "GH-x",
	} {
		if got := defang(in); got != want {
			t.Errorf("defang(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTheTitleAsItGoesOutIsCheckedBeforeAnyWrite(t *testing.T) {
	gh := fakes.NewGitHub("o/gate-api")
	defer gh.Close()
	e := newFakeEnv(t, gh, nil, nil, "s-1")
	for _, ti := range []string{strings.Repeat("<", 60), "<!-- hidden -->", "<!-- a --> <!-- b -->", "  <!-- x -->  "} {
		a := prArgs("agent/t-1")
		a["title"] = ti
		_, err := e.call("repo.pr.open", a)
		refusedWith(t, err, "GH-TITLE")
	}
	if len(gh.Calls()) != 0 {
		t.Fatalf("%d requests", len(gh.Calls()))
	}
	a := prArgs("agent/t-2")
	a["title"] = strings.Repeat("<", 50)
	if _, err := e.call("repo.pr.open", a); err != nil {
		t.Fatal(err)
	}
}
