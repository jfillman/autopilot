package backends

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
)

// GitHub: the real GitBackend (github_pr.GitHubPRs). A pull request is opened as the agents' own GitHub App, which this
// gateway never holds: for each call it asks Glidepath's broker (POST {BROKER_URL}/agent-installation-token) for a token,
// as its own identity with a projected token for the broker's audience, re-read on every call. The broker checks that the
// repository is an onboarded app's own and down-scopes the token to that one repository, contents and pull_requests write,
// never workflows. The installation token lives in one call's local variable: it is used for that call's requests, then
// dropped; never cached, logged, written, put in an error or returned.
//
// The pull request is built with the Git data API, so nothing is cloned and nothing is written to disk. The whole of what
// this client can send to GitHub is the closed list in endpoint (below): nine requests, GET or POST, each built from
// validated parts. There is no merge, no review, no PATCH, no PUT and no DELETE in it, so by no combination of calls can
// the gateway merge, approve, force-push, delete or move a branch, change protection, or write to an existing branch.

const (
	// maxBaseReads bounds the reads of the base tree for one pull request; past it the call is refused.
	maxBaseReads = 200

	// GitHubAPI is the public GitHub API. A deployment on another host (GitHub Enterprise) is GitHubOptions.API.
	GitHubAPI = "https://api.github.com"

	maxFiles     = 50
	maxFileBytes = 1 << 20
	// maxTotalBytes bounds one pull request's contents (Python: 50 files of 1 MiB each, 50 MiB in one request).
	maxTotalBytes = 8 << 20
	maxTitle      = 200
	maxPRBody     = 60000
	reasonMax     = 300
	maxAPIOK      = 8 << 20
	maxAPIErr     = 64 << 10
)

var (
	repoSeg  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	refChars = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	shaRx    = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	tokenRx  = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)
	// reserved are branch names an agent may not create, whatever the repository calls its default: a branch a person
	// or a protection rule treats as the line of release. The agent's branch is a new one, never one of these.
	reserved = map[string]bool{"main": true, "master": true, "head": true, "develop": true, "development": true, "trunk": true,
		"production": true, "prod": true, "staging": true, "release": true, "gh-pages": true}
	// mention is an @name GitHub would read as a mention: at the start or after a character that is not part of a word.
	mention = regexp.MustCompile("(^|[^A-Za-z0-9_])@([A-Za-z0-9])")
)

// GitHubError is the broker or GitHub failing. Its text (a status and the other side's own reason, never a token: both
// sides answer JSON with an error or message string, and the token only goes in an Authorization header) is what the agent
// is told after "backend error", and what the outcome's evidence carries, cut short.
type GitHubError struct {
	Msg    string
	Status int
}

func (e *GitHubError) Error() string { return e.Msg }
func (e *GitHubError) Kind() string  { return "GitHubError" }

// AgentMessage is str(self)[:REASON_MAX + 60].
func (e *GitHubError) AgentMessage() string { return clipRunes(e.Msg, reasonMax+60) }

// Evidence is str(self)[:160].
func (e *GitHubError) Evidence() string { return dispatch.ClipEvidence(e.Msg) }

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n])
	}
	return s
}

// pySplitSpace is " ".join(s.split()): Python's white space, which includes U+001C to U+001F. Go also turns every other
// control character into "?", so an answer cannot put an escape sequence or a NUL into what the agent reads.
func pySplitSpace(s string) string {
	isSpace := func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }
	f := strings.FieldsFunc(s, isSpace)
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}
		return r
	}, strings.Join(f, " "))
}

// ghErrorOf is GitHubError.of: "{what} {status}" and the other side's reason (its `key` string, white space collapsed,
// 300 characters).
func ghErrorOf(what string, status int, doc any, key string) *GitHubError {
	reason := ""
	if m, ok := doc.(map[string]any); ok {
		if s, ok := m[key].(string); ok {
			reason = clipRunes(pySplitSpace(s), reasonMax)
		}
	}
	msg := fmt.Sprintf("%s %d", what, status)
	if reason != "" {
		msg += ": " + reason
	}
	return &GitHubError{Msg: msg, Status: status}
}

// lostWrite is a write whose answer never arrived after the request was sent: the change may have been made. The agent is
// told so (Python says only that GitHub is unreachable, which reads as a failure).
const lostWrite = " (the request was sent and no answer came: the change may have been made)"

// GitHub is the GitBackend.
type GitHub struct {
	api       string
	broker    string
	tokenFile string
	owners    map[string]string // lower case -> lower case: the one spelling every request and receipt uses
	timeout   time.Duration
	apiHC     *http.Client
	brokerHC  *http.Client
	apiLim    limiter
	brokerLim limiter
	retry     retryPolicy
}

// GitHubOptions configures a GitHub backend.
type GitHubOptions struct {
	// API is the GitHub API base (default GitHubAPI). BrokerURL is Glidepath's token broker; there is no default (Python's
	// names a lab Service). TokenFile is the projected token the broker authenticates (AGENT_TOKEN_FILE). Owners are the
	// GitHub owners (users or organisations) this gateway may open pull requests for: no argument chooses one.
	API, BrokerURL, TokenFile string
	Owners                    []string
	Timeout                   time.Duration
	// AllowHTTP lets the broker URL (and an API URL) be plain http: for a lab that serves them so (BROKER_ALLOW_HTTP). Off by
	// default: a bearer token does not cross the network in the clear.
	AllowHTTP bool
	// AllowHTTPAPI is the same for the GitHub API URL, which only a test or an Enterprise lab sets: BROKER_ALLOW_HTTP does not
	// reach it.
	AllowHTTPAPI bool
}

