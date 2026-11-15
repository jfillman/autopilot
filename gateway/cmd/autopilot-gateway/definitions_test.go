package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/pgstate"
	"github.com/jfillman/autopilot/gateway/internal/readapi"
	"github.com/jfillman/autopilot/gateway/internal/tools"
	"go.yaml.in/yaml/v3"
)

// The definition set's settings (A-9 S4): Python's AGENT_XRS_ENABLED and AGENT_XRS_INTERVAL, and the two the Go gateway adds. A bad one
// stops the start; the loader never starts without being told which namespaces may define agents.

var githubOn = map[string]string{"GITHUB_PR_ENABLED": "1", "BROKER_URL": "https://b:8080", "GITHUB_OWNERS": "acme"}

func TestTheLoaderIsOffByDefaultAndOnWithGitHubOnlyWhenItKnowsWhereToRead(t *testing.T) {
	c, err := configFromEnv(envOf(base(nil)))
	if err != nil || c.defs.enabled {
		t.Fatalf("the loader is off without GitHub: %+v %v", c.defs, err)
	}
	if c.defs.interval != 60*time.Second || c.defs.maxTier != "T2" {
		t.Fatalf("defaults: %+v", c.defs)
	}
	// GitHub on turns the loader on (as in Python), and then it must be told where the reviewed Agent XRs are.
	if _, err := configFromEnv(envOf(base(githubOn))); err == nil || !strings.Contains(err.Error(), "AGENT_XRS_NAMESPACES") {
		t.Fatalf("on with no namespaces must stop the start: %v", err)
	}
	c, err = configFromEnv(envOf(base(merge(githubOn, map[string]string{"AGENT_XRS_NAMESPACES": "app-gate-cicd, app-other-cicd", "AGENT_XRS_INTERVAL": "5.5", "AGENT_XRS_MAX_TIER": "T1"}))))
	if err != nil {
		t.Fatal(err)
	}
	if !c.defs.enabled || len(c.defs.namespaces) != 2 || c.defs.namespaces[1] != "app-other-cicd" || c.defs.interval != 5500*time.Millisecond || c.defs.maxTier != "T1" {
		t.Fatalf("%+v", c.defs)
	}
	// off on purpose: bundled definitions only, no namespaces needed
	if c, err = configFromEnv(envOf(base(merge(githubOn, map[string]string{"AGENT_XRS_ENABLED": "0"})))); err != nil || c.defs.enabled {
		t.Fatalf("%+v %v", c.defs, err)
	}
}

func TestBadDefinitionSettingsStopTheStart(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"a bad flag", map[string]string{"AGENT_XRS_ENABLED": "maybe"}, "AGENT_XRS_ENABLED must be 0 or 1"},
		{"an interval under five seconds", map[string]string{"AGENT_XRS_INTERVAL": "4"}, "at least 5 seconds"},
		{"an interval that is not a number", map[string]string{"AGENT_XRS_INTERVAL": "soon"}, "AGENT_XRS_INTERVAL is a number"},
		{"an interval over a day", map[string]string{"AGENT_XRS_INTERVAL": "100000"}, "at most a day"},
		{"a tier that is not one", map[string]string{"AGENT_XRS_MAX_TIER": "T3"}, "T0, T1 or T2"},
		{"a wildcard namespace", map[string]string{"AGENT_XRS_NAMESPACES": "app-*"}, "not a namespace name"},
		{"every namespace", map[string]string{"AGENT_XRS_NAMESPACES": "*"}, "not a namespace name"},
		{"an upper-case namespace", map[string]string{"AGENT_XRS_NAMESPACES": "App"}, "not a namespace name"},
		{"a run's namespace", map[string]string{"AGENT_XRS_NAMESPACES": "agent-r-0a1b2c"}, "agent runs and the platform control it"},
		{"the run identity namespace", map[string]string{"AGENT_XRS_NAMESPACES": "autopilot-runs"}, "agent runs and the platform control it"},
		{"a system namespace", map[string]string{"AGENT_XRS_NAMESPACES": "kube-system"}, "agent runs and the platform control it"},
		{"too many namespaces", map[string]string{"AGENT_XRS_NAMESPACES": strings.Repeat("a,", 21) + "b"}, "more than 20 namespaces"},
		{"the loader without GitHub", map[string]string{"AGENT_XRS_ENABLED": "1", "AGENT_XRS_NAMESPACES": "a"}, "needs GITHUB_PR_ENABLED"},
	}
	for _, c := range cases {
		_, err := configFromEnv(envOf(base(c.env)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want a refusal naming %q, got %v", c.name, c.want, err)
		}
	}
}

