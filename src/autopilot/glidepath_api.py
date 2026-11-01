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

from clearance.dispatch import Denied

from . import glidepath_live

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


# A log or report bigger than this is cut to its last LOG_TAIL bytes (an agent reads the end of a failing step).
LOG_TAIL = 256 << 10
WATCH_DEFAULT, WATCH_MAX = 60, 120


def _stream_open(req: urllib.request.Request, timeout: float) -> tuple[int, Any]:
    """Open a server-sent event stream: (200, its lines) or (status, the error body). `timeout` bounds each read, so a
    silent connection fails; glidepath-api's 15 s heartbeat keeps a healthy one talking."""
    try:
        res = urllib.request.urlopen(req, timeout=timeout)  # noqa: S310 - the URL is configuration
    except urllib.error.HTTPError as e:
        return e.code, e.read(64 << 10)
    def lines():
        with res:
            for line in res:
                yield line
    return res.status, lines()


def _tail(body: bytes, key: str) -> dict:
    text = body.decode("utf-8", "replace")
    if len(body) <= LOG_TAIL:
        return {key: text}
    return {key: body[-LOG_TAIL:].decode("utf-8", "replace"), "truncated": True, "bytes": len(body)}


class GlidepathAPI:
    def __init__(self, base_url: str, token_file: str | Path, on_behalf_of: bool = False,
                 timeout: float = 30.0, opener: Opener = _urlopen, stream_opener=_stream_open):
        self.base = base_url.rstrip("/")
        self.token_file = Path(token_file)
        self.send_on_behalf_of = on_behalf_of
        self.timeout = timeout
        self._open = opener
        self._stream = stream_opener

    def _request(self, path: str, on_behalf_of: str, accept: str) -> urllib.request.Request:
        token = self.token_file.read_text().strip()
        headers = {"Authorization": f"Bearer {token}", "X-Glidepath-Authorization": f"Bearer {token}", "Accept": accept}
        if self.send_on_behalf_of and on_behalf_of:
            headers["X-Glidepath-On-Behalf-Of"] = on_behalf_of
        return urllib.request.Request(self.base + path, headers=headers, method="GET")

    def _get(self, path: str, on_behalf_of: str, accept: str = "application/json",
             query: dict[str, Any] | None = None, method: str = "GET",
             extra: dict[str, str] | None = None, body: dict[str, Any] | None = None) -> tuple[int, bytes]:
        q = {k: v for k, v in (query or {}).items() if v is not None}
        url = self.base + path + ("?" + urllib.parse.urlencode(q) if q else "")
        token = self.token_file.read_text().strip()
        headers = {"Authorization": f"Bearer {token}", "X-Glidepath-Authorization": f"Bearer {token}", "Accept": accept,
                   **(extra or {})}
        if self.send_on_behalf_of and on_behalf_of:
            headers["X-Glidepath-On-Behalf-Of"] = on_behalf_of
        data = None
        if method == "POST":
            data = json.dumps(body).encode() if body is not None else b""
            if body is not None:
                headers["Content-Type"] = "application/json"
        req = urllib.request.Request(url, headers=headers, method=method, data=data)
        try:
            return self._open(req, self.timeout)
        except OSError as e:
            raise GlidepathError(f"glidepath-api unreachable: {type(e).__name__}") from e

    def _json(self, path: str, on_behalf_of: str, query: dict[str, Any] | None = None) -> Any:
        status, body = self._get(path, on_behalf_of, query=query)
        if status == 200:
            return json.loads(body)
        return self._refusal(status, body)

    @staticmethod
    def _refusal(status: int, body: bytes, hint_422: str = "The PipelineRun is outside the shape Glidepath may start "
                 "or change; ask a human.") -> Any:
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
                         "Clearance's identity may not do this for this app in Glidepath; ask a human to grant it.")
        if status == 422:
            # Glidepath refused the write: the cluster's admission policy or RBAC, or (promote, rollback) one of its
            # release rules: out of order, a Ground target, an ineligible target. A policy answer, not a retry.
            raise Denied(rule or "GP-API-422", f"glidepath-api refused the write: {message}", hint or hint_422)
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

    # What is happening now (glidepath-api 0.8.0+). Results holds finished runs only; these read the cluster.
    def live(self, app, on_behalf_of):
        body = self._json(f"/v1/apps/{self._seg(app)}/live", on_behalf_of)
        return body if "error" in body else glidepath_live.summarize(body)

    def live_log(self, app, task_run, container, on_behalf_of):
        status, body = self._get(f"/v1/apps/{self._seg(app)}/live/taskruns/{self._seg(task_run)}/logs/{self._seg(container)}",
                                 on_behalf_of, accept="text/plain")
        if status == 200:
            return _tail(body, "log")
        return self._refusal(status, body)

    def watch(self, app, pipelinerun, timeout_seconds, on_behalf_of):
        """GET .../live/stream until `pipelinerun` finishes or timeout_seconds pass (glidepath_live.watch)."""
        timeout = float(min(WATCH_MAX, timeout_seconds or WATCH_DEFAULT))
        path = f"/v1/apps/{self._seg(app)}/live/stream"

        def open_stream(t):
            try:
                return self._stream(self._request(path, on_behalf_of, "text/event-stream"), min(t, 30.0))
            except OSError as e:
                return 599, str(e).encode()

        def poll():
            return self._json(f"/v1/apps/{self._seg(app)}/live", on_behalf_of)

        status, first = open_stream(timeout)
        if status in (401, 403, 404, 400):
            return self._refusal(status, first)
        return glidepath_live.watch(pipelinerun, timeout, lambda _t: (status, first), poll)

    # A promote or rollback release run (glidepath-api 0.8.0): its tasks in order, the PR, the change request.
    def release_run(self, app, name, on_behalf_of):
        return self._json(f"/v1/apps/{self._seg(app)}/releases/runs/{self._seg(name)}", on_behalf_of)

    # Supply-chain evidence and tests (phase 2).
    def evidence(self, app, image, on_behalf_of):
        return self._json(f"/v1/apps/{self._seg(app)}/evidence", on_behalf_of, {"image": image})

    def tests(self, app, limit, on_behalf_of):
        return self._json(f"/v1/apps/{self._seg(app)}/tests", on_behalf_of, {"limit": limit})

    def test_report(self, app, test_id, on_behalf_of):
        status, body = self._get(f"/v1/apps/{self._seg(app)}/tests/{self._seg(test_id)}/report", on_behalf_of,
                                 accept="application/xml")
        if status == 200:
            return _tail(body, "report")
        return self._refusal(status, body)

    # The app's recent CDEvents from glidepath-api's buffer (phase 5; best-effort, at most 24 h).
    def events(self, app, type_prefix, since, limit, on_behalf_of):
        return self._json("/v1/events/recent", on_behalf_of, {"app": app, "type": type_prefix, "since": since, "limit": limit})

    # Preview environments (0.10.0), read only.
    def previews(self, app, on_behalf_of):
        return self._json(f"/v1/apps/{self._seg(app)}/previews", on_behalf_of)

    # Writes (spec phase 3). glidepath-api requires a delegate's write to name the person, so Clearance must be
    # configured with on_behalf_of=True for these; the Idempotency-Key makes a retried re-run start no second run.
    def _post(self, path: str, on_behalf_of: str, extra: dict[str, str] | None = None,
              body: dict[str, Any] | None = None, **refusal: str) -> Any:
        status, raw = self._get(path, on_behalf_of, method="POST", extra=extra, body=body)
        if status in (200, 201):
            return json.loads(raw)
        if status == 409:
            err = json.loads(raw) if raw else {}
            return {"error": {"rule": err.get("rule", "GP-API-409"), "message": err.get("error", ""), "hint": err.get("hint", "")}}
        return self._refusal(status, raw, **refusal)

    def rerun(self, app, pipelinerun, idempotency_key, on_behalf_of):
        return self._post(f"/v1/apps/{self._seg(app)}/pipelineruns/{self._seg(pipelinerun)}/rerun", on_behalf_of,
                          {"Idempotency-Key": idempotency_key})

    def cancel(self, app, pipelinerun, on_behalf_of):
        return self._post(f"/v1/apps/{self._seg(app)}/pipelineruns/{self._seg(pipelinerun)}/cancel", on_behalf_of)

    # Releases (spec phase 4). Two reads, and two T2 writes that only start the app's release run: its
    # open-release-pr step opens the gitops PR with the platform's GitHub App, and a human merges it. Dry run goes in
    # the header, the form that also works through the API server's service proxy (which refuses ?dryRun).
    def environments(self, app, on_behalf_of):
        return self._json(f"/v1/apps/{self._seg(app)}/environments", on_behalf_of)

    def rollback_plan(self, app, env, cluster, on_behalf_of):
        return self._json(f"/v1/apps/{self._seg(app)}/environments/{self._seg(env)}/rollback-plan", on_behalf_of,
                          {"cluster": cluster})

    _RELEASE_422 = ("Read glidepath.environments.list (promote one stage at a time, to a Flight environment) or "
                    "glidepath.environments.rollbackPlan (roll back only to one of its targets); otherwise ask a human.")

    def _release(self, op, app, env, body, idempotency_key, dry_run, on_behalf_of):
        headers = {"Idempotency-Key": idempotency_key, "X-Glidepath-Dry-Run": "true" if dry_run else "false"}
        return self._post(f"/v1/apps/{self._seg(app)}/environments/{self._seg(env)}/{op}", on_behalf_of, headers, body,
                          hint_422=self._RELEASE_422)

    # cicd.yaml changes (glidepath-api 0.7.0, docs/admin/api.md "Validate a cicd.yaml change"): what a merge patch
    # would mean, as the cicd-validate check will judge it. T0: nothing is written. The verdict on the change (valid,
    # problems, changes, resulting) is the 200 body; a 400 about the request itself comes back as data.
    def validate_cicd(self, app, body, on_behalf_of):
        status, raw = self._get(f"/v1/apps/{self._seg(app)}/cicd/validations", on_behalf_of, method="POST", body=body)
        if status == 200:
            return json.loads(raw)
        return self._refusal(status, raw)

    def promote(self, app, env, body, idempotency_key, dry_run, on_behalf_of):
        return self._release("promote", app, env, body, idempotency_key, dry_run, on_behalf_of)

    def rollback(self, app, env, body, idempotency_key, dry_run, on_behalf_of):
        return self._release("rollback", app, env, body, idempotency_key, dry_run, on_behalf_of)