// NewGitHub is the backend, or says why its configuration is not usable.
func NewGitHub(o GitHubOptions) (*GitHub, error) {
	api := o.API
	if api == "" {
		api = GitHubAPI
	}
	a, err := checkBase("the GitHub API URL", api)
	if err == nil {
		err = requireHTTPS("the GitHub API URL", a, o.AllowHTTPAPI)
	}
	if err != nil {
		return nil, err
	}
	b, err := checkBase("BROKER_URL", o.BrokerURL)
	if err == nil {
		err = requireHTTPS("BROKER_URL", b, o.AllowHTTP)
	}
	if err != nil {
		return nil, err
	}
	if o.TokenFile == "" {
		return nil, errors.New("AGENT_TOKEN_FILE is the projected token the broker authenticates")
	}
	owners := map[string]string{}
	for _, ow := range o.Owners {
		if !repoSeg.MatchString(ow) || ow == "." || ow == ".." {
			return nil, fmt.Errorf("GITHUB_OWNERS: %s is not a GitHub owner name", pyStrRepr(ow))
		}
		owners[strings.ToLower(ow)] = strings.ToLower(ow)
	}
	if len(owners) == 0 {
		return nil, errors.New("GITHUB_OWNERS names the GitHub owners the agents' pull requests go to (at least one)")
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	return &GitHub{api: a, broker: b, tokenFile: o.TokenFile, owners: owners, timeout: o.Timeout,
		apiHC: client(nil, 3*time.Second, 16), brokerHC: client(nil, 3*time.Second, 8),
		apiLim: newLimiter(16), brokerLim: newLimiter(8), retry: defaultRetry()}, nil
}

// -- names ------------------------------------------------------------------------------------------------------------

func deniedRepo(v any) error {
	return &core.Denied{Rule: "GH-REPO", Message: echo(v) + " is not <owner>/<repo>", Hint: "Name the repository as owner/name."}
}

// CanonicalRepo is the one spelling of a repository every check, request and receipt uses: owner/name, each of 1 to 100 ASCII
// letters, digits, `.`, `_` and `-`, the name not `.` or `..` and not another spelling of itself (a `.git` suffix), in lower
// case (GitHub resolves names without case, so a check made on one spelling must hold for all of them). Nothing is trimmed:
// a name that is not already this shape is refused.
func CanonicalRepo(repo any) (string, bool) {
	s, ok := repo.(string)
	if !ok {
		return "", false
	}
	o, n, found := strings.Cut(s, "/")
	if !found || !repoSeg.MatchString(o) || !repoSeg.MatchString(n) || o == "." || o == ".." || n == "." || n == ".." ||
		strings.HasSuffix(strings.ToLower(n), ".git") {
		return "", false
	}
	return strings.ToLower(o) + "/" + strings.ToLower(n), true
}

// SplitRepo is _split, and the owner rule: the canonical repository (CanonicalRepo) whose owner is one the deployment is
// configured for. Owner and name come back in lower case.
func (g *GitHub) SplitRepo(repo any) (owner, name string, err error) {
	c, ok := CanonicalRepo(repo)
	if !ok {
		return "", "", deniedRepo(repo)
	}
	o, n, _ := strings.Cut(c, "/")
	canon, ok := g.owners[o]
	if !ok {
		return "", "", &core.Denied{Rule: "GH-OWNER", Message: pyStrRepr(clipRunes(o, 100)) + " is not an owner this gateway opens pull requests for",
			Hint: "Name a repository of an owner the deployment is configured for (GITHUB_OWNERS)."}
	}
	return canon, n, nil
}

// ValidRef is whether s is a ref an agent may name to read or to base a pull request on: Python's characters and rules
// (letters, digits, `.`, `_`, `/` and `-`, at most 200, no `..`, not starting with `/`, not ending `.lock` or `/`), and
// Go's git ref-format rules besides: no leading `-`, no empty or `.`-leading or `.lock`-ending component, no `//`, no
// trailing `.`. (`@{`, `~`, `^`, `:`, `?`, `*`, `[`, a backslash, a space and every control character are outside the
// characters.)
func ValidRef(s string) bool {
	if !refChars.MatchString(s) || strings.Contains(s, "..") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "-") ||
		strings.HasSuffix(s, ".lock") || strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, c := range strings.Split(s, "/") {
		if c == "" || strings.HasPrefix(c, ".") || strings.HasSuffix(c, ".lock") {
			return false
		}
	}
	return true
}

// AgentBranchPrefix is the namespace of the branches this gateway makes (branch_for's `agent/cicd-...`, the examples
// `agent/t-1234`): an agent cannot name, squat on or build on a person's branch.
const AgentBranchPrefix = "agent/"

// ValidNewBranch is ValidRef and what a branch the agent creates may be: in the gateway's own namespace (`agent/`, with
// something after it), not under `refs/`, not a name a person or a protection rule treats as a line of release (main,
// master, develop, trunk, production, staging, release, gh-pages, HEAD, in any case), and not the base.
func ValidNewBranch(s, base string) bool {
	if !strings.HasPrefix(s, AgentBranchPrefix) || len(s) == len(AgentBranchPrefix) || !ValidRef(s) || strings.HasPrefix(strings.ToLower(s), "refs/") || reserved[strings.ToLower(s)] || strings.EqualFold(s, base) {
		return false
	}
	return true
}

func deniedBranch(v any) error {
	return &core.Denied{Rule: "GH-BRANCH", Message: echo(v) + " is not a branch name", Hint: "Use a plain branch name like agent/t-1234."}
}

// recoveryCtx is the context of the one look a write makes after its answer was lost: the call's own time may be what ran
// out, so the look has a short time of its own.
func recoveryCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

// File is one file of a pull request.
type File struct{ Path, Content string }

// -- the closed list of requests --------------------------------------------------------------------------------------

type endpoint int

const (
	epGetRef      endpoint = iota // GET  /repos/{o}/{r}/git/ref/heads/{branch}
	epGetCommit                   // GET  /repos/{o}/{r}/git/commits/{sha}
	epPostTree                    // POST /repos/{o}/{r}/git/trees
	epPostCommit                  // POST /repos/{o}/{r}/git/commits
	epPostRef                     // POST /repos/{o}/{r}/git/refs
	epPostPull                    // POST /repos/{o}/{r}/pulls
	epListPulls                   // GET  /repos/{o}/{r}/pulls?head={o}:{branch}&state=all|open&per_page=n
	epGetContents                 // GET  /repos/{o}/{r}/contents/{path}?ref={ref}
	epGetRepo                     // GET  /repos/{o}/{r}
)

// AllowedGitHubRequests is the list, for the documentation and for a test that holds what is sent to it.
var AllowedGitHubRequests = []string{
	"GET /repos/{owner}/{repo}/git/ref/heads/{branch}",
	"GET /repos/{owner}/{repo}/git/commits/{sha}",
	"POST /repos/{owner}/{repo}/git/trees",
	"POST /repos/{owner}/{repo}/git/commits",
	"POST /repos/{owner}/{repo}/git/refs",
	"POST /repos/{owner}/{repo}/pulls",
	"GET /repos/{owner}/{repo}/pulls?head={owner}:{branch}&state={all|open}&per_page={5|10}",
	"GET /repos/{owner}/{repo}/contents/{path}?ref={ref}",
	"GET /repos/{owner}/{repo}",
}

