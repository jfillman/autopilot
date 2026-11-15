package backends

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/airframe"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/session"
)

// The handlers of the Airframe tools (airframe_tools.py's AirframeTools, airframe_apply.py, xr_request.py), behind the same scope
// as every write: the agent has a definition, the tool is in its grant and under its tier ceiling, the app is on
// profile.airframe.apps, and (for what writes) the repositories are on repos.allow and the paths are not protected, all decided
// again here before any request exists.
//
//	airframe.capabilities, explain       the vendored contract: nothing is read from anywhere
//	airframe.plan                        the planner: a pure function of the spec
//	airframe.status, describe, verify    reads of Airframe XRs, Argo CD Applications and Rollouts (airframe.State)
//	airframe.apply                       the plan's next step as a pull request (apply.go: GETs and the pull request's own POSTs)
//	xr.request                           one XR request file as a pull request on the tenants repository (CommitXR)
//
// There is no airframe.validate handler: Python's gateway image carries no airframe-validate and serve.py drops the tool, so a
// call to it is refused as having no backend (as there).

// AirframeConfig is what the Airframe handlers are built from.
type AirframeConfig struct {
	// Enabled is AIRFRAME_ENABLED: the airframe.* tools have handlers (and airframe.apply, with the GitHub backend).
	Enabled bool
	// State reads cluster state for status, describe and verify. nil: those three have no handler.
	State *airframe.State
	// Registry is the fleet as the planner knows it: the dev cluster, the upper clusters, the tenants repository and its owner.
	Registry airframe.Registry
	// XRRequests turns xr.request on (XR_REQUEST_ENABLED); it needs the GitHub backend.
	XRRequests bool
}

func plain(v any) any { return definitions.Plain(v) }

func argErr(name string, v any) error {
	return &core.Denied{Rule: "AF-ARG", Message: name + " is not what this tool takes: " + echo(v), Hint: "See the tool's arguments (airframe.capabilities describes them)."}
}

// optStr is an optional string argument: absent or null is "", anything else that is not a string is refused.
func afOptStr(a map[string]any, k string) (string, error) {
	v, ok := a[k]
	if !ok || v == nil {
		return "", nil
	}
	s, isStr := v.(string)
	if !isStr {
		return "", argErr(k, v)
	}
	return s, nil
}

func afReqStr(a map[string]any, k string) (string, error) {
	s, isStr := a[k].(string)
	if !isStr {
		return "", argErr(k, a[k])
	}
	return s, nil
}

// specApp is the app an AppSpec names, when it names one: the planner will refuse a spec with no app, and nothing is written for it.
func specApp(spec any) (string, bool) {
	m, ok := spec.(map[string]any)
	if !ok {
		return "", false
	}
	a, ok := m["app"].(string)
	return a, ok
}

func (c *AirframeConfig) handlers(sc *scope, git *GitHub, gp *Glidepath) map[string]dispatch.Handler {
	h := map[string]dispatch.Handler{}
	reg := c.Registry
	if c.Enabled {
		c.airframeTools(h, sc, git, gp)
	}
	if git != nil && c.XRRequests {
		h["xr.request"] = func(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
			d, f, err := sc.check("xr.request", s)
			if err != nil {
				return nil, err
			}
			// The app is one the definition names, and the tenants repository (the one place the file goes) is on its repos.allow.
			if err := sc.airframeApp(d, a["app"]); err != nil {
				return nil, err
			}
			if _, err := sc.repo(d, reg.Owner+"/"+reg.TenantsRepo, false); err != nil {
				return nil, err
			}
			spec, err := airframe.FromArgs(a["spec"])
			if err != nil {
				return nil, xrSpecErr(err)
			}
			built, err := airframe.BuildXR(airframe.XRRequest{App: a["app"], Env: a["env"], Kind: a["kind"], Name: a["name"], Spec: spec}, reg)
			if err != nil {
				return nil, err
			}
			return git.CommitXR(ctx, XRCommit{Built: built, Facts: f, DenyPaths: d.DenyPaths})
		}
	}
	return h
}

