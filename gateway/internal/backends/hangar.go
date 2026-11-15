package backends

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jfillman/autopilot/gateway/internal/auth"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
	"github.com/jfillman/autopilot/gateway/internal/dispatch"
	"github.com/jfillman/autopilot/gateway/internal/session"
	"github.com/jfillman/autopilot/gateway/internal/tools"
)

// The handlers of the Glidepath and repository tools (hangar_handlers in gateway.py, with the backends of glidepath.go,
// github.go and argosync.go). Each write handler runs behind scope: the gateway decides again, from the session's own
// grant and the agent's own definition, what the core decided before the call, so a call outside the grant touches nothing
// even if the core's decision were wrong or missing.

// UpperRepoPrefix is how Glidepath names a release repository (gitops-<app>): a tool for lower environments does not write
// to one. It is the naming the Hangar policy's CLEARANCE_ENV_UPPER_REPO_PREFIXES and app_of share.
const UpperRepoPrefix = "gitops-"

// DefinitionOf finds an agent's definition by name (the live set, else a candidate under Preflight): the scope of its calls.
type DefinitionOf func(agent string) *definitions.Definition

// scope is the gateway's own check of a call against the session's grant and the agent's definition.
type scope struct {
	def   DefinitionOf
	tools *tools.Registry
	apps  sync.Map // *Definition -> []string, profile.glidepath.apps
}

func refuse(rule, msg, hint string) error { return &core.Denied{Rule: rule, Message: msg, Hint: hint} }

// check is the part of every write's scope that does not depend on its arguments: the agent has a definition, the tool is in
// the session's grant and not above its tier ceiling (a propose-only tool is allowed to a ceiling of T1 and above).
func (sc *scope) check(tool string, s *session.Session) (*definitions.Definition, SessionFacts, error) {
	s.Lock()
	agent, granted, ceiling := s.Agent, slices.Contains(s.Limits.Tools, tool), s.Limits.TierCeiling
	f := SessionFacts{ID: s.ID, Principal: s.Principal, Agent: s.Agent, Task: s.TaskID}
	if s.Run != nil {
		f.Run = *s.Run
	}
	s.Unlock()
	d := sc.def(agent)
	if d == nil {
		return nil, f, refuse("R003", "unknown agent definition", "Spawn an agent that exists in the catalog.")
	}
	if !granted {
		return nil, f, refuse("R006", "tool is not permitted for this session", "Use a tool in your definition, or ask for the definition to be changed by a reviewed commit.")
	}
	spec, ok := sc.tools.Get(tool)
	if !ok {
		return nil, f, refuse("R006", "tool is not permitted for this session", "Use a tool in your definition, or ask for the definition to be changed by a reviewed commit.")
	}
	if len(ceiling) != 2 || ceiling[0] != 'T' || ceiling[1] < '0' || ceiling[1] > '3' {
		return nil, f, refuse("R007", "tool tier is above this session's ceiling", "Propose the change as a PR for a human to merge instead.")
	}
	if c := int(ceiling[1] - '0'); int(spec.Tier) > c && !(spec.ProposeOnly && c >= 1) {
		return nil, f, refuse("R007", "tool tier is above this session's ceiling", "Propose the change as a PR for a human to merge instead.")
	}
	return d, f, nil
}

// repo is the scope of a call that names a repository, and the one canonical (lower case) spelling of it that every later
// step uses: the allowlist match (repos.allow, matched without case), the rule that a tool for lower environments does not
// write to a release repository, the app the broker is asked about, the URL and the receipt. GitHub resolves names without
// case, so a check made on any other spelling would not hold for the repository the request reaches.
func (sc *scope) repo(d *definitions.Definition, repo any, lowerOnly bool) (string, error) {
	r, ok := repo.(string)
	if !ok || strings.Count(r, "/") != 1 || strings.HasPrefix(r, "/") || strings.HasSuffix(r, "/") {
		return "", refuse("R014", "repository is not on this agent's allowlist", "Work only in repositories your definition names.")
	}
	canon, ok := CanonicalRepo(r)
	if !ok {
		return "", deniedRepo(repo)
	}
	lower := make([]string, len(d.ReposAllow))
	for i, g := range d.ReposAllow {
		lower[i] = strings.ToLower(g)
	}
	if !repoAllowed(canon, lower) {
		return "", refuse("R014", "repository is not on this agent's allowlist", "Work only in repositories your definition names.")
	}
	if lowerOnly && strings.HasPrefix(canon[strings.LastIndex(canon, "/")+1:], UpperRepoPrefix) {
		return "", refuse("R012", "this tool acts on lower environments only, but the target is an upper environment", "Use repo.pr.open_upper to propose the change; a human merge applies it.")
	}
	return canon, nil
}

