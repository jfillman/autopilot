package backends

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jfillman/autopilot/gateway/internal/airframe"
	"github.com/jfillman/autopilot/gateway/internal/core"
)

// What xr.request and airframe.apply need of GitHub beyond S5b's pull request: github_pr.py's commit_xr, pr_paths, repo_ready,
// onboarding_prs, find_pr and read_file as airframe.apply calls them. Each is built from the same closed list of requests as
// every other (github.go: the one new entry is the list of a pull request's files); each mints its own installation token, for
// the one repository it names, and drops it; none writes except CommitXR, which opens one pull request as OpenPR does.

// OnboardingBranch is the branch Glidepath's onboarding resync opens its pull request from.
const OnboardingBranch = "onboarding-resync"

// maxPRPaths is how many of a pull request's files are listed (one page, as Python reads: the first 100).
const maxPRPaths = 100

// FindPRIn is find_pr: the pull request a branch already has (open, closed or merged), or nil. GET only. repo may be the tenants
// repository when tenants is set. With meta, an open or closed pull request is also checked to be this session's own (its head
// commit's message is the exact trailer block the gateway writes for this metadata): a pull request from a branch of the same name
// that something else made is APPLY-BRANCH-FOREIGN, not an answer. A merged one is reported as it is.
func (g *GitHub) FindPRIn(ctx context.Context, repo, branch string, tenants bool, meta map[string]string) (map[string]any, error) {
	owner, name, err := g.SplitRepo(repo)
	if err != nil {
		return nil, err
	}
	if !ValidRef(branch) {
		return nil, deniedBranch(branch)
	}
	token, err := g.mintFor(ctx, owner, name, tenants)
	if err != nil {
		return nil, err
	}
	pr, err := g.prOf(ctx, token, owner, name, branch)
	if err != nil || pr == nil {
		return nil, err
	}
	if meta != nil && pr.State != "merged" {
		if err := g.ownedBy(ctx, token, owner, name, pr.Commit, meta, branch); err != nil {
			return nil, err
		}
	}
	return pr.result([2]any{"state", pr.State}), nil
}

