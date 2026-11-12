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
opened the same way with a broker token asked for the tenants repo with no app (the broker refuses it with one,
glidepath#236). The broker must allow that repo for agent tokens (Glidepath agentToken); until it does it answers 403
and the agent is told so (XR-TENANTS-TOKEN).

ensure_pr, find_pr and repo_ready are airframe.apply's (airframe_apply.py): a PR per plan step on a branch whose name
is fixed by the plan, so a call that died halfway is resumed from what GitHub holds, not from anything kept here.
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


REASON_MAX = 300


class GitHubError(Exception):
    """The broker or GitHub failed. Its text (a status and the other side's own reason, never a token: both sides
    answer JSON with an error/message string, and the token only ever goes in an Authorization header) is what the
    agent is told after "backend error" (engine.dispatch reads agent_message) and what the evidence result carries,
    cut short."""

    def __init__(self, message: str, *, status: int | None = None, reason: str = ""):
        super().__init__(message)
        self.status, self.reason = status, reason

    @classmethod
    def of(cls, what: str, status: int, doc: Any = None, *, key: str = "message") -> "GitHubError":
        reason = ""
        if isinstance(doc, dict) and isinstance(doc.get(key), str):
            reason = " ".join(doc[key].split())[:REASON_MAX]
        return cls(f"{what} {status}" + (f": {reason}" if reason else ""), status=status, reason=reason)

    @property
    def agent_message(self) -> str:
        return str(self)[:REASON_MAX + 60]

    @property
    def evidence(self) -> str:
        """Short, for the outcome's result: the status and the start of the reason."""
        return str(self)[:160]


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
    def _installation_token(self, owner: str, repo: str, app: str | None = None, *, tenants: bool = False) -> str:
        # The tenants repo is no app's: the broker (glidepath#236) mints for it only when the request names no app,
        # and refuses it with one. Every other repo is asked for as an app's own.
        body = json.dumps({"owner": owner, "repo": repo} if tenants else
                          {"owner": owner, "repo": repo, "app": app or app_of(repo)}).encode()
        req = urllib.request.Request(self.broker + "/agent-installation-token", method="POST", data=body, headers={
            "Authorization": f"Bearer {self.token_file.read_text().strip()}", "Content-Type": "application/json"})
        try:
            status, out = self._open(req, self.timeout)
        except OSError as e:
            raise GitHubError(f"broker unreachable: {type(e).__name__}") from e
        doc = _json(out)
        if status == 200 and doc.get("token"):
            return doc["token"]
        if status == 403 and tenants:
            raise Denied("XR-TENANTS-TOKEN", f"Glidepath's broker does not mint agent tokens for {owner}/{repo}",
                         "xr.request and airframe.apply need the broker to allow the tenants repo for the agents' App "
                         "(Glidepath agentToken, AGENT_PIN_TENANTS_REPO) and the App installed there; ask a person, or open the XR file with repo.pr.open "
                         "on the app's own repo for them to move.")
        if status == 403:
            # The broker's second check: not an onboarded app's own repo. An answer the agent can act on.
            raise Denied("GH-REPO-NOT-APP", f"{owner}/{repo} is not an onboarded app's repo",
                         "Agents open PRs on an app's own repos only (<app>, gitops-<app>).")
        raise GitHubError.of("GitHub token broker", status, doc, key="error")

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
                token: str | None = None, tenants: bool = False) -> dict[str, Any]:
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
        token = token or self._installation_token(owner, name, token_app, tenants=tenants)
        root = f"/repos/{owner}/{name}"

        status, ref = self._gh(token, "GET", f"{root}/git/ref/heads/{base}")
        if status != 200:
            raise GitHubError.of(f"base {base}", status, ref)
        base_sha = ref["object"]["sha"]
        status, commit = self._gh(token, "GET", f"{root}/git/commits/{base_sha}")
        if status != 200:
            raise GitHubError.of("base commit", status, commit)
        status, tree = self._gh(token, "POST", f"{root}/git/trees", {"base_tree": commit["tree"]["sha"], "tree": entries})
        if status != 201:
            raise GitHubError.of("tree", status, tree)
        trailer = "".join(f"\nHangar-{k.title()}: {v}" for k, v in sorted(meta.items()))
        status, new = self._gh(token, "POST", f"{root}/git/commits",
                               {"message": f"{title}\n{trailer}", "tree": tree["sha"], "parents": [base_sha]})
        if status != 201:
            raise GitHubError.of("commit", status, new)
        status, last_ = self._gh(token, "POST", f"{root}/git/refs", {"ref": f"refs/heads/{branch}", "sha": new["sha"]})
        if status == 422:
            raise Denied("GH-BRANCH-EXISTS", f"branch {branch} already exists",
                         "Use a new branch name; Clearance never pushes to an existing branch.")
        if status != 201:
            raise GitHubError.of("ref", status, last_)
        pr = self._create_pr(token, root, title, branch, base, meta, body)
        return {"pr": pr.get("number"), "url": pr.get("html_url"), "branch": branch, "commit": new["sha"]}

    def _create_pr(self, token: str, root: str, title: str, branch: str, base: str, meta: dict[str, str],
                   body: str | None) -> dict:
        text = "Opened by a Hangar agent through Clearance.\n\n" + "\n".join(f"- {k}: `{v}`" for k, v in sorted(meta.items()))
        if body:
            # What the tool has to say about the change (glidepath.cicd.propose: the verdict and the fields it touches).
            text += "\n\n" + body[:60000]
        status, pr = self._gh(token, "POST", f"{root}/pulls", {"title": title, "head": branch, "base": base, "body": text})
        if status != 201:
            raise GitHubError.of("pull", status, pr)
        return pr

    # -- airframe.apply: resumable PRs ---------------------------------------------------------------------------
    def find_pr(self, repo: str, branch: str, *, tenants: bool = False) -> dict[str, Any] | None:
        """The PR a branch already has (open, closed or merged), or None. GET only."""
        owner, name = _split(repo)
        return self._pr_of(self._installation_token(owner, name, tenants=tenants), owner, name, branch)

    def _pr_of(self, token: str, owner: str, name: str, branch: str) -> dict[str, Any] | None:
        head = urllib.parse.quote(f"{owner}:{branch}", safe="")
        status, doc = self._gh_any(token, f"/repos/{owner}/{name}/pulls?head={head}&state=all&per_page=5")
        if status != 200:
            raise GitHubError.of("pulls", status, doc)
        prs = [p for p in doc if isinstance(p, dict) and (p.get("head") or {}).get("ref") == branch] if isinstance(doc, list) else []
        # merged first, then open, then closed; a closed PR whose branch a person deleted is gone: the branch name is free
        prs.sort(key=lambda p: 0 if p.get("merged_at") else 1 if p.get("state") == "open" else 2)
        pr = prs[0] if prs else None
        if pr is not None and not pr.get("merged_at") and pr.get("state") != "open":
            status, last_ = self._gh(token, "GET", f"/repos/{owner}/{name}/git/ref/heads/{branch}")
            if status == 404:
                return None
            if status != 200:
                raise GitHubError.of("ref", status, last_)
        if pr is None:
            return None
        state = "merged" if pr.get("merged_at") else pr.get("state", "open")
        return {"pr": pr.get("number"), "url": pr.get("html_url"), "branch": branch,
                "commit": (pr.get("head") or {}).get("sha"), "state": state}

    def pr_paths(self, repo: str, number: int, *, tenants: bool = False) -> list[str]:
        """The paths a PR actually carries (GET /pulls/{n}/files, first 100), for a receipt that names only those."""
        owner, name = _split(repo)
        token = self._installation_token(owner, name, tenants=tenants)
        status, doc = self._gh_any(token, f"/repos/{owner}/{name}/pulls/{int(number)}/files?per_page=100")
        if status != 200 or not isinstance(doc, list):
            raise GitHubError.of("pull files", status, doc)
        return sorted(f["filename"] for f in doc if isinstance(f, dict) and "filename" in f)

    def repo_ready(self, repo: str, base: str = "main", *, tenants: bool = False) -> bool:
        """Does the repo exist, onboarded, with its base branch? False while the broker still refuses it as no app's
        repo (403) or it has no base branch (404): a repo the platform has not made yet. GET only."""
        owner, name = _split(repo)
        try:
            token = self._installation_token(owner, name, tenants=tenants)
        except Denied:
            return False
        status, last_ = self._gh(token, "GET", f"/repos/{owner}/{name}/git/ref/heads/{base}")
        if status == 404:
            return False
        if status != 200:
            raise GitHubError.of(f"base {base}", status, last_)
        return True

    ONBOARDING_BRANCH = "onboarding-resync"

    def onboarding_prs(self, repo: str, *, tenants: bool = False) -> list[dict[str, Any]]:
        """The open PRs from Glidepath's onboarding resync branch on a repo ([] when the repo is not ready for the
        agents yet: a repo the broker refuses has none this tool could race). GET only."""
        owner, name = _split(repo)
        try:
            token = self._installation_token(owner, name, tenants=tenants)
        except Denied:
            return []
        head = urllib.parse.quote(f"{owner}:{self.ONBOARDING_BRANCH}", safe="")
        status, doc = self._gh_any(token, f"/repos/{owner}/{name}/pulls?head={head}&state=open&per_page=10")
        if status == 404:
            return []
        if status != 200 or not isinstance(doc, list):
            raise GitHubError(f"pulls: {status}")
        return [{"pr": p.get("number"), "url": p.get("html_url")} for p in doc if isinstance(p, dict)
                and p.get("state") == "open" and (p.get("head") or {}).get("ref") == self.ONBOARDING_BRANCH]

    def ensure_pr(self, repo: str, branch: str, title: str, files: list[dict], base: str = "main",
                  meta: dict[str, str] | None = None, body: str | None = None, *, tenants: bool = False) -> dict[str, Any]:
        """open_pr that survives an interruption: the PR this branch already has is returned (`resumed`), a branch
        left without its PR (the call died between the ref and the pull request) gets the PR, once its files are
        checked to be the ones asked for, and only a branch that does not exist is built from the base as open_pr
        does. Nothing is ever pushed to or forced on an existing branch."""
        owner, name = _split(repo)
        token = self._installation_token(owner, name, tenants=tenants)
        found = self._pr_of(token, owner, name, branch)
        if found is not None:
            return {**found, "resumed": True}
        status, ref = self._gh(token, "GET", f"/repos/{owner}/{name}/git/ref/heads/{branch}")
        if status == 200:
            for f in files:
                if self._text_at(token, owner, name, f["path"], branch) != f["content"]:
                    raise Denied("APPLY-BRANCH-DIVERGED", f"branch {branch} exists and {f['path']} on it is not what this "
                                 "change would write", "A person looks at the branch; the tool never pushes to or "
                                 "deletes an existing one.")
            pr = self._create_pr(token, f"/repos/{owner}/{name}", title, branch, base, meta or {}, body)
            return {"pr": pr.get("number"), "url": pr.get("html_url"), "branch": branch,
                    "commit": (ref.get("object") or {}).get("sha"), "state": "open", "resumed": True}
        if status != 404:
            raise GitHubError.of("ref", status, ref)
        return {**self.open_pr(repo, branch, title, files, base, meta=meta, body=body, token=token), "state": "open",
                "resumed": False}

    def _text_at(self, token: str, owner: str, name: str, path: str, ref: str) -> str | None:
        quoted = urllib.parse.quote(path.strip("/"), safe="/")
        status, doc = self._gh_any(token, f"/repos/{owner}/{name}/contents/{quoted}?ref={urllib.parse.quote(ref, safe='')}")
        if status == 404:
            return None
        if status != 200 or not isinstance(doc, dict) or doc.get("encoding") != "base64":
            raise GitHubError.of("contents", status, doc)
        try:
            return base64.b64decode(doc.get("content") or "").decode("utf-8")
        except UnicodeDecodeError:
            return None

    def read_file(self, repo: str, path: str, ref: str = "main", *, tenants: bool = False) -> dict[str, Any]:
        """repo.file.get: one file's text, or a directory's entries, at a ref. The same per-call broker token (the
        broker's repo rule applies to reads too); GET only. A file over 1 MiB or a binary one is named, not returned."""
        owner, name = _split(repo)
        if not _BRANCH.match(ref or "") or ".." in ref:
            raise Denied("GH-REF", f"{ref!r} is not a ref", "Pass a branch, tag or commit sha.")
        token = self._installation_token(owner, name, tenants=tenants)
        quoted = urllib.parse.quote(path.strip("/"), safe="/")
        status, doc = self._gh_any(token, f"/repos/{owner}/{name}/contents/{quoted}?ref={urllib.parse.quote(ref, safe='')}")
        if status == 404:
            return {"path": path, "ref": ref, "found": False}
        if status != 200:
            raise GitHubError.of("contents", status, doc)
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
        token = self._installation_token(owner, repo, tenants=True)
        for check, missing in ((f"tenants/{app}", "XR-APP-UNKNOWN"), (path, None)):
            status, doc = self._gh_any(token, f"/repos/{owner}/{repo}/contents/{urllib.parse.quote(check)}?ref=main")
            if status not in (200, 404):
                raise GitHubError.of("contents", status, doc)
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
                           [{"path": path, "content": text}], "main", meta=meta, token=token, tenants=True,
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