func TestSettingsThisSliceCarriesAreNotRefusedAsUnported(t *testing.T) {
	for _, k := range []string{"AGENT_XRS_ENABLED", "AGENT_XRS_INTERVAL", "AGENT_XRS_NAMESPACES", "AGENT_XRS_MAX_TIER"} {
		for _, u := range unported {
			if u == k {
				t.Errorf("%s is carried by this slice but still listed as not carried", k)
			}
		}
	}
	// xr.request is carried since S5c: it needs GitHub, not a refusal as not carried
	if _, err := configFromEnv(envOf(base(map[string]string{"XR_REQUEST_ENABLED": "1"}))); err == nil || strings.Contains(err.Error(), "not carried") {
		t.Fatalf("xr.request: %v", err)
	}
}

// A candidate's document is read like any definition, whatever JSON decoder made it.
func TestACandidateDocumentIsReadByTheSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "agents", "triage-agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	asRequest, _ := json.Marshal(doc) // as a request body decodes: numbers are float64
	var fromRequest any
	if err := json.Unmarshal(asRequest, &fromRequest); err != nil {
		t.Fatal(err)
	}
	parse := candidateParser(tools.Default())
	d, err := parse(fromRequest)
	if err != nil || d.Name != "triage-agent" {
		t.Fatalf("%v %v", d, err)
	}
	bad := map[string]any{}
	_ = json.Unmarshal(asRequest, &bad)
	bad["kind"] = "daemon"
	if _, err := parse(bad); err == nil || !strings.Contains(err.Error(), "'daemon' is not one of") {
		t.Fatalf("%v", err)
	}
	bad["kind"] = "task"
	bad["extra"] = 1
	if _, err := parse(bad); err == nil || !strings.Contains(err.Error(), "Additional properties are not allowed") {
		t.Fatalf("%v", err)
	}
	for _, v := range []any{nil, "x", []any{}, 3.0} {
		if _, err := parse(v); err == nil {
			t.Errorf("%v was parsed", v)
		}
	}
}