func (c *AirframeConfig) airframeTools(h map[string]dispatch.Handler, sc *scope, git *GitHub, gp *Glidepath) {
	reg := c.Registry
	h["airframe.capabilities"] = func(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
		if _, _, err := sc.check("airframe.capabilities", s); err != nil {
			return nil, err
		}
		kind, err := afOptStr(a, "kind")
		if err != nil {
			return nil, err
		}
		section, err := afOptStr(a, "section")
		if err != nil {
			return nil, err
		}
		v, err := airframe.Capabilities(kind, section)
		return plain(v), err
	}
	h["airframe.explain"] = func(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
		if _, _, err := sc.check("airframe.explain", s); err != nil {
			return nil, err
		}
		if _, ok := a["rule"]; !ok {
			return nil, argErr("rule", nil)
		}
		v, err := airframe.Explain(a["rule"])
		return plain(v), err
	}
	h["airframe.plan"] = func(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
		d, _, err := sc.check("airframe.plan", s)
		if err != nil {
			return nil, err
		}
		if app, ok := specApp(a["spec"]); ok {
			if err := sc.airframeApp(d, app); err != nil {
				return nil, err
			}
		}
		spec, base, err := specArgs(a)
		if err != nil {
			return nil, err
		}
		return airframe.PlanAnswer(spec, base, reg), nil
	}
	if c.State != nil {
		st := c.State
		appTool := func(tool string, fn func(ctx context.Context, a map[string]any, app string) (any, error)) dispatch.Handler {
			return func(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
				d, _, err := sc.check(tool, s)
				if err != nil {
					return nil, err
				}
				if err := sc.airframeApp(d, a["app"]); err != nil {
					return nil, err
				}
				v, err := fn(ctx, a, a["app"].(string))
				return plain(v), err
			}
		}
		h["airframe.status"] = appTool("airframe.status", func(ctx context.Context, a map[string]any, app string) (any, error) {
			env, err := afOptStr(a, "env")
			if err != nil {
				return nil, err
			}
			if _, has := a["env"]; has && a["env"] != nil {
				return st.Status(ctx, app, &env)
			}
			return st.Status(ctx, app, nil)
		})
		h["airframe.describe"] = appTool("airframe.describe", func(ctx context.Context, a map[string]any, app string) (any, error) {
			env, err := afReqStr(a, "env")
			if err != nil {
				return nil, err
			}
			return st.Describe(ctx, app, env)
		})
		h["airframe.verify"] = appTool("airframe.verify", func(ctx context.Context, a map[string]any, app string) (any, error) {
			kind, err := afReqStr(a, "kind")
			if err != nil {
				return nil, err
			}
			name, err := afReqStr(a, "name")
			if err != nil {
				return nil, err
			}
			env, err := afOptStr(a, "env")
			if err != nil {
				return nil, err
			}
			if _, has := a["env"]; has && a["env"] != nil {
				return st.Verify(ctx, app, kind, name, &env)
			}
			return st.Verify(ctx, app, kind, name, nil)
		})
	}
	if git != nil {
		h["airframe.apply"] = func(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
			d, f, err := sc.check("airframe.apply", s)
			if err != nil {
				return nil, err
			}
			if app, ok := specApp(a["spec"]); ok {
				if err := sc.airframeApp(d, app); err != nil {
					return nil, err
				}
			}
			return (&applier{git: git, gp: gp, reg: reg, sc: sc}).apply(ctx, d, f, a)
		}
	}
}

func xrSpecErr(err error) error {
	return &core.Denied{Rule: "XR-SPEC", Message: "spec is not a document this tool reads: " + clipRunes(err.Error(), 100), Hint: "Pass only the fields under spec: (airframe.capabilities lists them for the kind)."}
}

