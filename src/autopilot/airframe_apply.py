"""airframe.apply: the planner's change set, opened as pull requests by the agents' App, one per call, resumable.

airframe.plan says what an AppSpec means (airframe_plan.plan: a pure function). apply does it as far as an agent may:
it re-plans the spec itself (the agent never supplies files, paths or repos), then opens the plan's steps as PRs, in
order, as the agents' own GitHub App through github_pr.GitHubPRs. A person merges every one of them. The tool has no
merge, approve, push-to-existing-branch or workflow-dispatch call at all: the adapter it drives sends only GETs and
the POSTs that build a new branch and its PR (tests/test_airframe_apply.py reads every request a whole run makes).

What it opens, and what it leaves to people (the plan's own gates, airframe_plan.plan):

  tenants      the tenants repo: the app XR and every Flight environment's ApplicationEnvironment. Human merge.
  app-repo     the app's own repo: the Ground half of cicd.yaml (checked by glidepath-api as an agent's change, the rule
               set `glidepath.cicd.propose` uses, GP-SCOPE included) and one glidepath/envs/<env>.yaml per Ground
               environment. Auto-merge or human merge as the plan says; either way a person's click, not the tool's.
  cicd-flight  the Flight half of cicd.yaml (Flight environments, the release step): human-only fields. NOT opened.
  gitops       gitops-<app>, the upper environments' values: an upper repo, which a T1 tool does not write (T2 propose
               is repo.pr.open_upper's). NOT opened.
  repos-ready, tekton-resync, verify    waits and checks, not PRs: the platform does the first two, airframe.status,
               describe and verify do the third.

One PR per call, because one call is one Clearance decision and one receipt: the result carries the PR's repo, pr,
commit, branch and paths exactly as repo.pr.open's does (dispatch._effect), so Clearance's provenance vouches for each
PR as for any agent PR. A call opens the next step whose predecessors are merged; while a PR awaits its merge it
returns that PR again (`resumed`), and everything after it is `blocked`.

Resume is derived from GitHub, never from state kept here. Each step's branch is fixed by the plan
(agent/airframe-<app>-<step>), so a call that died after the commit and before the PR (or after the PR and before its
receipt was recorded) is finished by the next call: the PR the branch already has is returned, a branch without one gets
its PR once its files are checked to be the ones this plan writes (APPLY-BRANCH-DIVERGED otherwise), and a merged step
is skipped. A run killed at any point and started again with the same spec ends in the same PRs, no duplicates.
"""
from __future__ import annotations

import re
from typing import Any

import yaml

from .engine.dispatch import Denied

TENANTS = "tenants"
APP_REPO = "app-repo"
FLIGHT_HALF = {"cicd-flight": "the Flight half of cicd.yaml: Flight environments and the release step are human-only "
                               "fields (GP-SCOPE-003, GP-SCOPE-001)",
               "gitops": "gitops-<app> is an upper environment's repository: a person (or repo.pr.open_upper, T2, "
                         "propose only) writes it, once the tenants PR is merged and its values.yaml files exist"}
OPENED_BY_TOOL = (TENANTS, APP_REPO)


# Files Glidepath's onboarding scaffolds (glidepath/envs/<env>.yaml in the app repo: `envName: <env>` and a header
# comment; <cluster>/<env>/values.yaml in the gitops repo). apply merges the plan's keys into these instead of refusing them.
ONBOARDING_FILES = (re.compile(r"^glidepath/envs/[^/]+\.yaml$"), re.compile(r"^[^/]+(/[^/]+)?/values\.yaml$"))


def branch_for(app: str, step: str) -> str:
    return f"agent/airframe-{app}-{step}"


def _text(content: Any) -> str:
    return content if isinstance(content, str) else yaml.safe_dump(content, sort_keys=False)


def _header(text: str) -> str:
    """The leading comment block (and blank lines among it) of a YAML file, kept when the file is rewritten."""
    out = []
    for line in text.splitlines(keepends=True):
        if line.strip() == "" or line.lstrip().startswith("#"):
            out.append(line)
        else:
            break
    return "".join(out)


def _deep_merge(have: Any, want: Any, path: str, conflicts: list[str]) -> Any:
    """`have` with every key of `want` set. A key that `have` holds with a different value is a conflict (a person set
    it): named in `conflicts`, `have`'s value kept. Lists and scalars are compared whole, never merged."""
    if isinstance(have, dict) and isinstance(want, dict):
        out = dict(have)
        for k, v in want.items():
            out[k] = _deep_merge(have[k], v, f"{path}.{k}" if path else str(k), conflicts) if k in have else v
        return out
    if have != want:
        conflicts.append(path or "<root>")
    return have


