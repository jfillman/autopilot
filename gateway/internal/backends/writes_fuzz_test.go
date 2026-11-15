package backends

import (
	"context"
	"net/url"
	"path"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Fuzz targets for each builder that turns an agent's text into a URL, a ref or a path, each with an oracle that does not use
// the builder's own rule: it reads the finished request (or the accepted name) the way a server, git or GitHub would, and
// fails if anything an agent wrote has left its place.
//
//	go test -run xxx -fuzz FuzzGlidepathURL -fuzztime 60s ./internal/backends

func FuzzGlidepathURL(f *testing.F) {
	for _, v := range glidepathAttacks {
		f.Add(v, "gate-api", "0b9c3a54-1d2e-4f6a-8b7c-9d0e1f2a3b4c")
	}
	f.Add("gate-api", "gate-api", "x")
	gp, rt := recordingGlidepath(f)
	f.Fuzz(func(t *testing.T, v, w, x string) {
		for name, call := range glidepathCalls(gp, v) {
			rt.urls = nil
			_, _ = call()
			for _, raw := range rt.urls {
				u, err := url.Parse(raw)
				if err != nil {
					t.Fatalf("%s(%q): %v", name, v, err)
				}
				if u.Scheme != "https" || u.Host != "glidepath.example" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
					t.Fatalf("%s(%q): %s leaves the base", name, v, raw)
				}
				// read the path the way a server does: split the escaped path, decode each segment on its own
				segs := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
				if len(segs) < 3 || segs[0] != "gp" || segs[1] != "v1" {
					t.Fatalf("%s(%q): %s is not under the base path", name, v, raw)
				}
				for _, s := range segs {
					d, err := url.PathUnescape(s)
					if err != nil || d == "" || d == "." || d == ".." || strings.ContainsAny(d, "/\\\x00?#;") || strings.IndexFunc(d, unicode.IsControl) >= 0 || strings.ContainsRune(d, ' ') {
						t.Fatalf("%s(%q): segment %q of %s is not a plain name", name, v, s, raw)
					}
				}
				// the same read in any other way agrees: cleaning the path changes nothing
				if c := path.Clean(u.Path); c != u.Path {
					t.Fatalf("%s(%q): %s is not a clean path", name, v, raw)
				}
				for k := range u.Query() {
					if k != "app" && k != "type" && k != "since" && k != "limit" && k != "image" && k != "cluster" && k != "sinceHours" {
						t.Fatalf("%s(%q): a query key %q an agent chose", name, v, k)
					}
				}
			}
		}
	})
}

