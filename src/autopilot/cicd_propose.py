"""glidepath.cicd.validate and glidepath.cicd.propose: a cicd.yaml change, judged by glidepath-api before any PR.

validate is the read: POST /v1/apps/{app}/cicd/validations with the agent's merge patch and agent: true, the verdict
the cicd-validate pull-request check will give (glidepath docs/admin/api.md, "Validate a cicd.yaml change").

propose is the pull request. It asks the same question first, and what glidepath-api answers is the decision: a
problem of severity error is a refusal with that GP rule (GP-SCHEMA-*, GP-ENV-*, GP-PIPE-*, GP-SCOPE-*), the same id
the PR check would post, so the agent learns of it before anything exists on GitHub. A clean change becomes one
commit of cicd.yaml (the API's `resulting`, the file after the patch) on the app's own repo through the GitBackend,
as the agents' App, with the verdict and every field it touches in the PR body; a human merges it. The result names
the repo and the path, so Clearance's receipt (dispatch._effect) carries them and the agent gates vouch for the PR
like any repo.pr.open. Clearance holds no copy of the rules here: glidepath-api runs the one it shares with the check.
"""
from __future__ import annotations

import posixpath
import re
from typing import Any

from clearance.dispatch import Denied

from .github_pr import app_of

MAX_BODY_CHANGES = 30
_SAFE = re.compile(r"[^A-Za-z0-9._-]+")


def repo_is_apps(repo: str, app: str) -> bool:
    """The app's cicd.yaml lives in its own repo (<owner>/<app>), or in gitops-infra-<app> for an InfraService."""
    name = repo.rsplit("/", 1)[-1].lower()
    return name in (app.lower(), f"gitops-infra-{app.lower()}")


def branch_for(task: str | None, session: str) -> str:
    return "agent/cicd-" + _SAFE.sub("-", (task or session))[:40].strip("-")


def pr_body(verdict: dict[str, Any], why: str | None) -> str:
    """The PR description: the agent's reason, then what glidepath-api found and every field the change touches."""
    base = verdict.get("base") or {}
    lines = []
    if why:
        lines += [why.strip(), ""]
    lines += ["### cicd.yaml change, validated by glidepath-api", "",
              f"`{verdict.get('summary', '')}`", ""]
    changes = verdict.get("changes") or []
    if changes:
        lines += ["| field | change | before | after | scope |", "|---|---|---|---|---|"]
        for c in changes[:MAX_BODY_CHANGES]:
            lines.append(f"| `{c.get('pointer', '')}` | {c.get('change', '')} | {_cell(c, 'before')} | {_cell(c, 'after')} | {c.get('agentScope', '')} |")
        if len(changes) > MAX_BODY_CHANGES:
            lines.append(f"| … | and {len(changes) - MAX_BODY_CHANGES} more | | | |")
        lines.append("")
    warnings = [p for p in verdict.get("problems") or [] if p.get("severity") != "error"]
    if warnings:
        lines += ["Warnings (not blocking):"] + [f"- `{p.get('rule')}` {p.get('pointer')}: {p.get('message')}" for p in warnings] + [""]
    where = base.get("repo", "")
    if base.get("sha"):
        where += f"@{base['sha'][:7]}"
    lines.append(f"Validated as an agent's change against `{where}` (contract {verdict.get('contractVersion', '?')}); "
                 "the `cicd-validate` check re-runs the same rules on this PR.")
    return "\n".join(lines)


def _cell(c: dict[str, Any], k: str) -> str:
    if k not in c:
        return ""
    s = str(c[k]).replace("|", "\\|").replace("\n", " ")
    return f"`{s[:60]}`" if s else "`\"\"`"


def root_cicd_file(files: Any) -> dict[str, Any] | None:
    """The repo-root cicd.yaml among a PR's files, if any. A nested one (apps/x/cicd.yaml) is not the app's config
    glidepath-api reads; R021 still scopes it."""
    for f in files or []:
        if isinstance(f, dict) and isinstance(f.get("path"), str) and isinstance(f.get("content"), str):
            if posixpath.normpath(f["path"].strip("/")) == "cicd.yaml":
                return f
    return None