// withRepo is the call's arguments with the repository in its canonical spelling.
func withRepo(a map[string]any, canon string) map[string]any {
	out := make(map[string]any, len(a))
	for k, v := range a {
		out[k] = v
	}
	out["repo"] = canon
	return out
}

// app is the scope of a glidepath tool: the app is one the definition names under profile.glidepath.apps (fnmatch globs).
func (sc *scope) app(d *definitions.Definition, app any) error {
	a, ok := app.(string)
	if !ok {
		return refuse("R022", "that app is not on this agent's Glidepath allowlist", "Read only the apps your definition names under profile.glidepath.apps.")
	}
	var list []string
	if v, ok := sc.apps.Load(d); ok {
		list = v.([]string)
	} else {
		var p struct {
			Glidepath struct {
				Apps []string `json:"apps"`
			} `json:"glidepath"`
		}
		_ = json.Unmarshal(d.RawProfile, &p)
		list = p.Glidepath.Apps
		sc.apps.Store(d, list)
	}
	for _, g := range list {
		if auth.FnmatchCase(a, g) {
			return nil
		}
	}
	return refuse("R022", "that app is not on this agent's Glidepath allowlist", "Read only the apps your definition names under profile.glidepath.apps.")
}

// HangarConfig is what the Glidepath and repository handlers are built from.
type HangarConfig struct {
	Glidepath  *Glidepath
	GitHub     *GitHub
	ArgoSync   *ArgoSync
	Definition DefinitionOf
	Tools      *tools.Registry
}

// RemoteWrites are the tools whose handler writes to another system: a call that finished its write is reported as it
// finished even when the call's own time ran out a moment later (dispatch.Gateway.KeepAnswer).
var RemoteWrites = []string{"repo.pr.open", "repo.pr.open_upper", "glidepath.cicd.propose", "glidepath.run.rerun", "glidepath.run.cancel",
	"glidepath.promote", "glidepath.rollback", "argo.sync.lower"}

// rerunKey is glidepath-api's Idempotency-Key for a session's re-run of one run.
func rerunKey(sessionID, app, pipelinerun string) string {
	h := sha256.Sum256([]byte(sessionID + "/" + app + "/" + pipelinerun))
	return "clearance." + hex.EncodeToString(h[:])[:32]
}

// releaseKey is the Idempotency-Key for a session's promote or rollback: the session, the app, the target environment and
// cluster, and what it promotes from (or rolls back to). Not dryRun, so a dry run names the run the real call would start;
// not a rollback's reason, so a retry that rewords it still replays.
func releaseKey(sessionID, op, app, env, cluster, source string) string {
	h := sha256.Sum256([]byte(sessionID + "/" + op + "/" + app + "/" + env + "/" + cluster + "/" + source))
	return "clearance." + hex.EncodeToString(h[:])[:32]
}

func str(a map[string]any, k string) string { s, _ := a[k].(string); return s }

