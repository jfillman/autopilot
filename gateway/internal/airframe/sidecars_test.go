package airframe

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

func xrOf(t *testing.T, s string) map[string]any {
	t.Helper()
	var v any
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return stNorm(v).(map[string]any)
}

func TestRenderAndDigEqualPythonOnTheCasesThatMatter(t *testing.T) {
	xr := xrOf(t, `{"metadata":{"name":"db","namespace":"app-x-dev"},"spec":{"n":5,"f":1.5,"b":true,"s":"v","o":{"a":1},"l":[{"b":1},{"b":2},{"c":3}]},"status":{"u":"https://u"}}`)
	for tpl, want := range map[string]string{
		"{name}-rw": "db-rw", "{namespace}": "app-x-dev", "{spec.n}": "5", "{spec.f}": "1.5", "{spec.b}": "True", "{spec.s}.{status.u}": "v.https://u",
		"plain": "plain", "{": "{", "{}": "{}", "{ name }": "{ name }",
	} {
		got, err := Render(tpl, xr)
		if err != nil || got != want {
			t.Errorf("%q: %v %v, want %q", tpl, got, err, want)
		}
	}
	for _, tpl := range []string{"{spec.o}", "{spec.l}", "{spec.missing}", "{status.missing}", "{other}", "{spec.s.x}", "a{name}{spec.o}"} {
		if _, err := Render(tpl, xr); err == nil {
			t.Errorf("%q rendered", tpl)
		} else if _, ok := err.(*Unresolved); !ok {
			t.Errorf("%q: %v", tpl, err)
		}
	}
	if v, _ := Render(5, xr); v != 5 {
		t.Error("a template that is not a string passes through")
	}
	if got := Dig(xr, "spec.l[].b"); !stPyEq(got, []any{int64(1), int64(2)}) {
		t.Errorf("%v", got)
	}
	if got := Dig(xr, "spec.nope[].b"); got != nil { // no element held the key, so it was never a list walk
		t.Errorf("%v", got)
	}
	if got := Dig(xr, "spec.s.x"); got != nil {
		t.Errorf("%v", got)
	}
	// a value of an XR that is too long is not published
	long := xrOf(t, `{"spec":{"s":"`+strings.Repeat("a", 5000)+`"}}`)
	if _, err := Render("{spec.s}", long); err == nil {
		t.Error("a 5000-character value was rendered")
	}
}

func TestPythonEqualityOfNumbersAndBooleans(t *testing.T) {
	for _, c := range []struct {
		a, b any
		eq   bool
	}{{int64(1), 1.0, true}, {true, int64(1), true}, {false, int64(0), true}, {"1", int64(1), false}, {nil, int64(0), false},
		{[]any{int64(1)}, []any{1.0}, true}, {map[string]any{"a": true}, map[string]any{"a": int64(1)}, true}, {"a", "a", true}} {
		if stPyEq(c.a, c.b) != c.eq {
			t.Errorf("%v == %v", c.a, c.b)
		}
	}
}

type mapReader map[string]any

func (m mapReader) Get(ctx context.Context, path string) (any, bool, error) {
	v, ok := m[path]
	return v, ok, nil
}
func (m mapReader) List(ctx context.Context, path, sel string) ([]map[string]any, error) {
	return nil, nil
}

func TestAResourceStepReadsOnlyInsideTheAppsOwnNamespaces(t *testing.T) {
	run := func(tpl string) *definitions.OMap {
		v, err := definitions.DecodeJSON([]byte(`{"kind":"resource","resource":"slos.catalog.hangar.io","name":"{name}-slo","namespace":"` + tpl + `","condition":{"type":"Ready","status":"True"},"field":"status.ok","equals":true}`))
		if err != nil {
			t.Fatal(err)
		}
		return v.(*definitions.OMap)
	}
	xr := xrOf(t, `{"metadata":{"name":"x","namespace":"app-shop-cicd"},"spec":{"ns":"app-other-dev"}}`)
	got := xrOf(t, `{"status":{"ok":true,"conditions":[{"type":"Ready","status":"True"}]}}`)
	s := &State{Kube: mapReader{"/apis/catalog.hangar.io/v1alpha1/namespaces/app-shop-cicd/slos/x-slo": got}}
	r, err := s.resourceStep(context.Background(), map[string]bool{"app-shop-cicd": true}, run("{namespace}"), xr)
	if err != nil || r["status"] != "pass" || r["detail"] != "slos.catalog.hangar.io x-slo exists, Ready=True, status.ok=True" {
		t.Fatalf("%v %v", r, err)
	}
	// the namespace a tenant's XR spec names, another app's: skipped, no request
	for _, ns := range []string{"{spec.ns}", "kube-system", "app-shopx-dev", "infra-other-dev", "app-shop-api-dev", "app-shop-cicd-x"} {
		reads := 0
		cs := &State{Kube: countReader{&reads}}
		r, err = cs.resourceStep(context.Background(), map[string]bool{"app-shop-cicd": true}, run(ns), xr)
		if err != nil || r["status"] != "skipped" || reads != 0 {
			t.Errorf("%s: %v %v after %d reads", ns, r, err, reads)
		}
	}
	// a template the XR does not publish is a failed step, as in Python
	r, _ = s.resourceStep(context.Background(), map[string]bool{"app-shop-cicd": true}, run("{spec.nope}"), xr)
	if r["status"] != "fail" || r["detail"] != "cannot resolve {spec.nope}: the XR has not published it" {
		t.Fatalf("%v", r)
	}
}

type countReader struct{ n *int }

func (c countReader) Get(ctx context.Context, path string) (any, bool, error) {
	*c.n++
	return nil, false, nil
}
func (c countReader) List(ctx context.Context, path, sel string) ([]map[string]any, error) {
	*c.n++
	return nil, nil
}

func FuzzRenderDigWhen(f *testing.F) {
	f.Add("{name}-{spec.a.b}", `{"metadata":{"name":"n"},"spec":{"a":{"b":1}}}`, "spec.a[].b")
	f.Add("{status.x}{", `{"status":{"x":[1]}}`, "a.b.c")
	f.Add("{{name}}", `{}`, "[]")
	f.Fuzz(func(t *testing.T, tpl, doc, path string) {
		var v any
		if json.Unmarshal([]byte(doc), &v) != nil {
			return
		}
		xr, _ := stNorm(v).(map[string]any)
		out, err := Render(tpl, xr)
		if err == nil {
			s, _ := out.(string)
			if !strings.Contains(tpl, "{") && s != tpl {
				t.Fatalf("%q rendered as %q", tpl, s)
			}
			if len(s) > len(tpl)+maxRendered*strings.Count(tpl, "{") {
				t.Fatalf("an output of %d bytes from %d", len(s), len(tpl))
			}
		} else if _, ok := err.(*Unresolved); !ok {
			t.Fatalf("%v", err)
		}
		_ = Dig(xr, path)
		w := definitions.FromPlain(map[string]any{"field": path, "equals": "x"})
		_ = WhenMatches(w, xr)
		_ = WhenMatches(definitions.FromPlain(map[string]any{"field": path, "in": []any{"x", int64(1)}}), xr)
	})
}
