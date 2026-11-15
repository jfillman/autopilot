package definitions_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// A pin is held to what an app team may have, whoever wrote the XR: the namespace of an XR is an app team's.

// The platform's own policy for the live deployment (GITHUB_OWNERS=jfillman; the images the bundled agents run).
func livePolicy(t *testing.T) (definitions.PinPolicy, map[string]*definitions.Definition) {
	t.Helper()
	reg := tools.Default()
	bundled, err := definitions.LoadDir(filepath.Join("..", "..", "..", "agents"), reg)
	if err != nil {
		t.Fatal(err)
	}
	var images []string
	for _, d := range bundled {
		if d.Image != nil {
			images = append(images, *d.Image)
		}
	}
	return definitions.PinPolicy{Tools: reg, AllowTools: definitions.DefaultPinTools(reg), Owners: []string{"jfillman"}, ImageGlobs: images, BindApp: true}, bundled
}

// The one live pin, app-gate-api-cicd/gate-reader at the commit its XR names (jfillman/gate-api, agents/gate-reader.yaml), read from the
// repository and kept as testdata, still loads under the policy.
func TestTheLivePinGateReaderStillLoads(t *testing.T) {
	raw, err := os.ReadFile("testdata/gate-reader.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pol, bundled := livePolicy(t)
	doc, reason, _ := definitions.ReadPinnedFile("agents/gate-reader.yaml", string(raw), false)
	if reason != "" {
		t.Fatal(reason)
	}
	src := definitions.Source{Kind: "cluster", Repo: definitions.S("jfillman/gate-api"), Path: definitions.S("agents/gate-reader.yaml"),
		Commit: definitions.S("7f8d9657cb943f03d258c2fbb092cee5fb90f6fd"), Namespace: definitions.S("app-gate-api-cicd"), XR: definitions.S("gate-reader")}
	if err := pol.CheckSource("app-gate-api-cicd", "jfillman/gate-api"); err != nil {
		t.Fatal(err)
	}
	l, rej := definitions.FromPin(doc, src, "group:default/team-platform", bundled, tools.Default(), pol)
	if rej != nil {
		t.Fatalf("the live pin is refused: %s (%s)", rej.Reason, rej.Detail)
	}
	if l.Name() != "gate-reader" {
		t.Fatal(l.Name())
	}
}

// The reason a pin is published with names a rule and a place, never what the file said: a file with a fake secret in it, in every
// way a reason could quote it.
func TestAPublishedReasonNeverQuotesTheFile(t *testing.T) {
	const secret = "AKIAFAKESECRETVALUE0123"
	base := func(over string) string {
		return "name: gate-triage\ndescription: d\nowner: " + owner + "\nkind: task\nidentity: {type: workload, serviceAccount: autopilot-runs/gate-triage}\n" +
			"profile:\n  tierCeiling: T0\n  tools: []\n  limits: {ttlMinutes: 5, toolCalls: 0, githubCalls: 0, openPrs: 0, modelTokens: 0, children: 0}\n" + over
	}
	files := map[string]string{
		"an unknown key":         base(secret + "_KEY: 1\n"),
		"an unknown nested key":  strings.Replace(base(""), "tools: []", "tools: []\n  "+secret+": 1", 1),
		"a duplicate key":        base("name: " + secret + "\n"),
		"an ambiguous value":     strings.Replace(base(""), "description: d", "description: yes", 1),
		"a bad value":            strings.Replace(base(""), "kind: task", "kind: "+secret, 1),
		"a bad name":             strings.Replace(base(""), "name: gate-triage", "name: "+secret, 1),
		"a bad owner":            strings.Replace(base(""), "owner: "+owner, "owner: group:default/"+strings.ToLower(secret), 1),
		"an identity of another": strings.Replace(base(""), "autopilot-runs/gate-triage", "autopilot-runs/"+strings.ToLower(secret), 1),
		"a policy refusal":       strings.Replace(base(""), "tools: []", "tools: ["+secret+"]", 1),
		"a syntax error":         base("x: [" + secret + "\n"),
		"a match key":            base("triggers:\n  - {type: event, source: glidepath, bindingKey: gate-api.x, match: {" + secret + " b: c}}\n"),
		"not a mapping":          "- " + secret + "\n",
	}
	pol, bundled := livePolicy(t)
	src := definitions.Source{Kind: "cluster", Repo: definitions.S("jfillman/gate-api"), Path: definitions.S("agents/gate-triage.yaml"),
		Commit: definitions.S(commit), Namespace: definitions.S("app-gate-api-cicd"), XR: definitions.S("gate-triage")}
	for name, text := range files {
		var published, detail string
		doc, reason, d := definitions.ReadPinnedFile("agents/gate-triage.yaml", text, false)
		published, detail = reason, d
		if reason == "" {
			_, rej := definitions.FromPin(doc, src, owner, bundled, tools.Default(), pol)
			if rej == nil {
				t.Errorf("%s: loaded", name)
				continue
			}
			published, detail = rej.Reason, rej.Detail
		}
		if published == "" || strings.Contains(published, secret) || strings.Contains(strings.ToLower(published), strings.ToLower(secret)) {
			t.Errorf("%s: the published reason quotes the file: %q", name, published)
		}
		if !strings.Contains(published, "DF-") {
			t.Errorf("%s: the published reason has no rule id: %q", name, published)
		}
		if name != "a syntax error" && name != "not a mapping" && name != "a duplicate key" && name != "an ambiguous value" && !strings.Contains(strings.ToLower(detail), strings.ToLower(secret)) && name != "a match key" {
			t.Logf("%s: detail %q", name, detail) // the log has the story where there is one
		}
	}
}

// Through the loader: what reaches the core (the request) and the store has no secret either; the log does.
func TestTheLoaderPublishesRedactedReasonsAndLogsTheStory(t *testing.T) {
	const secret = "DB_PASSWORD_hunter2"
	r := newRig(t)
	r.p.Policy.Detailed = false
	d := defDoc("leaky", map[string]any{secret: "x"})
	r.pin("leaky", "app-gate-api-cicd", d)
	r.poll()
	for _, b := range r.pub.rej {
		for _, rj := range b {
			if strings.Contains(rj.Reason, secret) {
				t.Fatalf("published: %q", rj.Reason)
			}
		}
	}
	for _, row := range r.store.rows {
		if strings.Contains(strings.TrimSpace(strings.Join([]string{asString(row["reason"])}, "")), secret) {
			t.Fatalf("stored: %v", row)
		}
	}
	if !strings.Contains(r.logs.String(), secret) || !strings.Contains(r.logs.String(), "DF-SCHEMA") {
		t.Fatalf("the log carries the detail: %s", r.logs.String())
	}
}

func asString(v any) string { s, _ := v.(string); return s }

// Another app's repository is never fetched: no token is minted, nothing is read, whatever the XR's author wants.
func TestAPinCannotNameAnotherAppsRepositoryOrNamespace(t *testing.T) {
	for name, c := range map[string]struct{ ns, repo string }{
		"another app's repository":           {"app-gate-api-cicd", "acme/billing"},
		"another app's gitops repository":    {"app-gate-api-cicd", "acme/gitops-billing"},
		"an owner that is not the gateway's": {"app-gate-api-cicd", "evil/gate-api"},
		"a namespace that is no app's":       {"kube-system", "acme/gate-api"},
		"a platform namespace":               {"autopilot-system", "acme/gate-api"},
		"a namespace of another shape":       {"app-gate-api", "acme/gate-api"},
		"a repository that is not a name":    {"app-gate-api-cicd", "acme"},
		"a nested path as a repository":      {"app-gate-api-cicd", "acme/gate-api/x"},
	} {
		r := newRig(t, c.ns)
		r.p.Policy = definitions.PinPolicy{Tools: tools.Default(), Owners: []string{"acme"}, BindApp: true}
		x := xrOf("one", c.ns)
		x["spec"].(map[string]any)["source"].(map[string]any)["repo"] = c.repo
		r.lister.items = []map[string]any{x}
		ch := r.poll()
		if len(ch.Added) != 0 || len(ch.Rejected) != 1 || len(r.files.calls) != 0 || !strings.HasPrefix(ch.Rejected[0].Reason, "DF-POLICY") {
			t.Errorf("%s: %+v, fetched %v", name, ch, r.files.calls)
		}
	}
	// its own are fine
	for _, repo := range []string{"acme/gate-api", "acme/gitops-gate-api", "acme/gitops-infra-gate-api"} {
		pol := definitions.PinPolicy{Owners: []string{"acme"}, BindApp: true}
		if err := pol.CheckSource("app-gate-api-cicd", repo); err != nil {
			t.Errorf("%s: %v", repo, err)
		}
	}
}

// Nobody can take another team's agent away by creating an XR of its name.
func TestAnEstablishedAgentNameIsNotTakenAwayByAnotherNamespace(t *testing.T) {
	r := newRig(t, "app-gate-api-cicd", "app-other-cicd")
	r.pin("shared", "app-gate-api-cicd", defDoc("shared", nil))
	r.poll()
	if r.defs["shared"] == nil {
		t.Fatal("the holder loads")
	}
	// the attacker's XR of the same name, in a namespace of its own app
	other := defDoc("shared", map[string]any{})
	other["profile"] = map[string]any{"tierCeiling": "T1", "tools": []any{"repo.file.get"}, "repos": map[string]any{"allow": []any{"acme/other"}},
		"limits": defDoc("x", nil)["profile"].(map[string]any)["limits"]}
	x := xrOf("shared", "app-other-cicd")
	x["spec"].(map[string]any)["source"].(map[string]any)["repo"] = "acme/other"
	r.lister.items = append(r.lister.items, x)
	r.files.files["acme/other|agents/shared.yaml|"+commit] = map[string]any{"doc": other}
	ch := r.poll()
	if len(ch.Removed) != 0 || r.defs["shared"] == nil || r.set.Loaded("shared").Source.Namespace == nil || *r.set.Loaded("shared").Source.Namespace != "app-gate-api-cicd" {
		t.Fatalf("the holder lost its name: %+v", ch)
	}
	if len(ch.Rejected) != 1 || *ch.Rejected[0].Source.Namespace != "app-other-cicd" || !strings.Contains(ch.Rejected[0].Reason, "DF-NAME") {
		t.Fatalf("only the newcomer is refused: %+v", ch.Rejected)
	}
	// a restart: the stored set is loaded before the first poll, so the holder is still the holder
	set2 := definitions.NewSet(r.set.Bundled())
	loaded, rejected := definitions.FromRecords(r.mgr.Set.Records(), set2.Bundled(), tools.Default(), definitions.PinPolicy{})
	set2.ReplaceCluster(loaded, rejected)
	l2, rj := definitions.FromPin(definitions.FromPlain(other).(*definitions.OMap), pinSourceIn("shared", "app-other-cicd", "acme/other"), owner, set2.Bundled(), tools.Default(), definitions.PinPolicy{})
	if rj != nil {
		t.Fatal(rj.Reason)
	}
	ch2 := set2.ReplaceCluster(append(append([]*definitions.Loaded{}, loaded...), l2), nil)
	if len(ch2.Changed) != 0 || *set2.Loaded("shared").Source.Namespace != "app-gate-api-cicd" {
		t.Fatalf("a restart flipped the holder: %+v", ch2)
	}
	// a name two namespaces ask for at once, with no holder, goes to neither (a creator cannot win a race by being first)
	r2 := newRig(t, "app-gate-api-cicd", "app-other-cicd")
	r2.pin("fresh", "app-gate-api-cicd", defDoc("fresh", nil))
	x2 := xrOf("fresh", "app-other-cicd")
	x2["spec"].(map[string]any)["source"].(map[string]any)["repo"] = "acme/other"
	r2.lister.items = append(r2.lister.items, x2)
	r2.files.files["acme/other|agents/fresh.yaml|"+commit] = map[string]any{"doc": other2(other, "fresh")}
	if ch := r2.poll(); len(ch.Added) != 0 || len(ch.Rejected) != 2 {
		t.Fatalf("%+v", ch)
	}
}

func other2(d map[string]any, name string) map[string]any {
	c := map[string]any{}
	for k, v := range d {
		c[k] = v
	}
	c["name"] = name
	c["identity"] = map[string]any{"type": "workload", "serviceAccount": "autopilot-runs/" + name}
	return c
}

func pinSourceIn(name, ns, repo string) definitions.Source {
	return definitions.Source{Kind: "cluster", Repo: definitions.S(repo), Path: definitions.S("agents/" + name + ".yaml"), Commit: definitions.S(commit),
		Namespace: definitions.S(ns), XR: definitions.S(name)}
}

// One tenant cannot fill the budget the others share: per-pin and per-list caps, a per-namespace count, and the publish size is
// checked BEFORE the live set is swapped, dropping the namespace that takes the most.
func TestOneTenantCannotFillThePublishBudgetOrFreezeTheOthers(t *testing.T) {
	r := newRig(t, "app-gate-api-cicd", "app-hog-cicd")
	r.p.Policy = definitions.PinPolicy{Tools: tools.Default(), Detailed: true}
	r.mgr.MaxBytes = 6000
	r.pin("quiet", "app-gate-api-cicd", defDoc("quiet", nil))
	for i := 0; i < 3; i++ {
		name := "hog" + string(rune('a'+i))
		d := defDoc(name, nil)
		d["runtime"].(map[string]any)["instructions"] = strings.Repeat("x", 1500)
		x := xrOf(name, "app-hog-cicd")
		r.lister.items = append(r.lister.items, x)
		r.files.files[repo+"|agents/"+name+".yaml|"+commit] = map[string]any{"doc": d}
	}
	ch := r.poll()
	if r.defs["quiet"] == nil || r.defs["hoga"] != nil || r.defs["hogb"] != nil || r.defs["hogc"] != nil {
		t.Fatalf("the namespace that takes the most is dropped, the quiet one is not: %v", r.defs)
	}
	if len(ch.Rejected) != 3 || !strings.HasPrefix(ch.Rejected[0].Reason, "DF-BUDGET") {
		t.Fatalf("%+v", ch.Rejected)
	}
	if r.pub.count() != 1 || len(r.pub.pins[0]) != 1 {
		t.Fatalf("the set that fits is published: %d publishes", r.pub.count())
	}
	// and what the gateway runs is always what was published
	if r.mgr.Unpublished() {
		t.Fatal("unpublished")
	}
}

func TestOneNamespaceHoldsAtMostThirtyTwoPins(t *testing.T) {
	r := newRig(t)
	for i := 0; i < definitions.MaxPinsPerNamespace+5; i++ {
		name := "a" + strings.Repeat("b", 1) + string(rune('a'+i/26)) + string(rune('a'+i%26))
		r.pin(name, "app-gate-api-cicd", defDoc(name, nil))
	}
	ch := r.poll()
	if len(ch.Added) != definitions.MaxPinsPerNamespace || len(ch.Rejected) != 5 || !strings.HasPrefix(ch.Rejected[0].Reason, "DF-QUOTA") {
		t.Fatalf("%d added, %d rejected", len(ch.Added), len(ch.Rejected))
	}
}

// A refusal about the repository's state (the broker, no such file) is asked again after a while; one about the commit's content is not.
func TestARefusalAboutTheRepositoryIsAskedAgainButTheContentIsNot(t *testing.T) {
	r := newRig(t)
	r.pin("late", "app-gate-api-cicd", defDoc("late", nil))
	file := r.files.files[repo+"|agents/late.yaml|"+commit]
	r.files.files[repo+"|agents/late.yaml|"+commit] = map[string]any{"missing": true}
	if ch := r.poll(); len(ch.Rejected) != 1 {
		t.Fatalf("%+v", ch)
	}
	r.files.files[repo+"|agents/late.yaml|"+commit] = file // the repository is onboarded, the file pushed
	n := len(r.files.calls)
	r.poll()
	if len(r.files.calls) != n {
		t.Fatal("asked again at once")
	}
	r.now = r.now.Add(definitions.NegativeCacheLifetime + time.Minute)
	if ch := r.poll(); len(ch.Added) != 1 {
		t.Fatalf("a refusal that was about the repository must be asked again: %+v", ch)
	}
	// the broker's refusal is the same
	r2 := newRig(t)
	r2.pin("denied", "app-gate-api-cicd", defDoc("denied", nil))
	r2.files.files[repo+"|agents/denied.yaml|"+commit] = map[string]any{"denied": []any{"GH-BROKER", "not onboarded"}}
	if ch := r2.poll(); len(ch.Rejected) != 1 {
		t.Fatal("not rejected")
	}
	r2.files.files[repo+"|agents/denied.yaml|"+commit] = map[string]any{"doc": defDoc("denied", nil)}
	r2.now = r2.now.Add(definitions.NegativeCacheLifetime + time.Minute)
	if ch := r2.poll(); len(ch.Added) != 1 {
		t.Fatalf("%+v", ch)
	}
	// a file whose content is bad is bad at that commit for good
	r3 := newRig(t)
	r3.pin("bad", "app-gate-api-cicd", defDoc("bad", map[string]any{"kind": "daemon"}))
	r3.poll()
	n3 := len(r3.files.calls)
	r3.now = r3.now.Add(time.Hour)
	r3.poll()
	if len(r3.files.calls) != n3 {
		t.Fatal("a content refusal was asked again")
	}
	_ = context.Background
}
