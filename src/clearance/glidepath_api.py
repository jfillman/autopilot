"""The real GlidepathBackend: glidepath-api over HTTP (glidepath docs/admin/api.md, api/openapi.yaml).

Clearance calls it as its own identity, a bearer token read from a file on every call (a projected
ServiceAccount token the kubelet rotates; nothing keeps it). glidepath-api authorizes that identity
with a SubjectAccessReview in the app's -cicd namespace, so what Clearance may read at all is
ordinary RBAC there; profile.glidepath.apps narrows it per agent before any call is made.

When Clearance is one of glidepath-api's `api.delegates`, it also names the session's principal in
X-Glidepath-On-Behalf-Of, which glidepath-api writes into its audit line. Reads are authorized as
Clearance either way (phase 1 and 2); a non-delegate that sent the header would be refused, so it
is off unless configured.

The base URL is either the Service (in cluster) or an API server's services/proxy path to it. The
API server drops Authorization before forwarding, so the token also goes in
X-Glidepath-Authorization, as Tower does.
"""
from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Callable

from .dispatch import Denied

# Every refusal glidepath-api can give that is about who is asking, not what was asked.
_AUTH_STATUSES = (401, 403)


class GlidepathError(Exception):
    """glidepath-api failed or could not be reached. Never shown to the agent (dispatch says "backend error")."""


Opener = Callable[[urllib.request.Request, float], tuple[int, bytes]]


def _urlopen(req: urllib.request.Request, timeout: float) -> tuple[int, bytes]:
    try:
        with urllib.request.urlopen(req, timeout=timeout) as res:  # noqa: S310 - the URL is configuration
            return res.status, res.read(8 << 20)
    except urllib.error.HTTPError as e:
        return e.code, e.read(64 << 10)


class GlidepathAPI:
    def __init__(self, base_url: str, token_file: str | Path, on_behalf_of: bool = False,
                 timeout: float = 30.0, opener: Opener = _urlopen):
        self.base = base_url.rstrip("/")
        self.token_file = Path(token_file)
        self.send_on_behalf_of = on_behalf_of
        self.timeout = timeout
        self._open = opener

    def _get(self, path: str, on_behalf_of: str, accept: str = "application/json",
             query: dict[str, Any] | None = None) -> tuple[int, bytes]:
        q = {k: v for k, v in (query or {}).items() if v is not None}
        url = self.base + path + ("?" + urllib.parse.urlencode(q) if q else "")
        token = self.token_file.read_text().strip()
        headers = {"Authorization": f"Bearer {token}", "X-Glidepath-Authorization": f"Bearer {token}", "Accept": accept}
        if self.send_on_behalf_of and on_behalf_of:
            headers["X-Glidepath-On-Behalf-Of"] = on_behalf_of
        try:
            return self._open(urllib.request.Request(url, headers=headers), self.timeout)
        except OSError as e:
            raise GlidepathError(f"glidepath-api unreachable: {type(e).__name__}") from e

    def _json(self, path: str, on_behalf_of: str, query: dict[str, Any] | None = None) -> Any:
        status, body = self._get(path, on_behalf_of, query=query)
        if status == 200:
            return json.loads(body)
        return self._refusal(status, body)

    @staticmethod
    def _refusal(status: int, body: bytes) -> Any:
        """A refusal about the caller is a denial (Denied, with glidepath-api's rule id). A refusal
        about the question (400, 404: no such app or run, no archived log) is an answer the agent
        acts on, returned as data. Anything else is a backend failure."""
        try:
            err = json.loads(body)
        except ValueError:
            err = {}
        rule, message, hint = err.get("rule"), err.get("error", ""), err.get("hint", "")
        if status in _AUTH_STATUSES:
            raise Denied(rule or "GP-AUTHZ-001", f"glidepath-api refused: {message}", hint or
                         "Clearance's identity may not read this app in Glidepath; ask a human to grant it.")
        if status in (400, 404) and rule:
            return {"error": {"rule": rule, "message": message, "hint": hint}}
        raise GlidepathError(f"glidepath-api {status} {rule or ''}".strip())

    @staticmethod
    def _seg(s: str) -> str:
        return urllib.parse.quote(s, safe="")

    def runs(self, app, since_hours, limit, on_behalf_of):
        return self._json(f"/v1/apps/{self._seg(app)}/runs", on_behalf_of, {"sinceHours": since_hours, "limit": limit})

    def run(self, app, result, on_behalf_of):
        return self._json(f"/v1/apps/{self._seg(app)}/runs/{self._seg(result)}", on_behalf_of)

    def log(self, app, result, task_run, on_behalf_of):
        status, body = self._get(f"/v1/apps/{self._seg(app)}/runs/{self._seg(result)}/logs/{self._seg(task_run)}",
                                 on_behalf_of, accept="text/plain")
        if status == 200:
            return {"log": body.decode("utf-8", "replace")}
        return self._refusal(status, body)

    def explain(self, app, result, on_behalf_of):
        return self._json(f"/v1/apps/{self._seg(app)}/runs/{self._seg(result)}/explain", on_behalf_of)

    def releases(self, app, chain_id, on_behalf_of):
        if chain_id:
            return self._json(f"/v1/apps/{self._seg(app)}/releases/{self._seg(chain_id)}", on_behalf_of)
        return self._json(f"/v1/apps/{self._seg(app)}/releases", on_behalf_of)

    def contract(self, on_behalf_of):
        return self._json("/v1/contract", on_behalf_of)
