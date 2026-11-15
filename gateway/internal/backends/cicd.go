package backends

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jfillman/autopilot/gateway/internal/core"
)

// glidepath.cicd.validate and glidepath.cicd.propose, and the check a pull request that carries the app's cicd.yaml gets
// (cicd_propose.py). validate is the read: the verdict the cicd-validate pull-request check will give a change. propose is
// the pull request. It asks the same question first, and what glidepath-api answers is the decision: a problem of severity
// error is a refusal with that GP rule, so the agent learns of it before anything exists on GitHub. A clean change becomes
// one commit of cicd.yaml (the API's `resulting`, the file after the patch) on the app's own repo, as the agents' App, with
// the verdict and every field it touches in the PR body; a person merges it.
//
// A change in Glidepath's `propose` class (a Flight environment added or changed, the release step into one, a new
// environment's cluster: the verdict's `needsPerson`) is valid, and is the one kind of change marked for a person to merge:
// the commit carries a `Hangar-Merge: person` trailer, the PR body says so, and the result has `merge: person` and
// `needsPerson: [cicd.yaml]`, which the receipt records. The mark is made here, from glidepath-api's verdict; the agent cannot
// ask for it or turn it off.

const (
	maxBodyChanges = 30
	mergePerson    = "person"
)

var branchUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// repoIsApps: the app's cicd.yaml lives in its own repo (<owner>/<app>), or in gitops-infra-<app> for an InfraService.
func repoIsApps(repo, app string) bool {
	name := strings.ToLower(repo[strings.LastIndex(repo, "/")+1:])
	return name == strings.ToLower(app) || name == "gitops-infra-"+strings.ToLower(app)
}

// branchFor is branch_for: the default branch of a cicd.yaml proposal, from the task (or the session).
func branchFor(task, session string) string {
	id := task
	if id == "" {
		id = session
	}
	s := branchUnsafe.ReplaceAllString(id, "-")
	if r := []rune(s); len(r) > 40 {
		s = string(r[:40])
	}
	// The name still depends only on the task and the session, so a retry of the same session finds its own branch; the session's
	// own part keeps another session from making this one's name first.
	h := sha256.Sum256([]byte(session))
	return "agent/cicd-" + strings.Trim(s, "-") + "-" + hex.EncodeToString(h[:])[:8]
}

