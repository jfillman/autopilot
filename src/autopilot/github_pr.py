"""The real GitBackend for repo.pr.open: a PR opened as the agents' own GitHub App (hangar decision D4).

Clearance has already decided the call (tool policy, the definition's repos.allow, the path rules, the PR budget)
before this runs. Clearance holds no GitHub credential and no App key. Per PR it asks Glidepath's broker for a token
(`POST /agent-installation-token`, glidepath docs/admin/release.md "The agent installation token"), as its own
identity with a projected token for that endpoint's audience, re-read on every call. The broker checks again that the
repo is an onboarded app's own and down-scopes the token to that one repo: contents and pull_requests write, never
workflows. The token is used for this PR's requests only, then dropped: never cached, logged or returned.

The PR is built with the Git data API, so nothing is cloned and nothing is written to disk:
  GET  /repos/{o}/{r}/git/ref/heads/{base}       the base commit
  POST /repos/{o}/{r}/git/trees                   the files, on the base tree (contents inline)
  POST /repos/{o}/{r}/git/commits                 one commit, its message carrying the task and session
  POST /repos/{o}/{r}/git/refs                    refs/heads/{branch}; 422 (it exists) is refused, never forced
  POST /repos/{o}/{r}/pulls                       the PR, its body naming the agent, task, session and run
Every request is to api.github.com under /repos/{owner}/{repo}/, and nothing else is sent: no merge, no review,
no push to an existing branch.

repo.file.get (read_file) uses the same per-call token: GET /repos/{o}/{r}/contents/{path}?ref=.

xr.request (commit_xr, xr_request.py) is one new file in tenants/<app>/xr-requests/ of the dev cluster's tenants repo,
opened the same way with a broker token asked for <app> on the tenants repo. The broker must allow that repo for
agent tokens (Glidepath agentToken); until it does it answers 403 and the agent is told so (XR-TENANTS-TOKEN).
"""
from __future__ import annotations

import base64
import json
import re
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Callable

from .engine.dispatch import Denied

GITHUB_API = "https://api.github.com"
BROKER_URL = "http://cdevents-broker-auth.platform-system.svc:8080"
_SEG = re.compile(r"^[A-Za-z0-9._-]{1,100}$")
_BRANCH = re.compile(r"^[A-Za-z0-9._/-]{1,200}$")
MAX_FILES = 50
MAX_FILE_BYTES = 1 << 20


class GitHubError(Exception):
    """The broker or GitHub failed. Never shown to the agent (dispatch says "backend error")."""


Opener = Callable[[urllib.request.Request, float], tuple[int, bytes]]


def _urlopen(req: urllib.request.Request, timeout: float) -> tuple[int, bytes]:
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:  # noqa: S310 - the URLs are configuration
            return r.status, r.read(8 << 20)
    except urllib.error.HTTPError as e:
        return e.code, e.read(64 << 10)


def app_of(repo: str) -> str:
    """The app a repo belongs to by Glidepath's naming (<app>, gitops-<app>, gitops-infra-<app>); the broker checks
    the Repository CR."""
    for prefix in ("gitops-infra-", "gitops-"):
        if repo.startswith(prefix):
            return repo[len(prefix):]
    return repo


def _split(repo: str) -> tuple[str, str]:
    owner, _, name = repo.partition("/")
    if not _SEG.match(owner or "") or not _SEG.match(name or "") or name in (".", ".."):
        raise Denied("GH-REPO", f"{repo!r} is not <owner>/<repo>", "Name the repository as owner/name.")
    return owner, name


