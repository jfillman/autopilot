package backends

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/jfillman/autopilot/gateway/internal/airframe"
	"github.com/jfillman/autopilot/gateway/internal/core"
	"github.com/jfillman/autopilot/gateway/internal/definitions"
)

// airframe.configure (airframe_apply.py: configure and what it calls): one validated change to the chart values of ONE existing static
// lower environment, as one pull request for a person to review. apply provisions an app once, so a later change to it (an SLO, say) is
// this. Nothing is merged; the one file written is the glidepath/envs/ file whose `envName` is the environment.
//
// A pr-* preview (glidepath/pr-env.yaml, read from its own pull request's head), an agent-* environment and a Flight environment
// (gitops-<app>, a human-merge repository) are refused. The branch is derived from the change, with a part of the session added as for
// every branch this gateway makes (the same session's retry finds its own PR; another session cannot make this one's name first).

const maxEnvFiles = 60

// branchForConfigure is configure_branch with a part of the session added, as branchForStep does.
func branchForConfigure(app, env string, values any, replace bool, session string) string {
	h := sha256.Sum256([]byte(session))
	return airframe.ConfigureBranch(app, env, values, replace) + "-" + hex.EncodeToString(h[:])[:8]
}

func configureRefusal(env string) error {
	switch {
	case strings.HasPrefix(env, "pr-"):
		return &core.Denied{Rule: "CONFIGURE-PR-ENV", Message: env + " is a pull-request preview: its values are glidepath/pr-env.yaml, read from its own pull request's head",
			Hint: "Change glidepath/pr-env.yaml on that pull request's branch (a push by a person); once merged it reaches later previews. A new file under glidepath/envs/ would create a separate static environment, not change a preview."}
	case strings.HasPrefix(env, "agent-"):
		return &core.Denied{Rule: "CONFIGURE-AGENT-ENV", Message: env + " is an agent's own environment: it is made per run and has no values file this tool can find",
			Hint: "Configure a static lower environment (dev, staging...), or ask a person."}
	case env == "cicd" || strings.HasSuffix(env, "-cicd"):
		return &core.Denied{Rule: "CONFIGURE-ENV", Message: pyStrRepr(env) + " is not an environment name: it would read as a cicd namespace",
			Hint: "Name an environment of the app (airframe.status lists them)."}
	}
	return nil
}

func flightHint(repo, env string) string {
	app := repo[strings.LastIndex(repo, "/")+1:]
	return "configure changes a static lower environment's glidepath/envs/ file. If " + env + " is a Flight environment, its values are gitops-" + app +
		"/<cluster>/" + env + "/values.yaml, which only a person merges: ask one (human.request), or have them change it."
}