// The pin policy `serve` builds is the strict one: bound to the app, to the gateway's owners, to the images the bundled agents run and
// RUNTIME_IMAGES_EXTRA, and to the default tools unless AGENT_XRS_TOOLS says otherwise.
func TestThePolicyServeBuildsIsTheStrictOne(t *testing.T) {
	reg := tools.Default()
	bundled, err := definitions.LoadDir(filepath.Join("..", "..", "..", "agents"), reg)
	if err != nil {
		t.Fatal(err)
	}
	c, err := configFromEnv(envOf(base(merge(githubOn, map[string]string{"AGENT_XRS_NAMESPACES": "app-gate-api-cicd", "GITHUB_OWNERS": "acme,other", "AIRFRAME_OWNER": "acme", "RUNTIME_IMAGES_EXTRA": "ghcr.io/acme/*"}))))
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.defs.policy(reg, c.hangar.githubOwners, c.runtimeImages, bundled)
	if err != nil {
		t.Fatal(err)
	}
	if !p.BindApp || p.Detailed || len(p.Owners) != 2 || p.Tools == nil || p.MaxTier != "T2" {
		t.Fatalf("%+v", p)
	}
	for _, bad := range []string{"glidepath.promote", "glidepath.rollback", "argo.sync.lower", "airframe.apply", "run.spawn", "xr.request", "repo.pr.open_upper", "glidepath.run.rerun"} {
		if p.AllowTools[bad] {
			t.Errorf("%s is a default tool of a pin", bad)
		}
	}
	for _, good := range []string{"glidepath.runs.list", "repo.file.get", "repo.pr.open", "human.request", "catalog.read"} {
		if !p.AllowTools[good] {
			t.Errorf("%s is not a default tool of a pin", good)
		}
	}
	hasImage, hasExtra := false, false
	for _, g := range p.ImageGlobs {
		hasImage = hasImage || strings.HasPrefix(g, "ghcr.io/jfillman/autopilot-agent-runtime@sha256:")
		hasExtra = hasExtra || g == "ghcr.io/acme/*"
	}
	if !hasImage || !hasExtra {
		t.Fatalf("%v", p.ImageGlobs)
	}
	// the platform can list more, and only what is a tool
	c.defs.tools = []string{"repo.file.get", "argo.sync.lower"}
	p, err = c.defs.policy(reg, nil, nil, bundled)
	if err != nil || !p.AllowTools["argo.sync.lower"] || p.AllowTools["repo.pr.open"] {
		t.Fatalf("%v %v", p.AllowTools, err)
	}
	c.defs.tools = []string{"no.such.tool"}
	if _, err := c.defs.policy(reg, nil, nil, bundled); err == nil || !strings.Contains(err.Error(), "AGENT_XRS_TOOLS") {
		t.Fatalf("%v", err)
	}
}

// AGENT_XRS_NAMES: the agents whose names do not start with their app, each for one namespace the loader reads.
func TestAgentXRSNamesIsTheExceptionMapForNamesThatDoNotFollowTheConvention(t *testing.T) {
	env := merge(githubOn, map[string]string{"AGENT_XRS_NAMESPACES": "app-gate-api-cicd,app-other-cicd", "AGENT_XRS_NAMES": "gate-reader=app-gate-api-cicd, old-thing=app-other-cicd"})
	c, err := configFromEnv(envOf(base(env)))
	if err != nil || c.defs.names["gate-reader"] != "app-gate-api-cicd" || len(c.defs.names) != 2 {
		t.Fatalf("%+v %v", c.defs.names, err)
	}
	p, err := c.defs.policy(tools.Default(), nil, nil, nil)
	if err != nil || p.Names["gate-reader"] != "app-gate-api-cicd" {
		t.Fatalf("%+v %v", p.Names, err)
	}
	for name, v := range map[string]string{"no namespace": "gate-reader", "a namespace the loader does not read": "gate-reader=app-else-cicd", "a bad name": "Gate=app-gate-api-cicd",
		"a name twice": "x=app-gate-api-cicd,x=app-other-cicd"} {
		_, err := configFromEnv(envOf(base(merge(env, map[string]string{"AGENT_XRS_NAMES": v}))))
		if err == nil || !strings.Contains(err.Error(), "AGENT_XRS_NAMES") {
			t.Errorf("%s: %v", name, err)
		}
	}
	bundled := map[string]*definitions.Definition{"researcher": {Name: "researcher"}}
	c.defs.names = map[string]string{"researcher": "app-gate-api-cicd"}
	if _, err := c.defs.policy(tools.Default(), nil, nil, bundled); err == nil || !strings.Contains(err.Error(), "bundled") {
		t.Fatalf("a bundled name cannot be listed: %v", err)
	}
}