def check_files(glidepath: Any, principal: str, repo: str, files: Any, base: str = "main") -> Any:
    """repo.pr.open with the app's cicd.yaml among its files: glidepath-api's verdict first, the one propose gets, so
    every cicd.yaml that reaches GitHub through Clearance has had the schema and convention rules too (R021 ran the
    scope rules already). Only the repo-root file of the app's own repo (<app>, or gitops-infra-<app>) is the app's
    config; a gitops-<app> release repo's is not. An app glidepath-api doesn't know (404: not onboarded) has no
    cicd-validate check either, so its PR goes ahead on R021 alone. Returns the verdict, or None when nothing applied."""
    f = root_cicd_file(files)
    if f is None:
        return None
    app = app_of(repo.rsplit("/", 1)[-1])
    if not repo_is_apps(repo, app):
        return None
    verdict = glidepath.validate_cicd(app, {"content": f["content"], "agent": True, "baseRef": base or "main"}, principal)
    if isinstance(verdict, dict) and "error" in verdict:
        e = verdict["error"]
        if e.get("rule") == "GP-API-404":
            return None
        raise Denied(e.get("rule") or "GP-API-400", e.get("message") or "glidepath-api refused the cicd.yaml", e.get("hint", ""))
    errors = [p for p in verdict.get("problems") or [] if p.get("severity") == "error"]
    if errors:
        first = errors[0]
        raise Denied(first.get("rule") or "GP-SCHEMA-001", "cicd.yaml: " + "; ".join(p.get("message", "") for p in errors),
                     first.get("hint") or "Fix cicd.yaml (glidepath.cicd.validate shows the verdict before you open a PR).")
    return verdict


def guarded(inner: Any, glidepath: Any) -> Any:
    """A PR handler that asks glidepath-api about the app's cicd.yaml before opening the PR (check_files)."""
    def handler(session: Any, a: dict[str, Any]) -> Any:
        check_files(glidepath, session.principal, a["repo"], a.get("files"), a.get("base", "main"))
        return inner(session, a)
    return handler


def validate(glidepath: Any, principal: str, a: dict[str, Any]) -> Any:
    body: dict[str, Any] = {"patch": a["patch"], "agent": True}
    if a.get("baseRef"):
        body["baseRef"] = a["baseRef"]
    return glidepath.validate_cicd(a["app"], body, principal)


def propose(git: Any, glidepath: Any, session: Any, meta: dict[str, str], a: dict[str, Any]) -> dict[str, Any]:
    app, repo = a["app"], a["repo"]
    if not repo_is_apps(repo, app):
        raise Denied("GP-CICD-REPO", f"{repo} is not where {app}'s cicd.yaml lives",
                     f"Pass the app's own repository (<owner>/{app}), or <owner>/gitops-infra-{app} for an InfraService.")
    verdict = glidepath.validate_cicd(app, {"patch": a["patch"], "agent": True}, session.principal)
    if isinstance(verdict, dict) and "error" in verdict:
        # glidepath-api refused the request itself (400: the patch, the base file) or knows no such app (404).
        e = verdict["error"]
        raise Denied(e.get("rule") or "GP-API-400", e.get("message") or "glidepath-api refused the change", e.get("hint", ""))
    errors = [p for p in verdict.get("problems") or [] if p.get("severity") == "error"]
    if errors:
        first = errors[0]
        raise Denied(first.get("rule") or "GP-SCOPE-001", "; ".join(p.get("message", "") for p in errors),
                     first.get("hint") or "Fix the change; a field an agent may not change needs a person (human.request).")
    if not verdict.get("changes"):
        raise Denied("GP-CICD-NOOP", "the patch leaves cicd.yaml as it is", "Nothing to propose.")
    files = [{"path": "cicd.yaml", "content": verdict["resulting"]}]
    branch = a.get("branch") or branch_for(getattr(session, "task_id", None), session.id)
    if getattr(git, "takes_meta", False):
        pr = git.open_pr(repo, branch, a["title"].strip(), files, "main", meta=meta, body=pr_body(verdict, a.get("body")))
    else:
        pr = git.open_pr(repo, branch, a["title"].strip(), files, "main")
    return {**pr, "repo": repo, "paths": ["cicd.yaml"], "summary": verdict.get("summary"),
            "changes": verdict.get("changes"), "base": verdict.get("base")}