// specArgs reads an AppSpec and the app's current cicd.yaml (a mapping or its text) from the arguments.
func specArgs(a map[string]any) (spec, base any, err error) {
	if spec, err = airframe.FromArgs(a["spec"]); err != nil {
		return nil, nil, &core.Denied{Rule: "AF-SPEC-001", Message: "spec is not a document this tool reads: " + clipRunes(err.Error(), 100), Hint: "Fix the AppSpec; airframe.capabilities {section: appspec} says how it is shaped."}
	}
	if b, ok := a["baseCicd"]; ok && b != nil {
		if base, err = airframe.FromArgs(b); err != nil {
			return nil, nil, &core.Denied{Rule: "AF-SPEC-002", Message: "baseCicd is not a document this tool reads: " + clipRunes(err.Error(), 100), Hint: "Pass the app's cicd.yaml as it is, or as a mapping."}
		}
	}
	return spec, base, nil
}

// -- airframe.apply ---------------------------------------------------------------------------------------------------------

const (
	stepTenants    = "tenants"
	stepAppRepo    = "app-repo"
	mergePersonVal = "person"
)

var flightHalf = map[string]string{
	"cicd-flight": "the Flight half of cicd.yaml: Flight environments and the release step are proposals only a person may merge; this call did not set proposeFlight (or the definition may not)",
	"gitops": "gitops-<app> is an upper environment's repository: its values are a proposal only a person may merge; this call did not set proposeFlight (or the definition may not). Once the tenants PR is " +
		"merged and its values.yaml files exist",
}

// humanRefusals are what glidepath-api says of a field that stays human: the Flight step is reported for a person instead of failing.
var humanRefusals = map[string]bool{"GP-SCOPE-001": true, "GP-SCOPE-002": true, "GP-SCOPE-003": true}

// onboardingFiles are the files Glidepath's onboarding scaffolds, which apply merges the plan's keys into.
var onboardingFiles = []*regexp.Regexp{regexp.MustCompile(`\Aglidepath/envs/[^/]+\.yaml\z`), regexp.MustCompile(`\A[^/]+(/[^/]+)?/values\.yaml\z`)}

type applier struct {
	git *GitHub
	gp  *Glidepath
	reg airframe.Registry
	sc  *scope
}

// branchForStep is branch_for: one branch per plan step, fixed by the plan and the session (Python: agent/airframe-<app>-<step>),
// so a call that died is resumed by the same session from what GitHub holds, and another session cannot make this one's name.
func branchForStep(app, step, session string) string {
	h := sha256.Sum256([]byte(session))
	return AgentBranchPrefix + "airframe-" + app + "-" + step + "-" + hex.EncodeToString(h[:])[:8]
}

// planFile is one file of a step as apply writes it: its text, the value it was made of (to merge it into a scaffold) and
// whether the file must already exist.
type planFile struct {
	path, content string
	value         any
	mustExist     bool
}

func (ap *applier) repoOf(st *airframe.Step) string {
	if strings.Contains(st.Repo, "/") || st.Repo == "" {
		return st.Repo
	}
	return ap.reg.Owner + "/" + st.Repo
}