type ghReq struct {
	ep          endpoint
	owner, name string
	branch      string // epGetRef, epListPulls
	sha         string // epGetCommit
	path        string // epGetContents
	ref         string // epGetContents
	state       string // epListPulls
	perPage     int    // epListPulls
	body        any    // the POSTs
}

func escSeg(s string) string { return segEscape(s) }

// escPath escapes a repository path or a ref segment by segment, keeping the `/`.
func escPath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = escSeg(s)
	}
	return strings.Join(parts, "/")
}

// target is the method and path (with query) of a request, or an error: a part that is not valid for its place is never
// sent. It is the only place a path to GitHub is made.
func (r ghReq) target() (method, target string, err error) {
	if !repoSeg.MatchString(r.owner) || !repoSeg.MatchString(r.name) || r.owner == "." || r.owner == ".." || r.name == "." || r.name == ".." {
		return "", "", errors.New("repository")
	}
	root := "/repos/" + escSeg(r.owner) + "/" + escSeg(r.name)
	switch r.ep {
	case epGetRef:
		if !ValidRef(r.branch) {
			return "", "", errors.New("ref")
		}
		return http.MethodGet, root + "/git/ref/heads/" + escPath(r.branch), nil
	case epGetCommit:
		if !shaRx.MatchString(r.sha) {
			return "", "", errors.New("sha")
		}
		return http.MethodGet, root + "/git/commits/" + r.sha, nil
	case epGetRepo:
		return http.MethodGet, root, nil
	case epPostTree:
		return http.MethodPost, root + "/git/trees", nil
	case epPostCommit:
		return http.MethodPost, root + "/git/commits", nil
	case epPostRef:
		b, _ := r.body.(refBody)
		if !strings.HasPrefix(b.Ref, "refs/heads/") || !ValidNewBranch(strings.TrimPrefix(b.Ref, "refs/heads/"), "") || !shaRx.MatchString(b.Sha) {
			return "", "", errors.New("ref body")
		}
		return http.MethodPost, root + "/git/refs", nil
	case epPostPull:
		return http.MethodPost, root + "/pulls", nil
	case epListPulls:
		if !ValidRef(r.branch) || (r.state != "all" && r.state != "open") || (r.perPage != 5 && r.perPage != 10) {
			return "", "", errors.New("pulls query")
		}
		head := url.QueryEscape(r.owner + ":" + r.branch)
		return http.MethodGet, fmt.Sprintf("%s/pulls?head=%s&state=%s&per_page=%d", root, head, r.state, r.perPage), nil
	case epGetContents:
		// A path does not start with a slash (the core refuses one too); trailing slashes are dropped, as Python drops them.
		if strings.HasPrefix(r.path, "/") {
			return "", "", errors.New("path")
		}
		p := strings.TrimRight(r.path, "/")
		if p != "" {
			for _, seg := range strings.Split(p, "/") {
				if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "\x00\\") || strings.IndexFunc(seg, unicode.IsControl) >= 0 {
					return "", "", errors.New("path")
				}
			}
		}
		if !ValidRef(r.ref) {
			return "", "", errors.New("ref")
		}
		return http.MethodGet, root + "/contents/" + escPath(p) + "?ref=" + url.QueryEscape(r.ref), nil
	}
	return "", "", errors.New("endpoint")
}

type refBody struct {
	Ref string `json:"ref"`
	Sha string `json:"sha"`
}

type treeEntry struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

// writeLost is a write that was sent and not answered.
type writeLost struct{ GitHubError }

// gh sends one request of the closed list with an installation token and answers (status, JSON). A POST that got no answer
// is a *writeLost; a GET that got none is a GitHubError "GitHub unreachable".
func (g *GitHub) gh(ctx context.Context, token string, r ghReq) (int, any, error) {
	method, target, err := r.target()
	if err != nil {
		return 0, nil, &dispatch.KindError{Kind: "ValueError", Err: errors.New("a GitHub request part is not valid: " + err.Error())}
	}
	hdr := http.Header{"Authorization": {"Bearer " + token}, "Accept": {"application/vnd.github+json"}, "X-GitHub-Api-Version": {"2022-11-28"}}
	var body []byte
	if method == http.MethodPost {
		hdr.Set("Content-Type", "application/json")
		if body, err = json.Marshal(r.body); err != nil {
			return 0, nil, err
		}
	}
	var rp *reply
	if method == http.MethodGet {
		rp, err = sendRead(ctx, g.retry, g.apiHC, g.apiLim, g.timeout, g.api+target, hdr, maxAPIOK, maxAPIErr)
	} else {
		rp, err = send(ctx, g.apiHC, g.apiLim, g.timeout, method, g.api+target, hdr, body, maxAPIOK, maxAPIErr)
	}
	if err != nil {
		var te *transportError
		if errors.As(err, &te) {
			msg := "GitHub unreachable: " + te.pyClass()
			if method == http.MethodPost && te.sent {
				return 0, nil, &writeLost{GitHubError{Msg: msg + lostWrite}}
			}
			return 0, nil, &GitHubError{Msg: msg}
		}
		return 0, nil, err
	}
	return rp.status, docAny(redact(rp.body, token)), nil
}

// docAny is _gh_any's parse: the JSON (an object or a list), or {} for an empty or unreadable answer.
func docAny(b []byte) any {
	if len(b) == 0 {
		return map[string]any{}
	}
	v, err := decodeJSON(b)
	if err != nil {
		return map[string]any{}
	}
	return v
}

// docMap is _json: the JSON object, or {}.
func docMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// -- the token --------------------------------------------------------------------------------------------------------

// AppOf is app_of: the app a repository belongs to by Glidepath's naming (<app>, gitops-<app>, gitops-infra-<app>).
func AppOf(repo string) string {
	for _, p := range []string{"gitops-infra-", "gitops-"} {
		if strings.HasPrefix(strings.ToLower(repo), p) {
			return repo[len(p):]
		}
	}
	return repo
}

