package auth

import "testing"

// The same cases run through Python's fnmatch.fnmatchcase in tests/test_gateway_go_auth.py.
var fnmatchCases = []struct {
	name, pat string
	want      bool
}{
	{"ada@example.com", "*@example.com", true},
	{"ada@example.com", "*@example.org", false},
	{"ada@example.com", "ada@example.com", true},
	{"ADA@example.com", "ada@example.com", false},
	{"ns/sa", "ns/*", true},
	{"ns/a/b", "ns/*", true},
	{"ns/sa", "n?/s?", true},
	{"ns/sa", "n?/s", false},
	{"", "*", true},
	{"", "", true},
	{"a", "", false},
	{"abc", "a*c", true},
	{"abbbc", "a*b*c", true},
	{"ac", "a*c", true},
	{"acd", "a*c", false},
	{"a1", "a[0-9]", true},
	{"ab", "a[0-9]", false},
	{"ab", "a[!0-9]", true},
	{"a1", "a[!0-9]", false},
	{"a]", "a[]]", true},
	{"a-", "a[a-]", true},
	{"a[", "a[", true},
	{"a[b", "a[b", true},
	{"ab", "a[", false},
	{"a\nb", "a*b", true},
	{"repo:acme/shop:ref:refs/heads/main", "repo:acme/shop:*", true},
	{"repo:acme/shop2:ref:x", "repo:acme/shop:*", false},
	{"user:x", "[!u]ser:x", false},
	{"é", "?", true},
	{"ab", "[b-a]b", false},
	{"app-backstage-prod/backstage", "app-*/backstage", true},
}

func TestFnmatchCase(t *testing.T) {
	for _, c := range fnmatchCases {
		if got := fnmatchCase(c.name, c.pat); got != c.want {
			t.Errorf("fnmatchCase(%q, %q) = %v, want %v", c.name, c.pat, got, c.want)
		}
	}
}