func (ap *applier) apply(ctx context.Context, d *definitions.Definition, f SessionFacts, a map[string]any) (any, error) {
	// proposeFlight is the caller's ask. Only a literal true counts; the definition must set profile.airframe.proposeFlight.
	proposeFlight := a["proposeFlight"] == true
	if proposeFlight && !proposeFlightAllowed(d) {
		return nil, &core.Denied{Rule: "R025", Message: "this agent's definition may not open the Flight half of an AppSpec",
			Hint: "Call airframe.apply without proposeFlight: the Flight steps are reported in humanSteps for a person."}
	}
	specIn, base, err := specArgs(a)
	if err != nil {
		return nil, err
	}
	cs, answer := airframe.Changeset(specIn, base, ap.reg)
	if cs == nil {
		return plain(answer), nil // an AF-SPEC-* refusal or needsInput: nothing was opened
	}
	app := cs.App
	meta := f.Meta()
	// Every repository this call may touch is on the agent's repos.allow before the first request: the tenants repository, the
	// app's own and its gitops repository (whose onboarding pull requests are read).
	tenantsRepo := ap.reg.Owner + "/" + ap.reg.TenantsRepo
	repos := map[string]string{}
	for _, r := range []string{tenantsRepo, ap.reg.Owner + "/" + app, ap.reg.Owner + "/gitops-" + app} {
		canon, err := ap.sc.repo(d, r, false)
		if err != nil {
			return nil, err
		}
		repos[r] = canon
	}
	steps := []any{}
	var current map[string]any
	waiting := ""
	haveWaiting := false
	human := []any{}

	for _, st := range cs.Steps {
		repo := ap.repoOf(st)
		row := map[string]any{"id": st.ID, "title": st.Title, "repo": repo, "gate": st.Gate}
		steps = append(steps, row)
		flight := flightHalf[st.ID] != ""
		if flight && !proposeFlight {
			row["status"] = "human"
			row["paths"] = sortedPaths(st)
			human = append(human, map[string]any{"id": st.ID, "repo": repo, "paths": row["paths"], "reason": flightHalf[st.ID]})
			continue
		}
		if st.ID != stepTenants && st.ID != stepAppRepo && !flight {
			if st.WaitFor != nil && st.Gate != "verify" {
				row["status"] = "platform"
			} else {
				row["status"] = "check"
			}
			if st.WaitFor != nil {
				row["waitFor"] = *st.WaitFor
			}
			if st.Gate == "verify" {
				row["checks"] = strAny(st.Checks)
			}
			continue
		}
		if current != nil || haveWaiting {
			row["status"] = "blocked"
			continue
		}
		tenants := st.ID == stepTenants
		branch := branchForStep(app, st.ID, f.ID)
		found, err := ap.git.FindPRIn(ctx, repo, branch, tenants, meta)
		if err != nil {
			var den *core.Denied
			if !errors.As(err, &den) {
				return nil, err
			}
			found = nil // the repo is not an onboarded app's yet (the broker's 403): nothing exists
		}
		if found != nil && found["state"] == "merged" {
			row["status"], row["pr"], row["url"] = "merged", found["pr"], found["url"]
			continue
		}
		if found != nil && found["state"] == "closed" {
			waiting = fmt.Sprintf("PR #%v for step %s was closed without merging; apply never reopens or replaces a PR a person closed. They decide: reopen it, or delete its branch, after which the next call opens a new PR",
				found["pr"], st.ID)
			haveWaiting = true
			row["status"], row["pr"], row["url"], row["reason"] = "closed", found["pr"], found["url"], waiting
			continue
		}
		if found != nil {
			if flight {
				row["status"] = "awaiting-merge"
			} else {
				row["status"] = "open"
			}
			row["pr"], row["url"] = found["pr"], found["url"]
			if flight {
				row["mergeBy"] = mergePersonVal
			}
			// The receipt names what the PR carries, read from GitHub: a plan file that was a no-op is not in the PR.
			n, _ := found["pr"].(int64)
			paths, err := ap.git.PRPaths(ctx, repo, n, tenants)
			if err != nil {
				return nil, err
			}
			current = map[string]any{"pr": found["pr"], "url": found["url"], "branch": found["branch"], "commit": found["commit"], "state": found["state"],
				"resumed": true, "step": st.ID, "repo": repo, "paths": strAny(paths)}
			if flight {
				current["merge"], current["needsPerson"] = mergePersonVal, strAny(paths)
			}
			continue
		}
		if !tenants {
			ready, err := ap.git.RepoReady(ctx, repo, false)
			if err != nil {
				return nil, err
			}
			if !ready {
				waiting = repo + " does not exist yet or is not onboarded: the tenants PR is merged, the platform is still creating it"
				haveWaiting = true
				row["status"], row["reason"] = "waiting", waiting
				continue
			}
			// Onboarding is not finished while its resync PR is open: a file this call opens now would race that PR.
			var resync []map[string]any
			for _, r := range []string{repo, repo[:strings.LastIndex(repo, "/")] + "/gitops-" + app} {
				ps, err := ap.git.OnboardingPRs(ctx, r, false)
				if err != nil {
					return nil, err
				}
				resync = append(resync, ps...)
			}
			if len(resync) > 0 {
				urls := make([]string, len(resync))
				for i, p := range resync {
					urls[i], _ = p["url"].(string)
				}
				waiting = "the platform's onboarding resync PR is still open (" + strings.Join(urls, ", ") +
					"): a person merges it, then the platform finishes; apply waits so its files do not race it"
				haveWaiting = true
				row["status"], row["reason"] = "waiting", waiting
				continue
			}
		}
		files, verdict, why, err := ap.cicdFiles(ctx, f, app, st)
		if err != nil {
			var den *core.Denied
			if !(errors.As(err, &den) && flight && humanRefusals[den.Rule]) {
				return nil, err
			}
			// A field in the Flight half stays human (a pre-ADR key to drop, a cloud block): not this tool's to propose.
			row["status"], row["paths"], row["reason"] = "human", sortedPaths(st), den.Rule+": "+den.Message
			human = append(human, map[string]any{"id": st.ID, "repo": repo, "paths": row["paths"], "reason": strings.TrimSpace(den.Rule + ": " + den.Message + ". " + den.Hint)})
			continue
		}
		if why != "" {
			waiting, haveWaiting = why, true
			row["status"], row["reason"] = "waiting", why
			continue
		}
		if len(files) == 0 {
			row["status"], row["reason"] = "noop", "the repository already has everything this step writes"
			continue
		}
		kept := []File{}
		missing := ""
		for _, pf := range files {
			var have map[string]any
			if pf.path == "cicd.yaml" {
				have = map[string]any{"found": false}
			} else if have, err = ap.git.ReadFileIn(ctx, repo, pf.path, tenants); err != nil {
				return nil, err
			}
			if found, ok := have["found"].(bool); ok && !found {
				if pf.mustExist {
					missing = repo + " does not have " + pf.path + " yet: the environment's values file is created when its tenants PR is merged and the platform has bootstrapped it"
					break
				}
				kept = append(kept, File{Path: pf.path, Content: pf.content})
				continue
			}
			existing, isStr := have["content"].(string)
			switch {
			case isStr && sameFile(existing, pf):
				continue
			case isStr && matchesAny(onboardingFiles, pf.path):
				merged, err := mergeOnboardingFile(repo, pf.path, existing, pf)
				if err != nil {
					return nil, err
				}
				if merged != "" {
					kept = append(kept, File{Path: pf.path, Content: merged})
				}
			default:
				return nil, &core.Denied{Rule: "APPLY-EXISTS", Message: repo + " already has " + pf.path + " with other content",
					Hint: "apply writes new files only; change an existing one with repo.pr.open for a person to review (airframe.status shows where the app is)."}
			}
		}
		if missing != "" {
			waiting, haveWaiting = missing, true
			row["status"], row["reason"] = "waiting", missing
			continue
		}
		if len(kept) == 0 {
			row["status"], row["reason"] = "noop", "the repository already has everything this step writes"
			continue
		}
		// A Flight-half PR is marked for a person to merge: the trailer rides in the commit, the receipt in the result.
		person := flight || (verdict != nil && pyTruthy(verdict["needsPerson"]))
		m := meta
		if person {
			m = map[string]string{}
			for k, v := range meta {
				m[k] = v
			}
			m["merge"] = mergePersonVal
		}
		pr, err := ap.git.EnsurePR(ctx, OpenPRInput{Repo: repo, Branch: branch, Title: st.Title, Files: kept, Base: "", Meta: m,
			Body: applyBody(st, cs, verdict, person), DenyPaths: d.DenyPaths, Tenants: tenants})
		if err != nil {
			return nil, err
		}
		if person {
			row["status"] = "awaiting-merge"
		} else {
			row["status"] = "open"
		}
		row["pr"], row["url"] = pr["pr"], pr["url"]
		if person {
			row["mergeBy"] = mergePersonVal
		}
		var paths []string
		if r, _ := pr["resumed"].(bool); r {
			n, _ := pr["pr"].(int64)
			if paths, err = ap.git.PRPaths(ctx, repo, n, tenants); err != nil {
				return nil, err
			}
		} else {
			for _, k := range kept {
				paths = append(paths, k.Path)
			}
			sortStrings(paths)
		}
		current = map[string]any{"pr": pr["pr"], "url": pr["url"], "branch": pr["branch"], "commit": pr["commit"], "state": pr["state"],
			"resumed": pr["resumed"], "step": st.ID, "repo": repo, "paths": strAny(paths)}
		if person {
			current["merge"], current["needsPerson"] = mergePersonVal, strAny(paths)
		}
	}

	out := map[string]any{"app": app, "steps": steps, "humanSteps": human, "assumptions": strAny(cs.Assumptions), "warnings": strAny(cs.Warnings)}
	switch {
	case current != nil:
		out["state"], out["step"] = "awaiting-merge", current["step"]
		out["pr"], out["url"], out["branch"], out["commit"] = current["pr"], current["url"], current["branch"], current["commit"]
		out["repo"], out["paths"] = current["repo"], current["paths"]
		out["resumed"] = current["resumed"] == true
		out["next"] = "A person merges this PR; call airframe.apply again with the same spec to open the next step."
		if current["merge"] == mergePersonVal {
			out["merge"], out["needsPerson"] = mergePersonVal, current["needsPerson"]
			out["next"] = "Only a person may merge this PR (it changes a Flight environment or its values): ask one with human.request. Call airframe.apply again with the same spec after they merge it, to open the next step."
		}
	case haveWaiting:
		out["state"], out["reason"] = "waiting", waiting
		out["next"] = "Wait for the platform, then call airframe.apply again with the same spec."
	default:
		out["state"] = "agent-steps-merged"
		out["next"] = "Every step this tool opens is merged. The humanSteps (if any) are for a person; airframe.status, describe and verify show the environments coming up."
	}
	return out, nil
}