// mint asks the broker for an installation token for this one repository, as an app's own. Each call asks anew.
func (g *GitHub) mint(ctx context.Context, owner, name string) (string, error) {
	tok, err := readTokenMin(g.tokenFile)
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(tok, "\r\n") {
		return "", headerError()
	}
	body, _ := json.Marshal(map[string]string{"owner": owner, "repo": name, "app": AppOf(name)})
	hdr := http.Header{"Authorization": {"Bearer " + tok}, "Content-Type": {"application/json"}}
	rp, err := send(ctx, g.brokerHC, g.brokerLim, g.timeout, http.MethodPost, g.broker+"/agent-installation-token", hdr, body, maxAPIOK, maxAPIErr)
	if err != nil {
		var te *transportError
		if errors.As(err, &te) {
			return "", &GitHubError{Msg: "broker unreachable: " + te.pyClass()}
		}
		return "", err
	}
	raw := docMap(docAny(rp.body))
	if t, ok := raw["token"].(string); ok && t != "" && rp.status == 200 {
		if len(t) > 4096 || !tokenRx.MatchString(t) {
			return "", &GitHubError{Msg: "GitHub token broker answered with a token this gateway will not use", Status: 200}
		}
		return t, nil
	}
	// What the broker says in a failure is read with every credential taken out of it.
	minted, _ := raw["token"].(string)
	doc := docMap(docAny(redact(redact(rp.body, tok), minted)))
	if rp.status == 403 {
		return "", &core.Denied{Rule: "GH-REPO-NOT-APP", Message: owner + "/" + name + " is not an onboarded app's repo",
			Hint: "Agents open PRs on an app's own repos only (<app>, gitops-<app>)."}
	}
	return "", ghErrorOf("GitHub token broker", rp.status, doc, "error")
}

// -- opening a pull request -------------------------------------------------------------------------------------------

// PR is a pull request as the tools answer it.
type PR struct {
	Number int64
	URL    string
	Branch string
	Commit string
	State  string
}

func (p *PR) result(extra ...[2]any) map[string]any {
	m := map[string]any{"pr": p.Number, "url": p.URL, "branch": p.Branch, "commit": p.Commit}
	for _, kv := range extra {
		m[kv[0].(string)] = kv[1]
	}
	return m
}

func deniedFiles(msg, hint string) error {
	return &core.Denied{Rule: "GH-FILES", Message: msg, Hint: hint}
}

// OpenPRInput is one pull request to open.
type OpenPRInput struct {
	Repo, Branch, Title, Base string
	Files                     []File
	Meta                      map[string]string
	Body                      string
	// DenyPaths are the paths the agent may not write (its definition's, with the baseline).
	DenyPaths []string
	// Check runs once the base is known and before anything is written: the cicd.yaml check, asked about the resolved base.
	Check func(ctx context.Context, base string) error
}

func cleanMeta(v string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, v)
}

func metaKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func commitMessage(title string, meta map[string]string) string {
	var tr strings.Builder
	for _, k := range metaKeys(meta) {
		tr.WriteString("\nHangar-" + strings.ToUpper(k[:1]) + k[1:] + ": " + cleanMeta(meta[k]))
	}
	return title + "\n" + tr.String()
}

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?(?:-->|$)`)
	crossRef    = regexp.MustCompile(`#([0-9])`)
	closing     = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b`)
)

// defang makes text that an agent (or glidepath-api, from the agent's patch) wrote safe to put in a pull request: nobody is
// notified, no issue is linked or closed, nothing is hidden or raw. An @name that GitHub would read as a mention (also after a
// backtick: an unclosed code span still notifies), a `#12` or `owner/repo#12` reference and a closing keyword each get a
// zero-width space; HTML comments are removed, `<` is escaped and an image's `![` is broken. The cap is applied after, on what
// goes out.
func defang(s string) string {
	s = htmlComment.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, "![", "!\u200b[")
	s = mention.ReplaceAllString(s, "$1@\u200b$2")
	s = crossRef.ReplaceAllString(s, "#\u200b$1")
	s = closing.ReplaceAllStringFunc(s, func(w string) string { return w[:1] + "\u200b" + w[1:] })
	return clipRunes(s, maxPRBody)
}

// checkInput is the validation every pull request passes before any request exists.
func (g *GitHub) checkInput(in *OpenPRInput) (owner, name string, entries []treeEntry, err error) {
	if owner, name, err = g.SplitRepo(in.Repo); err != nil {
		return
	}
	if in.Base != "" && !ValidRef(in.Base) {
		return "", "", nil, deniedBranch(in.Base)
	}
	if !ValidNewBranch(in.Branch, in.Base) {
		return "", "", nil, deniedBranch(in.Branch)
	}
	if reservedTitle.MatchString(in.Title) {
		return "", "", nil, &core.Denied{Rule: "GH-TITLE", Message: "a title does not look like one of the gateway's trailers", Hint: "Give the pull request a plain one-line title."}
	}
	if t := pyStrip(in.Title); t == "" || utf8.RuneCountInString(in.Title) > maxTitle || strings.IndexFunc(in.Title, unicode.IsControl) >= 0 || !utf8.ValidString(in.Title) {
		return "", "", nil, &core.Denied{Rule: "GH-TITLE", Message: "a title is one line of 1 to 200 characters", Hint: "Give the pull request a short one-line title."}
	}
	if len(in.Files) == 0 || len(in.Files) > maxFiles {
		return "", "", nil, deniedFiles(fmt.Sprintf("a PR carries 1 to %d files", maxFiles), "Split the change.")
	}
	total := 0
	seen := map[string]bool{}
	for _, f := range in.Files {
		if len(f.Content) > maxFileBytes || !utf8.ValidString(f.Content) {
			return "", "", nil, deniedFiles("each file needs a path and text content under 1 MiB", "Fix the files list.")
		}
		n, ok := normalizePath(f.Path)
		if !ok || n != f.Path || !utf8.ValidString(f.Path) || strings.IndexFunc(f.Path, unicode.IsControl) >= 0 {
			return "", "", nil, deniedFiles("each file needs a normal repository path and text content under 1 MiB", "Fix the files list.")
		}
		if oddName(n) {
			return "", "", nil, &core.Denied{Rule: "GH-PATH", Message: pyStrRepr(clipRunes(f.Path, 100)) + " has a name a file system reads as another name",
				Hint: "Use plain file names: no trailing space or dot, no `~1` short names, no `:`."}
		}
		for _, seg := range strings.Split(n, "/") {
			if strings.EqualFold(seg, ".git") {
				return "", "", nil, deniedFiles("a path may not hold a .git component", "Fix the files list.")
			}
		}
		if seen[strings.ToLower(n)] {
			return "", "", nil, deniedFiles("a path is named twice", "Fix the files list.")
		}
		seen[strings.ToLower(n)] = true
		// A blob written at a path replaces whatever tree is there, so a path that is a protected directory itself (`.github`,
		// `.tekton`) would delete it from the new tree: the path is also tried as the directory that holds a protected file.
		denied := pathDenied(n, in.DenyPaths) || pathDenied(n+"/x", in.DenyPaths) || ancestorOfDenied(n, in.DenyPaths)
		// A tree written under a protected FILE replaces the file (`CODEOWNERS/x` takes the place of `CODEOWNERS`), so no ancestor
		// of a path may be a denied path either.
		for _, a := range properAncestors(n) {
			denied = denied || pathDenied(a, in.DenyPaths)
		}
		if denied {
			return "", "", nil, &core.Denied{Rule: "GH-PATH", Message: pyStrRepr(clipRunes(f.Path, 100)) + " is a protected path",
				Hint: "An agent does not change protected paths (CI, ownership, release files); ask a person."}
		}
		if total += len(f.Content); total > maxTotalBytes {
			return "", "", nil, deniedFiles("a PR carries at most 8 MiB of files in total", "Split the change.")
		}
		entries = append(entries, treeEntry{Path: f.Path, Mode: "100644", Type: "blob", Content: f.Content})
	}
	return
}