// hangarHandlers builds the handlers whose backend is configured.
func hangarHandlers(c HangarConfig) map[string]dispatch.Handler {
	h := map[string]dispatch.Handler{}
	if c.Tools == nil {
		c.Tools = tools.Default()
	}
	sc := &scope{def: c.Definition, tools: c.Tools}
	if sc.def == nil {
		sc.def = func(string) *definitions.Definition { return nil } // no definitions: every call is refused
	}
	gp, git := c.Glidepath, c.GitHub

	// withApp is a glidepath tool: in grant, and the app on the agent's allowlist.
	gpTool := func(tool string, fn func(ctx context.Context, f SessionFacts, a map[string]any) (any, error)) dispatch.Handler {
		return func(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
			d, f, err := sc.check(tool, s)
			if err != nil {
				return nil, err
			}
			if err := sc.app(d, a["app"]); err != nil {
				return nil, err
			}
			return fn(ctx, f, a)
		}
	}
	if gp != nil {
		h["glidepath.runs.list"] = gpTool("glidepath.runs.list", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.Runs(ctx, a["app"], a["sinceHours"], a["limit"], f.Principal)
		})
		h["glidepath.run.get"] = gpTool("glidepath.run.get", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.Run(ctx, a["app"], a["result"], f.Principal)
		})
		h["glidepath.logs.get"] = gpTool("glidepath.logs.get", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.Log(ctx, a["app"], a["result"], a["taskRun"], f.Principal)
		})
		h["glidepath.explain"] = gpTool("glidepath.explain", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.Explain(ctx, a["app"], a["result"], f.Principal)
		})
		h["glidepath.releases.get"] = gpTool("glidepath.releases.get", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.Releases(ctx, a["app"], a["chainId"], f.Principal)
		})
		h["glidepath.contract"] = func(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
			_, f, err := sc.check("glidepath.contract", s)
			if err != nil {
				return nil, err
			}
			return gp.Contract(ctx, f.Principal)
		}
		h["glidepath.live.list"] = gpTool("glidepath.live.list", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.Live(ctx, a["app"], f.Principal)
		})
		h["glidepath.live.watch"] = gpTool("glidepath.live.watch", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.Watch(ctx, a["app"], a["pipelinerun"], a["timeoutSeconds"], f.Principal)
		})
		h["glidepath.live.logs.get"] = gpTool("glidepath.live.logs.get", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.LiveLog(ctx, a["app"], a["taskRun"], a["container"], f.Principal)
		})
		h["glidepath.releaseRun.get"] = gpTool("glidepath.releaseRun.get", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.ReleaseRun(ctx, a["app"], a["name"], f.Principal)
		})
		h["glidepath.evidence.get"] = gpTool("glidepath.evidence.get", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.Evidence(ctx, a["app"], a["image"], f.Principal)
		})
		h["glidepath.tests.list"] = gpTool("glidepath.tests.list", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.Tests(ctx, a["app"], a["limit"], f.Principal)
		})
		h["glidepath.tests.report"] = gpTool("glidepath.tests.report", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.TestReport(ctx, a["app"], a["id"], f.Principal)
		})
		h["glidepath.events.recent"] = gpTool("glidepath.events.recent", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.Events(ctx, a["app"], a["type"], a["since"], a["limit"], f.Principal)
		})
		h["glidepath.previews.list"] = gpTool("glidepath.previews.list", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.Previews(ctx, a["app"], f.Principal)
		})
		h["glidepath.environments.list"] = gpTool("glidepath.environments.list", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.Environments(ctx, a["app"], f.Principal)
		})
		h["glidepath.environments.rollbackPlan"] = gpTool("glidepath.environments.rollbackPlan", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.RollbackPlan(ctx, a["app"], a["env"], a["cluster"], f.Principal)
		})
		h["glidepath.cicd.validate"] = gpTool("glidepath.cicd.validate", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return validateCICD(ctx, gp, f.Principal, a)
		})
		// The writes. The key is the session and the run (or the promotion), so an agent retrying the same call replays the
		// first answer instead of starting another.
		h["glidepath.run.rerun"] = gpTool("glidepath.run.rerun", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			app, pr := str(a, "app"), str(a, "pipelinerun")
			return gp.Rerun(ctx, a["app"], a["pipelinerun"], rerunKey(f.ID, app, pr), f.Principal)
		})
		h["glidepath.run.cancel"] = gpTool("glidepath.run.cancel", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			return gp.Cancel(ctx, a["app"], a["pipelinerun"], f.Principal)
		})
		h["glidepath.promote"] = gpTool("glidepath.promote", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			body, err := PromoteBody(a)
			if err != nil {
				return nil, err
			}
			key := releaseKey(f.ID, "promote", str(a, "app"), str(a, "env"), str(a, "cluster"), PromoteSource(a))
			return gp.Promote(ctx, a["app"], a["env"], body, key, dryRun(a), f.Principal)
		})
		h["glidepath.rollback"] = gpTool("glidepath.rollback", func(ctx context.Context, f SessionFacts, a map[string]any) (any, error) {
			body, err := RollbackBody(a)
			if err != nil {
				return nil, err
			}
			key := releaseKey(f.ID, "rollback", str(a, "app"), str(a, "env"), str(a, "cluster"), str(a, "targetReleaseId"))
			return gp.Rollback(ctx, a["app"], a["env"], body, key, dryRun(a), f.Principal)
		})
	}
	if git != nil {
		h["repo.file.get"] = func(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
			d, _, err := sc.check("repo.file.get", s)
			if err != nil {
				return nil, err
			}
			canon, err := sc.repo(d, a["repo"], false)
			if err != nil {
				return nil, err
			}
			path := ""
			if p, ok := a["path"]; ok && p != nil && p != "." {
				ps, isStr := p.(string)
				if !isStr {
					return nil, refuse("GH-PATH", echo(p)+" is not a path in the repository", "Pass a path such as docs/readme.md (no '..').")
				}
				path = ps
			}
			var ref any = "main"
			if r, ok := a["ref"]; ok {
				ref = r
			}
			rs, isStr := ref.(string)
			if !isStr {
				return nil, &core.Denied{Rule: "GH-REF", Message: echo(ref) + " is not a ref", Hint: "Pass a branch, tag or commit sha."}
			}
			return git.ReadFile(ctx, canon, path, rs)
		}
		open := func(tool string, lowerOnly bool) dispatch.Handler {
			return func(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
				d, f, err := sc.check(tool, s)
				if err != nil {
					return nil, err
				}
				canon, err := sc.repo(d, a["repo"], lowerOnly)
				if err != nil {
					return nil, err
				}
				a = withRepo(a, canon)
				in, err := openInput(a, f.Meta(), d.DenyPaths)
				if err != nil {
					return nil, err
				}
				// A pull request that carries the app's cicd.yaml gets glidepath-api's verdict first, whichever tool carries it, asked
				// about the base the pull request has once the gateway knows it.
				if gp != nil {
					in.Check = cicdCheck(gp, f.Principal, a["repo"], a["files"])
				}
				return git.OpenPR(ctx, in)
			}
		}
		h["repo.pr.open"] = open("repo.pr.open", true)
		h["repo.pr.open_upper"] = open("repo.pr.open_upper", false)
		if gp != nil {
			h["glidepath.cicd.propose"] = func(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
				d, f, err := sc.check("glidepath.cicd.propose", s)
				if err != nil {
					return nil, err
				}
				if err := sc.app(d, a["app"]); err != nil {
					return nil, err
				}
				canon, err := sc.repo(d, a["repo"], true)
				if err != nil {
					return nil, err
				}
				return proposeCICD(ctx, git, gp, f, d.DenyPaths, withRepo(a, canon))
			}
		}
	}
	if c.ArgoSync != nil {
		syncer := c.ArgoSync
		h["argo.sync.lower"] = func(ctx context.Context, s *session.Session, a map[string]any) (any, error) {
			d, _, err := sc.check("argo.sync.lower", s)
			if err != nil {
				return nil, err
			}
			appOK := func(app string) error { return sc.app(d, app) }
			s.Lock()
			who := SessionFacts{ID: s.ID, Principal: s.Principal, Agent: s.Agent, Task: s.TaskID}
			if s.Run != nil {
				who.Run = *s.Run
			}
			s.Unlock()
			m := who.Meta()
			m["principal"] = who.Principal
			return syncer.Sync(ctx, a["app"], a["project"], m, appOK)
		}
	}
	return h
}