func strAny(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

func sortStrings(l []string) {
	for i := 1; i < len(l); i++ {
		for j := i; j > 0 && l[j] < l[j-1]; j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
}

func sortedPaths(st *airframe.Step) []any {
	p := make([]string, len(st.Files))
	for i, f := range st.Files {
		p[i] = f.Path
	}
	sortStrings(p)
	return strAny(p)
}

func matchesAny(rs []*regexp.Regexp, s string) bool {
	for _, r := range rs {
		if r.MatchString(s) {
			return true
		}
	}
	return false
}

// -- the files of a step ----------------------------------------------------------------------------------------------------

func textOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return airframe.DumpYAML(v)
}

// cicdFiles is _cicd_files: the step's files, with cicd.yaml as glidepath-api resolves the plan's merge patch against the
// app's real file under the agent rules (the cicd.yaml glidepath.cicd.propose would write). (files, verdict, "", nil), or
// (nil, nil, why, nil) while glidepath-api does not know the app yet (not onboarded: a wait, not an error).
func (ap *applier) cicdFiles(ctx context.Context, f SessionFacts, app string, st *airframe.Step) ([]planFile, map[string]any, string, error) {
	var files []planFile
	var verdict map[string]any
	for _, fc := range st.Files {
		switch {
		case fc.Op == "merge-patch" && fc.Path == "cicd.yaml":
			if ap.gp == nil {
				return nil, nil, "", &core.Denied{Rule: "APPLY-NO-GLIDEPATH", Message: "airframe.apply cannot check cicd.yaml without glidepath-api", Hint: "Ask a person to configure GLIDEPATH_API_URL."}
			}
			v, err := ap.gp.ValidateCICD(ctx, app, map[string]any{"patch": plain(fc.Content), "agent": true}, f.Principal)
			if err != nil {
				return nil, nil, "", err
			}
			vm, ok := v.(map[string]any)
			if !ok {
				return nil, nil, "", &dispatchKind{"AttributeError", "the verdict is not an object"}
			}
			verdict = vm
			if e, isErr := vm["error"]; isErr {
				em := obj(e)
				if em["rule"] == "GP-API-404" {
					return nil, nil, "glidepath-api does not know " + app + " yet (its onboarding has not finished)", nil
				}
				return nil, nil, "", &core.Denied{Rule: or(text(em, "rule"), "GP-API-400"), Message: or(text(em, "message"), "glidepath-api refused the cicd.yaml change"), Hint: text(em, "hint")}
			}
			if first, msgs := firstError(vm); first != nil {
				return nil, nil, "", &core.Denied{Rule: or(ruleOf(first), "GP-SCOPE-001"), Message: "cicd.yaml: " + strings.Join(msgs, "; "),
					Hint: or(text(first, "hint"), "Fix the AppSpec; a field an agent may not change needs a person.")}
			}
			if pyTruthy(vm["changes"]) {
				res, ok := vm["resulting"].(string)
				if !ok {
					return nil, nil, "", badAnswer("the verdict")
				}
				files = append(files, planFile{path: "cicd.yaml", content: res})
			}
		case fc.Op == "create":
			files = append(files, planFile{path: fc.Path, content: textOf(fc.Content), value: fc.Content})
		case fc.Op == "merge-patch" && onboardingFiles[1].MatchString(fc.Path):
			// The Flight values: merged into the file the composition bootstraps. Missing: a wait, not an error.
			files = append(files, planFile{path: fc.Path, content: textOf(fc.Content), value: fc.Content, mustExist: true})
		default:
			return nil, nil, "", &core.Denied{Rule: "APPLY-OP", Message: fc.Path + ": the plan's " + fc.Op + " on this file is not one apply writes", Hint: "Open this change as a person, or with repo.pr.open."}
		}
	}
	return files, verdict, "", nil
}

// sameFile is whether the file in the repository is the one the plan writes: the same text, or the same YAML (a file written by
// another formatter, or by the Python gateway, holds the same values in another layout).
func sameFile(existing string, pf planFile) bool {
	if existing == pf.content {
		return true
	}
	if pf.value == nil {
		return false
	}
	have, err := definitions.ParseYAML([]byte(existing))
	if err != nil {
		return false
	}
	return airframe.PyEq(definitions.Plain(have), definitions.Plain(pf.value))
}

// header is _header: the leading comment block (and blank lines among it) of a YAML file, kept when the file is rewritten.
func header(text string) string {
	var out strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			out.WriteString(line)
		} else {
			break
		}
	}
	return out.String()
}

