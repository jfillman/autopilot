package definitions_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// Strict reading (value.go): what is refused for how it is written, and the bounds that keep a hostile file's cost bounded. The
// corpus holds the Python comparison; these are the limits and the shapes that need a number or a clock.

func parseErr(t *testing.T, src string) error {
	t.Helper()
	_, err := definitions.ParseYAML([]byte(src))
	return err
}

func TestTheBoundsOfOneFile(t *testing.T) {
	var se *definitions.StrictError
	// size
	big := "a: " + strings.Repeat("x", definitions.MaxFileBytes) + "\n"
	if err := parseErr(t, big); !errors.As(err, &se) || !strings.Contains(err.Error(), "larger than 64 KiB") {
		t.Fatalf("a file over the limit: %v", err)
	}
	if err := parseErr(t, "a: "+strings.Repeat("x", definitions.MaxFileBytes-10)+"\n"); err != nil {
		t.Fatalf("a file under it: %v", err)
	}
	// nodes
	var b strings.Builder
	for i := 0; i < definitions.MaxNodes; i++ {
		fmt.Fprintf(&b, "k%d: 1\n", i)
	}
	if err := parseErr(t, b.String()); !errors.As(err, &se) || !strings.Contains(err.Error(), "YAML nodes") {
		t.Fatalf("too many nodes: %v", err)
	}
	b.Reset()
	for i := 0; i < definitions.MaxNodes/2-10; i++ {
		fmt.Fprintf(&b, "k%d: 1\n", i)
	}
	if err := parseErr(t, b.String()); err != nil {
		t.Fatalf("a file of a few thousand nodes: %v", err)
	}
	// depth, in flow and block style
	if err := parseErr(t, "a: "+strings.Repeat("[", definitions.MaxDepth+2)+strings.Repeat("]", definitions.MaxDepth+2)+"\n"); !errors.As(err, &se) {
		t.Fatalf("flow nesting: %v", err)
	}
	deep := ""
	for i := 0; i <= definitions.MaxDepth+1; i++ {
		deep += strings.Repeat("  ", i) + "a:\n"
	}
	if err := parseErr(t, deep+strings.Repeat("  ", definitions.MaxDepth+2)+"b: 1\n"); !errors.As(err, &se) {
		t.Fatalf("block nesting: %v", err)
	}
}

// An alias is refused where it is read, so a file of a few lines can never expand into a billion values.
func TestAnAliasBombCostsNothing(t *testing.T) {
	src := "a: &a [x,x,x,x,x,x,x,x,x]\n"
	for i := 0; i < 30; i++ {
		src += fmt.Sprintf("k%d: &k%d [*%s,*%s,*%s,*%s,*%s,*%s,*%s,*%s,*%s]\n", i, i, prev(i), prev(i), prev(i), prev(i), prev(i), prev(i), prev(i), prev(i), prev(i))
	}
	start := time.Now()
	err := parseErr(t, src)
	var se *definitions.StrictError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "anchors and aliases") || time.Since(start) > time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}

func prev(i int) string {
	if i == 0 {
		return "a"
	}
	return fmt.Sprintf("k%d", i-1)
}

