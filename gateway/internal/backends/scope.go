package backends

import (
	"path"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// Path and repository scope, as the Clearance core decides it (engine/scope.py, ported in the core's internal/scope). The
// core has already decided every call before a handler runs; the write handlers here decide the same rules again from the
// session's own agent definition, so a mistake or a gap in one is not a gap in the other.

// normalizePath is scope.normalize: a repository-relative path, normalised; ok is false for an empty path, a NUL or a
// backslash, an absolute path and one that leaves the repository.
func normalizePath(p string) (string, bool) {
	if p == "" || strings.ContainsAny(p, "\x00\\") || strings.HasPrefix(p, "/") {
		return "", false
	}
	n := path.Clean(p)
	if n == ".." || strings.HasPrefix(n, "../") || n == "." {
		return "", false
	}
	return n, true
}

// globBody is scope._glob_body: `**` crosses directories, `*` and `?` do not.
func globBody(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); {
		switch {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i += 2
		case g[i] == '*':
			b.WriteString("[^/]*")
			i++
		case g[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			_, size := utf8.DecodeRuneInString(g[i:])
			b.WriteString(regexp.QuoteMeta(g[i : i+size]))
			i += size
		}
	}
	return b.String()
}

var globCache sync.Map // glob -> *regexp.Regexp

// globRegexp compiles a path glob. strict ends at the end of the text; the core's deny match, like Python's `$`, also
// matches just before a final newline (a deny that matches more is the safer way to be wrong). A glob that does not
// compile matches nothing, which for a deny list is why a path is also checked in lower case below.
func globRegexp(g string, strict bool) *regexp.Regexp {
	key := g
	if strict {
		key = "\x00" + g
	}
	if v, ok := globCache.Load(key); ok {
		return v.(*regexp.Regexp)
	}
	end := `\n?\z`
	if strict {
		end = `\z`
	}
	re, err := regexp.Compile(`\A` + globBody(g) + end)
	if err != nil {
		re = regexp.MustCompile(`\A\z.`) // matches nothing
	}
	globCache.Store(key, re)
	return re
}

// repoAllowed is scope.repo_allowed: owner/name against globs like `owner/*`. An empty list allows nothing.
func repoAllowed(repo string, allow []string) bool {
	if strings.Count(repo, "/") != 1 || strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
		return false
	}
	for _, a := range allow {
		if globRegexp(a, true).MatchString(repo) {
			return true
		}
	}
	return false
}

// pathDenied reports whether a normalised path matches a deny glob. Go also tries the path and the glob in lower case: a
// file system that ignores case (a clone on a Mac, or CODEOWNERS written as Codeowners) must not slip past `CODEOWNERS`.
func pathDenied(norm string, deny []string) bool {
	low := strings.ToLower(norm)
	for _, g := range deny {
		if globRegexp(g, false).MatchString(norm) || globRegexp(strings.ToLower(g), false).MatchString(low) {
			return true
		}
	}
	return false
}