var shortName = regexp.MustCompile(`~[0-9]`)

// reservedTitle is a title that starts like one of the trailers the gateway writes (Hangar-Session: ...).
var reservedTitle = regexp.MustCompile(`(?i)^\s*hangar-[a-z]+\s*:`)

func properAncestors(p string) []string {
	segs := strings.Split(p, "/")
	var out []string
	for i := 1; i < len(segs); i++ {
		out = append(out, strings.Join(segs[:i], "/"))
	}
	return out
}

// oddName says a path has a segment that a file system may read as another name: a space or dot at an end (Windows drops
// them, so `.github ` is `.github`), an 8.3 short name (`GITHUB~1`), or a `:` (a stream, `.github::$DATA`).
func oddName(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg != strings.TrimSpace(seg) || strings.HasSuffix(seg, ".") || shortName.MatchString(seg) || strings.Contains(seg, ":") {
			return true
		}
	}
	return false
}

func hasWildcard(s string) bool { return strings.ContainsAny(s, "*?[") }

// ancestorOfDenied says a path is a proper ancestor of what a deny pattern names by its literal leading directories: a
// blob written at `deploy` replaces the directory `deploy` and takes `deploy/prod/values.yaml` with it. Patterns that start
// with a wildcard (`**/release.yaml`) name no directory; for those the base tree is read (refuseDirectories). Matching is
// without case.
func ancestorOfDenied(p string, deny []string) bool {
	segs := strings.Split(strings.ToLower(p), "/")
	for _, g := range deny {
		gs := strings.Split(strings.ToLower(g), "/")
		lit := 0
		for lit < len(gs) && !hasWildcard(gs[lit]) {
			lit++
		}
		bound := lit
		if lit == len(gs) { // fully literal: only a proper ancestor, the path itself is matched on its own
			bound = lit - 1
		}
		if len(segs) > bound {
			continue
		}
		same := true
		for i := range segs {
			same = same && segs[i] == gs[i]
		}
		if same {
			return true
		}
	}
	return false
}

// refuseDirectories reads what the base commit has at each file's path and at each ancestor of it (the contents request of the
// closed list, once for each path, at most maxBaseReads in all, and past that the call is refused): a blob sent with a base tree
// replaces a directory the base has there, with every file beneath it, and a tree sent under a path the base has as a file
// replaces that file. Whatever the deny list says, a file does not take a directory's place and nothing is written under a file.
func (g *GitHub) refuseDirectories(ctx context.Context, token, owner, name, baseSha string, files []treeEntry) error {
	state := map[string]string{} // path -> "tree", "blob" or "none"
	reads := 0
	read := func(p string) (string, error) {
		if st, ok := state[p]; ok {
			return st, nil
		}
		if reads >= maxBaseReads {
			return "", deniedFiles("the change touches more of the base than the gateway reads", "Split the change.")
		}
		reads++
		status, doc, err := g.gh(ctx, token, ghReq{ep: epGetContents, owner: owner, name: name, path: p, ref: baseSha})
		if err != nil {
			return "", err
		}
		st := "blob"
		switch {
		case status == 404:
			st = "none"
		case status != 200:
			return "", ghErrorOf("contents", status, doc, "message")
		default:
			if _, isDir := doc.([]any); isDir {
				st = "tree"
			}
		}
		state[p] = st
		return st, nil
	}
	for _, f := range files {
		segs := strings.Split(f.Path, "/")
		for k := 1; k <= len(segs); k++ {
			p := strings.Join(segs[:k], "/")
			st, err := read(p)
			if err != nil {
				return err
			}
			if k == len(segs) {
				if st == "tree" {
					return &core.Denied{Rule: "GH-PATH", Message: pyStrRepr(clipRunes(f.Path, 100)) + " is a directory in the base",
						Hint: "A file cannot take the place of a directory; name a file inside it."}
				}
				break
			}
			switch st {
			case "none":
				k = len(segs) // nothing below a path the base does not have
			case "blob":
				return &core.Denied{Rule: "GH-PATH", Message: pyStrRepr(clipRunes(f.Path, 100)) + " is under " + pyStrRepr(clipRunes(p, 100)) + ", a file in the base",
					Hint: "Nothing is written under a file; name another path."}
			}
		}
	}
	return nil
}

// defaultBranch is the repository's default branch, the only base an agent's pull request has.
func (g *GitHub) defaultBranch(ctx context.Context, token, owner, name string) (string, error) {
	status, doc, err := g.gh(ctx, token, ghReq{ep: epGetRepo, owner: owner, name: name})
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", ghErrorOf("repository", status, doc, "message")
	}
	b, _ := docMap(doc)["default_branch"].(string)
	if !ValidRef(b) {
		return "", badAnswer("repository")
	}
	return b, nil
}