func FuzzGitHubRequestTarget(f *testing.F) {
	for _, s := range []string{"main", "agent/x", "../x", "a/../b", "a%2fb", "a b", "a?b", "a#b", "refs/heads/x", "é", "a\x00", "x@{1}", "-x", "a//b", ".x"} {
		f.Add("o", "gate-api", s, s, "a/b.txt", strings.Repeat("a", 40))
	}
	f.Fuzz(func(t *testing.T, owner, name, branch, ref, p, sha string) {
		for _, ep := range []endpoint{epGetRef, epGetCommit, epPostTree, epPostCommit, epPostRef, epPostPull, epListPulls, epGetContents, epGetRepo, epListPullFiles} {
			r := ghReq{ep: ep, owner: owner, name: name, number: int64(len(branch)) - 3, branch: branch, ref: ref, path: p, sha: sha, state: "all", perPage: 5,
				body: refBody{Ref: "refs/heads/" + branch, Sha: sha}}
			method, target, err := r.target()
			if err != nil {
				continue
			}
			if method != "GET" && method != "POST" {
				t.Fatalf("method %s", method)
			}
			u, err := url.Parse("https://api.github.com" + target)
			if err != nil || u.Host != "api.github.com" || u.Scheme != "https" || u.User != nil || u.Fragment != "" {
				t.Fatalf("%q leaves the host: %v", target, err)
			}
			segs := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
			if len(segs) < 3 || segs[0] != "repos" {
				t.Fatalf("%q is not under /repos", target)
			}
			dec := make([]string, len(segs))
			for i, s := range segs {
				d, err := url.PathUnescape(s)
				if err != nil {
					t.Fatalf("%q: %v", target, err)
				}
				dec[i] = d
			}
			if dec[1] != owner || dec[2] != name || !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(owner+name) || owner == ".." || owner == "." || name == ".." || name == "." {
				t.Fatalf("%q: the owner and name are not the ones given, as single plain segments (%q %q)", target, owner, name)
			}
			// every segment after the repository is a name of the template or one segment of an argument: none is a dot
			// segment, empty, or holds a slash, a backslash or a control character once decoded. (A file name may hold a space:
			// it is a segment of a path, escaped.)
			for i, d := range dec {
				plain := ep != epGetContents || i < 4
				if i >= 3 && plain && (d == "" || d == "." || d == "..") {
					t.Fatalf("%q: segment %q", target, d)
				}
				if strings.ContainsAny(d, "/\\\x00") || strings.IndexFunc(d, unicode.IsControl) >= 0 || (plain && strings.ContainsAny(d, " ?#")) {
					t.Fatalf("%q: segment %q", target, d)
				}
			}
			if ep == epGetContents {
				trimmed := strings.TrimRight(p, "/")
				if got := strings.Join(dec[4:], "/"); got != trimmed {
					t.Fatalf("%q: the path read back is %q, not %q", target, got, trimmed)
				}
				for _, d := range dec[4:] {
					if (d == "" && trimmed != "") || d == "." || d == ".." {
						t.Fatalf("%q: a path segment %q", target, d)
					}
				}
				if q := u.Query(); len(q) != 1 || q.Get("ref") != ref || !gitRefOK(ref) {
					t.Fatalf("%q: ref %q", target, ref)
				}
			}
			if ep == epListPulls {
				q := u.Query()
				if q.Get("head") != owner+":"+branch || len(q) != 3 || !gitRefOK(branch) {
					t.Fatalf("%q: head %q", target, q.Get("head"))
				}
			}
			if ep == epGetRef && (!gitRefOK(branch) || strings.Join(dec[6:], "/") != branch) {
				t.Fatalf("%q: branch %q", target, branch)
			}
			if ep == epPostRef && (!gitRefOK(branch) || strings.HasPrefix(strings.ToLower(branch), "refs/") || reservedByOracle(branch)) {
				t.Fatalf("a ref body for %q was accepted", branch)
			}
			if ep == epGetCommit && (len(sha) != 40 && len(sha) != 64) {
				t.Fatalf("%q: sha %q", target, sha)
			}
		}
	})
}

func reservedByOracle(b string) bool {
	switch strings.ToLower(b) {
	case "main", "master", "head", "develop", "development", "trunk", "production", "prod", "staging", "release", "gh-pages":
		return true
	}
	return false
}

func FuzzBranchNames(f *testing.F) {
	for _, s := range append(branchesToAsk(), "agent/x", "main", "refs/heads/main", "release", "a.lock", "a/.b", "@{", "-x") {
		f.Add(s, "main")
	}
	f.Fuzz(func(t *testing.T, s, base string) {
		if ValidRef(s) {
			if !gitRefOK(s) || strings.HasPrefix(s, "-") || !utf8.ValidString(s) || len(s) > 200 {
				t.Fatalf("ValidRef accepts %q", s)
			}
		}
		if ValidNewBranch(s, base) {
			if !gitRefOK(s) || strings.HasPrefix(s, "-") || !strings.HasPrefix(s, "agent/") || len(s) == 6 || reservedByOracle(s) || strings.EqualFold(s, base) {
				t.Fatalf("ValidNewBranch accepts %q (base %q)", s, base)
			}
		}
	})
}

// protectedByOracle is the baseline deny list (.tekton, CODEOWNERS, .github, AppProject files, release files) read straight
// from its words, in any case, at the depth each applies to.
func protectedByOracle(p string) bool {
	comps := strings.Split(strings.ToLower(p), "/")
	// a protected file at any level: the path is it, or is under it (a tree written there replaces the file)
	for _, last := range comps {
		if last == "codeowners" || last == "release.yaml" || strings.HasSuffix(last, ".release.yaml") || (strings.HasPrefix(last, "appproject") && strings.HasSuffix(last, ".yaml")) {
			return true
		}
	}
	if comps[0] == ".github" {
		return true
	}
	for _, c := range comps {
		if c == ".tekton" {
			return true
		}
	}
	return false
}