class GitHubPRs:
    takes_meta = True   # gateway.hangar_handlers passes pr_meta(session)

    def __init__(self, token_file: str | Path, *, broker_url: str = BROKER_URL, api: str = GITHUB_API,
                 timeout: float = 30.0, opener: Opener = _urlopen, registry: Any = None, contract: dict | None = None):
        self.token_file, self.broker, self.api = Path(token_file), broker_url.rstrip("/"), api.rstrip("/")
        self.timeout, self._open = timeout, opener
        # xr.request's: the dev cluster, its tenants repo and owner (airframe_plan.Registry), and Airframe's contract.
        self.registry, self.contract = registry, contract

    # -- the token ------------------------------------------------------------------------------------------------
    def _installation_token(self, owner: str, repo: str, app: str | None = None) -> str:
        body = json.dumps({"owner": owner, "repo": repo, "app": app or app_of(repo)}).encode()
        req = urllib.request.Request(self.broker + "/agent-installation-token", method="POST", data=body, headers={
            "Authorization": f"Bearer {self.token_file.read_text().strip()}", "Content-Type": "application/json"})
        try:
            status, out = self._open(req, self.timeout)
        except OSError as e:
            raise GitHubError(f"broker unreachable: {type(e).__name__}") from e
        doc = _json(out)
        if status == 200 and doc.get("token"):
            return doc["token"]
        if status == 403 and app is not None:
            raise Denied("XR-TENANTS-TOKEN", f"Glidepath's broker does not mint agent tokens for {owner}/{repo}",
                         "xr.request needs the broker to allow the tenants repo for the agents' App (Glidepath "
                         "agentToken) and the App installed there; ask a person, or open the XR file with repo.pr.open "
                         "on the app's own repo for them to move.")
        if status == 403:
            # The broker's second check: not an onboarded app's own repo. An answer the agent can act on.
            raise Denied("GH-REPO-NOT-APP", f"{owner}/{repo} is not an onboarded app's repo",
                         "Agents open PRs on an app's own repos only (<app>, gitops-<app>).")
        raise GitHubError(f"broker {status}: {str(doc.get('error', ''))[:200]}")

    def _gh(self, token: str, method: str, path: str, body: dict | None = None) -> tuple[int, dict]:
        req = urllib.request.Request(self.api + path, method=method,
                                     data=json.dumps(body).encode() if body is not None else None, headers={
                                         "Authorization": f"Bearer {token}", "Accept": "application/vnd.github+json",
                                         "X-GitHub-Api-Version": "2022-11-28", "Content-Type": "application/json"})
        try:
            status, out = self._open(req, self.timeout)
        except OSError as e:
            raise GitHubError(f"GitHub unreachable: {type(e).__name__}") from e
        return status, _json(out)

    # -- GitBackend -----------------------------------------------------------------------------------------------
    def open_pr(self, repo: str, branch: str, title: str, files: list[dict], base: str = "main",
                meta: dict[str, str] | None = None, body: str | None = None, *, token_app: str | None = None,
                token: str | None = None) -> dict[str, Any]:
        owner, name = _split(repo)
        for b in (branch, base):
            if not _BRANCH.match(b or "") or ".." in b or b.startswith("/") or b.endswith((".lock", "/")):
                raise Denied("GH-BRANCH", f"{b!r} is not a branch name", "Use a plain branch name like agent/t-1234.")
        if not files or len(files) > MAX_FILES:
            raise Denied("GH-FILES", f"a PR carries 1 to {MAX_FILES} files", "Split the change.")
        entries = []
        for f in files:
            path, content = f.get("path"), f.get("content")
            if not isinstance(path, str) or not isinstance(content, str) or len(content.encode()) > MAX_FILE_BYTES:
                raise Denied("GH-FILES", "each file needs a path and text content under 1 MiB", "Fix the files list.")
            entries.append({"path": path, "mode": "100644", "type": "blob", "content": content})
        meta = meta or {}
        token = token or self._installation_token(owner, name, token_app)
        root = f"/repos/{owner}/{name}"

        status, ref = self._gh(token, "GET", f"{root}/git/ref/heads/{base}")
        if status != 200:
            raise GitHubError(f"base {base}: {status}")
        base_sha = ref["object"]["sha"]
        status, commit = self._gh(token, "GET", f"{root}/git/commits/{base_sha}")
        if status != 200:
            raise GitHubError(f"base commit: {status}")
        status, tree = self._gh(token, "POST", f"{root}/git/trees", {"base_tree": commit["tree"]["sha"], "tree": entries})
        if status != 201:
            raise GitHubError(f"tree: {status}")
        trailer = "".join(f"\nHangar-{k.title()}: {v}" for k, v in sorted(meta.items()))
        status, new = self._gh(token, "POST", f"{root}/git/commits",
                               {"message": f"{title}\n{trailer}", "tree": tree["sha"], "parents": [base_sha]})
        if status != 201:
            raise GitHubError(f"commit: {status}")
        status, _ = self._gh(token, "POST", f"{root}/git/refs", {"ref": f"refs/heads/{branch}", "sha": new["sha"]})
        if status == 422:
            raise Denied("GH-BRANCH-EXISTS", f"branch {branch} already exists",
                         "Use a new branch name; Clearance never pushes to an existing branch.")
        if status != 201:
            raise GitHubError(f"ref: {status}")
        text = "Opened by a Hangar agent through Clearance.\n\n" + "\n".join(f"- {k}: `{v}`" for k, v in sorted(meta.items()))
        if body:
            # What the tool has to say about the change (glidepath.cicd.propose: the verdict and the fields it touches).
            text += "\n\n" + body[:60000]
        status, pr = self._gh(token, "POST", f"{root}/pulls", {"title": title, "head": branch, "base": base, "body": text})
        if status != 201:
            raise GitHubError(f"pull: {status}")
        return {"pr": pr.get("number"), "url": pr.get("html_url"), "branch": branch, "commit": new["sha"]}

    def read_file(self, repo: str, path: str, ref: str = "main") -> dict[str, Any]:
        """repo.file.get: one file's text, or a directory's entries, at a ref. The same per-call broker token (the
        broker's repo rule applies to reads too); GET only. A file over 1 MiB or a binary one is named, not returned."""
        owner, name = _split(repo)
        if not _BRANCH.match(ref or "") or ".." in ref:
            raise Denied("GH-REF", f"{ref!r} is not a ref", "Pass a branch, tag or commit sha.")
        token = self._installation_token(owner, name)
        quoted = urllib.parse.quote(path.strip("/"), safe="/")
        status, doc = self._gh_any(token, f"/repos/{owner}/{name}/contents/{quoted}?ref={urllib.parse.quote(ref, safe='')}")
        if status == 404:
            return {"path": path, "ref": ref, "found": False}
        if status != 200:
            raise GitHubError(f"contents: {status}")
        if isinstance(doc, list):
            return {"path": path, "ref": ref, "type": "dir",
                    "entries": [{"name": e.get("name"), "type": e.get("type"), "size": e.get("size")} for e in doc[:500]]}
        if doc.get("type") != "file":
            return {"path": path, "ref": ref, "type": doc.get("type")}
        size = int(doc.get("size") or 0)
        if size > MAX_FILE_BYTES or doc.get("encoding") != "base64":
            return {"path": path, "ref": ref, "type": "file", "size": size, "content": None,
                    "note": "too large to return (over 1 MiB)"}
        raw = base64.b64decode(doc.get("content") or "")
        try:
            text = raw.decode("utf-8")
        except UnicodeDecodeError:
            return {"path": path, "ref": ref, "type": "file", "size": size, "content": None, "note": "binary"}
        return {"path": path, "ref": ref, "type": "file", "size": size, "sha": doc.get("sha"), "content": text}

    def _gh_any(self, token: str, path: str) -> tuple[int, Any]:
        req = urllib.request.Request(self.api + path, method="GET", headers={
            "Authorization": f"Bearer {token}", "Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"})
        try:
            status, out = self._open(req, self.timeout)
        except OSError as e:
            raise GitHubError(f"GitHub unreachable: {type(e).__name__}") from e
        try:
            return status, json.loads(out) if out else {}
        except ValueError:
            return status, {}

    def commit_xr(self, app: str, env: str, kind: str, spec: dict[str, Any], name: str | None = None,
                  meta: dict[str, str] | None = None) -> dict[str, Any]:
        """xr.request: one new XR request file on the tenants repo, as a PR (xr_request.py builds the file)."""
        from . import xr_request
        if self.registry is None or self.contract is None:
            raise NotImplementedError("xr.request needs the dev cluster's registry and Airframe's contract")
        reg = self.registry
        path, text, xr = xr_request.build(app, env, kind, spec, name, reg, self.contract)
        owner, repo = reg.owner, reg.tenants_repo
        token = self._installation_token(owner, repo, app)
        for check, missing in ((f"tenants/{app}", "XR-APP-UNKNOWN"), (path, None)):
            status, doc = self._gh_any(token, f"/repos/{owner}/{repo}/contents/{urllib.parse.quote(check)}?ref=main")
            if status not in (200, 404):
                raise GitHubError(f"contents: {status}")
            if missing and status == 404:
                raise Denied(missing, f"{app} has no tenants/{app}/ in {owner}/{repo}",
                             "Request XRs for an onboarded app (its tenants directory exists).")
            if not missing and status == 200:
                raise Denied("XR-EXISTS", f"{path} already exists",
                             "xr.request only adds a new request; change an existing one with repo.pr.open for a "
                             "person to review, or pick another name.")
        task = (meta or {}).get("task") or "task"
        branch = f"agent/{task}-xr-{xr['metadata']['name']}"[:200]
        out = self.open_pr(f"{owner}/{repo}", branch, f"Request {kind} {xr['metadata']['name']} for {app} ({env})",
                           [{"path": path, "content": text}], "main", meta=meta, token=token,
                           body=f"An XR request from xr.request: `{kind}` `{xr['metadata']['name']}` in "
                                f"`app-{app}-cicd`, for {app}'s {env} environment. A person reviews and merges it; "
                                f"Argo CD's {app}-xr-requests Application then applies it.")
        return {**out, "path": path, "kind": kind, "name": xr["metadata"]["name"], "namespace": xr["metadata"]["namespace"]}


def _json(b: bytes) -> dict:
    try:
        d = json.loads(b) if b else {}
    except ValueError:
        return {}
    return d if isinstance(d, dict) else {}