// mergeOnboardingFile is merge_onboarding_file: the merged text of an onboarding-scaffolded file with the plan's keys added, or
// "" when it already holds them all. APPLY-CONFLICT when a key the plan sets has another value there; APPLY-EXISTS when the file
// is not a YAML mapping the strict reader accepts (it is not the scaffold, so a person looks).
func mergeOnboardingFile(repo, path, existing string, pf planFile) (string, error) {
	notScaffold := &core.Denied{Rule: "APPLY-EXISTS", Message: repo + " already has " + path + " with other content",
		Hint: "apply merges into the file Glidepath's onboarding scaffolds; this one is not a YAML mapping. Change it with repo.pr.open for a person to review (airframe.status shows where the app is)."}
	var have *definitions.OMap
	if header(existing) == existing {
		have = &definitions.OMap{M: map[string]any{}} // only a comment header
	} else {
		m, err := definitions.ParseYAML([]byte(existing))
		if err != nil {
			return "", notScaffold
		}
		have = m
	}
	want, ok := pf.value.(*definitions.OMap)
	if !ok {
		return "", notScaffold
	}
	var conflicts []string
	merged := deepMerge(have, want, "", &conflicts).(*definitions.OMap)
	if len(conflicts) > 0 {
		sortStrings(conflicts)
		return "", &core.Denied{Rule: "APPLY-CONFLICT", Message: repo + " " + path + " already sets " + strings.Join(conflicts, ", ") + " to a different value than the plan: a person set it",
			Hint: "Change the AppSpec to match the file, or have a person change the file (repo.pr.open); apply never overwrites a value someone set."}
	}
	if airframe.PyEq(definitions.Plain(merged), definitions.Plain(have)) {
		return "", nil
	}
	return header(existing) + airframe.DumpYAML(merged), nil
}