func TestReadingIsTheSameForJSONAndYAMLAndRefusesWhatIsAmbiguous(t *testing.T) {
	good := map[string]any{"a": int64(1), "b": "x", "c": []any{true, nil, int64(-3)}}
	y, err := definitions.ParseYAML([]byte("a: 1\nb: x\nc: [true, ~, -3]\n"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := definitions.DecodeJSON([]byte(`{"a":1,"b":"x","c":[true,null,-3]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(definitions.Plain(y), good) || !reflect.DeepEqual(definitions.Plain(j), good) {
		t.Fatalf("%v %v", definitions.Plain(y), definitions.Plain(j))
	}
	for _, src := range []string{`{"a":1,"a":2}`, `{"a":{"b":1,"b":2}}`, `{"a":9007199254740993}`, `{"a":1} {"b":2}`, `{"a":`} {
		if _, err := definitions.DecodeJSON([]byte(src)); err == nil {
			t.Errorf("%s was accepted", src)
		}
	}
	if _, err := definitions.DecodeJSON([]byte(strings.Repeat("[", definitions.MaxDepth+2) + strings.Repeat("]", definitions.MaxDepth+2))); err == nil {
		t.Error("deep JSON was accepted")
	}
	// 20.0 is a float, not an integer, from JSON as from YAML
	v, _ := definitions.DecodeJSON([]byte(`{"a":20.0}`))
	if _, ok := definitions.Plain(v).(map[string]any)["a"].(float64); !ok {
		t.Errorf("20.0 read as %T", definitions.Plain(v).(map[string]any)["a"])
	}
}

func TestTheFixturesDeviationsAreEachNamedAndEveryOneHasAReason(t *testing.T) {
	f := loadFile(t)
	n := 0
	for _, c := range f.Cases {
		if c.Go == nil {
			continue
		}
		n++
		if len(c.Why) < 40 {
			t.Errorf("%s: a `why` that does not say why: %q", c.Name, c.Why)
		}
	}
	if n < 30 {
		t.Fatalf("only %d deliberate differences: the strictness cases are missing", n)
	}
}

func FuzzParseYAML(f *testing.F) {
	for _, s := range []string{"a: 1\n", "a: [1, 2]\n", "a: &x 1\nb: *x\n", "---\na: 1\n---\nb: 2\n", "a: yes\n", "? [a]\n: b\n", "a: !!str 1\n", "<<: {a: 1}\n", "a: 0755\n", "a: 1:30\n", "{a: {b: [c, d]}}"} {
		f.Add([]byte(s))
	}
	if raw, err := os.ReadFile("testdata/definitions.json"); err == nil {
		var file defsFile
		if json.Unmarshal(raw, &file) == nil {
			for i, c := range file.Cases {
				if i%7 == 0 && len(c.Text) < 4000 {
					f.Add([]byte(c.Text))
				}
			}
		}
	}
	reg := tools.Default()
	f.Fuzz(func(t *testing.T, src []byte) {
		doc, err := definitions.ParseYAML(src)
		if err != nil {
			return
		}
		// What is accepted is plain data that survives JSON unchanged: strings, whole numbers, booleans, null, lists, objects.
		plain := definitions.Plain(doc)
		raw, err := json.Marshal(plain)
		if err != nil {
			t.Fatalf("an accepted document does not encode: %v", err)
		}
		again, err := definitions.DecodeJSON(raw)
		if err != nil {
			t.Fatalf("an accepted document does not read back as JSON: %v: %s", err, raw)
		}
		if !reflect.DeepEqual(normNums(definitions.Plain(again)), normNums(plain)) {
			t.Fatalf("an accepted document changes through JSON:\n%v\n%v", plain, definitions.Plain(again))
		}
		// And the definition decision never panics, and a definition it accepts is one the schema accepts.
		if d, err := definitions.ParseDoc(doc, reg); err == nil {
			if d.Name == "" || definitions.ValidateSchema(doc) != nil || d.Doc == nil {
				t.Fatalf("a definition without a name or that the schema refuses: %+v", d)
			}
			if d.IdentityType == "workload" && (d.ServiceAccount == nil || *d.ServiceAccount == "") {
				t.Fatalf("a workload definition without a service account")
			}
		}
	})
}

// normNums reads a whole float64 as the integer JSON writes it as (20.0 and 20 are one JSON number; the reader keeps them apart).
func normNums(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = normNums(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normNums(e)
		}
		return out
	case float64:
		if x == float64(int64(x)) {
			return int64(x)
		}
	}
	return v
}

func FuzzValidateSchema(f *testing.F) {
	raw, _ := os.ReadFile("testdata/agents.json")
	var agents []struct{ Doc json.RawMessage }
	_ = json.Unmarshal(raw, &agents)
	for _, a := range agents {
		f.Add([]byte(a.Doc))
	}
	reg := tools.Default()
	f.Fuzz(func(t *testing.T, src []byte) {
		v, err := definitions.DecodeJSON(src)
		if err != nil {
			return
		}
		doc, ok := v.(*definitions.OMap)
		if !ok {
			return
		}
		e1 := definitions.ValidateSchema(doc)
		e2 := definitions.ValidateSchema(doc)
		if (e1 == nil) != (e2 == nil) || (e1 != nil && e1.Error() != e2.Error()) {
			t.Fatalf("validation is not deterministic: %v vs %v", e1, e2)
		}
		if _, err := definitions.ParseDoc(doc, reg); (err == nil) != (e1 == nil) && err != nil {
			var de *definitions.DefinitionError
			if !errors.As(err, &de) {
				t.Fatalf("a document the schema accepts failed to build: %v", err)
			}
		}
	})
}
