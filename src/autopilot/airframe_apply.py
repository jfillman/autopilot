"""airframe.apply: the planner's change set, opened as pull requests by the agents' App, one per call, resumable.

airframe.plan says what an AppSpec means (airframe_plan.plan: a pure function). apply does it as far as an agent may:
it re-plans the spec itself (the agent never supplies files, paths or repos), then opens the plan's steps as PRs, in
order, as the agents' own GitHub App through github_pr.GitHubPRs. A person merges every one of them. The tool has no
merge, approve, push-to-existing-branch or workflow-dispatch call at all: the adapter it drives sends only GETs and
the POSTs that build a new branch and its PR (tests/test_airframe_apply.py reads every request a whole run makes).

The Flight half (cicd-flight, gitops) is Glidepath's `propose` class: an agent may propose it, only a person may merge it.
It is opened only when the call says `proposeFlight: true`, which the Clearance core allows only a definition that sets
profile.airframe.proposeFlight (R025, policy/hangar/bundle.json). Each such PR is marked for a person to merge: a
`Hangar-Merge: person` commit trailer and the PR body say so, the result has `merge: person` and `needsPerson: [paths]`
(which Clearance's receipt records, so `Clearance / agent-scope` is neutral "needs a person" until a person merges it and
fails if a bot or an agent does), and the step's status is `awaiting-merge`. Without proposeFlight the Flight half is only
reported (`humanSteps`), as before. cicd.yaml's change is still validated by glidepath-api as an agent's change: a field
that stays `human` (a legacy key to drop, a cloud block, a step's cluster) is refused there and the step is reported
for a person instead.

What it opens, and what it leaves to people (the plan's own gates, airframe_plan.plan):

  tenants      the tenants repo: the app XR and every Flight environment's ApplicationEnvironment. Human merge.
  app-repo     the app's own repo: the Ground half of cicd.yaml (checked by glidepath-api as an agent's change, the rule
               set `glidepath.cicd.propose` uses, GP-SCOPE included) and one glidepath/envs/<env>.yaml per Ground
               environment. Auto-merge or human merge as the plan says; either way a person's click, not the tool's.
  cicd-flight  the Flight half of cicd.yaml (Flight environments, the release step): Glidepath's `propose` class, opened
               with proposeFlight as a PR marked for a person to merge; else reported. A human-only field in it is not opened.
  gitops       gitops-<app>, the upper environments' values: the same, once each environment's values.yaml exists.
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

import hashlib
import json
import re
from typing import Any

import yaml

from . import airframe_plan
from .engine.dispatch import Denied

TENANTS = "tenants"
APP_REPO = "app-repo"
FLIGHT_HALF = {"cicd-flight": "the Flight half of cicd.yaml: Flight environments and the release step are proposals only a "
                               "person may merge; this call did not set proposeFlight (or the definition may not)",
               "gitops": "gitops-<app> is an upper environment's repository: its values are a proposal only a person may "
                         "merge; this call did not set proposeFlight (or the definition may not). Once the tenants PR is "
                         "merged and its values.yaml files exist"}
MERGE_PERSON = "person"
# A refusal glidepath-api gives a field that stays human: the Flight step is reported for a person instead of failing the call.
HUMAN_REFUSALS = ("GP-SCOPE-001", "GP-SCOPE-002", "GP-SCOPE-003")
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
        elif f.op == "merge-patch" and ONBOARDING_FILES[1].match(f.path):
            # The Flight values: merged into the file the composition bootstraps. Missing: a wait, not an error (a person
            # has not merged the tenants PR, or the platform is still bootstrapping the environment).
            files.append({"path": f.path, "content": _text(f.content), "mustExist": True})
        else:
            raise Denied("APPLY-OP", f"{f.path}: the plan's {f.op} on this file is not one apply writes",
                         "Open this change as a person, or with repo.pr.open.")
    return files, verdict, None


def _body(step: Any, cs: Any, verdict: dict | None, person: bool = False) -> str:
    lines = [f"### airframe.apply: step `{step.id}` of {cs.app}'s plan", "", f"Gate: **{step.gate}**. A person merges this; "
             "the tool that opened it has no way to.", ""]
    if person:
        lines += ["**Needs a person to merge.** This is a change only a person may land (Glidepath's `propose` class: Flight "
                  "environments, the release step, the Flight values). `Clearance / agent-scope` is neutral until a person "
                  "merges it, and fails if a bot or an agent does.", ""]
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
    # proposeFlight is the caller's ask; the Clearance core refuses it (R025) for a definition that does not set
    # profile.airframe.proposeFlight, before this runs. Only a literal true counts.
    propose_flight = a.get("proposeFlight") is True
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
        flight = st.id in FLIGHT_HALF
        if flight and not propose_flight:
            row["status"] = "human"
            row["paths"] = sorted(f.path for f in st.files)
            human.append({"id": st.id, "repo": repo, "paths": row["paths"], "reason": FLIGHT_HALF[st.id]})
            continue
        if st.id not in OPENED_BY_TOOL and not flight:
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
            row.update(status="awaiting-merge" if flight else "open", pr=found["pr"], url=found["url"])
            if flight:
                row["mergeBy"] = MERGE_PERSON
            # The receipt names what the PR carries, read from GitHub: a plan file that was a no-op (cicd.yaml already as
            # wanted) is not in the PR, and provenance would vouch for a path the PR never wrote.
            paths = git.pr_paths(repo, found["pr"], tenants=tenants)
            current = {**found, "resumed": True, "step": st.id, "repo": repo, "paths": paths}
            if flight:
                current.update(merge=MERGE_PERSON, needsPerson=paths)
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
        try:
            files, verdict, why = _cicd_files(glidepath, session, app, st)
        except Denied as e:
            if not (flight and e.rule in HUMAN_REFUSALS):
                raise
            # A field in the Flight half stays human (a pre-ADR key to drop, a cloud block): not this tool's to propose.
            row.update(status="human", paths=sorted(f.path for f in st.files), reason=f"{e.rule}: {e.message}")
            human.append({"id": st.id, "repo": repo, "paths": row["paths"], "reason": f"{e.rule}: {e.message}. {e.hint}".strip()})
            continue
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
                if f.get("mustExist"):
                    waiting = (f"{repo} does not have {f['path']} yet: the environment's values file is created when its tenants "
                               "PR is merged and the platform has bootstrapped it")
                    break
                kept.append({k: v for k, v in f.items() if k != "mustExist"})
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
        if waiting is not None:
            row.update(status="waiting", reason=waiting)
            continue
        files = kept
        if not files:
            row.update(status="noop", reason="the repository already has everything this step writes")
            continue
        # A Flight-half PR is marked for a person to merge: the trailer rides in the commit, the receipt in the result.
        person = flight or bool(verdict and verdict.get("needsPerson"))
        pr = git.ensure_pr(repo, branch, st.title, files, "main", meta={**meta, "merge": MERGE_PERSON} if person else meta,
                           body=_body(st, cs, verdict, person), tenants=tenants)
        row.update(status="awaiting-merge" if person else "open", pr=pr["pr"], url=pr["url"])
        if person:
            row["mergeBy"] = MERGE_PERSON
        paths = git.pr_paths(repo, pr["pr"], tenants=tenants) if pr.get("resumed") else sorted(f["path"] for f in files)
        current = {**pr, "step": st.id, "repo": repo, "paths": paths}
        if person:
            current.update(merge=MERGE_PERSON, needsPerson=paths)

    out: dict[str, Any] = {"app": app, "steps": steps, "humanSteps": human, "assumptions": cs.assumptions,
                           "warnings": cs.warnings}
    if current is not None:
        out.update(state="awaiting-merge", step=current["step"],
                   pr=current["pr"], url=current["url"], branch=current["branch"], commit=current["commit"],
                   repo=current["repo"], paths=current["paths"], resumed=bool(current.get("resumed")),
                   next="A person merges this PR; call airframe.apply again with the same spec to open the next step.")
        if current.get("merge") == MERGE_PERSON:
            out.update(merge=MERGE_PERSON, needsPerson=current["needsPerson"],
                       next="Only a person may merge this PR (it changes a Flight environment or its values): ask one with "
                            "human.request. Call airframe.apply again with the same spec after they merge it, to open the next step.")
    elif waiting is not None:
        out.update(state="waiting", reason=waiting, next="Wait for the platform, then call airframe.apply again with the same spec.")
    else:
        out.update(state="agent-steps-merged", next="Every step this tool opens is merged. The humanSteps (if any) are for a person; "
                   "airframe.status, describe and verify show the environments coming up.")
    return out


# -- airframe.configure: change an existing app's chart values -------------------------------------------------------------
#
# apply provisions an app once: its branches are fixed per step and a merged step is skipped, so it cannot carry a later
# change. configure is the other half: one validated change to the chart values of ONE environment that already exists, as
# one PR for a person to review. It never merges, never pushes to an existing branch, and writes exactly one file.
#
# Where the values are: a Ground (lower) environment's are in the app repo, in the glidepath/envs/ file whose `envName:`
# is the environment (the file's name is a path, not its identity). A pr-* preview reads glidepath/pr-env.yaml from its own
# PR's head, so a change for it belongs on that PR's branch: refused. agent-* environments are refused too (their values
# are not a file this tool can find). A Flight environment's values are gitops-<app>/<cluster>/<env>/values.yaml, a
# human-merge repository: refused, not proposed (apply's proposeFlight rule, R025, is about an AppSpec's plan and has no
# meaning for a patch to an existing file; a person changes those files).

MAX_ENV_FILES = 60
APP_NAME = re.compile(r"[a-z][a-z0-9-]{1,38}[a-z0-9]")
ENV_NAME = re.compile(r"[a-z0-9]([a-z0-9-]{0,40}[a-z0-9])?")


def configure_branch(app: str, env: str, values: Any, replace: bool) -> str:
    """The branch of one change: derived from what it says (the app, the environment, the canonical patch and whether it
    replaces), so a retry of the same call finds the PR the first one opened, and a different change gets its own."""
    doc = json.dumps({"env": env, "replace": replace, "values": values}, sort_keys=True, separators=(",", ":"))
    return f"agent/airframe-{app}-configure-{env}-{hashlib.sha256(doc.encode()).hexdigest()[:10]}"


def canonical(v: Any) -> Any:
    """The patch with every mapping's keys sorted, so the merged file and the branch do not depend on the order the caller
    wrote them in (the Go gateway reads arguments as sorted maps)."""
    if isinstance(v, dict):
        return {k: canonical(v[k]) for k in sorted(v)}
    if isinstance(v, list):
        return [canonical(x) for x in v]
    return v


def _named(items: Any) -> bool:
    """A list of mappings that each have a unique string `name`: an entry is identified by it (slos, components, env...)."""
    if not isinstance(items, list):
        return False
    names = [i.get("name") if isinstance(i, dict) else None for i in items]
    return all(isinstance(n, str) for n in names) and len(set(names)) == len(names)


def _merge_values(have: Any, want: Any, path: str, conflicts: list[str], replaced: list[str], replace: bool) -> Any:
    """`have` with the patch `want` merged in.
      mappings   merged key by key: a key only the patch has is added, a key only the file has is kept
      lists      of named entries (every item a mapping with a unique string `name`; slos, components, env, ...): an entry
                 the file lacks is appended, an identical one is kept, one with the same `name` and other content is a
                 conflict (the whole entry is compared, never merged field by field)
      any other  list or scalar: the same value is kept; another value is a conflict
    A conflict is named in `conflicts` and the file's value kept, unless `replace`: then the patch's value (or entry)
    wins and its path is named in `replaced`."""
    if isinstance(have, dict) and isinstance(want, dict):
        out = dict(have)
        for k, v in want.items():
            sub = f"{path}.{k}" if path else str(k)
            out[k] = _merge_values(have[k], v, sub, conflicts, replaced, replace) if k in have else v
        return out
    if _named(have) and _named(want):
        out = list(have)
        index = {i["name"]: n for n, i in enumerate(out)}
        for item in want:
            sub = f"{path}[{item['name']}]"
            if item["name"] not in index:
                out.append(item)
            elif out[index[item["name"]]] != item:
                if replace:
                    out[index[item["name"]]] = item
                    replaced.append(sub)
                else:
                    conflicts.append(sub)
        return out
    if have != want:
        if replace:
            replaced.append(path or "<root>")
            return want
        conflicts.append(path or "<root>")
    return have


def _chart_errors(doc: dict, whole_file: bool = True) -> list[str]:
    """chartValues' findings. For a whole file only its chart keys are checked (envName and anything that is not a human-owned
    chart key, such as a release pipeline's keys, are not the patch's to check); a patch is checked as it is, so a key that
    is not a human-owned chart key (envName, release, anything unknown) is refused."""
    part = {k: v for k, v in doc.items() if k in airframe_plan.CHART_KEYS} if whole_file else doc
    errs = sorted(airframe_plan.CHART_VALUES.iter_errors(part), key=lambda x: list(x.absolute_path))
    return [f"{'/'.join(str(p) for p in e.absolute_path) or '<root>'}: {e.message}" for e in errs]


def _configure_refusal(env: str) -> Denied | None:
    if env.startswith("pr-"):
        return Denied("CONFIGURE-PR-ENV", f"{env} is a pull-request preview: its values are glidepath/pr-env.yaml, read from "
                      "its own pull request's head",
                      "Change glidepath/pr-env.yaml on that pull request's branch (a push by a person); once merged it reaches later "
                      "previews. A new file under glidepath/envs/ would create a separate static environment, not change a preview.")
    if env.startswith("agent-"):
        return Denied("CONFIGURE-AGENT-ENV", f"{env} is an agent's own environment: it is made per run and has no values file this "
                      "tool can find", "Configure a static lower environment (dev, staging...), or ask a person.")
    if env == "cicd" or env.endswith("-cicd"):
        return Denied("CONFIGURE-ENV", f"{env!r} is not an environment name: it would read as a cicd namespace",
                      "Name an environment of the app (airframe.status lists them).")
    return None


def _flight_hint(repo: str, env: str) -> str:
    app = repo.rsplit("/", 1)[-1]
    return (f"configure changes a static lower environment's glidepath/envs/ file. If {env} is a Flight environment, its values are "
            f"gitops-{app}/<cluster>/{env}/values.yaml, which only a person merges: ask one (human.request), or have them change it.")


def _find_env_file(git: Any, repo: str, env: str) -> tuple[str, str]:
    """(path, text) of the glidepath/envs file whose `envName` is `env`. The file name is only a path: every .yaml/.yml file of
    the directory is read (the conventional <env>.yaml first) and the one that says `envName: <env>` is the file."""
    listing = git.read_file(repo, airframe_plan.ENVS_DIR, "main")
    if listing.get("found", True) is False or listing.get("type") != "dir":
        raise Denied("CONFIGURE-NO-ENV", f"{repo} has no {airframe_plan.ENVS_DIR}/ directory: {env} is not a static lower environment of this app",
                     _flight_hint(repo, env))
    names = sorted((e["name"] for e in listing.get("entries") or []
                    if e.get("type") == "file" and isinstance(e.get("name"), str) and e["name"].endswith((".yaml", ".yml"))),
                   key=lambda n: (n not in (f"{env}.yaml", f"{env}.yml"), n))
    if len(names) > MAX_ENV_FILES:
        raise Denied("CONFIGURE-TOO-MANY", f"{repo} has more than {MAX_ENV_FILES} files in {airframe_plan.ENVS_DIR}/", "A person changes it.")
    found: list[tuple[str, str]] = []
    for n in names:
        f = git.read_file(repo, f"{airframe_plan.ENVS_DIR}/{n}", "main")
        text = f.get("content")
        if not isinstance(text, str):
            continue
        try:
            doc = yaml.safe_load(text)
        except yaml.YAMLError:
            continue
        if isinstance(doc, dict) and doc.get("envName") == env:
            found.append((f"{airframe_plan.ENVS_DIR}/{n}", text))
    if not found:
        raise Denied("CONFIGURE-NO-ENV", f"no file in {repo}'s {airframe_plan.ENVS_DIR}/ has `envName: {env}`", _flight_hint(repo, env))
    if len(found) > 1:
        raise Denied("CONFIGURE-AMBIGUOUS", f"{', '.join(p for p, _ in found)} all say `envName: {env}`",
                     "A person removes the duplicate; this tool will not pick one.")
    return found[0]


def configure_body(app: str, env: str, path: str, values: dict, replaced: list[str], person: bool, invalid_before: bool) -> str:
    lines = [f"### airframe.configure: {app}'s `{env}` environment", "",
             f"Changes the chart values in `{path}` (the file whose `envName` is `{env}`). A person reviews and merges this; "
             "the tool that opened it has no way to.", ""]
    if person:
        lines += ["**Needs a person to merge.** It sets a critical-risk chart key.", ""]
    lines += ["Keys set: " + ", ".join(f"`{k}`" for k in values), ""]
    if replaced:
        lines += ["Replaced (the caller asked to replace a different value that was there):"] + [f"- `{r}`" for r in replaced] + [""]
    lines += ["Validated against the AppSpec's `chartValues` schema, as the patch and as the whole file."]
    if invalid_before:
        lines.append("(The file already failed that schema before this change.)")
    return "\n".join(lines)


def configure(git: Any, airframe: Any, session: Any, meta: dict[str, str], a: dict[str, Any]) -> dict[str, Any]:
    """airframe.configure: see the section comment. One PR per call; the same call again returns that PR."""
    app, env, values, replace = a.get("app"), a.get("env"), a.get("values"), a.get("replace", False)
    if not isinstance(app, str) or not APP_NAME.fullmatch(app):
        raise Denied("CONFIGURE-APP", "app is not an application name", "Pass the app's name, as airframe.status lists it.")
    if not isinstance(env, str) or not ENV_NAME.fullmatch(env):
        raise Denied("CONFIGURE-ENV", "env is not an environment name", "Pass the environment's name (dev, staging...).")
    refusal = _configure_refusal(env)
    if refusal is not None:
        raise refusal
    if not isinstance(values, dict) or not values:
        raise Denied("CONFIGURE-VALUES", "values must be a non-empty mapping of chart values",
                     "Pass only what to change, e.g. {slos: [{name: ..., ...}]}; airframe.capabilities {section: appspec} lists the chart values.")
    if not isinstance(replace, bool):
        raise Denied("CONFIGURE-REPLACE", "replace must be true or false", "Leave it out to refuse a conflicting value.")
    values = canonical(values)
    errs = _chart_errors(values, whole_file=False)
    if errs:
        raise Denied("CONFIGURE-VALUES", f"values do not validate against the chart values schema: {errs[0]}",
                     "Fix the value; envName, release and platform-owned keys are not settable. airframe.capabilities {section: appspec} says how each is shaped.")
    repo = f"{airframe.registry.owner}/{app}"
    path, text = _find_env_file(git, repo, env)
    try:
        have = yaml.safe_load(text)
    except yaml.YAMLError:
        have = None
    if have is None:
        have = {}
    conflicts: list[str] = []
    replaced: list[str] = []
    merged = _merge_values(have, values, "", conflicts, replaced, replace)
    if conflicts:
        raise Denied("CONFIGURE-CONFLICT", f"{repo} {path} already sets {', '.join(sorted(conflicts))} to a different value than the patch",
                     "Name a new entry, change the patch to match, or pass replace: true to have the patch win (a person reviews the PR either way).")
    after = _chart_errors(merged)
    if after:
        raise Denied("CONFIGURE-MERGED", f"{path} would not validate against the chart values schema after this change: {after[0]}",
                     "Fix the patch, or have a person fix the file first.")
    out: dict[str, Any] = {"app": app, "env": env, "repo": repo, "path": path}
    if merged == have:
        return {**out, "state": "noop", "reason": f"{path} already holds these values", "paths": [],
                "next": "Nothing to change; the file already has them."}
    new_text = _header(text) + yaml.safe_dump(merged, sort_keys=False)
    branch = configure_branch(app, env, values, replace)
    person = any(airframe_plan.risk(k) == "critical" for k in values)
    title = f"{app} {env}: set {', '.join(list(values)[:4])}"
    pr = git.ensure_pr(repo, branch, title, [{"path": path, "content": new_text}], "main",
                       meta={**meta, "merge": MERGE_PERSON} if person else meta,
                       body=configure_body(app, env, path, values, sorted(replaced), person, bool(_chart_errors(have))))
    if pr.get("state") == "merged":
        raise Denied("CONFIGURE-SUPERSEDED", f"PR #{pr['pr']} with this exact change was merged, and {path} no longer holds it",
                     "Someone changed the file since. Change the patch, or ask a person.")
    out.update(pr=pr["pr"], url=pr["url"], branch=pr["branch"], commit=pr["commit"], resumed=bool(pr.get("resumed")))
    if pr.get("state") == "closed":
        return {**out, "state": "closed", "paths": [],
                "reason": f"PR #{pr['pr']} for this change was closed without merging; configure never reopens or replaces a PR a person "
                          "closed. They decide: reopen it, or delete its branch, after which the next call opens a new PR"}
    paths = git.pr_paths(repo, pr["pr"]) if pr.get("resumed") else [path]
    out.update(state="awaiting-merge", paths=paths,
               next="A person reviews and merges this PR; the environment picks the change up when it syncs.")
    if person:
        out.update(merge=MERGE_PERSON, needsPerson=paths)
    if replaced:
        out["replaced"] = sorted(replaced)
    return out
