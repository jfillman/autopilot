package airframe

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

func readBack(t testing.TB, v any) any {
	t.Helper()
	text := DumpYAML(v)
	got, err := definitions.ParseYAML([]byte(text))
	if err != nil {
		t.Fatalf("the strict reader refuses what was written: %v\n%s", err, text)
	}
	return got
}

func TestDumpYAMLRoundTrips(t *testing.T) {
	odd := []string{"yes", "No", "on", "off", "y", "n", "true", "null", "~", "", " ", " lead", "trail ", "1", "1.5", "1e3", "0x1f", "0o7", "010", "1_000", "1:30",
		"2001-01-01", "2001-01-01T10:00:00Z", "-", "- a", "? a", ": a", "a: b", "a #b", "#a", "&a", "*a", "!a", "|", ">", "'", "\"", "%a", "@a", "`a", "{a}", "[a]",
		"a\nb", "a\n", "\na", "a\tb", "café", "☃", "\U0001f600", "a b", "x" + strings.Repeat("long ", 80), "=", "<<", ".inf", "-.inf", ".nan", "0", "-1", "+1"}
	var items []any
	for _, s := range odd {
		items = append(items, s)
	}
	o := &definitions.OMap{M: map[string]any{}}
	for i, s := range odd {
		o.Set(s+strings.Repeat("k", i%3), s)
	}
	doc := &definitions.OMap{M: map[string]any{}}
	doc.Set("zeta", int64(1))
	doc.Set("alpha", []any{int64(-5), true, false, nil, &definitions.OMap{M: map[string]any{}}, []any{},
		[]any{[]any{int64(1)}, &definitions.OMap{M: map[string]any{"a": nil}, Keys: []string{"a"}}}})
	doc.Set("strings", items)
	doc.Set("map", o)
	doc.Set("big", int64(1<<53))
	doc.Set("nested", &definitions.OMap{M: map[string]any{"b": &definitions.OMap{M: map[string]any{"c": []any{"x"}}, Keys: []string{"c"}}}, Keys: []string{"b"}})
	got := readBack(t, doc)
	if !reflect.DeepEqual(got, any(doc)) {
		t.Fatalf("read back differently:\n%s", DumpYAML(doc))
	}
	if keys := got.(*definitions.OMap).Keys; keys[0] != "zeta" || keys[1] != "alpha" {
		t.Fatalf("the order of the keys is not kept: %v", keys)
	}
}

func TestDumpYAMLKeepsLongLinesAndBlocks(t *testing.T) {
	long := strings.Repeat("word ", 100)
	out := DumpYAML(plMk("a", long, "b", "l1\nl2\n"))
	if strings.Count(out, "\n") > 5 {
		t.Fatalf("a long scalar was folded:\n%s", out)
	}
}

// The strict reader takes whole numbers only (a definition has no float), so floats are read back with the ordinary reader.
func TestDumpYAMLFloats(t *testing.T) {
	for _, f := range []float64{0, -0.5, 1e300, 5e-324, math.MaxFloat64, 123456789.125, 1e20, 1e-7, 99.9} {
		var got map[string]any
		text := DumpYAML(plMk("F", f))
		if err := yaml.Unmarshal([]byte(text), &got); err != nil {
			t.Fatal(err)
		}
		if g, ok := got["F"].(float64); !ok || g != f {
			t.Fatalf("%v written as %q read back as %#v", f, text, got["F"])
		}
	}
}

func FuzzDumpYAML(f *testing.F) {
	f.Add(`{"a":["yes",1,2.5,null,{"b":"x\ny"}],"c":" ","d":"é","e":1e20}`)
	f.Add(`{"k":["~", "0x10", "1:30", "- x", {"k": [[], {}]}]}`)
	f.Add(`{"s":"just a string: with # stuff"}`)
	f.Fuzz(func(t *testing.T, doc string) {
		v, err := decodeNum([]byte(doc))
		if err != nil {
			return
		}
		a, err := FromArgs(v)
		if err != nil {
			return
		}
		if _, isMap := a.(*definitions.OMap); !isMap {
			return // the strict reader reads mappings only
		}
		if hasFloat(a) || hasControl(a) {
			return // the strict reader takes whole numbers only, and no control character
		}
		text := DumpYAML(a)
		got, err := definitions.ParseYAML([]byte(text))
		if err != nil {
			// The strict reader bounds a file's size and depth; anything else is a document that was written wrongly.
			for _, bound := range []string{"larger than", "deep", "nodes", "nest"} {
				if strings.Contains(err.Error(), bound) {
					return
				}
			}
			t.Fatalf("refused: %v\n%s", err, text)
		}
		if !reflect.DeepEqual(got, a) {
			t.Fatalf("read back differently:\n%s", text)
		}
	})
}

func hasFloat(v any) bool {
	switch x := v.(type) {
	case float64:
		return true
	case []any:
		for _, e := range x {
			if hasFloat(e) {
				return true
			}
		}
	case *definitions.OMap:
		for _, k := range x.Keys {
			if hasFloat(x.M[k]) {
				return true
			}
		}
	}
	return false
}

func hasControl(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.IndexFunc(x, func(r rune) bool { return unicode.IsControl(r) && r != '\n' }) >= 0 || strings.ContainsRune(x, '\r')
	case []any:
		for _, e := range x {
			if hasControl(e) {
				return true
			}
		}
	case *definitions.OMap:
		for _, k := range x.Keys {
			if hasControl(k) || hasControl(x.M[k]) {
				return true
			}
		}
	}
	return false
}
