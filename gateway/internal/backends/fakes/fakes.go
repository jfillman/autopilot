// Package fakes are in-memory stand-ins for the upstreams of the Glidepath and repository tools, served over HTTP, for tests:
// a GitHub (with Glidepath's token broker), a glidepath-api that keeps its Idempotency-Keys and a Kubernetes API with one
// Argo CD Application. Each can do what a real one does when it is unlucky: do the write and then drop the connection
// without answering, so a test can hold what the gateway does about a lost answer.
package fakes

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Seen is one request a fake received.
type Seen struct {
	Method, Target string
	Header         http.Header
	Body           []byte
}

func drop(w http.ResponseWriter) {
	if h, ok := w.(http.Hijacker); ok {
		if c, _, err := h.Hijack(); err == nil {
			_ = c.Close()
		}
	}
}

func reply(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func sha(parts ...string) string {
	h := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

// -- GitHub -----------------------------------------------------------------------------------------------------------

// PullRequest is one pull request of the fake.
type PullRequest struct {
	Number      int
	Head, Base  string
	Title, Body string
	State       string
	MergedAt    any
	CommitSHA   string
}

// GitHub is a GitHub with the Git data API's few calls and Glidepath's token broker. Repositories are "owner/name".
type GitHub struct {
	Srv *httptest.Server

	mu        sync.Mutex
	Token     string
	Onboarded map[string]bool // repo -> the broker mints for it
	branches  map[string]map[string]string
	commits   map[string]map[string]map[string]any // repo -> sha -> {tree, message, parents}
	trees     map[string]map[string][]any
	files     map[string]map[string]map[string]string // repo -> sha(commit) -> path -> content
	PRs       map[string][]*PullRequest
	Requests  []Seen
	// DropAfter makes the fake do a request and then drop the connection without answering: it is asked "METHOD path" and the
	// number of times it has seen that request (1 for the first).
	DropAfter func(key string, n int) bool
	counts    map[string]int
	defaults  map[string]string
	special   map[string]string
	Minted    int
}

var (
	reRef      = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/git/ref/heads/(.+)$`)
	reCommit   = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/git/commits/([0-9a-f]+)$`)
	reCommits  = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/git/commits$`)
	reTrees    = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/git/trees$`)
	reRefs     = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/git/refs$`)
	rePulls    = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/pulls$`)
	reContents = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/contents/(.*)$`)
	reRepo     = regexp.MustCompile(`^/repos/([^/]+/[^/]+)$`)
	rePullFile = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/pulls/([0-9]+)/files$`)
)

// NewGitHub is a fake with one repository's worth of state per onboarded name; each has a main branch.
func NewGitHub(onboarded ...string) *GitHub {
	g := &GitHub{Token: "ghs_faketoken", Onboarded: map[string]bool{}, branches: map[string]map[string]string{}, commits: map[string]map[string]map[string]any{},
		trees: map[string]map[string][]any{}, files: map[string]map[string]map[string]string{}, PRs: map[string][]*PullRequest{}, counts: map[string]int{}}
	for _, r := range onboarded {
		g.Onboarded[r] = true
		base := sha(r, "base")
		g.branches[r] = map[string]string{"main": base}
		g.commits[r] = map[string]map[string]any{base: {"tree": sha(r, "tree0")}}
		g.trees[r] = map[string][]any{sha(r, "tree0"): nil}
		g.files[r] = map[string]map[string]string{base: {}}
	}
	g.Srv = httptest.NewServer(http.HandlerFunc(g.serve))
	return g
}

// Close stops the fake.
func (g *GitHub) Close() { g.Srv.Close() }

// BrokerURL is where the token broker is.
func (g *GitHub) BrokerURL() string { return g.Srv.URL + "/broker" }

// Branch is the commit a branch points at, or "".
func (g *GitHub) Branch(repo, name string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.branches[repo][name]
}

