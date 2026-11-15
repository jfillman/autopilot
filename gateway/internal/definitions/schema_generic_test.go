package definitions

import (
	"sync"
	"testing"
)

// The generic validator (A-9 S5c) reads the keywords the AppSpec schema uses; the AppSpec corpus (internal/airframe,
// TestPlanToolsEqualPython) holds its messages equal to Python's. These are the keywords on their own.

func mustSchemaOf(t *testing.T, doc string) *Schema {
	t.Helper()
	s, err := NewSchema([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func inst(t *testing.T, doc string) any {
	t.Helper()
	v, err := DecodeJSON([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNewSchemaRefusesWhatItCannotCheck(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown keyword":     `{"type":"object","patternProperties":{"^a":{"type":"string"}}}`,
		"remote ref":          `{"$ref":"https://example.com/x.json"}`,
		"unknown in defs":     `{"$defs":{"a":{"dependentRequired":{}}},"type":"object"}`,
		"unknown under allOf": `{"allOf":[{"contains":{}}]}`,
		"unknown under not":   `{"not":{"uniqueKeys":true}}`,
		"not json":            `{`,
		"not an object":       `[]`,
		"bad pattern":         `{"pattern":"(?<=a)b"}`,
	} {
		if _, err := NewSchema([]byte(doc)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestGenericKeywords(t *testing.T) {
	s := mustSchemaOf(t, `{"$defs":{"n":{"type":"integer","exclusiveMinimum":0}},"type":"object","properties":{
		"a":{"$ref":"#/$defs/n"},
		"b":{"anyOf":[{"type":"string"},{"type":"integer"}]},
		"c":{"allOf":[{"type":"array","minItems":2},{"maxItems":3}]},
		"d":{"not":{"required":["x","y"]}},
		"e":{"type":"object","maxProperties":1},
		"f":{"type":"string","format":"date-time","x-anything":1,"default":"x"}},"x-kubernetes-validations":[{"rule":"x"}]}`)
	for _, c := range []struct{ doc, want string }{
		{`{}`, ""},
		{`{"a":0}`, "0 is less than or equal to the minimum of 0"},
		{`{"a":"x"}`, "'x' is not of type 'integer'"},
		{`{"b":1.5}`, "1.5 is not valid under any of the given schemas"},
		{`{"b":"s"}`, ""},
		{`{"c":[1]}`, "[1] is too short"},
		{`{"c":[1,2,3,4]}`, "[1, 2, 3, 4] is too long"},
		{`{"d":{"x":1,"y":2}}`, "{'x': 1, 'y': 2} should not be valid under {'required': ['x', 'y']}"},
		{`{"d":{"x":1}}`, ""},
		{`{"e":{"p":1,"q":2}}`, "{'p': 1, 'q': 2} has too many properties"},
		{`{"f":"not a date"}`, ""},
	} {
		got := s.Best(inst(t, c.doc))
		switch {
		case c.want == "" && got != nil:
			t.Errorf("%s: %q", c.doc, got.Message)
		case c.want != "" && (got == nil || got.Message != c.want):
			t.Errorf("%s: got %v, want %q", c.doc, got, c.want)
		}
	}
	if e := s.Best(inst(t, `{"c":[1]}`)); len(e.Path) != 1 || e.Path[0] != "c" {
		t.Errorf("path: %v", e.Path)
	}
}

func TestOneOfValidUnderMoreThanOne(t *testing.T) {
	s := mustSchemaOf(t, `{"oneOf":[{"required":["a"]},{"required":["b"]},{"required":["c"]}]}`)
	got := s.Best(inst(t, `{"a":1,"b":2,"c":3}`))
	want := "{'a': 1, 'b': 2, 'c': 3} is valid under each of {'required': ['b']}, {'required': ['c']}, {'required': ['a']}"
	if got == nil || got.Message != want {
		t.Fatalf("got %v", got)
	}
	if got := s.Best(inst(t, `{"b":2}`)); got != nil {
		t.Fatal(got.Message)
	}
}

func TestErrorsAtAndSort(t *testing.T) {
	s := mustSchemaOf(t, `{"$defs":{"v":{"type":"object","properties":{"z":{"type":"integer"},"a":{"type":"integer"},"l":{"type":"array","items":{"type":"integer"}}}}}}`)
	errs := s.ErrorsAt("#/$defs/v", inst(t, `{"z":"x","a":"y","l":[1,"b","c"]}`))
	if len(errs) != 4 {
		t.Fatalf("%d errors", len(errs))
	}
	SortByPath(errs)
	if errs[0].Path[0] != "a" || errs[1].Path[0] != "l" || errs[1].Path[1] != int64(1) || errs[3].Path[0] != "z" {
		t.Fatalf("%v", errs)
	}
}

// compile keeps a cache of patterns: a call from two goroutines at once must not be a race (run with -race).
func TestSchemasAreSafeToUseTogether(t *testing.T) {
	s := mustSchemaOf(t, `{"type":"object","properties":{"a":{"type":"string","pattern":"^[a-z]+$"},"b":{"pattern":"^[0-9]+$"}}}`)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.Best(inst(t, `{"a":"x1","b":"2"}`))
			}
		}()
	}
	wg.Wait()
}
