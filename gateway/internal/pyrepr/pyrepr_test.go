package pyrepr_test

import (
	"testing"

	"github.com/jfillman/autopilot/gateway/internal/pyrepr"
)

type om struct {
	keys []string
	m    map[string]any
}

func (o om) OrderedKeys() []string           { return o.keys }
func (o om) OrderedGet(k string) (any, bool) { v, ok := o.m[k]; return v, ok }

// The messages the schema validator builds are compared with Python's, so the repr they quote must be Python's.
func TestReprIsPythons(t *testing.T) {
	for _, c := range []struct {
		in   any
		want string
	}{
		{nil, "None"}, {true, "True"}, {false, "False"}, {int64(-3), "-3"}, {20.0, "20.0"}, {1.5, "1.5"}, {1e16, "1e+16"}, {0.00001, "1e-05"},
		{"a", "'a'"}, {"it's", `"it's"`}, {`say "hi" it's`, `'say "hi" it\'s'`}, {"a\nb\t\\", `'a\nb\t\\'`}, {"\x01", `'\x01'`}, {"é中", "'é中'"},
		{[]any{"a", int64(1), nil}, "['a', 1, None]"}, {[]string{"T0", "T1"}, "['T0', 'T1']"},
		{om{[]string{"b", "a"}, map[string]any{"a": int64(1), "b": []any{}}}, "{'b': [], 'a': 1}"}, {om{}, "{}"},
	} {
		if got := pyrepr.Value(c.in); got != c.want {
			t.Errorf("repr(%#v) = %s, want %s", c.in, got, c.want)
		}
	}
	if pyrepr.Text("x") != "x" || pyrepr.Text(int64(5)) != "5" || pyrepr.Text(nil) != "None" {
		t.Error("str()")
	}
	for f, want := range map[float64]string{100: "100.0", 1e15: "1000000000000000.0", 123456.789: "123456.789", 1.0 / 3: "0.3333333333333333", -0.5: "-0.5"} {
		if got := pyrepr.Float(f); got != want {
			t.Errorf("repr(%v) = %s, want %s", f, got, want)
		}
	}
}