// Branches lists a repository's branches.
func (g *GitHub) Branches(repo string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for b := range g.branches[repo] {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// SeedFiles puts files in the base commit of a repository's main branch (what the repository already holds).
func (g *GitHub) SeedFiles(repo string, files map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	base := g.branches[repo]["main"]
	if base == "" {
		base = g.branches[repo][g.defaults[repo]]
	}
	for k, v := range files {
		g.files[repo][base][k] = v
	}
}

// SetDefaultBranch renames the repository's main branch: the default branch the fake reports.
func (g *GitHub) SetDefaultBranch(repo, name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.branches[repo][name] = g.branches[repo]["main"]
	delete(g.branches[repo], "main")
	if g.defaults == nil {
		g.defaults = map[string]string{}
	}
	g.defaults[repo] = name
}

// SeedPR is a pull request that already exists for a branch.
func (g *GitHub) SeedPR(repo, branch string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.PRs[repo] = append(g.PRs[repo], &PullRequest{Number: len(g.PRs[repo]) + 1, Head: branch, Base: "main", State: "open", CommitSHA: g.branches[repo][branch]})
}

// SeedEntry makes the contents answer for a path an object of this type (symlink, submodule) rather than a file.
func (g *GitHub) SeedEntry(repo, path, typ string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.special == nil {
		g.special = map[string]string{}
	}
	g.special[repo+"/"+path] = typ
}

// SeedBranch makes a branch of the commit a call would have made (a crash left it there): the commit holds these files and
// carries this message.
func (g *GitHub) SeedBranch(repo, name string, files map[string]string, message string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := sha(repo, "seed", name)
	g.commits[repo][c] = map[string]any{"tree": sha(repo, "seedtree", name), "message": message}
	g.files[repo][c] = files
	g.branches[repo][name] = c
	return c
}

// PullRequests is what a repository holds.
func (g *GitHub) PullRequests(repo string) []*PullRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]*PullRequest(nil), g.PRs[repo]...)
}

// CommitFiles are the files a commit holds (path -> content).
func (g *GitHub) CommitFiles(repo, sha string) map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]string{}
	for p, c := range g.files[repo][sha] {
		out[p] = c
	}
	return out
}

// Calls are the requests seen, in order.
func (g *GitHub) Calls() []Seen {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Seen(nil), g.Requests...)
}

func (g *GitHub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Method + " " + r.URL.Path
	g.mu.Lock()
	g.Requests = append(g.Requests, Seen{r.Method, r.URL.RequestURI(), r.Header.Clone(), body})
	g.counts[key]++
	n := g.counts[key]
	dropIt := g.DropAfter != nil && g.DropAfter(key, n)
	g.mu.Unlock()
	rec := &recorder{header: http.Header{}}
	g.handle(rec, r, body)
	if dropIt {
		drop(w)
		return
	}
	for k, v := range rec.header {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.status)
	_, _ = w.Write(rec.body)
}

