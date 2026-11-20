package backends

import "testing"

// CodeRabbit on autopilot#121: `**` must cross a newline (RE2's `.` does not without (?s)), and `[` is a literal in globBody,
// so the ancestor test must not read it as a wildcard.
func TestADoubleStarDenyMatchesAPathWithANewline(t *testing.T) {
	for _, c := range []struct {
		path string
		deny []string
		want bool
	}{
		{"docs/a\nb.md", []string{"docs/**"}, true},
		{"a\nb/release.yaml", []string{"**/release.yaml"}, true},
		{"docs2/a\nb.md", []string{"docs/**"}, false},
	} {
		if got := pathProtected(c.path, c.deny); got != c.want {
			t.Errorf("pathProtected(%q, %v) = %v, want %v", c.path, c.deny, got, c.want)
		}
	}
}

func TestABracketInADenyGlobIsALiteralInTheAncestorTestToo(t *testing.T) {
	deny := []string{"deploy/[prod]/values.yaml"}
	for p, want := range map[string]bool{"deploy/[prod]/values.yaml": true, "deploy/[prod]": true, "deploy": true, "deploy/p": false} {
		if got := pathProtected(p, deny); got != want {
			t.Errorf("pathProtected(%q) = %v, want %v", p, got, want)
		}
	}
}