def merge_onboarding_file(repo: str, path: str, existing: str, planned: str) -> str | None:
    """The merged text of an onboarding-scaffolded file with the plan's keys added, or None when it already holds them
    all (nothing to write). APPLY-CONFLICT when a key the plan sets has another value there; APPLY-EXISTS when the file
    is not a YAML mapping (it is not the scaffold, so a person looks)."""
    try:
        have, want = yaml.safe_load(existing), yaml.safe_load(planned)
    except yaml.YAMLError:
        have, want = [], None
    if have is None:
        have = {}                       # only a comment header
    if not isinstance(have, dict) or not isinstance(want, dict):
        raise Denied("APPLY-EXISTS", f"{repo} already has {path} with other content",
                     "apply merges into the file Glidepath's onboarding scaffolds; this one is not a YAML mapping. Change "
                     "it with repo.pr.open for a person to review (airframe.status shows where the app is).")
    conflicts: list[str] = []
    merged = _deep_merge(have, want, "", conflicts)
    if conflicts:
        raise Denied("APPLY-CONFLICT", f"{repo} {path} already sets {', '.join(sorted(conflicts))} to a different value than "
                     "the plan: a person set it", "Change the AppSpec to match the file, or have a person change the file "
                     "(repo.pr.open); apply never overwrites a value someone set.")
    if merged == have:
        return None
    return _header(existing) + yaml.safe_dump(merged, sort_keys=False)


def _cicd_files(glidepath: Any, session: Any, app: str, step: Any) -> tuple[list[dict], dict | None, str | None]:
    """The step's files, with cicd.yaml as glidepath-api resolves the plan's merge patch against the app's real file
    under the agent rules (the cicd.yaml glidepath.cicd.propose would write). (files, verdict, None), or ([], None, why)
    while glidepath-api does not know the app yet (not onboarded: a wait, not an error). Errors are refusals."""
    files: list[dict] = []
    verdict = None
    for f in step.files:
        if f.op == "merge-patch" and f.path == "cicd.yaml":
            if glidepath is None:
                raise Denied("APPLY-NO-GLIDEPATH", "airframe.apply cannot check cicd.yaml without glidepath-api",
                             "Ask a person to configure GLIDEPATH_API_URL.")
            verdict = glidepath.validate_cicd(app, {"patch": f.content, "agent": True}, session.principal)
            if isinstance(verdict, dict) and "error" in verdict:
                e = verdict["error"]
                if e.get("rule") == "GP-API-404":
                    return [], None, f"glidepath-api does not know {app} yet (its onboarding has not finished)"
                raise Denied(e.get("rule") or "GP-API-400", e.get("message") or "glidepath-api refused the cicd.yaml change",
                             e.get("hint", ""))
            errors = [p for p in verdict.get("problems") or [] if p.get("severity") == "error"]
            if errors:
                first = errors[0]
                raise Denied(first.get("rule") or "GP-SCOPE-001", "cicd.yaml: " + "; ".join(p.get("message", "") for p in errors),
                             first.get("hint") or "Fix the AppSpec; a field an agent may not change needs a person.")
            if verdict.get("changes"):
                files.append({"path": "cicd.yaml", "content": verdict["resulting"]})
        elif f.op == "create":
            files.append({"path": f.path, "content": _text(f.content)})
        else:
            raise Denied("APPLY-OP", f"{f.path}: the plan's {f.op} on this file is not one apply writes",
                         "Open this change as a person, or with repo.pr.open.")
    return files, verdict, None


def _body(step: Any, cs: Any, verdict: dict | None) -> str:
    lines = [f"### airframe.apply: step `{step.id}` of {cs.app}'s plan", "", f"Gate: **{step.gate}**. A person merges this; "
             "the tool that opened it has no way to.", ""]
    if step.effects:
        lines += ["Effects once merged:"] + [f"- {e}" for e in step.effects] + [""]
    if step.checks:
        lines += ["Checks:"] + [f"- {c}" for c in step.checks] + [""]
    if verdict and verdict.get("summary"):
        lines += [f"cicd.yaml, validated by glidepath-api as an agent's change: `{verdict['summary']}`", ""]
    if cs.assumptions:
        lines += ["Plan assumptions:"] + [f"- {a}" for a in cs.assumptions] + [""]
    if cs.warnings:
        lines += ["Plan warnings:"] + [f"- {w}" for w in cs.warnings] + [""]
    others = [s.id for s in cs.steps if s.id != step.id and s.repo]
    lines.append(f"The plan's other steps: {', '.join(others)}.")
    return "\n".join(lines)