type recorder struct {
	header http.Header
	status int
	body   []byte
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(c int)           { r.status = c }
func (r *recorder) Write(b []byte) (int, error) { r.body = append(r.body, b...); return len(b), nil }

func (g *GitHub) handle(w http.ResponseWriter, r *http.Request, body []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := r.URL.Path
	if p == "/broker/agent-installation-token" && r.Method == "POST" {
		var req struct{ Owner, Repo, App string }
		_ = json.Unmarshal(body, &req)
		if !g.Onboarded[req.Owner+"/"+req.Repo] {
			reply(w, 403, map[string]string{"error": "not an onboarded app's repo"})
			return
		}
		g.Minted++
		reply(w, 200, map[string]any{"token": g.Token, "expiresAt": "z"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+g.Token {
		reply(w, 401, map[string]string{"message": "Bad credentials"})
		return
	}
	switch {
	case reRepo.MatchString(p) && r.Method == "GET":
		m := reRepo.FindStringSubmatch(p)
		if _, ok := g.branches[m[1]]; !ok {
			reply(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		def := "main"
		if d, ok := g.defaults[m[1]]; ok {
			def = d
		}
		reply(w, 200, map[string]any{"default_branch": def, "full_name": m[1]})
	case reRef.MatchString(p) && r.Method == "GET":
		m := reRef.FindStringSubmatch(p)
		if c, ok := g.branches[m[1]][m[2]]; ok {
			reply(w, 200, map[string]any{"ref": "refs/heads/" + m[2], "object": map[string]any{"sha": c, "type": "commit"}})
			return
		}
		reply(w, 404, map[string]string{"message": "Not Found"})
	case reCommit.MatchString(p) && r.Method == "GET":
		m := reCommit.FindStringSubmatch(p)
		if c, ok := g.commits[m[1]][m[2]]; ok {
			reply(w, 200, map[string]any{"sha": m[2], "tree": map[string]any{"sha": c["tree"]}, "message": c["message"]})
			return
		}
		reply(w, 404, map[string]string{"message": "Not Found"})
	case reTrees.MatchString(p) && r.Method == "POST":
		m := reTrees.FindStringSubmatch(p)
		var req struct {
			BaseTree string `json:"base_tree"`
			Tree     []any  `json:"tree"`
		}
		_ = json.Unmarshal(body, &req)
		id := sha(m[1], "tree", string(body))
		g.trees[m[1]][id] = req.Tree
		reply(w, 201, map[string]any{"sha": id})
	case reCommits.MatchString(p) && r.Method == "POST":
		m := reCommits.FindStringSubmatch(p)
		var req struct {
			Message string
			Tree    string
			Parents []string
		}
		_ = json.Unmarshal(body, &req)
		id := sha(m[1], "commit", string(body))
		g.commits[m[1]][id] = map[string]any{"tree": req.Tree, "message": req.Message, "parents": req.Parents}
		files := map[string]string{}
		for _, e := range g.trees[m[1]][req.Tree] {
			em := e.(map[string]any)
			files[em["path"].(string)] = em["content"].(string)
		}
		g.files[m[1]][id] = files
		reply(w, 201, map[string]any{"sha": id})
	case reRefs.MatchString(p) && r.Method == "POST":
		m := reRefs.FindStringSubmatch(p)
		var req struct{ Ref, Sha string }
		_ = json.Unmarshal(body, &req)
		name := strings.TrimPrefix(req.Ref, "refs/heads/")
		if _, exists := g.branches[m[1]][name]; exists {
			reply(w, 422, map[string]string{"message": "Reference already exists"})
			return
		}
		g.branches[m[1]][name] = req.Sha
		reply(w, 201, map[string]any{"ref": req.Ref, "object": map[string]string{"sha": req.Sha}})
	case rePulls.MatchString(p) && r.Method == "POST":
		m := reRepoPulls(p)
		var req struct{ Title, Head, Base, Body string }
		_ = json.Unmarshal(body, &req)
		pr := &PullRequest{Number: len(g.PRs[m]) + 1, Head: req.Head, Base: req.Base, Title: req.Title, Body: req.Body, State: "open", CommitSHA: g.branches[m][req.Head]}
		g.PRs[m] = append(g.PRs[m], pr)
		reply(w, 201, map[string]any{"number": pr.Number, "html_url": fmt.Sprintf("https://github.example/%s/pull/%d", m, pr.Number)})
	case rePulls.MatchString(p) && r.Method == "GET":
		m := reRepoPulls(p)
		head := strings.TrimPrefix(r.URL.Query().Get("head"), strings.SplitN(m, "/", 2)[0]+":")
		out := []any{}
		for _, pr := range g.PRs[m] {
			if pr.Head == head {
				out = append(out, map[string]any{"number": pr.Number, "html_url": fmt.Sprintf("https://github.example/%s/pull/%d", m, pr.Number),
					"state": pr.State, "merged_at": pr.MergedAt, "head": map[string]any{"ref": pr.Head, "sha": pr.CommitSHA}})
			}
		}
		reply(w, 200, out)
	case rePullFile.MatchString(p) && r.Method == "GET":
		// the files a pull request carries: the head commit's files that the default branch's does not have, or has otherwise
		m := rePullFile.FindStringSubmatch(p)
		out := []any{}
		for _, pr := range g.PRs[m[1]] {
			if fmt.Sprint(pr.Number) != m[2] {
				continue
			}
			base := g.files[m[1]][g.branches[m[1]]["main"]]
			names := []string{}
			for path, content := range g.files[m[1]][pr.CommitSHA] {
				if b, has := base[path]; !has || b != content {
					names = append(names, path)
				}
			}
			sort.Strings(names)
			for _, n := range names {
				out = append(out, map[string]any{"filename": n, "status": "added"})
			}
		}
		reply(w, 200, out)
	case reContents.MatchString(p) && r.Method == "GET":
		m := reContents.FindStringSubmatch(p)
		ref := r.URL.Query().Get("ref")
		c, isBranch := g.branches[m[1]][ref]
		if !isBranch {
			c = ref
		}
		var entries []any
		for p := range g.files[m[1]][c] {
			if strings.HasPrefix(p, strings.TrimRight(m[2], "/")+"/") {
				entries = append(entries, map[string]any{"name": p, "type": "file"})
			}
		}
		if typ, ok := g.special[m[1]+"/"+strings.TrimRight(m[2], "/")]; ok {
			reply(w, 200, map[string]any{"type": typ, "name": m[2]})
			return
		}
		if len(entries) > 0 {
			reply(w, 200, entries)
			return
		}
		if content, ok := g.files[m[1]][c][m[2]]; ok {
			reply(w, 200, map[string]any{"type": "file", "size": len(content), "sha": "f", "encoding": "base64", "content": b64(content)})
			return
		}
		reply(w, 404, map[string]string{"message": "Not Found"})
	default:
		reply(w, 404, map[string]string{"message": "Not Found: " + key(r)})
	}
}

func key(r *http.Request) string { return r.Method + " " + r.URL.Path }

func reRepoPulls(p string) string { return rePulls.FindStringSubmatch(p)[1] }

func b64(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := []byte(s)
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var v uint32
		n := 0
		for j := 0; j < 3; j++ {
			v <<= 8
			if i+j < len(b) {
				v |= uint32(b[i+j])
				n++
			}
		}
		for j := 0; j < 4; j++ {
			if j <= n {
				out.WriteByte(alphabet[(v>>(18-6*uint(j)))&63])
			} else {
				out.WriteByte('=')
			}
		}
	}
	return out.String()
}

// -- glidepath-api ----------------------------------------------------------------------------------------------------

// Glidepath is a glidepath-api that starts a run for a re-run, once for each Idempotency-Key.
type Glidepath struct {
	Srv       *httptest.Server
	mu        sync.Mutex
	Requests  []Seen
	Started   map[string]string // Idempotency-Key -> the run it started
	Cancelled []string
	DropAfter func(key string, n int) bool
	counts    map[string]int
}

// NewGlidepath is a fake glidepath-api.
func NewGlidepath() *Glidepath {
	g := &Glidepath{Started: map[string]string{}, counts: map[string]int{}}
	g.Srv = httptest.NewServer(http.HandlerFunc(g.serve))
	return g
}

// Close stops the fake.
func (g *Glidepath) Close() { g.Srv.Close() }

// StartedCount is how many runs were started.
func (g *Glidepath) StartedCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.Started)
}

// Calls are the requests seen, in order.
func (g *Glidepath) Calls() []Seen {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Seen(nil), g.Requests...)
}

func (g *Glidepath) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	k := r.Method + " " + r.URL.Path
	g.mu.Lock()
	g.Requests = append(g.Requests, Seen{r.Method, r.URL.RequestURI(), r.Header.Clone(), body})
	g.counts[k]++
	dropIt := g.DropAfter != nil && g.DropAfter(k, g.counts[k])
	var status int
	var out any
	switch {
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/rerun"):
		key := r.Header.Get("Idempotency-Key")
		run, seen := g.Started[key]
		if !seen {
			run = fmt.Sprintf("run-%d", len(g.Started)+1)
			g.Started[key] = run
		}
		status, out = 201, map[string]any{"name": run, "replayed": seen}
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/cancel"):
		g.Cancelled = append(g.Cancelled, r.URL.Path)
		status, out = 200, map[string]any{"status": "Cancelled"}
	default:
		status, out = 404, map[string]any{"rule": "GP-API-404", "error": "no such thing"}
	}
	g.mu.Unlock()
	if dropIt {
		drop(w)
		return
	}
	reply(w, status, out)
}

// -- Kubernetes -------------------------------------------------------------------------------------------------------

// Kube is a Kubernetes API with Argo CD Applications that start a sync when a merge patch sets their operation.
type Kube struct {
	Srv       *httptest.Server
	mu        sync.Mutex
	Apps      map[string]map[string]any // path -> object
	Requests  []Seen
	Patches   int
	DropAfter func(key string, n int) bool
	counts    map[string]int
}

// NewKube is a fake API server holding the Applications by path.
func NewKube(apps map[string]map[string]any) *Kube {
	k := &Kube{Apps: apps, counts: map[string]int{}}
	k.Srv = httptest.NewServer(http.HandlerFunc(k.serve))
	return k
}

// Close stops the fake.
func (k *Kube) Close() { k.Srv.Close() }

// PatchCount is how many patches were applied.
func (k *Kube) PatchCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.Patches
}

