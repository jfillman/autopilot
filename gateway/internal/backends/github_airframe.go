package backends

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
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

// maxPendingCandidates bounds the pull requests whose files are read to find a pending request for the same file.
const maxPendingCandidates = 5

// maxTenantsListing is how many entries of tenants/ one listing can be believed to be all of (GitHub lists at most 1,000).
const maxTenantsListing = 1000

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

// ReadFileAt is read_file at a ref the caller names (a commit it pinned with BaseSHAIn), without asking for the default branch: the
// per-call token and the one contents request.
func (g *GitHub) ReadFileAt(ctx context.Context, repo, path, ref string) (map[string]any, error) {
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
	token, err := g.mintFor(ctx, owner, name, false)
	if err != nil {
		return nil, err
	}
	return g.readWith(ctx, token, owner, name, path, ref)
}

// BaseSHAIn is base_sha: the default branch and the commit it is at now. A caller that reads files at it (ReadFileAt) and passes both as
// OpenPRInput.Base and BaseSHA builds its branch on exactly what it read, for the branch it read: if the default branch is another one
// by the time the PR is made, EnsurePR refuses (GH-BASE) instead of opening a PR against it for a branch built on the old one.
func (g *GitHub) BaseSHAIn(ctx context.Context, repo string) (base, sha string, err error) {
	owner, name, err := g.SplitRepo(repo)
	if err != nil {
		return "", "", err
	}
	token, err := g.mintFor(ctx, owner, name, false)
	if err != nil {
		return "", "", err
	}
	base, err = g.defaultBranch(ctx, token, owner, name)
	if err != nil {
		return "", "", err
	}
	status, ref, err := g.gh(ctx, token, ghReq{ep: epGetRef, owner: owner, name: name, branch: base})
	if err != nil {
		return "", "", err
	}
	if status != 200 {
		return "", "", ghErrorOf("base "+base, status, ref, "message")
	}
	sha, ok := shaAt(ref, "object", "sha")
	if !ok {
		return "", "", badAnswer("base " + base)
	}
	return base, sha, nil
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
func branchForXR(f SessionFacts, app, name string) string {
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
	return AgentBranchPrefix + s + "-xr-" + app + "--" + name + "-" + hex.EncodeToString(h[:])[:8]
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
	in := OpenPRInput{Repo: g.tenants, Branch: branchForXR(c.Facts, b.App, b.Name), Title: "Request " + b.Kind + " " + b.Name + " for " + b.App + " (" + b.Env + ")",
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
	// The apps that are onboarded are the directories of tenants/ (one bounded listing). The requesting app is one of them, and every
	// name the cloud holds for everyone in the request belongs to the app with the longest name that starts it. Past the bound, or on
	// any answer that is not the listing, nothing is decided: the request is refused.
	status, doc, err := g.gh(ctx, token, ghReq{ep: epGetContents, owner: owner, name: name, path: "tenants", ref: in.Base})
	if err != nil {
		return nil, err
	}
	list, isDir := doc.([]any)
	if status == 404 || (status == 200 && !isDir) {
		return nil, &core.Denied{Rule: "XR-APP-UNKNOWN", Message: b.App + " has no tenants/" + b.App + "/ in " + g.tenants,
			Hint: "Request XRs for an onboarded app (its tenants directory exists)."}
	} else if status != 200 {
		return nil, ghErrorOf("contents", status, doc, "message")
	}
	if len(list) >= maxTenantsListing {
		return nil, &core.Denied{Rule: "XR-APP-UNKNOWN", Message: "tenants/ lists " + itoa(len(list)) + " entries or more: the apps that own a name cannot be decided",
			Hint: "Ask a person."}
	}
	var onboarded []string
	for _, e := range list {
		if m, ok := e.(map[string]any); ok && m["type"] == "dir" {
			if n, ok := m["name"].(string); ok && repoSeg.MatchString(n) {
				onboarded = append(onboarded, n)
			}
		}
	}
	if !slices.Contains(onboarded, b.App) {
		return nil, &core.Denied{Rule: "XR-APP-UNKNOWN", Message: b.App + " has no tenants/" + b.App + "/ in " + g.tenants,
			Hint: "Request XRs for an onboarded app (its tenants directory exists)."}
	}
	if err := b.CheckOwners(onboarded); err != nil {
		return nil, err
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
	// A pull request of another session (or task) that already adds this file is a pending request, not this session's to resume:
	// the branches of an XR request end `-xr-<name>-<8 hex>`, so one page of the open pull requests says.
	status, doc, err = g.gh(ctx, token, ghReq{ep: epListOpenPulls, owner: owner, name: name})
	if err != nil {
		return nil, err
	}
	open, ok := doc.([]any)
	if status != 200 || !ok {
		return nil, ghErrorOf("pulls", status, doc, "message")
	}
	// The branch of an XR request names the app and the XR (`-xr-<app>--<name>-<8 hex>`), so the candidates are the open pull requests of
	// this app's request for this name; each counts only if it really adds the exact path (its changed files say so). Another app's
	// request of the same name is not a candidate and is never named.
	tail := regexp.MustCompile(`-xr-` + regexp.QuoteMeta(b.App) + `--` + regexp.QuoteMeta(b.Name) + `-[0-9a-f]{8}\z`)
	checked := 0
	for _, x := range open {
		pm, _ := x.(map[string]any)
		ref, _ := obj(pm["head"])["ref"].(string)
		if ref == "" || ref == in.Branch || !strings.HasPrefix(ref, AgentBranchPrefix) || !tail.MatchString(ref) {
			continue
		}
		n, ok := asInt64(pm["number"])
		if !ok || checked >= maxPendingCandidates {
			continue
		}
		checked++
		st, fdoc, ferr := g.gh(ctx, token, ghReq{ep: epListPullFiles, owner: owner, name: name, number: n})
		if ferr != nil {
			return nil, ferr
		}
		files, isList := fdoc.([]any)
		if st != 200 || !isList {
			return nil, ghErrorOf("pull files", st, fdoc, "message")
		}
		for _, f := range files {
			if fn, _ := obj(f)["filename"].(string); fn == b.Path {
				return nil, &core.Denied{Rule: "XR-PENDING", Message: "pull request #" + itoa(int(n)) + " already requests " + b.Path,
					Hint: "A person merges or closes that one; xr.request does not open a second pull request for the same file."}
			}
		}
	}
	// One page of open pull requests is all that is read: when it is full, a pending request may be beyond it. A second pull request
	// for the same new file is harmless (the second cannot merge cleanly), so the request goes on and the answer says it was not checked.
	truncated := len(open) >= 100
	pr, err := g.ensureWith(ctx, token, owner, name, in, entries)
	if err != nil {
		return nil, err
	}
	delete(pr, "state")
	if r, _ := pr["resumed"].(bool); !r {
		delete(pr, "resumed")
	}
	pr["path"], pr["kind"], pr["name"], pr["namespace"] = b.Path, b.Kind, b.Name, b.Namespace
	if truncated {
		pr["pendingCheck"] = "incomplete: the tenants repository has 100 or more open pull requests, so a pending request for this file may not have been seen"
	}
	return pr, nil
}

func errorsAsDenied(err error, d **core.Denied) bool { return errors.As(err, d) }
func itoa(n int) string                              { return strconv.Itoa(n) }