// findEnvFile is _find_env_file: (path, text) of the glidepath/envs file whose `envName` is env. The file name is only a path: every
// .yaml/.yml file of the directory is read (the conventional <env>.yaml first) and the one that says `envName: <env>` is the file.
func (ap *applier) findEnvFile(ctx context.Context, repo, env, ref string) (string, string, error) {
	listing, err := ap.git.ReadFileAt(ctx, repo, airframe.EnvsDir, ref)
	if err != nil {
		return "", "", err
	}
	if f, ok := listing["found"].(bool); (ok && !f) || listing["type"] != "dir" {
		return "", "", &core.Denied{Rule: "CONFIGURE-NO-ENV", Message: repo + " has no " + airframe.EnvsDir + "/ directory: " + env + " is not a static lower environment of this app",
			Hint: flightHint(repo, env)}
	}
	var names []string
	for _, e := range anyList(listing["entries"]) {
		em := obj(e)
		if n, ok := em["name"].(string); ok && em["type"] == "file" && (strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml")) {
			names = append(names, n)
		}
	}
	conventional := func(n string) bool { return n == env+".yaml" || n == env+".yml" }
	sort.SliceStable(names, func(i, j int) bool {
		if ci, cj := conventional(names[i]), conventional(names[j]); ci != cj {
			return ci
		}
		return names[i] < names[j]
	})
	if len(names) > maxEnvFiles {
		return "", "", &core.Denied{Rule: "CONFIGURE-TOO-MANY", Message: repo + " has more than 60 files in " + airframe.EnvsDir + "/", Hint: "A person changes it."}
	}
	var paths, texts []string
	for _, n := range names {
		f, err := ap.git.ReadFileAt(ctx, repo, airframe.EnvsDir+"/"+n, ref)
		if err != nil {
			return "", "", err
		}
		text, ok := f["content"].(string)
		if !ok {
			continue
		}
		doc, err := definitions.ParseYAML([]byte(text))
		if err != nil {
			continue
		}
		if name, ok := doc.M["envName"].(string); ok && name == env {
			paths, texts = append(paths, airframe.EnvsDir+"/"+n), append(texts, text)
		}
	}
	switch len(paths) {
	case 0:
		return "", "", &core.Denied{Rule: "CONFIGURE-NO-ENV", Message: "no file in " + repo + "'s " + airframe.EnvsDir + "/ has `envName: " + env + "`", Hint: flightHint(repo, env)}
	case 1:
		return paths[0], texts[0], nil
	}
	return "", "", &core.Denied{Rule: "CONFIGURE-AMBIGUOUS", Message: strings.Join(paths, ", ") + " all say `envName: " + env + "`",
		Hint: "A person removes the duplicate; this tool will not pick one."}
}

func configureBody(app, env, path string, values *definitions.OMap, replaced []string, person, invalidBefore bool) string {
	lines := []string{"### airframe.configure: " + app + "'s `" + env + "` environment", "",
		"Changes the chart values in `" + path + "` (the file whose `envName` is `" + env + "`). A person reviews and merges this; the tool that opened it has no way to.", ""}
	if person {
		lines = append(lines, "**Needs a person to merge.** It sets a critical-risk chart key.", "")
	}
	keys := make([]string, len(values.Keys))
	for i, k := range values.Keys {
		keys[i] = "`" + k + "`"
	}
	lines = append(lines, "Keys set: "+strings.Join(keys, ", "), "")
	if len(replaced) > 0 {
		lines = append(lines, "Replaced (the caller asked to replace a different value that was there):")
		for _, r := range replaced {
			lines = append(lines, "- `"+r+"`")
		}
		lines = append(lines, "")
	}
	lines = append(lines, "Validated against the AppSpec's `chartValues` schema, as the patch and as the whole file.")
	if invalidBefore {
		lines = append(lines, "(The file already failed that schema before this change.)")
	}
	return strings.Join(lines, "\n")
}

// configuredMerge is _merged: the env file at the commit sha and the patch merged into it.
func (ap *applier) configuredMerge(ctx context.Context, repo, env, sha string, values *definitions.OMap, replace bool) (path, text string, have, merged *definitions.OMap, conflicts, replaced []string, err error) {
	if path, text, err = ap.findEnvFile(ctx, repo, env, sha); err != nil {
		return
	}
	if have, err = definitions.ParseYAML([]byte(text)); err != nil {
		return
	}
	merged = airframe.MergeValues(have, values, "", &conflicts, &replaced, replace).(*definitions.OMap)
	return
}

// pathProtected is whether the agent's denyPaths (with the baseline) name path, or a directory or file it would take the place of:
// the check EnsurePR makes of every file, made by configure before anything is read for the write.
func pathProtected(n string, deny []string) bool {
	denied := pathDenied(n, deny) || pathDenied(n+"/x", deny) || ancestorOfDenied(n, deny)
	for _, a := range properAncestors(n) {
		denied = denied || pathDenied(a, deny)
	}
	return denied
}