// Calls are the requests seen, in order.
func (k *Kube) Calls() []Seen {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]Seen(nil), k.Requests...)
}

func (k *Kube) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Method + " " + r.URL.Path
	k.mu.Lock()
	k.Requests = append(k.Requests, Seen{r.Method, r.URL.RequestURI(), r.Header.Clone(), body})
	k.counts[key]++
	dropIt := k.DropAfter != nil && k.DropAfter(key, k.counts[key])
	app, ok := k.Apps[r.URL.Path]
	var status int
	var out any
	switch {
	case !ok:
		status, out = 404, map[string]string{"message": "not found"}
	case r.Method == "GET":
		status, out = 200, app
	case r.Method == "PATCH":
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		rv, _ := app["metadata"].(map[string]any)["resourceVersion"].(string)
		if want, _ := p["metadata"].(map[string]any)["resourceVersion"].(string); want != rv {
			status, out = 409, map[string]string{"message": "conflict"}
			break
		}
		app["operation"] = p["operation"]
		app["metadata"].(map[string]any)["resourceVersion"] = rv + "1"
		k.Patches++
		status, out = 200, app
	default:
		status, out = 405, map[string]string{}
	}
	k.mu.Unlock()
	if dropIt {
		drop(w)
		return
	}
	reply(w, status, out)
}