func needsPersonOf(verdict map[string]any) []map[string]any {
	var out []map[string]any
	for _, x := range anyList(verdict["needsPerson"]) {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func fld(m map[string]any, k, def string) string {
	if v, ok := m[k]; ok {
		return pyStr(v)
	}
	return def
}

func cell(c map[string]any, k string) string {
	v, ok := c[k]
	if !ok {
		return ""
	}
	s := strings.ReplaceAll(strings.ReplaceAll(pyStr(v), "|", "\\|"), "\n", " ")
	s = strings.ReplaceAll(s, "`", "'")
	if s == "" {
		return "`\"\"`"
	}
	return "`" + clipRunes(s, 60) + "`"
}

// prBody is pr_body: the PR description, the agent's reason, then what glidepath-api found and every field the change
// touches.
func prBody(verdict map[string]any, why string) string {
	base := obj(verdict["base"])
	var lines []string
	if why != "" {
		lines = append(lines, pyStrip(why), "")
	}
	lines = append(lines, "### cicd.yaml change, validated by glidepath-api", "", "`"+fld(verdict, "summary", "")+"`", "")
	if needs := needsPersonOf(verdict); len(needs) > 0 {
		lines = append(lines, "### Needs a person to merge", "",
			"This change is in Glidepath's `propose` class: an agent may propose it, only a person may merge it. "+
				"`Clearance / agent-scope` stays neutral until one does, and fails if a bot or an agent merges it.", "")
		for i, n := range needs {
			if i >= maxBodyChanges {
				break
			}
			lines = append(lines, "- `"+fld(n, "pointer", "")+"`: "+fld(n, "message", ""))
		}
		lines = append(lines, "")
	}
	if changes := anyList(verdict["changes"]); len(changes) > 0 {
		lines = append(lines, "| field | change | before | after | scope |", "|---|---|---|---|---|")
		for i, x := range changes {
			if i >= maxBodyChanges {
				break
			}
			c := obj(x)
			lines = append(lines, fmt.Sprintf("| `%s` | %s | %s | %s | %s |", fld(c, "pointer", ""), fld(c, "change", ""), cell(c, "before"), cell(c, "after"), fld(c, "agentScope", "")))
		}
		if len(changes) > maxBodyChanges {
			lines = append(lines, fmt.Sprintf("| … | and %d more | | | |", len(changes)-maxBodyChanges))
		}
		lines = append(lines, "")
	}
	var warnings []map[string]any
	for _, x := range anyList(verdict["problems"]) {
		if p := obj(x); p["severity"] != "error" {
			warnings = append(warnings, p)
		}
	}
	if len(warnings) > 0 {
		lines = append(lines, "Warnings (not blocking):")
		for _, p := range warnings {
			lines = append(lines, fmt.Sprintf("- `%s` %s: %s", pyStr(p["rule"]), pyStr(p["pointer"]), pyStr(p["message"])))
		}
		lines = append(lines, "")
	}
	where := fld(base, "repo", "")
	if sha, ok := base["sha"]; pyTruthy(sha) && ok {
		where += "@" + clipRunes(pyStr(sha), 7)
	}
	lines = append(lines, fmt.Sprintf("Validated as an agent's change against `%s` (contract %s); the `cicd-validate` check re-runs the same rules on this PR.",
		where, fld(verdict, "contractVersion", "?")))
	return strings.Join(lines, "\n")
}

// rootCICDFile is root_cicd_file: the repo-root cicd.yaml among a PR's files, if any. A nested one (apps/x/cicd.yaml) is not
// the app's config glidepath-api reads.
func rootCICDFile(files any) (map[string]any, bool) {
	for _, x := range anyList(files) {
		f, ok := x.(map[string]any)
		if !ok {
			continue
		}
		p, pok := f["path"].(string)
		_, cok := f["content"].(string)
		if pok && cok && path.Clean(strings.Trim(p, "/")) == "cicd.yaml" {
			return f, true
		}
	}
	return nil, false
}

func firstError(verdict map[string]any) (map[string]any, []string) {
	var msgs []string
	var first map[string]any
	for _, x := range anyList(verdict["problems"]) {
		if p := obj(x); p["severity"] == "error" {
			if first == nil {
				first = p
			}
			msgs = append(msgs, text(p, "message"))
		}
	}
	return first, msgs
}

// cicdCheck is check_files as a check that runs once the pull request's base is known: repo.pr.open with the app's cicd.yaml
// among its files gets glidepath-api's verdict first, the one propose gets, so every cicd.yaml that reaches GitHub through the
// gateway has had the schema and convention rules too, asked about the base the pull request really has (the default branch).
// Only the repo-root file of the app's own repo (<app>, or gitops-infra-<app>) is the app's config; a gitops-<app> release repo's
// is not. An app glidepath-api does not know (404: not onboarded) has no cicd-validate check either, so its PR goes ahead. nil
// means nothing applies.
func cicdCheck(gp *Glidepath, principal string, repo any, files any) func(ctx context.Context, base string) error {
	f, ok := rootCICDFile(files)
	if !ok {
		return nil
	}
	rs, _ := repo.(string)
	app := AppOf(rs[strings.LastIndex(rs, "/")+1:])
	if !repoIsApps(rs, app) {
		return nil
	}
	return func(ctx context.Context, base string) error {
		verdict, err := gp.ValidateCICD(ctx, app, map[string]any{"content": f["content"], "agent": true, "baseRef": base}, principal)
		if err != nil {
			return err
		}
		vm, isMap := verdict.(map[string]any)
		if !isMap {
			// an answer that is not a verdict is no verdict: the pull request does not go ahead on it
			return &dispatchKind{"AttributeError", "the verdict is not an object"}
		}
		if e, isErr := vm["error"]; isErr {
			em := obj(e)
			if em["rule"] == "GP-API-404" {
				return nil
			}
			return &core.Denied{Rule: or(text(em, "rule"), "GP-API-400"), Message: or(text(em, "message"), "glidepath-api refused the cicd.yaml"), Hint: text(em, "hint")}
		}
		if first, msgs := firstError(vm); first != nil {
			return &core.Denied{Rule: or(ruleOf(first), "GP-SCHEMA-001"), Message: "cicd.yaml: " + strings.Join(msgs, "; "),
				Hint: or(text(first, "hint"), "Fix cicd.yaml (glidepath.cicd.validate shows the verdict before you open a PR).")}
		}
		return nil
	}
}

// validateCICD is validate: the verdict glidepath-api gives a change.
func validateCICD(ctx context.Context, gp *Glidepath, principal string, a map[string]any) (any, error) {
	patch, ok := a["patch"].(map[string]any)
	if !ok {
		return nil, deniedArg("patch", a["patch"])
	}
	body := map[string]any{"patch": patch, "agent": true}
	if pyTruthy(a["baseRef"]) {
		ref, ok := a["baseRef"].(string)
		if !ok || !ValidRef(ref) {
			return nil, deniedArg("baseRef", a["baseRef"])
		}
		body["baseRef"] = ref
	}
	return gp.ValidateCICD(ctx, a["app"], body, principal)
}

// SessionFacts are what a handler reads of its session: who, for the pull request's trailer and body.
type SessionFacts struct {
	ID, Principal, Agent, Task, Run string
}

// Meta is pr_meta: what an agent PR carries about its origin (the agent, task, session and run that are set).
func (f SessionFacts) Meta() map[string]string {
	m := map[string]string{}
	for k, v := range map[string]string{"agent": f.Agent, "task": f.Task, "session": f.ID, "run": f.Run} {
		if v != "" {
			m[k] = v
		}
	}
	return m
}

// propose is glidepath.cicd.propose.
func proposeCICD(ctx context.Context, git *GitHub, gp *Glidepath, f SessionFacts, denyPaths []string, a map[string]any) (any, error) {
	app, _ := a["app"].(string)
	repo, _ := a["repo"].(string)
	if _, err := seg("app", a["app"]); err != nil {
		return nil, err
	}
	if _, ok := a["repo"].(string); !ok {
		return nil, deniedRepo(a["repo"])
	}
	if !repoIsApps(repo, app) {
		return nil, &core.Denied{Rule: "GP-CICD-REPO", Message: repo + " is not where " + app + "'s cicd.yaml lives",
			Hint: "Pass the app's own repository (<owner>/" + app + "), or <owner>/gitops-infra-" + app + " for an InfraService."}
	}
	patch, ok := a["patch"].(map[string]any)
	if !ok {
		return nil, deniedArg("patch", a["patch"])
	}
	title, ok := a["title"].(string)
	if !ok {
		return nil, &core.Denied{Rule: "GH-TITLE", Message: "a title is one line of 1 to 200 characters", Hint: "Give the pull request a short one-line title."}
	}
	why := ""
	if pyTruthy(a["body"]) {
		w, ok := a["body"].(string)
		if !ok || utf8.RuneCountInString(w) > 4000 {
			return nil, deniedArg("body", a["body"])
		}
		why = w
	}
	branch := ""
	if pyTruthy(a["branch"]) {
		b, ok := a["branch"].(string)
		if !ok {
			return nil, deniedBranch(a["branch"])
		}
		branch = b
	}
	verdict, err := gp.ValidateCICD(ctx, app, map[string]any{"patch": patch, "agent": true}, f.Principal)
	if err != nil {
		return nil, err
	}
	vm, isMap := verdict.(map[string]any)
	if !isMap {
		return nil, &dispatchKind{"AttributeError", "the verdict is not an object"}
	}
	if e, isErr := vm["error"]; isErr {
		// glidepath-api refused the request itself (400: the patch, the base file) or knows no such app (404).
		em := obj(e)
		return nil, &core.Denied{Rule: or(ruleOf(em), "GP-API-400"), Message: or(text(em, "message"), "glidepath-api refused the change"), Hint: text(em, "hint")}
	}
	if first, msgs := firstError(vm); first != nil {
		return nil, &core.Denied{Rule: or(ruleOf(first), "GP-SCOPE-001"), Message: strings.Join(msgs, "; "),
			Hint: or(text(first, "hint"), "Fix the change; a field an agent may not change needs a person (human.request).")}
	}
	if !pyTruthy(vm["changes"]) {
		return nil, &core.Denied{Rule: "GP-CICD-NOOP", Message: "the patch leaves cicd.yaml as it is", Hint: "Nothing to propose."}
	}
	resulting, ok := vm["resulting"].(string)
	if !ok {
		return nil, badAnswer("the verdict")
	}
	if branch == "" {
		branch = branchFor(f.Task, f.ID)
	}
	needs := needsPersonOf(vm)
	meta := f.Meta()
	if len(needs) > 0 {
		meta["merge"] = mergePerson
	}
	pr, err := git.OpenPR(ctx, OpenPRInput{Repo: repo, Branch: branch, Title: pyStrip(title), Base: "",
		Files: []File{{Path: "cicd.yaml", Content: resulting}}, Meta: meta, Body: prBody(vm, why), DenyPaths: denyPaths})
	if err != nil {
		return nil, err
	}
	out := pr
	out["repo"] = repo
	out["paths"] = []any{"cicd.yaml"}
	out["summary"] = vm["summary"]
	out["changes"] = vm["changes"]
	out["base"] = vm["base"]
	if len(needs) > 0 {
		fields := make([]any, len(needs))
		for i, n := range needs {
			fields[i] = n["pointer"]
		}
		out["merge"], out["needsPerson"], out["needsPersonFields"] = mergePerson, []any{"cicd.yaml"}, fields
	}
	return out, nil
}