// configure is configure: see the comment on this file. The PR of the change's branch is looked up first (an open one is returned, a
// closed one reported, whatever the base holds now; a merged one is a noop or CONFIGURE-SUPERSEDED); only a change with no PR reads the
// file, at one commit of the base, and builds its branch on exactly that commit.
func (ap *applier) configure(ctx context.Context, d *definitions.Definition, f SessionFacts, a map[string]any) (any, error) {
	app, appOK := a["app"].(string)
	if !appOK || !airframe.ConfigureAppName(app) {
		return nil, &core.Denied{Rule: "CONFIGURE-APP", Message: "app is not an application name", Hint: "Pass the app's name, as airframe.status lists it."}
	}
	env, envOK := a["env"].(string)
	if !envOK || !airframe.ConfigureEnvName(env) {
		return nil, &core.Denied{Rule: "CONFIGURE-ENV", Message: "env is not an environment name", Hint: "Pass the environment's name (dev, staging...)."}
	}
	if err := configureRefusal(env); err != nil {
		return nil, err
	}
	raw, err := airframe.FromArgs(a["values"])
	values, isMap := raw.(*definitions.OMap)
	if err != nil || !isMap || len(values.Keys) == 0 {
		return nil, &core.Denied{Rule: "CONFIGURE-VALUES", Message: "values must be a non-empty mapping of chart values",
			Hint: "Pass only what to change, e.g. {slos: [{name: ..., ...}]}; airframe.capabilities {section: appspec} lists the chart values."}
	}
	replace := false
	if r, has := a["replace"]; has {
		var isBool bool
		if replace, isBool = r.(bool); !isBool {
			return nil, &core.Denied{Rule: "CONFIGURE-REPLACE", Message: "replace must be true or false", Hint: "Leave it out to refuse a conflicting value."}
		}
	}
	if errs := airframe.ChartErrors(values, false); len(errs) > 0 {
		return nil, &core.Denied{Rule: "CONFIGURE-VALUES", Message: "values do not validate against the chart values schema: " + errs[0],
			Hint: "Fix the value; envName, release and platform-owned keys are not settable. airframe.capabilities {section: appspec} says how each is shaped."}
	}
	repo, err := ap.sc.repo(d, ap.reg.Owner+"/"+app, false)
	if err != nil {
		return nil, err
	}
	branch := branchForConfigure(app, env, values, replace, f.ID)
	person := false
	for _, k := range values.Keys {
		person = person || airframe.CriticalKey(k)
	}
	meta := f.Meta()
	if person {
		m := map[string]string{}
		for k, v := range meta {
			m[k] = v
		}
		m["merge"] = mergePersonVal
		meta = m
	}
	out := map[string]any{"app": app, "env": env, "repo": repo}
	noop := func(path string) (any, error) {
		out["path"], out["state"], out["reason"], out["paths"] = path, "noop", path+" already holds these values", []any{}
		out["next"] = "Nothing to change; the file already has them."
		return out, nil
	}
	superseded := func(n int64, path string) error {
		return &core.Denied{Rule: "CONFIGURE-SUPERSEDED", Message: "PR #" + itoa(int(n)) + " with this exact change was merged, and " + path + " no longer holds it",
			Hint: "Someone changed the file since. Change the patch, or ask a person."}
	}
	closed := func(n int64) any {
		out["state"], out["paths"] = "closed", []any{}
		out["reason"] = "PR #" + itoa(int(n)) + " for this change was closed without merging; configure never reopens or replaces a PR a person closed. They decide: reopen it, or delete its branch, after which the next call opens a new PR"
		return out
	}
	found, err := ap.git.FindPRIn(ctx, repo, branch, false, f.Meta())
	if err != nil {
		return nil, err
	}
	if found != nil {
		n, _ := found["pr"].(int64)
		if found["state"] == "merged" {
			sha, err := ap.git.BaseSHAIn(ctx, repo)
			if err != nil {
				return nil, err
			}
			path, _, have, merged, conflicts, _, err := ap.configuredMerge(ctx, repo, env, sha, values, replace)
			if err != nil {
				return nil, err
			}
			if len(conflicts) == 0 && airframe.PyEq(merged, have) {
				return noop(path)
			}
			return nil, superseded(n, path)
		}
		out["pr"], out["url"], out["branch"], out["commit"], out["resumed"] = found["pr"], found["url"], found["branch"], found["commit"], true
		if found["state"] == "closed" {
			return closed(n), nil
		}
		paths, err := ap.git.PRPaths(ctx, repo, n, false)
		if err != nil {
			return nil, err
		}
		if len(paths) == 1 {
			out["path"] = paths[0]
		}
		out["state"], out["paths"] = "awaiting-merge", strAny(paths)
		out["next"] = "A person reviews and merges this PR; the environment picks the change up when it syncs."
		if person {
			out["merge"], out["needsPerson"] = mergePersonVal, strAny(paths)
		}
		return out, nil
	}
	sha, err := ap.git.BaseSHAIn(ctx, repo)
	if err != nil {
		return nil, err
	}
	path, text, have, merged, conflicts, replaced, err := ap.configuredMerge(ctx, repo, env, sha, values, replace)
	if err != nil {
		return nil, err
	}
	out["path"] = path
	if len(conflicts) > 0 {
		return nil, &core.Denied{Rule: "CONFIGURE-CONFLICT", Message: repo + " " + path + " already sets " + strings.Join(airframe.SortedStrings(conflicts), ", ") + " to a different value than the patch",
			Hint: "Name a new entry, change the patch to match, or pass replace: true to have the patch win (a person reviews the PR either way)."}
	}
	if errs := airframe.ChartErrors(merged, true); len(errs) > 0 {
		return nil, &core.Denied{Rule: "CONFIGURE-MERGED", Message: path + " would not validate against the chart values schema after this change: " + errs[0],
			Hint: "Fix the patch, or have a person fix the file first."}
	}
	if airframe.PyEq(merged, have) {
		return noop(path)
	}
	if n, ok := normalizePath(path); !ok || pathProtected(n, d.DenyPaths) {
		return nil, &core.Denied{Rule: "GH-PATH", Message: pyStrRepr(clipRunes(path, 100)) + " is a protected path",
			Hint: "An agent does not change protected paths (CI, ownership, release files); ask a person."}
	}
	newText := header(text) + airframe.DumpYAML(merged)
	title := app + " " + env + ": set " + strings.Join(values.Keys[:min(4, len(values.Keys))], ", ")
	invalidBefore := len(airframe.ChartErrors(have, true)) > 0
	pr, err := ap.git.EnsurePR(ctx, OpenPRInput{Repo: repo, Branch: branch, Title: title, Files: []File{{Path: path, Content: newText}}, Base: "", Meta: meta,
		Body: configureBody(app, env, path, values, airframe.SortedStrings(replaced), person, invalidBefore), DenyPaths: d.DenyPaths, BaseSHA: sha})
	if err != nil {
		return nil, err
	}
	n, _ := pr["pr"].(int64)
	if pr["state"] == "merged" {
		return nil, superseded(n, path)
	}
	out["pr"], out["url"], out["branch"], out["commit"] = pr["pr"], pr["url"], pr["branch"], pr["commit"]
	resumed, _ := pr["resumed"].(bool)
	out["resumed"] = resumed
	if pr["state"] == "closed" {
		return closed(n), nil
	}
	paths := []string{path}
	if resumed {
		if paths, err = ap.git.PRPaths(ctx, repo, n, false); err != nil {
			return nil, err
		}
	}
	out["state"], out["paths"] = "awaiting-merge", strAny(paths)
	out["next"] = "A person reviews and merges this PR; the environment picks the change up when it syncs."
	if person {
		out["merge"], out["needsPerson"] = mergePersonVal, strAny(paths)
	}
	if len(replaced) > 0 {
		out["replaced"] = strAny(airframe.SortedStrings(replaced))
	}
	return out, nil
}
