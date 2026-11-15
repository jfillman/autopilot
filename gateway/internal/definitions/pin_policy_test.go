package definitions_test

import (
	"context"
	"encoding/json"
	"fmt"
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
func livePolicy(t *testing.T, names map[string]string) (definitions.PinPolicy, map[string]*definitions.Definition) {
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
	return definitions.PinPolicy{Tools: reg, AllowTools: definitions.DefaultPinTools(reg), Owners: []string{"jfillman"}, ImageGlobs: images, BindApp: true, Names: names}, bundled
}

// The one live pin, app-gate-api-cicd/gate-reader at the commit its XR names (jfillman/gate-api, agents/gate-reader.yaml), read from the
// repository and kept as testdata, still loads under the policy.
func TestTheLivePinGateReaderStillLoads(t *testing.T) {
	raw, err := os.ReadFile("testdata/gate-reader.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pol, bundled := livePolicy(t, map[string]string{"gate-reader": "app-gate-api-cicd"})
	doc, reason, _ := definitions.ReadPinnedFile("agents/gate-reader.yaml", string(raw), false)
	if reason != "" {
		t.Fatal(reason)
	}
	src := definitions.Source{Kind: "cluster", Repo: definitions.S("jfillman/gate-api"), Path: definitions.S("agents/gate-reader.yaml"),
		Commit: definitions.S("7f8d9657cb943f03d258c2fbb092cee5fb90f6fd"), Namespace: definitions.S("app-gate-api-cicd"), XR: definitions.S("gate-reader")}
	if err := pol.CheckSource("app-gate-api-cicd", "gate-reader", "jfillman/gate-api"); err != nil {
		t.Fatal(err)
	}
	l, rej := definitions.FromPin(doc, src, "group:default/team-platform", bundled, tools.Default(), pol)
	if rej != nil {
		t.Fatalf("the live pin is refused: %s (%s)", rej.Reason, rej.Detail)
	}
	if l.Name() != "gate-reader" {
		t.Fatal(l.Name())
	}
	// without its entry in AGENT_XRS_NAMES (its name does not start with its app) it is refused, before anything is fetched, whatever else is right
	bare, _ := livePolicy(t, nil)
	if err := bare.CheckSource("app-gate-api-cicd", "gate-reader", "jfillman/gate-api"); err == nil || !strings.Contains(err.Error(), "metadata.name") {
		t.Fatalf("gate-reader needs its entry: %v", err)
	}
	if _, rej := definitions.FromPin(doc, src, "group:default/team-platform", bundled, tools.Default(), bare); rej == nil || !strings.HasPrefix(rej.Reason, "DF-NAME") {
		t.Fatalf("%v", rej)
	}
	// and the entry is for its namespace alone
	other, _ := livePolicy(t, map[string]string{"gate-reader": "app-other-cicd"})
	if err := other.CheckSource("app-gate-api-cicd", "gate-reader", "jfillman/gate-api"); err == nil {
		t.Fatal("an entry for another namespace admits nothing here")
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
	pol, bundled := livePolicy(t, map[string]string{"gate-triage": "app-gate-api-cicd"})
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
		x := xrOf("gate-api-one", c.ns)
		x["spec"].(map[string]any)["source"].(map[string]any)["repo"] = c.repo
		r.lister.items = []map[string]any{x}
		ch := r.poll()
		if len(ch.Added) != 0 || len(ch.Rejected) != 1 || len(r.files.calls) != 0 || !(strings.HasPrefix(ch.Rejected[0].Reason, "DF-POLICY") || strings.HasPrefix(ch.Rejected[0].Reason, "DF-XR") || strings.HasPrefix(ch.Rejected[0].Reason, "DF-NAME")) {
			t.Errorf("%s: %+v, fetched %v", name, ch, r.files.calls)
		}
	}
	// its own are fine
	for _, repo := range []string{"acme/gate-api", "acme/gitops-gate-api", "acme/gitops-infra-gate-api"} {
		pol := definitions.PinPolicy{Owners: []string{"acme"}, BindApp: true}
		if err := pol.CheckSource("app-gate-api-cicd", "gate-api-x", repo); err != nil {
			t.Errorf("%s: %v", repo, err)
		}
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

// -- what one tenant can do to another -----------------------------------------------------------------------------------------------

func appRig(t *testing.T, namespaces ...string) *rig {
	t.Helper()
	r := newRig(t, namespaces...)
	r.p.Policy = definitions.PinPolicy{Tools: tools.Default(), Owners: []string{"acme"}, ImageGlobs: []string{"ghcr.io/acme/runtime@*"}, BindApp: true, Detailed: false}
	r.mgr.Namespaces = len(namespaces)
	r.p.Policy.Namespaces = namespaces
	return r
}

func appPin(r *rig, name, ns, repository string) {
	app, _ := definitions.AppOfNamespace(ns)
	d := defDoc(name, nil)
	d["profile"].(map[string]any)["repos"] = map[string]any{"allow": []any{"acme/" + app}}
	x := xrOf(name, ns)
	x["spec"].(map[string]any)["source"].(map[string]any)["repo"] = repository
	r.lister.items = append(r.lister.items, x)
	r.files.files[repository+"|agents/"+name+".yaml|"+commit] = map[string]any{"doc": d}
}

// The rows a poll publishes have a fixed largest size, whatever an XR writes: nothing of its own strings is stored or echoed when they are
// not in the form a pin's are, and an XR that is not rejected for its form is rejected with fields that are.
func TestXRFieldsAreBoundedBeforeTheyAreStoredOrEchoed(t *testing.T) {
	r := appRig(t, "app-hog-cicd", "app-gate-api-cicd")
	appPin(r, "gate-api-quiet", "app-gate-api-cicd", "acme/gate-api")
	huge := strings.Repeat("p", 50_000)
	for i := 0; i < 3; i++ {
		x := xrOf(fmt.Sprintf("hog-%d", i), "app-hog-cicd")
		src := x["spec"].(map[string]any)["source"].(map[string]any)
		src["path"], src["repo"], src["commit"] = huge, "evil/"+huge, huge
		x["spec"].(map[string]any)["owner"] = huge
		r.lister.items = append(r.lister.items, x)
	}
	long := xrOf("hog-long-name", "app-hog-cicd")
	long["metadata"].(map[string]any)["name"] = strings.Repeat("n", 200)
	r.lister.items = append(r.lister.items, long)
	ch := r.poll()
	if r.defs["gate-api-quiet"] == nil || r.pub.count() != 1 || r.mgr.Unpublished() {
		t.Fatalf("another namespace's pin must load and be published: %+v", ch)
	}
	body, _ := json.Marshal(struct{ A, B any }{r.pub.pins, r.pub.rej})
	if len(body) > 40_000 || len(r.pub.rej[0]) == 0 {
		t.Fatalf("the rows are not bounded: %d bytes, %d rejections", len(body), len(r.pub.rej[0]))
	}
	for _, rj := range r.pub.rej[0] {
		if len(rj.Reason) > 400 || len(rj.Name) > 70 || (rj.Source.Path != nil && len(*rj.Source.Path) > 200) || (rj.Source.Repo != nil && len(*rj.Source.Repo) > 201) || (rj.Source.Commit != nil && len(*rj.Source.Commit) > 100) {
			t.Fatalf("a rejected row of an unbounded size: %+v", rj)
		}
	}
	rows, _ := json.Marshal(r.store.rows)
	if len(rows) > 40_000 {
		t.Fatalf("the stored rows are %d bytes", len(rows))
	}
}

// 257 XRs in one namespace freeze nothing: the namespace gets its quota (the first by name), the rest are counted in one row, the poll
// goes on, and the other namespaces' pins are untouched.
func TestAHugeNamespaceCostsOnlyItself(t *testing.T) {
	r := appRig(t, "app-hog-cicd", "app-gate-api-cicd")
	appPin(r, "gate-api-quiet", "app-gate-api-cicd", "acme/gate-api")
	for i := 0; i < 300; i++ {
		r.lister.items = append(r.lister.items, xrOf(fmt.Sprintf("hog-%03d", i), "app-hog-cicd"))
	}
	ch := r.poll()
	if r.defs["gate-api-quiet"] == nil || r.mgr.Unpublished() || r.pub.count() != 1 {
		t.Fatalf("the poll must go on: %+v", ch)
	}
	hog := 0
	var summary bool
	for _, rj := range r.pub.rej[0] {
		if rj.Source.Namespace != nil && *rj.Source.Namespace == "app-hog-cicd" {
			hog++
			summary = summary || strings.Contains(rj.Reason, "more rejected pins in this namespace are not listed")
		}
	}
	if hog != definitions.MaxRejectedPerNamespace+1 || !summary {
		t.Fatalf("%d rows of the namespace, summary %v", hog, summary)
	}
}

// Each namespace has a share of the budget fixed by configuration: one over its share loses its own rows and nothing else, and the
// bundled set is published whatever it does.
func TestANamespaceOverItsShareLosesOnlyItsOwnRows(t *testing.T) {
	r := appRig(t, "app-hog-cicd", "app-gate-api-cicd")
	r.mgr.NamespaceShare = 9000
	appPin(r, "gate-api-quiet", "app-gate-api-cicd", "acme/gate-api")
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("hog-%d", i)
		d := defDoc(name, nil)
		d["profile"].(map[string]any)["repos"] = map[string]any{"allow": []any{"acme/hog"}}
		d["runtime"].(map[string]any)["instructions"] = strings.Repeat("x", 1500)
		x := xrOf(name, "app-hog-cicd")
		x["spec"].(map[string]any)["source"].(map[string]any)["repo"] = "acme/hog"
		r.lister.items = append(r.lister.items, x)
		r.files.files["acme/hog|agents/"+name+".yaml|"+commit] = map[string]any{"doc": d}
	}
	// rejected rows of the hog too: they count against its share, not the quiet namespace's
	for i := 0; i < 20; i++ {
		x := xrOf(fmt.Sprintf("hog-bad-%02d", i), "app-hog-cicd")
		x["spec"].(map[string]any)["source"].(map[string]any)["repo"] = "evil/x"
		r.lister.items = append(r.lister.items, x)
	}
	ch := r.poll()
	if r.defs["gate-api-quiet"] == nil || r.defs["hog-0"] != nil {
		t.Fatalf("the namespace over its share loses its own pins, the other does not: %v", r.defs)
	}
	if r.pub.count() != 1 || len(r.pub.pins[0]) != 1 || len(r.pub.bundled[0]) != 1 || r.mgr.Unpublished() {
		t.Fatalf("what fits is published, with the bundled set: %d", r.pub.count())
	}
	budgetRows := 0
	for _, rj := range r.pub.rej[0] {
		if strings.HasPrefix(rj.Reason, "DF-BUDGET") && *rj.Source.Namespace == "app-hog-cicd" {
			budgetRows++
		}
	}
	if budgetRows != 1 || len(r.pub.rej[0]) != 1 || len(ch.Rejected) != 1 {
		t.Fatalf("one row says so: %+v", r.pub.rej[0])
	}
}

// An agent's name belongs to its namespace's app, decided from the XR alone: no holder, no state, nothing to take, squat or move.
func TestAnAgentNameBelongsToItsAppAndNothingCanMoveIt(t *testing.T) {
	// the rules
	pol := definitions.PinPolicy{BindApp: true, Names: map[string]string{"gate-reader": "app-gate-api-cicd"}}
	for _, c := range []struct {
		ns, name string
		ok       bool
	}{{"app-gate-api-cicd", "gate-api", true}, {"app-gate-api-cicd", "gate-api-triage", true}, {"app-gate-api-cicd", "gate-reader", true},
		{"app-gate-api-cicd", "gate-apix", false}, {"app-gate-api-cicd", "gate", false}, {"app-gate-api-cicd", "triage-agent", false},
		{"app-other-cicd", "gate-reader", false}, {"app-other-cicd", "other-x", true}, {"app-gate-cicd", "gate-api-x", true}, {"app-gate-api-cicd", "billing-x", false}} {
		err := pol.CheckSource(c.ns, c.name, "acme/"+strings.TrimSuffix(strings.TrimPrefix(c.ns, "app-"), "-cicd"))
		if (err == nil) != c.ok {
			t.Errorf("%s in %s: %v", c.name, c.ns, err)
		}
	}
	// the squat, the holder's outage and a cutover with an empty store, in one sequence
	r := appRig(t, "app-gate-api-cicd", "app-other-cicd")
	appPin(r, "gate-api-reader", "app-gate-api-cicd", "acme/gate-api")
	appPin(r, "other-thing", "app-other-cicd", "acme/other")
	// the attacker, in its own app's namespace, asks for the holder's name
	squat := defDoc("gate-api-reader", nil)
	squat["profile"].(map[string]any)["repos"] = map[string]any{"allow": []any{"acme/other"}}
	sx := xrOf("gate-api-reader", "app-other-cicd")
	sx["spec"].(map[string]any)["source"].(map[string]any)["repo"] = "acme/other"
	r.lister.items = append(r.lister.items, sx)
	r.files.files["acme/other|agents/gate-api-reader.yaml|"+commit] = map[string]any{"doc": squat}
	ch := r.poll()
	holder := r.set.Loaded("gate-api-reader")
	if holder == nil || *holder.Source.Namespace != "app-gate-api-cicd" || r.defs["other-thing"] == nil {
		t.Fatalf("%+v", ch)
	}
	sawSquat := false
	for _, rj := range ch.Rejected {
		sawSquat = sawSquat || (*rj.Source.Namespace == "app-other-cicd" && strings.HasPrefix(rj.Reason, "DF-NAME"))
	}
	if !sawSquat {
		t.Fatalf("the squatter is refused for the name: %+v", ch.Rejected)
	}
	for _, f := range r.files.calls {
		if strings.Contains(f, "acme/other|agents/gate-api-reader") {
			t.Fatal("the squatter's file was fetched")
		}
	}
	// the holder bumps its commit and the fetch fails once: it stays (re-added), the squatter does not take its name
	c2 := strings.Repeat("b", 40)
	for _, x := range r.lister.items {
		if md := x["metadata"].(map[string]any); md["name"] == "gate-api-reader" && md["namespace"] == "app-gate-api-cicd" {
			x["spec"].(map[string]any)["source"].(map[string]any)["commit"] = c2
		}
	}
	r.files.files[repo+"|agents/gate-api-reader.yaml|"+c2] = map[string]any{"transient": true}
	r.poll()
	if l := r.set.Loaded("gate-api-reader"); l == nil || *l.Source.Namespace != "app-gate-api-cicd" {
		t.Fatalf("the holder's outage moved its name: %v", l)
	}
	// the fetch works again: the holder is the holder, at its new commit
	r.files.files[repo+"|agents/gate-api-reader.yaml|"+c2] = r.files.files[repo+"|agents/gate-api-reader.yaml|"+commit]
	if ch := r.poll(); len(ch.Changed) != 1 || *r.set.Loaded("gate-api-reader").Source.Namespace != "app-gate-api-cicd" {
		t.Fatalf("%+v", ch)
	}
	// a cutover with an empty store: a fresh gateway, the squatter's XR is first in the list, nothing in the store
	r2 := appRig(t, "app-gate-api-cicd", "app-other-cicd")
	r2.lister.items = []map[string]any{sx}
	r2.files.files = r.files.files
	appPin(r2, "gate-api-reader", "app-gate-api-cicd", "acme/gate-api")
	r2.lister.items = []map[string]any{sx, r2.lister.items[len(r2.lister.items)-1]}
	if ch := r2.poll(); r2.set.Loaded("gate-api-reader") == nil || *r2.set.Loaded("gate-api-reader").Source.Namespace != "app-gate-api-cicd" || len(ch.Added) != 1 {
		t.Fatalf("an empty store flipped the name: %+v", ch)
	}
	// two XRs of one name in ONE namespace cannot exist (k8s names are unique), but two pins of one name reaching the set are both refused
	set := definitions.NewSet(nil)
	a, b0 := mustLoad(t, "dup"), mustLoad(t, "dup")
	b1 := *b0
	b1.Source.Namespace = definitions.S("app-other-cicd")
	b := &b1
	if ch := set.ReplaceCluster([]*definitions.Loaded{a, b}, nil); len(ch.Added) != 0 || len(ch.Rejected) != 2 {
		t.Fatalf("%+v", ch)
	}
}

// An app named so that the name-based mapping of its repositories is ambiguous has no pins: `gitops-infra-foo` is foo's, never infra-foo's.
func TestAnAppWhoseRepositoriesWouldBeAmbiguousHasNoPins(t *testing.T) {
	for ns, ok := range map[string]bool{"app-foo-cicd": true, "app-x-cicd": true, "app-infra-foo-cicd": false, "app-gitops-x-cicd": false, "app-infra-cicd": true, "app-gate-api-cicd": true, "app-cicd": false} {
		if _, got := definitions.AppOfNamespace(ns); got != ok {
			t.Errorf("%s: %v", ns, got)
		}
	}
	pol := definitions.PinPolicy{BindApp: true, Owners: []string{"o"}}
	if err := pol.CheckSource("app-foo-cicd", "foo-x", "o/gitops-infra-foo"); err != nil {
		t.Fatalf("foo's own gitops repository: %v", err)
	}
	if err := pol.CheckSource("app-foo-cicd", "foo-x", "o/gitops-infra-bar"); err == nil {
		t.Fatal("another app's")
	}
	if err := pol.CheckSource("app-infra-foo-cicd", "infra-foo-x", "o/gitops-infra-foo"); err == nil {
		t.Fatal("an app named infra-* has no pins")
	}
	if err := pol.CheckSource("app-x-cicd", "x-a", "o/gitops-x"); err != nil {
		t.Fatal(err)
	}
}

// Prefix-related apps cannot contend for a name: its owner is the namespace listed for it, else the configured namespace whose app is the
// longest match of its start, and any other namespace's XR of it is refused alone: the owner's agent is never lost.
func TestPrefixRelatedAppsCannotTakeEachOthersNames(t *testing.T) {
	for _, tc := range []struct {
		nsA, nsB, name, xrNS, wantOwner string
	}{
		{"app-gate-cicd", "app-gate-api-cicd", "gate-api-reader", "app-gate-cicd", "app-gate-api-cicd"},
		{"app-gate-cicd", "app-gate-api-cicd", "gate-api-reader", "app-gate-api-cicd", "app-gate-api-cicd"},
		{"app-gate-cicd", "app-gate-api-cicd", "gate-reader", "app-gate-api-cicd", "app-gate-cicd"},
		{"app-gate-cicd", "app-gate-api-cicd", "gate-reader", "app-gate-cicd", "app-gate-cicd"},
	} {
		pol := definitions.PinPolicy{BindApp: true, Namespaces: []string{tc.nsA, tc.nsB}}
		owner, ok := pol.NameOwner(tc.name)
		err := pol.CheckSource(tc.xrNS, tc.name, "acme/"+strings.TrimSuffix(strings.TrimPrefix(tc.xrNS, "app-"), "-cicd"))
		if !ok || owner != tc.wantOwner || (err == nil) != (tc.xrNS == tc.wantOwner) {
			t.Errorf("%+v: owner %q, %v", tc, owner, err)
		}
	}
	// the listed name overrides the prefix claimant: gate-reader=app-gate-api-cicd also starts with gate-
	pol := definitions.PinPolicy{BindApp: true, Namespaces: []string{"app-gate-cicd", "app-gate-api-cicd"}, Names: map[string]string{"gate-reader": "app-gate-api-cicd"}}
	if err := pol.CheckSource("app-gate-cicd", "gate-reader", "acme/gate"); err == nil {
		t.Error("the prefix claimant took a listed name")
	}
	if err := pol.CheckSource("app-gate-api-cicd", "gate-reader", "acme/gate-api"); err != nil {
		t.Error(err)
	}
	// a three-level chain
	chain := definitions.PinPolicy{BindApp: true, Namespaces: []string{"app-a-b-c-cicd", "app-a-cicd", "app-a-b-cicd"}}
	for name, want := range map[string]string{"a": "app-a-cicd", "a-x": "app-a-cicd", "a-b": "app-a-b-cicd", "a-b-x": "app-a-b-cicd", "a-bc": "app-a-cicd",
		"a-b-c": "app-a-b-c-cicd", "a-b-c-x": "app-a-b-c-cicd", "a-b-cd": "app-a-b-cicd"} {
		if got, _ := chain.NameOwner(name); got != want {
			t.Errorf("%s: owner %q, want %q", name, got, want)
		}
	}
	if _, ok := chain.NameOwner("zzz"); ok {
		t.Error("a name of no app has no owner")
	}
	// through the loader: the attacker's XR of the victim's name loses alone; the victim's agent stays, and its file is the only one fetched
	r := appRig(t, "app-gate-cicd", "app-gate-api-cicd")
	appPin(r, "gate-api-reader", "app-gate-api-cicd", "acme/gate-api")
	atk := defDoc("gate-api-reader", nil)
	atk["profile"].(map[string]any)["repos"] = map[string]any{"allow": []any{"acme/gate"}}
	ax := xrOf("gate-api-reader", "app-gate-cicd")
	ax["spec"].(map[string]any)["source"].(map[string]any)["repo"] = "acme/gate"
	r.lister.items = append(r.lister.items, ax)
	r.files.files["acme/gate|agents/gate-api-reader.yaml|"+commit] = map[string]any{"doc": atk}
	ch := r.poll()
	l := r.set.Loaded("gate-api-reader")
	if l == nil || *l.Source.Namespace != "app-gate-api-cicd" || len(ch.Rejected) != 1 || !strings.HasPrefix(ch.Rejected[0].Reason, "DF-NAME") || *ch.Rejected[0].Source.Namespace != "app-gate-cicd" {
		t.Fatalf("%+v", ch)
	}
	for _, f := range r.files.calls {
		if strings.Contains(f, "acme/gate|") {
			t.Fatal("the claimant's file was fetched")
		}
	}
}

// The budget is divided by the number of namespaces the loader reads.
func TestTheBudgetIsDividedByTheNumberOfNamespaces(t *testing.T) {
	build := func(namespaces int) bool {
		r := newRig(t)
		r.mgr.MaxBytes, r.mgr.Namespaces = 30000, namespaces
		l := mustLoad(t, "gate-api-big")
		l.Doc["padding"] = strings.Repeat("z", 7000)
		r.mgr.ApplyCluster(context.Background(), []*definitions.Loaded{l}, nil)
		return r.defs["gate-api-big"] != nil
	}
	if !build(1) || build(4) {
		t.Fatal("one namespace has the budget; four have a quarter each")
	}
}

// The core's limit is the sum of the namespaces' shares: with many namespaces each gets 256 / namespaces.
func TestTheQuotaIsTheCoresLimitDividedByTheNamespaces(t *testing.T) {
	var ns []string
	for i := 0; i < 32; i++ {
		ns = append(ns, fmt.Sprintf("app-n%02d-cicd", i))
	}
	r := newRig(t, ns...)
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("a%02d", i)
		r.lister.items = append(r.lister.items, xrOf(name, "app-n00-cicd"))
		r.files.files[repo+"|agents/"+name+".yaml|"+commit] = map[string]any{"doc": defDoc(name, nil)}
	}
	ch := r.poll()
	if len(ch.Added) != definitions.MaxPins/32 || len(ch.Rejected) != 20-definitions.MaxPins/32 {
		t.Fatalf("%d added, %d rejected: want %d each of 256 / 32", len(ch.Added), len(ch.Rejected), definitions.MaxPins/32)
	}
}

// Every string an XR supplies is checked before it is stored or echoed: a repository or an owner that is too long or malformed, and a path that is
// not a clean relative file path, is recorded with placeholders.
func TestEachXRFieldIsCheckedAndNeverEchoed(t *testing.T) {
	long := strings.Repeat("q", 150)
	for name, mut := range map[string]func(spec, src map[string]any){
		"a repository with a long owner":   func(spec, src map[string]any) { src["repo"] = long + "/gate-api" },
		"a repository with a long name":    func(spec, src map[string]any) { src["repo"] = "acme/" + long },
		"a repository with a space":        func(spec, src map[string]any) { src["repo"] = "ac me/gate-api" },
		"a repository with a second slash": func(spec, src map[string]any) { src["repo"] = "acme/gate-api/x" },
		"an owner that is long":            func(spec, src map[string]any) { spec["owner"] = long },
		"an owner with a space":            func(spec, src map[string]any) { spec["owner"] = "group:default/a b" },
		"a leading slash":                  func(spec, src map[string]any) { src["path"] = "/agents/x.yaml" },
		"a double slash":                   func(spec, src map[string]any) { src["path"] = "agents//x.yaml" },
		"a dot segment":                    func(spec, src map[string]any) { src["path"] = "./agents/x.yaml" },
		"a dot dot segment":                func(spec, src map[string]any) { src["path"] = "agents/../x.yaml" },
		"a trailing slash":                 func(spec, src map[string]any) { src["path"] = "agents/" },
		"a long path":                      func(spec, src map[string]any) { src["path"] = strings.Repeat("a/", 101) + "x" },
	} {
		r := appRig(t, "app-gate-api-cicd")
		x := xrOf("gate-api-one", "app-gate-api-cicd")
		spec := x["spec"].(map[string]any)
		mut(spec, spec["source"].(map[string]any))
		r.lister.items = []map[string]any{x}
		ch := r.poll()
		if len(ch.Rejected) != 1 || !strings.HasPrefix(ch.Rejected[0].Reason, "DF-XR") || len(r.files.calls) != 0 {
			t.Errorf("%s: %+v", name, ch)
			continue
		}
		rj := ch.Rejected[0]
		if *rj.Source.Repo != "invalid" || *rj.Source.Path != "invalid" || *rj.Source.Commit != "invalid" {
			t.Errorf("%s: its strings were kept: %v %v %v", name, *rj.Source.Repo, *rj.Source.Path, *rj.Source.Commit)
		}
		if body, _ := json.Marshal(r.pub.rej); strings.Contains(string(body), long) || strings.Contains(string(body), "ac me") || strings.Contains(string(body), "a b") {
			t.Errorf("%s: echoed", name)
		}
	}
	// a clean path and a plain repository are fine
	r := appRig(t, "app-gate-api-cicd")
	appPin(r, "gate-api-ok", "app-gate-api-cicd", "acme/gate-api")
	if ch := r.poll(); len(ch.Added) != 1 {
		t.Fatalf("%+v", ch)
	}
}