func dryRun(a map[string]any) bool { b, _ := a["dryRun"].(bool); return b }

// baseOf is a.get("base"): absent means the repository's default branch (Python says "main"), which the backend resolves.
func baseOf(a map[string]any) any {
	if b, ok := a["base"]; ok {
		return b
	}
	return ""
}

// openInput reads repo.pr.open's arguments.
func openInput(a map[string]any, meta map[string]string, deny []string) (OpenPRInput, error) {
	in := OpenPRInput{Meta: meta, DenyPaths: deny}
	var ok bool
	if in.Repo, ok = a["repo"].(string); !ok {
		return in, deniedRepo(a["repo"])
	}
	base, isStr := baseOf(a).(string)
	if _, present := a["base"]; !isStr || (present && base == "") {
		return in, deniedBranch(baseOf(a))
	}
	in.Base = base
	if in.Branch, ok = a["branch"].(string); !ok {
		return in, deniedBranch(a["branch"])
	}
	if in.Title, ok = a["title"].(string); !ok {
		return in, refuse("GH-TITLE", "a title is one line of 1 to 200 characters", "Give the pull request a short one-line title.")
	}
	files, ok := a["files"].([]any)
	if !ok {
		return in, deniedFiles(fmt.Sprintf("a PR carries 1 to %d files", maxFiles), "Split the change.")
	}
	if len(files) > maxFiles {
		return in, deniedFiles(fmt.Sprintf("a PR carries 1 to %d files", maxFiles), "Split the change.")
	}
	for _, x := range files {
		f, ok := x.(map[string]any)
		p, pok := f["path"].(string)
		cnt, cok := f["content"].(string)
		if !ok || !pok || !cok {
			return in, deniedFiles("each file needs a path and text content under 1 MiB", "Fix the files list.")
		}
		in.Files = append(in.Files, File{Path: p, Content: cnt})
	}
	return in, nil
}
