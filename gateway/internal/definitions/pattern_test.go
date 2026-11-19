package definitions

import (
	"encoding/json"
	"os"
	"testing"
)

// translatePattern keeps Python's Unicode `\s` meaning in Go's ASCII-only RE2.
func TestTranslatePattern(t *testing.T) {
	w := "[" + pyWhite + "]"
	n := "[^" + pyWhite + "]"
	for _, c := range []struct{ in, want string }{
		{`a\sb`, "a" + w + "b"},
		{`\s*`, w + "*"},
		{`\S+`, n + "+"},
		{`\\s`, `\\s`},                // an escaped backslash, then an s: not a whitespace escape
		{`\\\s`, `\\` + w},            // escaped backslash, then a real \s
		{`[\s]`, "[" + pyWhite + "]"}, // already in a class: the bare set
		{`[^\s@]`, "[^" + pyWhite + "@]"},
		{`[a\s]x\s`, "[a" + pyWhite + "]x" + w},
		{`[]\s]`, `[\]` + pyWhite + `]`}, // a leading ] is a literal, the class goes on
		{`\d\.\b`, `\d\.\b`},             // other escapes pass through
		{`^[a-z]+$`, `^[a-z]+$`},
	} {
		if got := translatePattern(c.in); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTranslatePatternRefusesAnUnmappableClassEscape(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal(`\S inside a character class must panic`)
		}
	}()
	translatePattern(`[\S]`)
}

func TestTranslatedWhitespaceIsUnicode(t *testing.T) {
	re := compile(`^\s+$`)
	for _, s := range []string{" ", "\t", " ", " ", "\u0085", "　", "\x1c"} {
		if !re.MatchString(s) {
			t.Errorf("%q should be whitespace", s)
		}
	}
	if compile(`^\S+$`).MatchString("a b") {
		t.Error(`\S must not match a no-break space`)
	}
	if compile(`^\\s$`).MatchString(" ") || !compile(`^\\s$`).MatchString(`\s`) {
		t.Error(`\\s is a backslash then an s`)
	}
}

// testdata/pattern_parity.json holds, for each \s pattern of the vendored AppSpec schema, Python's re.search answer for a set of
// strings; testdata/gen_pattern_parity.py wrote it by running Python. Go must answer the same.
func TestPatternParity(t *testing.T) {
	b, err := os.ReadFile("testdata/pattern_parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var table []struct {
		Pattern string          `json:"pattern"`
		Results map[string]bool `json:"results"`
	}
	if err := json.Unmarshal(b, &table); err != nil {
		t.Fatal(err)
	}
	if len(table) == 0 {
		t.Fatal("no patterns in the table")
	}
	for _, e := range table {
		re := compile(e.Pattern)
		matched := 0
		for s, want := range e.Results {
			if got := re.MatchString(s); got != want {
				t.Errorf("pattern %.40q on %q: Go %v, Python %v", e.Pattern, s, got, want)
			}
			if want {
				matched++
			}
		}
		if matched == 0 || matched == len(e.Results) {
			t.Errorf("pattern %.40q: %d of %d samples match, so the table proves nothing", e.Pattern, matched, len(e.Results))
		}
	}
}