// PRPaths is pr_paths: the paths a pull request carries (GET /pulls/{n}/files, the first 100), sorted, for a receipt that names
// only those.
func (g *GitHub) PRPaths(ctx context.Context, repo string, number int64, tenants bool) ([]string, error) {
	owner, name, err := g.SplitRepo(repo)
	if err != nil {
		return nil, err
	}
	if number < 1 {
		return nil, &core.Denied{Rule: "GH-PR", Message: "a pull request number is a positive whole number", Hint: "Name a pull request of the list."}
	}
	token, err := g.mintFor(ctx, owner, name, tenants)
	if err != nil {
		return nil, err
	}
	status, doc, err := g.gh(ctx, token, ghReq{ep: epListPullFiles, owner: owner, name: name, number: number})
	if err != nil {
		return nil, err
	}
	list, ok := doc.([]any)
	if status != 200 || !ok {
		return nil, ghErrorOf("pull files", status, doc, "message")
	}
	out := []string{}
	for _, x := range list {
		if len(out) >= maxPRPaths {
			break
		}
		if f, ok := x.(map[string]any); ok {
			if n, isStr := f["filename"].(string); isStr && utf8.ValidString(n) && len(n) <= 1024 {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// RepoReady is repo_ready: does the repository exist, onboarded, with its base branch? False while the broker still refuses it
// as no app's repo (a 403) or it has no default branch yet (a 404): a repository the platform has not made yet. GET only. The
// base is the repository's default branch.
func (g *GitHub) RepoReady(ctx context.Context, repo string, tenants bool) (bool, error) {
	owner, name, err := g.SplitRepo(repo)
	if err != nil {
		return false, err
	}
	token, err := g.mintFor(ctx, owner, name, tenants)
	if err != nil {
		var den *core.Denied
		if errorsAsDenied(err, &den) {
			return false, nil
		}
		return false, err
	}
	status, doc, err := g.gh(ctx, token, ghReq{ep: epGetRepo, owner: owner, name: name})
	if err != nil {
		return false, err
	}
	if status == 404 {
		return false, nil
	}
	if status != 200 {
		return false, ghErrorOf("repository", status, doc, "message")
	}
	base, _ := docMap(doc)["default_branch"].(string)
	if !ValidRef(base) {
		return false, badAnswer("repository")
	}
	status, ref, err := g.gh(ctx, token, ghReq{ep: epGetRef, owner: owner, name: name, branch: base})
	if err != nil {
		return false, err
	}
	if status == 404 {
		return false, nil
	}
	if status != 200 {
		return false, ghErrorOf("base "+base, status, ref, "message")
	}
	return true, nil
}

// OnboardingPRs is onboarding_prs: the open pull requests from Glidepath's onboarding resync branch on a repository ([] when the
// repository is not ready for the agents yet: one the broker refuses has none this tool could race). GET only.
func (g *GitHub) OnboardingPRs(ctx context.Context, repo string, tenants bool) ([]map[string]any, error) {
	owner, name, err := g.SplitRepo(repo)
	if err != nil {
		return nil, err
	}
	token, err := g.mintFor(ctx, owner, name, tenants)
	if err != nil {
		var den *core.Denied
		if errorsAsDenied(err, &den) {
			return []map[string]any{}, nil
		}
		return nil, err
	}
	status, doc, err := g.gh(ctx, token, ghReq{ep: epListPulls, owner: owner, name: name, branch: OnboardingBranch, state: "open", perPage: 10})
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return []map[string]any{}, nil
	}
	list, ok := doc.([]any)
	if status != 200 || !ok {
		return nil, &GitHubError{Msg: "pulls: " + itoa(status), Status: status}
	}
	out := []map[string]any{}
	for _, x := range list {
		p, ok := x.(map[string]any)
		if !ok || p["state"] != "open" || obj(p["head"])["ref"] != OnboardingBranch {
			continue
		}
		n, _ := asInt64(p["number"])
		u, _ := p["html_url"].(string)
		out = append(out, map[string]any{"pr": n, "url": clipRunes(u, 512)})
	}
	return out, nil
}

// ReadFileIn is read_file at the repository's default branch, for a repository that may be the tenants repository: what
// airframe.apply asks of a file it is about to write.
func (g *GitHub) ReadFileIn(ctx context.Context, repo, path string, tenants bool) (map[string]any, error) {
	owner, name, err := g.SplitRepo(repo)
	if err != nil {
		return nil, err
	}
	if _, _, err := (ghReq{ep: epGetContents, owner: owner, name: name, path: path, ref: "main"}).target(); err != nil {
		return nil, &core.Denied{Rule: "GH-PATH", Message: pyStrRepr(clipRunes(path, 100)) + " is not a path in the repository", Hint: "Pass a path such as docs/readme.md (no '..')."}
	}
	token, err := g.mintFor(ctx, owner, name, tenants)
	if err != nil {
		return nil, err
	}
	base, err := g.defaultBranch(ctx, token, owner, name)
	if err != nil {
		return nil, err
	}
	return g.readWith(ctx, token, owner, name, path, base)
}

// XRCommit is one xr.request: the request already built (airframe.BuildXR) and who asks.
type XRCommit struct {
	Built     *airframe.BuiltXR
	Facts     SessionFacts
	DenyPaths []string
}

var xrBranchUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// branchForXR is the branch of an XR request: the task (or the session), the XR's name and a part made from the session, so the
// same session's retry finds its own branch and another session cannot make this one's name first. Python: agent/<task>-xr-<name>.
func branchForXR(f SessionFacts, name string) string {
	id := f.Task
	if id == "" {
		id = f.ID
	}
	// letters, digits, `_` and `-` only: a task id with dots could make `..` or `.lock`, which no branch name may hold
	s := strings.Trim(xrBranchUnsafe.ReplaceAllString(id, "-"), "-")
	if r := []rune(s); len(r) > 40 {
		s = strings.Trim(string(r[:40]), "-")
	}
	if s == "" {
		s = "task"
	}
	h := sha256.Sum256([]byte(f.ID))
	return AgentBranchPrefix + s + "-xr-" + name + "-" + hex.EncodeToString(h[:])[:8]
}

// CommitXR is commit_xr: one new XR request file on the tenants repository, as a pull request. Everything about the file was
// decided before this (airframe.BuildXR); here: the app has a directory in the tenants repository, the file is new, and the
// pull request is opened once (a retry of the same session, even after a lost answer, finds its own pull request and returns it
// as resumed: never a second one).
func (g *GitHub) CommitXR(ctx context.Context, c XRCommit) (map[string]any, error) {
	b := c.Built
	if g.tenants == "" {
		return nil, &core.Denied{Rule: "XR-TENANTS-TOKEN", Message: "no tenants repository is configured", Hint: "xr.request needs AIRFRAME_TENANTS_REPO."}
	}
	in := OpenPRInput{Repo: g.tenants, Branch: branchForXR(c.Facts, b.Name), Title: "Request " + b.Kind + " " + b.Name + " for " + b.App + " (" + b.Env + ")",
		Files: []File{{Path: b.Path, Content: b.Text}}, Meta: c.Facts.Meta(), DenyPaths: c.DenyPaths, Tenants: true,
		Body: "An XR request from xr.request: `" + b.Kind + "` `" + b.Name + "` in `" + b.Namespace + "`, for " + b.App + "'s " + b.Env +
			" environment. A person reviews and merges it; Argo CD's " + b.App + "-xr-requests Application then applies it."}
	owner, name, entries, err := g.checkInput(&in)
	if err != nil {
		return nil, err
	}
	token, err := g.mintFor(ctx, owner, name, true)
	if err != nil {
		return nil, err
	}
	if err := g.resolveBase(ctx, token, owner, name, &in); err != nil {
		return nil, err
	}
	// The app is onboarded (it has a directory here) and the request is new (no file of that name yet, on the default branch).
	status, doc, err := g.gh(ctx, token, ghReq{ep: epGetContents, owner: owner, name: name, path: "tenants/" + b.App, ref: in.Base})
	if err != nil {
		return nil, err
	}
	if _, isDir := doc.([]any); status == 404 || (status == 200 && !isDir) {
		return nil, &core.Denied{Rule: "XR-APP-UNKNOWN", Message: b.App + " has no tenants/" + b.App + "/ in " + g.tenants,
			Hint: "Request XRs for an onboarded app (its tenants directory exists)."}
	} else if status != 200 {
		return nil, ghErrorOf("contents", status, doc, "message")
	}
	status, doc, err = g.gh(ctx, token, ghReq{ep: epGetContents, owner: owner, name: name, path: b.Path, ref: in.Base})
	if err != nil {
		return nil, err
	}
	if status == 200 {
		return nil, &core.Denied{Rule: "XR-EXISTS", Message: b.Path + " already exists",
			Hint: "xr.request only adds a new request; change an existing one with repo.pr.open for a person to review, or pick another name."}
	}
	if status != 404 {
		return nil, ghErrorOf("contents", status, doc, "message")
	}
	pr, err := g.ensureWith(ctx, token, owner, name, in, entries)
	if err != nil {
		return nil, err
	}
	delete(pr, "state")
	if r, _ := pr["resumed"].(bool); !r {
		delete(pr, "resumed")
	}
	pr["path"], pr["kind"], pr["name"], pr["namespace"] = b.Path, b.Kind, b.Name, b.Namespace
	return pr, nil
}

func errorsAsDenied(err error, d **core.Denied) bool { return errors.As(err, d) }
func itoa(n int) string                              { return strconv.Itoa(n) }