// resolveBase is the base of a pull request: the default branch, which an empty base means; any other is refused (the
// definition has no field that allows another).
func (g *GitHub) resolveBase(ctx context.Context, token, owner, name string, in *OpenPRInput) error {
	def, err := g.defaultBranch(ctx, token, owner, name)
	if err != nil {
		return err
	}
	if in.Base != "" && in.Base != def {
		return &core.Denied{Rule: "GH-BASE", Message: "a pull request is based on the repository's default branch, " + pyStrRepr(def) + ", not " + echo(in.Base),
			Hint: "Leave base out; an agent's definition cannot name another."}
	}
	in.Base = def
	if strings.EqualFold(in.Branch, def) {
		return deniedBranch(in.Branch)
	}
	return nil
}

// ownedBy checks that the commit a branch holds was made by this gateway for this session: its message carries the
// Hangar-Session trailer the gateway wrote (a title is one line, so an agent cannot write one).
func (g *GitHub) ownedBy(ctx context.Context, token, owner, name, sha string, meta map[string]string, branch string) error {
	foreign := &core.Denied{Rule: "APPLY-BRANCH-FOREIGN", Message: "branch " + branch + " was not made by this session",
		Hint: "A person looks at the branch; the tool never builds on, pushes to or deletes a branch it did not make."}
	if meta["session"] == "" || !shaRx.MatchString(sha) {
		return foreign
	}
	status, doc, err := g.gh(ctx, token, ghReq{ep: epGetCommit, owner: owner, name: name, sha: sha})
	if err != nil {
		return err
	}
	if status != 200 {
		return foreign
	}
	// The message is the one the gateway writes: a subject line, a blank line and exactly the trailers this call's metadata makes,
	// in order. A line anywhere else (a subject that looks like a trailer, a second block) is not the gateway's.
	msg, _ := docMap(doc)["message"].(string)
	if !trailerBlockOwned(msg, meta) {
		return foreign
	}
	return nil
}

// trailerBlockOwned says a commit message is the one the gateway writes for this metadata: one subject line, a blank line, and
// exactly the trailers commitMessage makes, in order, and nothing after them.
func trailerBlockOwned(msg string, meta map[string]string) bool {
	lines := strings.Split(strings.TrimRight(msg, "\n"), "\n")
	want := trailerLines(meta)
	if len(want) == 0 || len(lines) != 2+len(want) || lines[1] != "" {
		return false
	}
	for i, w := range want {
		if lines[2+i] != w {
			return false
		}
	}
	return true
}

// trailerLines are the trailers commitMessage writes for this metadata, in its order.
func trailerLines(meta map[string]string) []string {
	var out []string
	for _, k := range metaKeys(meta) {
		out = append(out, "Hangar-"+strings.ToUpper(k[:1])+k[1:]+": "+cleanMeta(meta[k]))
	}
	return out
}

func badAnswer(what string) error {
	return &dispatch.KindError{Kind: "KeyError", Err: errors.New(what + ": the answer lacks what it should hold")}
}

// shaAt reads doc[k1][k2] (or doc[k1] when k2 is empty) as a commit id.
func shaAt(doc any, k1, k2 string) (string, bool) {
	m, ok := doc.(map[string]any)
	if !ok {
		return "", false
	}
	var s string
	if k2 == "" {
		s, _ = m[k1].(string)
	} else {
		s, _ = obj(m[k1])[k2].(string)
	}
	return s, shaRx.MatchString(s)
}

// OpenPR is open_pr: the pull request, built with the Git data API, on a branch that must not exist yet. It never pushes to
// or forces an existing branch. A token is minted for this call and dropped with it.
func (g *GitHub) OpenPR(ctx context.Context, in OpenPRInput) (map[string]any, error) {
	owner, name, entries, err := g.checkInput(&in)
	if err != nil {
		return nil, err
	}
	token, err := g.mint(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	if err := g.resolveBase(ctx, token, owner, name, &in); err != nil {
		return nil, err
	}
	if in.Check != nil {
		if err := in.Check(ctx, in.Base); err != nil {
			return nil, err
		}
	}
	pr, err := g.openPR(ctx, token, owner, name, in, entries)
	if err != nil {
		return nil, err
	}
	return pr.result(), nil
}

func (g *GitHub) openPR(ctx context.Context, token, owner, name string, in OpenPRInput, entries []treeEntry) (*PR, error) {
	status, ref, err := g.gh(ctx, token, ghReq{ep: epGetRef, owner: owner, name: name, branch: in.Base})
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, ghErrorOf("base "+in.Base, status, ref, "message")
	}
	baseSha, ok := shaAt(ref, "object", "sha")
	if !ok {
		return nil, badAnswer("base " + in.Base)
	}
	status, commit, err := g.gh(ctx, token, ghReq{ep: epGetCommit, owner: owner, name: name, sha: baseSha})
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, ghErrorOf("base commit", status, commit, "message")
	}
	baseTree, ok := shaAt(commit, "tree", "sha")
	if !ok {
		return nil, badAnswer("base commit")
	}
	if err := g.refuseDirectories(ctx, token, owner, name, baseSha, entries); err != nil {
		return nil, err
	}
	status, tree, err := g.gh(ctx, token, ghReq{ep: epPostTree, owner: owner, name: name,
		body: map[string]any{"base_tree": baseTree, "tree": entries}})
	if err != nil {
		return nil, err
	}
	if status != 201 {
		return nil, ghErrorOf("tree", status, tree, "message")
	}
	treeSha, ok := shaAt(tree, "sha", "")
	if !ok {
		return nil, badAnswer("tree")
	}
	status, nc, err := g.gh(ctx, token, ghReq{ep: epPostCommit, owner: owner, name: name,
		body: map[string]any{"message": commitMessage(in.Title, in.Meta), "tree": treeSha, "parents": []string{baseSha}}})
	if err != nil {
		return nil, err
	}
	if status != 201 {
		return nil, ghErrorOf("commit", status, nc, "message")
	}
	newSha, ok := shaAt(nc, "sha", "")
	if !ok {
		return nil, badAnswer("commit")
	}
	status, last, err := g.gh(ctx, token, ghReq{ep: epPostRef, owner: owner, name: name, body: refBody{Ref: "refs/heads/" + in.Branch, Sha: newSha}})
	var lost *writeLost
	switch {
	case errors.As(err, &lost):
		// The answer was lost: the branch is ours if it exists and holds the commit this call made. Look once (a GET); a
		// branch that is not there, or is someone else's, is the error, said to maybe have happened.
		rctx, stop := recoveryCtx(ctx)
		defer stop()
		st, ref, gerr := g.gh(rctx, token, ghReq{ep: epGetRef, owner: owner, name: name, branch: in.Branch})
		if gerr != nil || st != 200 {
			return nil, err
		}
		if s, _ := shaAt(ref, "object", "sha"); s != newSha {
			return nil, err
		}
	case err != nil:
		return nil, err
	case status == 422:
		return nil, &core.Denied{Rule: "GH-BRANCH-EXISTS", Message: "branch " + in.Branch + " already exists",
			Hint: "Use a new branch name; Clearance never pushes to an existing branch."}
	case status != 201:
		return nil, ghErrorOf("ref", status, last, "message")
	}
	pr, err := g.createPR(ctx, token, owner, name, in.Title, in.Branch, in.Base, in.Meta, in.Body)
	if err != nil {
		return nil, err
	}
	pr.Commit = newSha
	return pr, nil
}

