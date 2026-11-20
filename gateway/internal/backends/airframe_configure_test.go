package backends

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// airframe.configure's equivalence with Python is TestAirframeToolsEqualPython (the airframe.configure cases of airframe.json). These
// are the scope checks Go makes itself, where Python's Clearance core decides them before the handler runs.

func configureFixture(t *testing.T, name string) (*writesFile, writesCase) {
	t.Helper()
	var f writesFile
	loadJSON(t, "airframe.json", &f)
	for _, c := range f.Cases {
		if c.Tool == "airframe.configure" && c.Name == name {
			return &f, c
		}
	}
	t.Fatalf("no fixture case %q", name)
	return nil, writesCase{}
}

func TestConfigureIsScopedByAppAndRepository(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*definitions.Definition)
		rule string
	}{
		{"an app the definition does not name (R023)", func(d *definitions.Definition) { d.RawProfile = []byte(`{"airframe":{"apps":["other"]}}`) }, "R023"},
		{"no airframe apps at all (R023)", func(d *definitions.Definition) { d.RawProfile = []byte(`{"glidepath":{"apps":["*"]}}`) }, "R023"},
		{"a repository outside repos.allow (R014)", func(d *definitions.Definition) { d.ReposAllow = []string{"o/other"} }, "R014"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := configureFixture(t, "an SLO is added to the dev environment file")
			airframeDefOverride = tc.mod
			t.Cleanup(func() { airframeDefOverride = nil })
			e := newAirframeEnv(t, f.Tokens, &c)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, err := e.h["airframe.configure"](ctx, e.sess, expand(c.Args).(map[string]any))
			var den *core.Denied
			if !errors.As(err, &den) || den.Rule != tc.rule {
				t.Fatalf("want %s, got %v", tc.rule, err)
			}
			if n := len(e.requests()); n != 0 {
				t.Fatalf("%d requests were made before the refusal", n)
			}
		})
	}
}

func TestConfigureOnlyEverReadsAndOpensOnePullRequest(t *testing.T) {
	f, c := configureFixture(t, "an SLO is added to the dev environment file")
	e := newAirframeEnv(t, f.Tokens, &c)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.h["airframe.configure"](ctx, e.sess, expand(c.Args).(map[string]any)); err != nil {
		t.Fatal(err)
	}
	pulls := 0
	for _, r := range e.requests() {
		switch {
		case r.Method == "GET":
		case r.Method == "POST" && strings.HasSuffix(r.Target, "/pulls"):
			pulls++
		case r.Method == "POST" && (strings.HasSuffix(r.Target, "/git/trees") || strings.HasSuffix(r.Target, "/git/commits") ||
			strings.HasSuffix(r.Target, "/git/refs") || strings.HasSuffix(r.Target, "/agent-installation-token")):
		default:
			t.Fatalf("a %s request: %s", r.Method, r.Target)
		}
	}
	if pulls != 1 {
		t.Fatalf("pull requests opened: %d", pulls)
	}
}

func TestConfigureRefusesAProtectedPathBeforeAnyWrite(t *testing.T) {
	f, c := configureFixture(t, "an SLO is added to the dev environment file")
	airframeDefOverride = func(d *definitions.Definition) {
		d.DenyPaths = append(append([]string(nil), d.DenyPaths...), "glidepath/envs/**")
	}
	t.Cleanup(func() { airframeDefOverride = nil })
	e := newAirframeEnv(t, f.Tokens, &c)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := e.h["airframe.configure"](ctx, e.sess, expand(c.Args).(map[string]any))
	var den *core.Denied
	if !errors.As(err, &den) || den.Rule != "GH-PATH" || !strings.Contains(den.Message, "glidepath/envs/dev.yaml") {
		t.Fatalf("want GH-PATH naming the file, got %v", err)
	}
	for _, r := range e.requests() {
		if r.Method != "GET" && !strings.HasSuffix(r.Target, "/agent-installation-token") {
			t.Fatalf("a write was sent: %s %s", r.Method, r.Target)
		}
	}
}

// The file is read at one commit and the branch is built on exactly that commit, so a change that lands on the base in between cannot be
// written over: the base branch's ref is asked once, every read of the call is at that commit, and the new commit's parent is it.
func TestConfigureBuildsOnTheCommitItRead(t *testing.T) {
	f, c := configureFixture(t, "an SLO is added to the dev environment file")
	e := newAirframeEnv(t, f.Tokens, &c)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.h["airframe.configure"](ctx, e.sess, expand(c.Args).(map[string]any)); err != nil {
		t.Fatal(err)
	}
	var parent string
	refs, reads := 0, 0
	for _, r := range e.requests() {
		if r.Method == "POST" && strings.HasSuffix(r.Target, "/git/commits") {
			if ps, _ := r.Body.(map[string]any)["parents"].([]any); len(ps) == 1 {
				parent, _ = ps[0].(string)
			}
		}
		if r.Method == "GET" && strings.HasSuffix(r.Target, "/git/ref/heads/main") {
			refs++
		}
	}
	if parent == "" || refs != 1 {
		t.Fatalf("parent %q, base ref asked %d times", parent, refs)
	}
	for _, r := range e.requests() {
		if r.Method == "GET" && strings.Contains(r.Target, "/contents/glidepath/envs") {
			reads++
			if !strings.HasSuffix(r.Target, "?ref="+parent) {
				t.Fatalf("read %s, not at the commit the branch is built on (%s)", r.Target, parent)
			}
		}
	}
	if reads < 2 {
		t.Fatalf("%d reads of the env files", reads)
	}
}
