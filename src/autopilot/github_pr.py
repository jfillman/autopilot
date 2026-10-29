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

xr.request (commit_xr) has no adapter yet; serve.build_handlers drops it.
"""
from __future__ import annotations

import json
import re
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Callable

from clearance.dispatch import Denied

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
                 timeout: float = 30.0, opener: Opener = _urlopen):
        self.token_file, self.broker, self.api = Path(token_file), broker_url.rstrip("/"), api.rstrip("/")
        self.timeout, self._open = timeout, opener

    # -- the token ------------------------------------------------------------------------------------------------
    def _installation_token(self, owner: str, repo: str) -> str:
        body = json.dumps({"owner": owner, "repo": repo, "app": app_of(repo)}).encode()
        req = urllib.request.Request(self.broker + "/agent-installation-token", method="POST", data=body, headers={
            "Authorization": f"Bearer {self.token_file.read_text().strip()}", "Content-Type": "application/json"})
        try:
            status, out = self._open(req, self.timeout)
        except OSError as e:
            raise GitHubError(f"broker unreachable: {type(e).__name__}") from e
        doc = _json(out)
        if status == 200 and doc.get("token"):
            return doc["token"]
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
                meta: dict[str, str] | None = None) -> dict[str, Any]:
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
        token = self._installation_token(owner, name)
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
        body = "Opened by a Hangar agent through Clearance.\n\n" + "\n".join(f"- {k}: `{v}`" for k, v in sorted(meta.items()))
        status, pr = self._gh(token, "POST", f"{root}/pulls", {"title": title, "head": branch, "base": base, "body": body})
        if status != 201:
            raise GitHubError(f"pull: {status}")
        return {"pr": pr.get("number"), "url": pr.get("html_url"), "branch": branch, "commit": new["sha"]}

    def commit_xr(self, app: str, env: str, kind: str, spec: dict[str, Any]) -> dict[str, Any]:
        raise NotImplementedError("xr.request has no adapter")


def _json(b: bytes) -> dict:
    try:
        d = json.loads(b) if b else {}
    except ValueError:
        return {}
    return d if isinstance(d, dict) else {}