func FuzzFilePaths(f *testing.F) {
	for _, s := range []string{"a/b.txt", ".github/x", ".GITHUB/x", "a/.tekton/x", "CODEOWNERS", "a/Codeowners", "x/release.yaml", "x.release.yaml", "a/AppProject-x.yaml",
		"a/../.github/x", "./a", "a//b", "/a", "a/", ".git/x", "a/.GIT/x", "a\\b", "a\x00b", "a\nb", "..", "", "a/./b", "é/ü.txt"} {
		f.Add(s)
	}
	g, _ := NewGitHub(GitHubOptions{BrokerURL: "http://b", TokenFile: "x", Owners: []string{"o"}, AllowHTTP: true, AllowHTTPAPI: true})
	deny := []string{".tekton/**", "**/.tekton/**", "CODEOWNERS", "**/CODEOWNERS", ".github/**", "**/appproject*.yaml", "**/AppProject*.yaml", "release.yaml",
		"**/release.yaml", "*.release.yaml", "**/*.release.yaml"}
	f.Fuzz(func(t *testing.T, p string) {
		in := OpenPRInput{Repo: "o/r", Branch: "agent/x", Title: "t", Base: "main", Files: []File{{Path: p, Content: "x"}}, DenyPaths: deny}
		_, _, entries, err := g.checkInput(&in)
		if err != nil {
			return
		}
		if len(entries) != 1 || entries[0].Path != p {
			t.Fatalf("the path written is not the path checked: %q", p)
		}
		comps := strings.Split(p, "/")
		if p == "" || path.Clean(p) != p || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") || strings.IndexFunc(p, unicode.IsControl) >= 0 || !utf8.ValidString(p) {
			t.Fatalf("accepted %q", p)
		}
		for _, c := range comps {
			if c == "" || c == "." || c == ".." || strings.EqualFold(c, ".git") {
				t.Fatalf("accepted %q", p)
			}
		}
		for _, seg := range comps {
			if seg != strings.TrimSpace(seg) || strings.HasSuffix(seg, ".") || strings.Contains(seg, ":") || regexp.MustCompile(`~[0-9]`).MatchString(seg) {
				t.Fatalf("accepted a name a file system reads as another: %q", p)
			}
		}
		if protectedByOracle(p) {
			t.Fatalf("a protected path was accepted: %q", p)
		}
	})
}

// mentionByOracle finds an @name that GitHub would read as a mention: an @ followed by a letter or digit, at the start or after
// a character that is not a letter, digit, underscore or backtick.
func mentionByOracle(s string) bool {
	rs := []rune(s)
	for i, r := range rs {
		if r != '@' || i+1 >= len(rs) {
			continue
		}
		n := rs[i+1]
		if !(n >= 'a' && n <= 'z' || n >= 'A' && n <= 'Z' || n >= '0' && n <= '9') {
			continue
		}
		if i == 0 {
			return true
		}
		p := rs[i-1]
		if !(p >= 'a' && p <= 'z' || p >= 'A' && p <= 'Z' || p >= '0' && p <= '9' || p == '_') {
			return true
		}
	}
	return false
}

func FuzzDefang(f *testing.F) {
	for _, s := range []string{"@alice", "a @b", "@@x", "x@y", "`@x`", "line\n@x", "@​x", "@", "", "@1", "(@x)", "\"@x\"", "-@x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := defang(s)
		if mentionByOracle(out) || strings.Contains(out, "<!--") || strings.Contains(out, "![") || strings.Contains(out, "<") ||
			regexp.MustCompile(`#[0-9]`).MatchString(out) || regexp.MustCompile(`(?i)\b(close[sd]?|fix(e[sd])?|resolve[sd]?)\b[\s:]*(https?://|#)`).MatchString(out) {
			t.Fatalf("defang(%q) = %q still notifies, links, closes or hides", s, out)
		}
		if len([]rune(out)) > 60000 {
			t.Fatalf("%d runes", len([]rune(out)))
		}
		if !strings.ContainsAny(s, "@#<!") && len([]rune(s)) < 1000 && out != s {
			t.Fatalf("plain text changed: %q", s)
		}
	})
}