def apply(git: Any, glidepath: Any, airframe: Any, session: Any, meta: dict[str, str], a: dict[str, Any]) -> dict[str, Any]:
    cs, answer = airframe.changeset(a["spec"], a.get("baseCicd"))
    if cs is None:
        return answer                   # an AF-SPEC-* refusal or needsInput: nothing was opened
    reg, app = airframe.registry, cs.app
    steps: list[dict[str, Any]] = []
    current: dict[str, Any] | None = None       # the PR this call returns: the first step not yet merged
    waiting: str | None = None
    human: list[dict[str, Any]] = []

    for st in cs.steps:
        # The plan names the tenants repo bare (the registry's tenantsRepo) and every other repo owner/name.
        repo = st.repo if "/" in st.repo or not st.repo else f"{reg.owner}/{st.repo}"
        row: dict[str, Any] = {"id": st.id, "title": st.title, "repo": repo, "gate": st.gate}
        steps.append(row)
        if st.id in FLIGHT_HALF:
            row["status"] = "human"
            row["paths"] = sorted(f.path for f in st.files)
            human.append({"id": st.id, "repo": repo, "paths": row["paths"], "reason": FLIGHT_HALF[st.id]})
            continue
        if st.id not in OPENED_BY_TOOL:
            row["status"] = "platform" if st.wait_for and st.gate != "verify" else "check"
            if st.wait_for:
                row["waitFor"] = st.wait_for
            if st.gate == "verify":
                row["checks"] = st.checks
            continue
        if current is not None or waiting is not None:
            row["status"] = "blocked"
            continue
        tenants = st.id == TENANTS
        branch = branch_for(app, st.id)
        try:
            found = git.find_pr(repo, branch, tenants=tenants)
        except Denied:
            found = None                # the repo is not an onboarded app's yet (the broker's 403): nothing exists
        if found and found["state"] == "merged":
            row.update(status="merged", pr=found["pr"], url=found["url"])
            continue
        if found and found["state"] == "closed":
            waiting = (f"PR #{found['pr']} for step {st.id} was closed without merging; apply never reopens or replaces a "
                       "PR a person closed. They decide: reopen it, or delete its branch, after which the next call opens a new PR")
            row.update(status="closed", pr=found["pr"], url=found["url"], reason=waiting)
            continue
        if found:
            row.update(status="open", pr=found["pr"], url=found["url"])
            # The receipt names what the PR carries, read from GitHub: a plan file that was a no-op (cicd.yaml already as
            # wanted) is not in the PR, and provenance would vouch for a path the PR never wrote.
            current = {**found, "resumed": True, "step": st.id, "repo": repo, "paths": git.pr_paths(repo, found["pr"], tenants=tenants)}
            continue
        if not tenants and not git.repo_ready(repo):
            waiting = f"{repo} does not exist yet or is not onboarded: the tenants PR is merged, the platform is still creating it"
            row.update(status="waiting", reason=waiting)
            continue
        if not tenants:
            # Onboarding is not finished while its resync PR is open (CicdOnboarded=True comes first): a file this call
            # opens now would race that PR and conflict with it.
            resync = [p for r in (repo, f"{repo.rsplit('/', 1)[0]}/gitops-{app}") for p in git.onboarding_prs(r)]
            if resync:
                waiting = ("the platform's onboarding resync PR is still open (" + ", ".join(p["url"] for p in resync) +
                           "): a person merges it, then the platform finishes; apply waits so its files do not race it")
                row.update(status="waiting", reason=waiting)
                continue
        files, verdict, why = _cicd_files(glidepath, session, app, st)
        if why:
            waiting = why
            row.update(status="waiting", reason=why)
            continue
        if not files:
            row.update(status="noop", reason="the repository already has everything this step writes")
            continue
        kept = []
        for f in files:
            have = {"found": False} if f["path"] == "cicd.yaml" else git.read_file(repo, f["path"], "main", tenants=tenants)
            if have.get("found", True) is False:
                kept.append(f)
            elif have.get("content") == f["content"]:
                continue
            elif any(r.match(f["path"]) for r in ONBOARDING_FILES) and isinstance(have.get("content"), str):
                merged = merge_onboarding_file(repo, f["path"], have["content"], f["content"])
                if merged is not None:
                    kept.append({"path": f["path"], "content": merged})
            else:
                raise Denied("APPLY-EXISTS", f"{repo} already has {f['path']} with other content",
                             "apply writes new files only; change an existing one with repo.pr.open for a person to "
                             "review (airframe.status shows where the app is).")
        files = kept
        if not files:
            row.update(status="noop", reason="the repository already has everything this step writes")
            continue
        pr = git.ensure_pr(repo, branch, st.title, files, "main", meta=meta, body=_body(st, cs, verdict), tenants=tenants)
        row.update(status="open", pr=pr["pr"], url=pr["url"])
        paths = git.pr_paths(repo, pr["pr"], tenants=tenants) if pr.get("resumed") else sorted(f["path"] for f in files)
        current = {**pr, "step": st.id, "repo": repo, "paths": paths}

    out: dict[str, Any] = {"app": app, "steps": steps, "humanSteps": human, "assumptions": cs.assumptions,
                           "warnings": cs.warnings}
    if current is not None:
        out.update(state="awaiting-merge", step=current["step"],
                   pr=current["pr"], url=current["url"], branch=current["branch"], commit=current["commit"],
                   repo=current["repo"], paths=current["paths"], resumed=bool(current.get("resumed")),
                   next="A person merges this PR; call airframe.apply again with the same spec to open the next step.")
    elif waiting is not None:
        out.update(state="waiting", reason=waiting, next="Wait for the platform, then call airframe.apply again with the same spec.")
    else:
        out.update(state="agent-steps-merged", next="Every step this tool opens is merged. The humanSteps are for a person; "
                   "airframe.status, describe and verify show the environments coming up.")
    return out