// deepMerge is _deep_merge: `have` with every key of `want` set. A key `have` holds with a different value is a conflict (a
// person set it), `have`'s value kept. Lists and scalars are compared whole, never merged.
func deepMerge(have, want any, path string, conflicts *[]string) any {
	hm, hok := have.(*definitions.OMap)
	wm, wok := want.(*definitions.OMap)
	if hok && wok {
		out := &definitions.OMap{M: map[string]any{}}
		for _, k := range hm.Keys {
			out.Set(k, hm.M[k])
		}
		for _, k := range wm.Keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if hv, has := hm.M[k]; has {
				out.Set(k, deepMerge(hv, wm.M[k], p, conflicts))
			} else {
				out.Set(k, wm.M[k])
			}
		}
		return out
	}
	if !airframe.PyEq(definitions.Plain(have), definitions.Plain(want)) {
		if path == "" {
			path = "<root>"
		}
		*conflicts = append(*conflicts, path)
	}
	return have
}

// applyBody is _body: the pull request's description.
func applyBody(st *airframe.Step, cs *airframe.ChangeSet, verdict map[string]any, person bool) string {
	lines := []string{"### airframe.apply: step `" + st.ID + "` of " + cs.App + "'s plan", "",
		"Gate: **" + st.Gate + "**. A person merges this; the tool that opened it has no way to.", ""}
	if person {
		lines = append(lines, "**Needs a person to merge.** This is a change only a person may land (Glidepath's `propose` class: Flight environments, the release step, the Flight values). `Clearance / agent-scope` is neutral until a person merges it, and fails if a bot or an agent does.", "")
	}
	if len(st.Effects) > 0 {
		lines = append(lines, "Effects once merged:")
		for _, e := range st.Effects {
			lines = append(lines, "- "+e)
		}
		lines = append(lines, "")
	}
	if len(st.Checks) > 0 {
		lines = append(lines, "Checks:")
		for _, c := range st.Checks {
			lines = append(lines, "- "+c)
		}
		lines = append(lines, "")
	}
	if verdict != nil && pyTruthy(verdict["summary"]) {
		lines = append(lines, "cicd.yaml, validated by glidepath-api as an agent's change: `"+pyStr(verdict["summary"])+"`", "")
	}
	if len(cs.Assumptions) > 0 {
		lines = append(lines, "Plan assumptions:")
		for _, e := range cs.Assumptions {
			lines = append(lines, "- "+e)
		}
		lines = append(lines, "")
	}
	if len(cs.Warnings) > 0 {
		lines = append(lines, "Plan warnings:")
		for _, e := range cs.Warnings {
			lines = append(lines, "- "+e)
		}
		lines = append(lines, "")
	}
	var others []string
	for _, s := range cs.Steps {
		if s.ID != st.ID && s.Repo != "" {
			others = append(others, s.ID)
		}
	}
	lines = append(lines, "The plan's other steps: "+strings.Join(others, ", ")+".")
	return strings.Join(lines, "\n")
}
