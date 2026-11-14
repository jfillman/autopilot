// Package glob is Python's fnmatch.fnmatchcase: the rules the allow-lists and admin lists of the Python gateway are written
// for (`*` matches `/`). Shared by the authenticators and the person controls.
package glob

// Match reports whether name matches the glob pattern with Python's fnmatch.fnmatchcase rules: `*` matches any
// run of characters (including `/` and none), `?` one character, `[seq]` one character of seq, `[!seq]` one not in
// seq, ranges `a-z` inside a set; a `[` with no closing `]` is a literal. Case-sensitive. The allow-lists
// (AUTOPILOT_OIDC_ALLOWED_USERS, TOWER_SERVICE_ACCOUNTS, GitHub `sub` globs) are written for these rules, so they are
// reproduced here rather than approximated with path.Match (whose `*` stops at `/`).
func Match(name, pattern string) bool {
	toks := globTokens(pattern)
	n := []rune(name)
	ti, ni := 0, 0
	starTi, starNi := -1, 0
	for ni < len(n) {
		if ti < len(toks) && toks[ti].kind == tokStar {
			starTi, starNi = ti, ni
			ti++
			continue
		}
		if ti < len(toks) && toks[ti].matches(n[ni]) {
			ti++
			ni++
			continue
		}
		if starTi >= 0 {
			starNi++
			ni = starNi
			ti = starTi + 1
			continue
		}
		return false
	}
	for ti < len(toks) && toks[ti].kind == tokStar {
		ti++
	}
	return ti == len(toks)
}

// MatchAny reports whether name matches any of the globs (none matches nothing: an empty list allows nobody).
func MatchAny(name string, globs []string) bool {
	for _, g := range globs {
		if Match(name, g) {
			return true
		}
	}
	return false
}

const (
	tokLit = iota
	tokAny
	tokStar
	tokSet
)

type globRange struct{ lo, hi rune }

type globTok struct {
	kind   int
	r      rune
	neg    bool
	ranges []globRange
}

func (t globTok) matches(c rune) bool {
	switch t.kind {
	case tokLit:
		return t.r == c
	case tokAny:
		return true
	case tokSet:
		in := false
		for _, g := range t.ranges {
			if c >= g.lo && c <= g.hi {
				in = true
				break
			}
		}
		return in != t.neg
	}
	return false
}

func globTokens(p string) []globTok {
	pr := []rune(p)
	var out []globTok
	for i := 0; i < len(pr); {
		c := pr[i]
		i++
		switch c {
		case '*':
			if len(out) == 0 || out[len(out)-1].kind != tokStar {
				out = append(out, globTok{kind: tokStar})
			}
		case '?':
			out = append(out, globTok{kind: tokAny})
		case '[':
			j := i
			if j < len(pr) && pr[j] == '!' {
				j++
			}
			if j < len(pr) && pr[j] == ']' {
				j++
			}
			for j < len(pr) && pr[j] != ']' {
				j++
			}
			if j >= len(pr) {
				out = append(out, globTok{kind: tokLit, r: '['})
				continue
			}
			set := pr[i:j]
			i = j + 1
			t := globTok{kind: tokSet}
			if len(set) > 0 && set[0] == '!' {
				t.neg = true
				set = set[1:]
			}
			for k := 0; k < len(set); k++ {
				if k+2 < len(set) && set[k+1] == '-' {
					t.ranges = append(t.ranges, globRange{set[k], set[k+2]}) // lo > hi matches nothing, as in Python
					k += 2
				} else {
					t.ranges = append(t.ranges, globRange{set[k], set[k]})
				}
			}
			out = append(out, t)
		default:
			out = append(out, globTok{kind: tokLit, r: c})
		}
	}
	return out
}
