package airframe

import (
	"reflect"
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

func cfgDoc(t *testing.T, v any) any {
	t.Helper()
	d, err := FromArgs(v)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The branch is the one Python's configure_branch names for the same change (golden values from airframe_apply.configure_branch).
func TestConfigureBranchEqualsPython(t *testing.T) {
	for _, tc := range []struct {
		app, env string
		values   any
		replace  bool
		want     string
	}{
		{"parachute", "dev", map[string]any{"slos": []any{map[string]any{"name": "é☃", "objective": 99.95, "n": 1, "x": nil, "l": []any{1, "a", true}}}, "serviceMonitor": map[string]any{"enabled": true}}, true,
			"agent/airframe-parachute-configure-dev-c7b93301b8"},
		{"app-x", "test", map[string]any{"configMaps": []any{map[string]any{"name": "c", "data": map[string]any{"k": "v\n\"q\"\\"}}}}, false,
			"agent/airframe-app-x-configure-test-af5424c5f0"},
	} {
		if got := ConfigureBranch(tc.app, tc.env, cfgDoc(t, tc.values), tc.replace); got != tc.want {
			t.Errorf("%s/%s: %s, want %s", tc.app, tc.env, got, tc.want)
		}
	}
}

func TestMergeValuesRules(t *testing.T) {
	merge := func(have, want any, replace bool) (any, []string, []string) {
		var c, r []string
		out := MergeValues(cfgDoc(t, have), cfgDoc(t, want), "", &c, &r, replace)
		return definitions.Plain(out), c, r
	}
	slo := func(name string, obj int64) map[string]any { return map[string]any{"name": name, "objective": obj} }
	// a named entry the file lacks is appended, the file's own kept in order
	out, c, _ := merge(map[string]any{"slos": []any{slo("a", 99)}}, map[string]any{"slos": []any{slo("b", 99)}}, false)
	if !reflect.DeepEqual(out, map[string]any{"slos": []any{slo("a", 99), slo("b", 99)}}) || len(c) != 0 {
		t.Fatalf("%v %v", out, c)
	}
	// an identical entry is kept, one with the same name and other content is a conflict naming it
	_, c, _ = merge(map[string]any{"slos": []any{slo("a", 99)}}, map[string]any{"slos": []any{slo("a", 99)}}, false)
	if len(c) != 0 {
		t.Fatalf("identical: %v", c)
	}
	out, c, _ = merge(map[string]any{"slos": []any{slo("a", 99)}}, map[string]any{"slos": []any{slo("a", 98)}}, false)
	if !reflect.DeepEqual(c, []string{"slos[a]"}) || !reflect.DeepEqual(out, map[string]any{"slos": []any{slo("a", 99)}}) {
		t.Fatalf("conflict: %v %v", out, c)
	}
	// replace makes the patch win, in place
	out, c, r := merge(map[string]any{"slos": []any{slo("a", 99), slo("b", 99)}}, map[string]any{"slos": []any{slo("a", 98)}}, true)
	if len(c) != 0 || !reflect.DeepEqual(r, []string{"slos[a]"}) || !reflect.DeepEqual(out, map[string]any{"slos": []any{slo("a", 98), slo("b", 99)}}) {
		t.Fatalf("replace: %v %v %v", out, c, r)
	}
	// a list that is not of uniquely named mappings is compared whole; a scalar likewise
	_, c, _ = merge(map[string]any{"a": []any{1, 2}, "l": []any{map[string]any{"name": "x"}, map[string]any{"name": "x"}}, "s": 1},
		map[string]any{"a": []any{1, 3}, "l": []any{map[string]any{"name": "y"}}, "s": 2}, false)
	if !reflect.DeepEqual(c, []string{"a", "l", "s"}) {
		t.Fatalf("whole: %v", c)
	}
	// 1 == 1.0 as in Python
	_, c, _ = merge(map[string]any{"s": 1}, map[string]any{"s": 1.5}, false)
	if len(c) != 1 {
		t.Fatalf("%v", c)
	}
}

func TestChartErrorsPatchVersusWholeFile(t *testing.T) {
	file := cfgDoc(t, map[string]any{"envName": "dev", "release": map[string]any{"image": "x"}, "slos": []any{}}).(*definitions.OMap)
	if e := ChartErrors(file, true); len(e) != 0 {
		t.Fatalf("a whole file's other keys are not checked: %v", e)
	}
	if e := ChartErrors(file, false); len(e) == 0 {
		t.Fatal("a patch is checked as it is: envName and release are not chart values")
	}
	if !CriticalKey("extraManifests") || CriticalKey("slos") {
		t.Fatal("extraManifests is the critical-risk key")
	}
}