// The api must show what serve decided. serve holds every stored pin to the strict policy at start; a pin that no longer passes (a policy
// tightened since the row was written) is rejected there, and the decision is STORED, so the api's refresher, which reads the store and
// decides nothing, shows it as rejected too (it used to re-decide with a weaker policy and show it loaded).
func TestTheAPIShowsThePinServeRejected(t *testing.T) {
	dsn, _ := scratchDB(t)
	ctx := context.Background()
	st, err := pgstate.Open(ctx, dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)
	reg := tools.Default()
	bundled, err := definitions.LoadDir(filepath.Join("..", "..", "..", "agents"), reg)
	if err != nil {
		t.Fatal(err)
	}
	doc := func(name string, tool string) map[string]any {
		return map[string]any{"name": name, "description": "d", "owner": "group:default/team-a", "kind": "task",
			"identity": map[string]any{"type": "workload", "serviceAccount": "autopilot-runs/" + name},
			"profile": map[string]any{"tierCeiling": "T1", "tools": []any{tool}, "repos": map[string]any{"allow": []any{"acme/gate-api"}},
				"limits": map[string]any{"ttlMinutes": 20, "toolCalls": 1, "githubCalls": 1, "openPrs": 0, "modelTokens": 1, "children": 0}},
			"runtime": map[string]any{"image": "ghcr.io/acme/agent@sha256:" + strings.Repeat("a", 64)}}
	}
	src := func(n string) map[string]any {
		return map[string]any{"repo": "acme/gate-api", "path": "agents/" + n + ".yaml", "commit": strings.Repeat("c", 40), "namespace": "app-gate-api-cicd", "xr": n}
	}
	rows := []map[string]any{
		{"name": "gate-api-fine", "state": "loaded", "source": src("gate-api-fine"), "doc": doc("gate-api-fine", "repo.file.get")},
		// argo.sync.lower is not a tool a pin gets by default: the strict policy refuses it, whatever an older gateway stored.
		{"name": "gate-api-risky", "state": "loaded", "source": src("gate-api-risky"), "doc": doc("gate-api-risky", "argo.sync.lower")},
	}
	if err := st.ReplaceDefinitions(ctx, rows); err != nil {
		t.Fatal(err)
	}
	c, err := configFromEnv(envOf(base(merge(githubOn, map[string]string{"AGENT_XRS_NAMESPACES": "app-gate-api-cicd", "GITHUB_OWNERS": "acme", "RUNTIME_IMAGES_EXTRA": "ghcr.io/acme/*"}))))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := c.defs.policy(reg, c.hangar.githubOwners, c.runtimeImages, bundled)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := st.Definitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	serveSet := definitions.NewSet(bundled)
	loadStored(ctx, serveSet, st, rowsOf(raw), reg, policy, log)
	loaded, rejected, _ := serveSet.ClusterSet()
	if len(loaded) != 1 || loaded[0].Name() != "gate-api-fine" || len(rejected) != 1 || rejected[0].Name != "gate-api-risky" {
		t.Fatalf("serve decided: %d loaded, rejected %+v", len(loaded), rejected)
	}

	// The api, as its own process: it opens the database read-only and refreshes from the store.
	apiStore, err := readapi.OpenPg(ctx, dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer apiStore.Close()
	apiSet := definitions.NewSet(bundled)
	(&readapi.Refresher{Set: apiSet, Store: apiStore, Reg: reg, Every: time.Hour, Log: log}).Once(ctx)
	loaded, rejected, _ = apiSet.ClusterSet()
	if len(loaded) != 1 || loaded[0].Name() != "gate-api-fine" || len(rejected) != 1 || rejected[0].Name != "gate-api-risky" {
		t.Fatalf("the api shows %d loaded, rejected %+v: it must show what serve decided", len(loaded), rejected)
	}
	if apiSet.Snapshot()["gate-api-risky"] != nil {
		t.Fatal("the api would run an agent serve rejected")
	}
	// A restart that decides nothing new writes nothing: the store is not rewritten when serve agrees with it.
	before, _ := st.Definitions(ctx)
	loadStored(ctx, definitions.NewSet(bundled), st, rowsOf(before), reg, policy, log)
	after, _ := st.Definitions(ctx)
	if len(before) != len(after) {
		t.Fatalf("%d rows became %d", len(before), len(after))
	}
}