// prFrom reads a created pull request's number and address.
func prFrom(doc any, branch string) (*PR, bool) {
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, false
	}
	n, ok := asInt64(m["number"])
	u, _ := m["html_url"].(string)
	if !ok || n < 1 || len(u) > 512 {
		return nil, false
	}
	if pu, err := url.Parse(u); err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || pu.Host == "" {
		return nil, false
	}
	return &PR{Number: n, URL: u, Branch: branch}, true
}

func asInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	case int64:
		return x, true
	case int:
		return int64(x), true
	}
	return 0, false
}

// createPR is _create_pr. The body is Python's template (who opened it, from the session) and, when the caller has one, its
// text, cut at 60000 characters and defanged (no @mention notifies anybody).
func (g *GitHub) createPR(ctx context.Context, token, owner, name, title, branch, base string, meta map[string]string, body string) (*PR, error) {
	var sb strings.Builder
	sb.WriteString("Opened by a Hangar agent through Clearance.\n\n")
	for i, k := range metaKeys(meta) {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("- " + k + ": `" + cleanMeta(meta[k]) + "`")
	}
	if body != "" {
		sb.WriteString("\n\n" + defang(body))
	}
	status, pr, err := g.gh(ctx, token, ghReq{ep: epPostPull, owner: owner, name: name,
		body: map[string]any{"title": title, "head": branch, "base": base, "body": sb.String()}})
	var lost *writeLost
	if errors.As(err, &lost) {
		// Sent, no answer: look for the pull request of this branch before saying anything (a GET).
		rctx, stop := recoveryCtx(ctx)
		defer stop()
		if found, ferr := g.prOf(rctx, token, owner, name, branch); ferr == nil && found != nil {
			return found, nil
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if status != 201 {
		return nil, ghErrorOf("pull", status, pr, "message")
	}
	p, ok := prFrom(pr, branch)
	if !ok {
		return nil, badAnswer("pull")
	}
	return p, nil
}

// -- resuming ---------------------------------------------------------------------------------------------------------

// prOf is _pr_of: the pull request a branch already has (merged first, then open, then closed), or nil. A closed one whose
// branch is gone leaves the name free. GET only.
func (g *GitHub) prOf(ctx context.Context, token, owner, name, branch string) (*PR, error) {
	status, doc, err := g.gh(ctx, token, ghReq{ep: epListPulls, owner: owner, name: name, branch: branch, state: "all", perPage: 5})
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, ghErrorOf("pulls", status, doc, "message")
	}
	var prs []map[string]any
	if list, ok := doc.([]any); ok {
		for _, it := range list {
			if m, ok := it.(map[string]any); ok && obj(m["head"])["ref"] == branch {
				prs = append(prs, m)
			}
		}
	}
	rank := func(p map[string]any) int {
		switch {
		case pyTruthy(p["merged_at"]):
			return 0
		case p["state"] == "open":
			return 1
		}
		return 2
	}
	sort.SliceStable(prs, func(i, j int) bool { return rank(prs[i]) < rank(prs[j]) })
	if len(prs) == 0 {
		return nil, nil
	}
	pr := prs[0]
	if !pyTruthy(pr["merged_at"]) && pr["state"] != "open" {
		st, last, err := g.gh(ctx, token, ghReq{ep: epGetRef, owner: owner, name: name, branch: branch})
		if err != nil {
			return nil, err
		}
		if st == 404 {
			return nil, nil
		}
		if st != 200 {
			return nil, ghErrorOf("ref", st, last, "message")
		}
	}
	state := "open"
	if pyTruthy(pr["merged_at"]) {
		state = "merged"
	} else if s, ok := pr["state"].(string); ok {
		state = s
	}
	n, _ := asInt64(pr["number"])
	u, _ := pr["html_url"].(string)
	sha, _ := obj(pr["head"])["sha"].(string)
	return &PR{Number: n, URL: u, Branch: branch, Commit: sha, State: state}, nil
}

// FindPR is find_pr: the pull request a branch already has, or nil. GET only.
func (g *GitHub) FindPR(ctx context.Context, repo, branch string) (map[string]any, error) {
	owner, name, err := g.SplitRepo(repo)
	if err != nil {
		return nil, err
	}
	if !ValidRef(branch) {
		return nil, deniedBranch(branch)
	}
	token, err := g.mint(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	pr, err := g.prOf(ctx, token, owner, name, branch)
	if err != nil || pr == nil {
		return nil, err
	}
	return pr.result([2]any{"state", pr.State}), nil
}

// textAt is _text_at: a file's text on a ref, or ("", false) when it is not there or not text.
func (g *GitHub) textAt(ctx context.Context, token, owner, name, path, ref string) (string, bool, error) {
	status, doc, err := g.gh(ctx, token, ghReq{ep: epGetContents, owner: owner, name: name, path: path, ref: ref})
	if err != nil {
		return "", false, err
	}
	if status == 404 {
		return "", false, nil
	}
	m, isMap := doc.(map[string]any)
	if status != 200 || !isMap || m["encoding"] != "base64" {
		return "", false, ghErrorOf("contents", status, doc, "message")
	}
	raw, err := b64(m["content"])
	if err != nil {
		return "", false, err
	}
	if !utf8.Valid(raw) {
		return "", false, nil
	}
	return string(raw), true, nil
}

// b64 is base64.b64decode of a content string: its line breaks are ignored; a malformed one is Python's binascii.Error.
func b64(v any) ([]byte, error) {
	s, _ := v.(string)
	s = strings.NewReplacer("\n", "", "\r", "").Replace(s)
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, &dispatch.KindError{Kind: "Error", Err: errors.New("the file's content is not base64")}
	}
	return raw, nil
}

// EnsurePR is ensure_pr: open_pr that survives an interruption. The pull request this branch already has is returned
// (resumed); a branch left without its pull request (the call died between the ref and the pull request) gets the pull
// request once its files are checked to be the ones asked for; only a branch that does not exist is built from the base as
// OpenPR does. Nothing is ever pushed to or forced on an existing branch.
func (g *GitHub) EnsurePR(ctx context.Context, in OpenPRInput) (map[string]any, error) {
	owner, name, entries, err := g.checkInput(&in)
	if err != nil {
		return nil, err
	}
	token, err := g.mint(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	if err := g.resolveBase(ctx, token, owner, name, &in); err != nil {
		return nil, err
	}
	found, err := g.prOf(ctx, token, owner, name, in.Branch)
	if err != nil {
		return nil, err
	}
	if found != nil {
		if err := g.ownedBy(ctx, token, owner, name, found.Commit, in.Meta, in.Branch); err != nil {
			return nil, err
		}
		// and its files are the ones asked for (read at the head commit, which a merged PR's deleted branch still has)
		for _, f := range in.Files {
			got, ok, err := g.textAt(ctx, token, owner, name, f.Path, found.Commit)
			if err != nil {
				return nil, err
			}
			if !ok || got != f.Content {
				return nil, &core.Denied{Rule: "APPLY-BRANCH-DIVERGED", Message: "the pull request of branch " + in.Branch + " holds a " + f.Path + " that is not what this change would write",
					Hint: "A person looks at the branch; the tool never pushes to or deletes an existing one."}
			}
		}
		return found.result([2]any{"state", found.State}, [2]any{"resumed", true}), nil
	}
	status, ref, err := g.gh(ctx, token, ghReq{ep: epGetRef, owner: owner, name: name, branch: in.Branch})
	if err != nil {
		return nil, err
	}
	switch {
	case status == 200:
		sha, _ := shaAt(ref, "object", "sha")
		if err := g.ownedBy(ctx, token, owner, name, sha, in.Meta, in.Branch); err != nil {
			return nil, err
		}
		for _, f := range in.Files {
			got, ok, err := g.textAt(ctx, token, owner, name, f.Path, in.Branch)
			if err != nil {
				return nil, err
			}
			if !ok || got != f.Content {
				return nil, &core.Denied{Rule: "APPLY-BRANCH-DIVERGED", Message: "branch " + in.Branch + " exists and " + f.Path + " on it is not what this change would write",
					Hint: "A person looks at the branch; the tool never pushes to or deletes an existing one."}
			}
		}
		pr, err := g.createPR(ctx, token, owner, name, in.Title, in.Branch, in.Base, in.Meta, in.Body)
		if err != nil {
			return nil, err
		}
		pr.Commit = sha
		return pr.result([2]any{"state", "open"}, [2]any{"resumed", true}), nil
	case status != 404:
		return nil, ghErrorOf("ref", status, ref, "message")
	}
	pr, err := g.openPR(ctx, token, owner, name, in, entries)
	if err != nil {
		return nil, err
	}
	return pr.result([2]any{"state", "open"}, [2]any{"resumed", false}), nil
}

// -- reading ----------------------------------------------------------------------------------------------------------

// ReadFile is read_file (repo.file.get): one file's text, or a directory's entries, at a ref, with the same per-call
// token. A file over 1 MiB or a binary one is named, not returned.
func (g *GitHub) ReadFile(ctx context.Context, repo any, path string, ref string) (map[string]any, error) {
	owner, name, err := g.SplitRepo(repo)
	if err != nil {
		return nil, err
	}
	if !ValidRef(ref) {
		return nil, &core.Denied{Rule: "GH-REF", Message: echo(ref) + " is not a ref", Hint: "Pass a branch, tag or commit sha."}
	}
	if _, _, err := (ghReq{ep: epGetContents, owner: owner, name: name, path: path, ref: ref}).target(); err != nil {
		return nil, &core.Denied{Rule: "GH-PATH", Message: pyStrRepr(clipRunes(path, 100)) + " is not a path in the repository", Hint: "Pass a path such as docs/readme.md (no '..')."}
	}
	token, err := g.mint(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	status, doc, err := g.gh(ctx, token, ghReq{ep: epGetContents, owner: owner, name: name, path: path, ref: ref})
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return map[string]any{"path": path, "ref": ref, "found": false}, nil
	}
	if status != 200 {
		return nil, ghErrorOf("contents", status, doc, "message")
	}
	if list, ok := doc.([]any); ok {
		entries := []any{}
		for i, e := range list {
			if i >= 500 {
				break
			}
			em := obj(e)
			entries = append(entries, map[string]any{"name": em["name"], "type": em["type"], "size": em["size"]})
		}
		return map[string]any{"path": path, "ref": ref, "type": "dir", "entries": entries}, nil
	}
	m := docMap(doc)
	if m["type"] != "file" {
		return map[string]any{"path": path, "ref": ref, "type": m["type"]}, nil
	}
	var size int64
	if pyTruthy(m["size"]) {
		size, _ = asInt64(m["size"])
	}
	if size > maxFileBytes || m["encoding"] != "base64" {
		return map[string]any{"path": path, "ref": ref, "type": "file", "size": size, "content": nil, "note": "too large to return (over 1 MiB)"}, nil
	}
	raw, err := b64(m["content"])
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(raw) {
		return map[string]any{"path": path, "ref": ref, "type": "file", "size": size, "content": nil, "note": "binary"}, nil
	}
	return map[string]any{"path": path, "ref": ref, "type": "file", "size": size, "sha": m["sha"], "content": string(raw)}, nil
}
