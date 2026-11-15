package airframe

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

var testReg = Registry{DevCluster: "kind-dev", UpperClusters: []string{"kind-prod"}, TenantsRepo: "gitops-cluster-dev-tenants", Owner: "jfillman"}

func jsonArgs(t testing.TB, doc string) any {
	t.Helper()
	v, err := decodeNum([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	a, err := FromArgs(v)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

const goodSpec = `{"apiVersion":"airframe/v1","kind":"AppSpec","app":"parachute","stack":"python","environments":{"ground":["dev"],"flight":[{"name":"prod"}]}}`

func TestFromArgs(t *testing.T) {
	v, err := FromArgs(map[string]any{"b": json.Number("20"), "a": []any{json.Number("1.5"), json.Number("2e3"), json.Number("20.0"), nil, true, "x"}, "c": 7, "d": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	o := v.(*definitions.OMap)
	if strings.Join(o.Keys, ",") != "a,b,c,d" {
		t.Fatalf("the keys of a plain map are sorted: %v", o.Keys)
	}
	if o.M["b"] != int64(20) || o.M["c"] != int64(7) || o.M["d"] != int64(3) {
		t.Fatalf("whole numbers are int64: %#v", o.M)
	}
	l := o.M["a"].([]any)
	if l[0] != 1.5 || l[1] != 2000.0 || l[2] != 20.0 {
		t.Fatalf("a number written with a point or an exponent is a float: %#v", l)
	}
	for _, bad := range []any{math.NaN(), math.Inf(1), json.Number("1e999"), json.Number("NaN"), struct{}{}, []string{"x"}} {
		if _, err := FromArgs(bad); err == nil {
			t.Errorf("%#v was accepted", bad)
		}
	}
	if v, err := FromArgs(json.Number("9007199254740993")); err != nil || v != 9007199254740992.0 {
		t.Errorf("an integer past 2^53 is a float: %v %v", v, err)
	}
}

func TestFromArgsBoundsADocument(t *testing.T) {
	deep := any("x")
	for i := 0; i < 100; i++ {
		deep = []any{deep}
	}
	if _, err := FromArgs(deep); err == nil {
		t.Error("100 levels were accepted")
	}
	wide := make([]any, 25000)
	for i := range wide {
		wide[i] = json.Number("1")
	}
	if _, err := FromArgs(wide); err == nil {
		t.Error("25000 values were accepted")
	}
	ok := make([]any, 1000)
	if _, err := FromArgs(ok); err != nil {
		t.Errorf("1000 values were refused: %v", err)
	}
}

func TestPlanNeverChangesOrKeepsTheCallersSpec(t *testing.T) {
	spec := jsonArgs(t, goodSpec).(*definitions.OMap)
	before := definitions.Plain(plCopyV(spec))
	cs, err := Plan(spec, testReg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(definitions.Plain(spec)); string(b) != mustJSON(before) {
		t.Fatalf("Plan changed the spec: %s", b)
	}
	// What the plan holds is its own: changing it does not change the spec, and changing the spec afterwards does not change it.
	x := cs.Steps[0].Files[0].Content.(*definitions.OMap)
	x.Set("spec", "tampered")
	spec.Set("app", "other")
	if got := cs.AsMap()["app"]; got != "parachute" {
		t.Fatalf("the plan follows the spec: %v", got)
	}
	again, _ := Plan(jsonArgs(t, goodSpec), testReg, nil)
	if _, ok := again.Steps[0].Files[0].Content.(*definitions.OMap).M["spec"].(*definitions.OMap); !ok {
		t.Fatal("a plan shared state with an earlier one")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestPlanRefusals(t *testing.T) {
	cases := map[string]struct {
		doc  string
		base any
		want string
		kind string
	}{
		"schema":      {`{"app":"x"}`, nil, "<root>: 'apiVersion' is a required property", "spec"},
		"overrides":   {strings.Replace(goodSpec, `"stack"`, `"overrides":{"qa":{}},"stack"`, 1), nil, "overrides/qa: no environment named 'qa' (dev, prod)", "spec"},
		"two uppers":  {goodSpec, nil, "", "needs"},
		"not mapping": {goodSpec, []any{"x"}, "baseCicd is not a mapping", "spec"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			reg := testReg
			if c.kind == "needs" {
				reg.UpperClusters = []string{"a", "b"}
			}
			_, err := Plan(jsonArgs(t, c.doc), reg, c.base)
			var se *SpecError
			var ni *NeedsInput
			switch c.kind {
			case "needs":
				if !errors.As(err, &ni) || len(ni.Options) != 2 {
					t.Fatalf("want needs input, got %v", err)
				}
			default:
				if !errors.As(err, &se) || se.Msg != c.want {
					t.Fatalf("got %v, want %q", err, c.want)
				}
			}
		})
	}
}

func TestChangesetAnswersForBaseCicdText(t *testing.T) {
	spec := jsonArgs(t, goodSpec)
	for _, bad := range []string{"a: [", "- a", "", "x: &a 1\ny: *a\n", "a: 1\na: 2\n"} {
		cs, ans := Changeset(spec, bad, testReg)
		if cs != nil || ans == nil {
			t.Fatalf("%q was accepted", bad)
		}
		if r := ans.(*definitions.OMap).M["error"].(*definitions.OMap).M["rule"]; r != "AF-SPEC-002" {
			t.Fatalf("%q: %v", bad, r)
		}
	}
	if cs, _ := Changeset(spec, "deploy:\n  environments:\n  - name: dev\n    tier: ground\n", testReg); cs == nil {
		t.Fatal("a good cicd.yaml was refused")
	}
	if cs, ans := Changeset(spec, 7, testReg); cs != nil || ans == nil {
		t.Fatal("a number as cicd.yaml was accepted")
	}
}

// A spec an agent chose is bounded and never panics the planner.
func TestAHostileSpecIsRefusedOrBounded(t *testing.T) {
	mega := strings.Repeat("a", 1<<20)
	cases := map[string]any{
		"deep":        nested(200),
		"huge string": map[string]any{"apiVersion": "airframe/v1", "kind": "AppSpec", "app": mega, "stack": "go", "environments": map[string]any{"ground": []any{"dev"}, "flight": []any{}}},
		"huge env":    map[string]any{"apiVersion": "airframe/v1", "kind": "AppSpec", "app": "a-b", "stack": "go", "environments": map[string]any{"ground": []any{"dev"}, "flight": []any{}}, "description": mega},
		"nan":         map[string]any{"app": math.NaN()},
		"array":       []any{},
		"null":        nil,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			a, err := FromArgs(doc)
			if err != nil {
				return // refused at the door
			}
			_, _ = Changeset(a, nil, testReg)
			_ = PlanAnswer(a, nil, testReg)
		})
	}
	wide := make([]any, 5000)
	for i := range wide {
		wide[i] = map[string]any{"name": "e"}
	}
	a, err := FromArgs(map[string]any{"apiVersion": "airframe/v1", "kind": "AppSpec", "app": "a-b", "stack": "go", "environments": map[string]any{"ground": []any{"dev"}, "flight": wide}})
	if err != nil {
		t.Fatal(err)
	}
	if cs, ans := Changeset(a, nil, testReg); cs != nil || ans == nil {
		t.Fatal("5000 flight environments of one name were planned")
	}
}

func nested(n int) any {
	var v any = "x"
	for i := 0; i < n; i++ {
		v = map[string]any{"k": v}
	}
	return v
}

func FuzzPlan(f *testing.F) {
	f.Add(goodSpec, "")
	f.Add(strings.Replace(goodSpec, `"stack":"python"`, `"stack":"go","stackOptions":{"port":9000},"config":{"rollout":{"replicas":2,"ports":[{"name":"http","containerPort":9000}]}}`, 1), "deploy:\n  environments:\n  - {name: dev, tier: ground}\n")
	f.Add(`{"app":1}`, "a: [")
	f.Fuzz(func(t *testing.T, doc, base string) {
		v, err := decodeNum([]byte(doc))
		if err != nil {
			return
		}
		a, err := FromArgs(v)
		if err != nil {
			return
		}
		var b any
		if base != "" {
			b = base
		}
		cs, ans := Changeset(a, b, testReg)
		if (cs == nil) == (ans == nil) {
			t.Fatalf("a plan or an answer, never both or neither")
		}
		if cs != nil {
			m := cs.AsMap()
			if _, err := json.Marshal(m); err != nil {
				t.Fatal(err)
			}
			if cs.Step("verify") == nil || cs.Steps[len(cs.Steps)-1].ID != "verify" {
				t.Fatal("a plan ends with verify")
			}
		}
	})
}

func FuzzFromArgs(f *testing.F) {
	f.Add(`{"a":[1,2.5,"x",null,true,{"b":1e999}]}`)
	f.Add(`[[[[[]]]]]`)
	f.Fuzz(func(t *testing.T, doc string) {
		v, err := decodeNum([]byte(doc))
		if err != nil {
			return
		}
		a, err := FromArgs(v)
		if err != nil {
			return
		}
		// What comes out is the ordered model only, and writes out as JSON again.
		if _, err := json.Marshal(definitions.Plain(a)); err != nil {
			t.Fatalf("%v", err)
		}
	})
}