func FuzzSplitRepo(f *testing.F) {
	for _, s := range []string{"o/r", "O/r", "o/", "/r", "o/r/x", "o/..", "o/.", "o/r\n", "evil/r", "o/" + strings.Repeat("r", 101), "o\\r", "o/r r", "ö/r"} {
		f.Add(s)
	}
	g, _ := NewGitHub(GitHubOptions{BrokerURL: "http://b", TokenFile: "x", Owners: []string{"o", "Acme"}, AllowHTTP: true, AllowHTTPAPI: true})
	f.Fuzz(func(t *testing.T, s string) {
		owner, name, err := g.SplitRepo(s)
		if err != nil {
			return
		}
		parts := strings.Split(s, "/")
		if len(parts) != 2 || !strings.EqualFold(parts[0], owner) || !strings.EqualFold(parts[1], name) || name != strings.ToLower(name) || strings.HasSuffix(name, ".git") || (owner != "o" && owner != "acme") ||
			name == "." || name == ".." || !regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`).MatchString(name) {
			t.Fatalf("SplitRepo(%q) = %q %q", s, owner, name)
		}
	})
}

func FuzzGlidepathBodies(f *testing.F) {
	for _, s := range []string{"c", "c\n", "../x", "a b", "", "ok-1"} {
		f.Add(s, s, s)
	}
	f.Fuzz(func(t *testing.T, cluster, env, id string) {
		if b, err := PromoteBody(map[string]any{"cluster": cluster, "sourceEnv": env, "sourceCluster": cluster}); err == nil {
			for _, v := range []string{b["cluster"].(string), b["sourceEnv"].(string)} {
				if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`).MatchString(v) {
					t.Fatalf("a promote body holds %q", v)
				}
			}
		}
		if b, err := RollbackBody(map[string]any{"cluster": cluster, "targetReleaseId": id, "reason": env}); err == nil {
			if v := b["targetReleaseId"].(string); strings.IndexFunc(v, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
				t.Fatalf("a rollback body holds %q", v)
			}
		}
		_ = context.Background()
	})
}

func FuzzCanonicalRepo(f *testing.F) {
	for _, s := range []string{"o/r", "O/R", "O/GitOps-X", "o/r.git", "o/r.GIT", "o/.", "o/..", "./r", "o/r\n", "o/r ", "ö/r", "o/r/x", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, ok := CanonicalRepo(s)
		if !ok {
			return
		}
		o, n, found := strings.Cut(c, "/")
		if !found || c != strings.ToLower(c) || !regexp.MustCompile(`^[a-z0-9._-]{1,100}$`).MatchString(o) || !regexp.MustCompile(`^[a-z0-9._-]{1,100}$`).MatchString(n) ||
			o == "." || o == ".." || n == "." || n == ".." || strings.HasSuffix(n, ".git") || strings.ToLower(s) != c {
			t.Fatalf("CanonicalRepo(%q) = %q", s, c)
		}
		if again, ok := CanonicalRepo(c); !ok || again != c {
			t.Fatalf("not a fixed point: %q", c)
		}
	})
}

func FuzzTrailerOwnership(f *testing.F) {
	good := "t\n\nHangar-Agent: a\nHangar-Session: s-1"
	for _, m := range []string{good, good + "\n", "Hangar-Session: s-1\n\nHangar-Agent: a", good + "\nHangar-Merge: person", "t\nHangar-Agent: a\nHangar-Session: s-1", "", "\n\n", good + "\n\nHangar-Session: s-1", "a\nb\n\nHangar-Agent: a\nHangar-Session: s-1"} {
		f.Add(m)
	}
	meta := map[string]string{"agent": "a", "session": "s-1"}
	f.Fuzz(func(t *testing.T, msg string) {
		if !trailerBlockOwned(msg, meta) {
			return
		}
		// read it independently: the message is one subject line, a blank line and the trailers, whole
		trimmed := strings.TrimRight(msg, "\n")
		const block = "\n\nHangar-Agent: a\nHangar-Session: s-1"
		if !strings.HasSuffix(trimmed, block) || strings.Contains(strings.TrimSuffix(trimmed, block), "\n") {
			t.Fatalf("accepted %q", msg)
		}
	})
}
