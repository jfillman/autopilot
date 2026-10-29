"""The T0 read adapters behind the core tools: argo.app.get, metrics.query and logs.query.

argo.app.get reads the Argo CD Application object through the Kubernetes API (airframe_api.KubeReader, GET-only
and allowlisted), as Clearance's own identity, which already reads applications for the airframe.* tools. No Argo
CD account and no stored token: the Application's status is what the Argo CD API would answer for a get. What is
returned is a summary (source, destination, sync, health, the last operation, conditions and the resources that are
not synced or not healthy), never spec.source's Helm values, which can carry anything a chart was given.

metrics.query is an instant PromQL query against a Prometheus-compatible API (Thanos Query on hangar-dev), and
logs.query a LogQL range query over the last hour against Loki. Both are read-only by API, unauthenticated in the
lab (the observability Services take no credential), and bounded here: the query length, the series, streams and
lines returned, and each line's length. A query the server rejects (bad syntax, a timeout) is an answer the agent
can fix, so it comes back as data with an `error`, not as a backend failure.

catalog.read and app.api.get have no adapter yet; serve.build_handlers drops them.
"""
from __future__ import annotations

import json
import re
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Callable

from clearance.dispatch import Denied

from .airframe_api import KubeReader, argo_path

MAX_QUERY = 4096
MAX_SERIES = 200
MAX_LINES = 200
MAX_LINE = 2000
MAX_MESSAGE = 500
MAX_RESOURCES = 50
_NAME = re.compile(r"^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$")


class ReadError(Exception):
    """A read backend failed or could not be reached. Never shown to the agent (dispatch says "backend error")."""


Opener = Callable[[urllib.request.Request, float], tuple[int, bytes]]


def _urlopen(req: urllib.request.Request, timeout: float) -> tuple[int, bytes]:
    try:
        with urllib.request.urlopen(req, timeout=timeout) as res:  # noqa: S310 - the URL is configuration
            return res.status, res.read(16 << 20)
    except urllib.error.HTTPError as e:
        return e.code, e.read(64 << 10)


def _clip(s: Any, n: int = MAX_MESSAGE) -> Any:
    return s[:n] + "..." if isinstance(s, str) and len(s) > n else s


# -- Argo CD --------------------------------------------------------------------------------------------------------

def app_summary(obj: dict[str, Any]) -> dict[str, Any]:
    meta, spec, st = obj.get("metadata") or {}, obj.get("spec") or {}, obj.get("status") or {}
    sources = spec.get("sources") or ([spec["source"]] if spec.get("source") else [])
    op = st.get("operationState") or {}
    resources = [r for r in st.get("resources") or []
                 if r.get("status") not in (None, "Synced") or (r.get("health") or {}).get("status") not in (None, "Healthy")]
    return {
        "app": meta.get("name"), "namespace": meta.get("namespace"), "project": spec.get("project"),
        "sources": [{k: s[k] for k in ("repoURL", "path", "chart", "targetRevision") if k in s} for s in sources],
        "destination": {k: v for k, v in (spec.get("destination") or {}).items() if k in ("server", "name", "namespace")},
        "sync": {"status": (st.get("sync") or {}).get("status"), "revision": (st.get("sync") or {}).get("revision"),
                 "revisions": (st.get("sync") or {}).get("revisions")},
        "health": {"status": (st.get("health") or {}).get("status"), "message": _clip((st.get("health") or {}).get("message"))},
        "operation": {"phase": op.get("phase"), "message": _clip(op.get("message")), "startedAt": op.get("startedAt"),
                      "finishedAt": op.get("finishedAt")} if op else None,
        "conditions": [{"type": c.get("type"), "message": _clip(c.get("message"))} for c in st.get("conditions") or []],
        "unhealthyResources": [{"kind": r.get("kind"), "namespace": r.get("namespace"), "name": r.get("name"),
                                "status": r.get("status"), "health": (r.get("health") or {}).get("status"),
                                "message": _clip((r.get("health") or {}).get("message"))}
                               for r in resources[:MAX_RESOURCES]],
        "unhealthyResourcesTruncated": len(resources) > MAX_RESOURCES,
        "reconciledAt": st.get("reconciledAt"),
    }


class KubeArgo:
    """ArgoBackend over the Application objects: get only. argo.sync.lower is a T1 write and has no adapter here."""

    def __init__(self, reader: KubeReader, namespaces: tuple[str, ...] = ("argocd",)):
        self.kube, self.namespaces = reader, namespaces

    def get_app(self, app: str) -> dict[str, Any]:
        if not isinstance(app, str) or not _NAME.match(app):
            raise Denied("READ-ARGO-NAME", f"{app!r} is not an Application name",
                         "Pass the Argo CD Application's name, e.g. flight-api-dev.")
        for ns in self.namespaces:
            obj = self.kube.get(argo_path("applications", ns, app))
            if obj is not None:
                return app_summary(obj)
        return {"app": app, "found": False, "searched": list(self.namespaces)}

    def sync(self, app: str, project: str) -> dict[str, Any]:
        raise NotImplementedError("argo.sync.lower has no adapter")


# -- Prometheus and Loki ----------------------------------------------------------------------------------------------

class _HTTPRead:
    def __init__(self, base_url: str, timeout: float = 30.0, opener: Opener = _urlopen):
        self.base, self.timeout, self._open = base_url.rstrip("/"), timeout, opener

    def _query(self, path: str, q: dict[str, Any], what: str) -> tuple[bool, Any]:
        """(True, data) on success; (False, an answer for the agent) when the server rejected the query."""
        if not isinstance(q.get("query"), str) or not q["query"].strip():
            raise Denied("READ-QUERY", f"empty {what} query", f"Pass a {what} expression.")
        if len(q["query"]) > MAX_QUERY:
            raise Denied("READ-QUERY", f"{what} query longer than {MAX_QUERY} characters", "Shorten the query.")
        req = urllib.request.Request(self.base + path + "?" + urllib.parse.urlencode(q), method="GET",
                                     headers={"Accept": "application/json"})
        try:
            status, body = self._open(req, self.timeout)
        except OSError as e:
            raise ReadError(f"{what} backend unreachable: {type(e).__name__}") from e
        try:
            doc = json.loads(body) if body else {}
        except ValueError:
            doc = {}
        if status == 200 and doc.get("status") == "success":
            return True, doc.get("data") or {}
        if status in (400, 422) or (status == 503 and doc.get("errorType") == "timeout"):
            return False, {"error": doc.get("errorType") or "bad_data",
                           "message": _clip(doc.get("error") or body[:MAX_MESSAGE].decode("utf-8", "replace"))}
        raise ReadError(f"{what} backend {status}")


class PrometheusAPI(_HTTPRead):
    """Instant queries (GET /api/v1/query) against a Prometheus-compatible API."""

    def promql(self, q: str) -> dict[str, Any]:
        ok, data = self._query("/api/v1/query", {"query": q, "timeout": "20s"}, "PromQL")
        if not ok:
            return data
        result = data.get("result")
        if data.get("resultType") in ("vector", "matrix") and isinstance(result, list):
            return {"resultType": data.get("resultType"), "result": result[:MAX_SERIES],
                    "series": len(result), "truncated": len(result) > MAX_SERIES}
        return {"resultType": data.get("resultType"), "result": result, "truncated": False}


class LokiAPI(_HTTPRead):
    """Range queries over the last hour (GET /loki/api/v1/query_range), newest first. One line more than is returned
    is asked for, so a result Loki itself cut at the limit still says `truncated`."""

    def logql(self, q: str) -> dict[str, Any]:
        ok, data = self._query("/loki/api/v1/query_range",
                               {"query": q, "since": "1h", "limit": MAX_LINES + 1, "direction": "backward"}, "LogQL")
        if not ok:
            return data
        rtype, result = data.get("resultType"), data.get("result") or []
        if rtype != "streams":
            return {"resultType": rtype, "result": result[:MAX_SERIES], "truncated": len(result) > MAX_SERIES}
        out, lines, truncated = [], 0, False
        for stream in result:
            values = []
            for ts, line in stream.get("values") or []:
                if lines >= MAX_LINES:
                    truncated = True
                    break
                values.append([ts, _clip(line, MAX_LINE)])
                lines += 1
            if values:
                out.append({"stream": stream.get("stream"), "values": values})
            if truncated:
                break
        return {"resultType": rtype, "result": out, "lines": lines, "truncated": truncated}


class Reads:
    """ReadBackend for the core read tools: PromQL and LogQL when configured. catalog and app_api have no adapter
    (serve.build_handlers drops catalog.read and app.api.get, so these are never called)."""

    def __init__(self, prometheus: PrometheusAPI | None = None, loki: LokiAPI | None = None):
        self.prometheus, self.loki = prometheus, loki

    def promql(self, q: str) -> Any:
        return self.prometheus.promql(q)

    def logql(self, q: str) -> Any:
        return self.loki.logql(q)

    def catalog(self, query: str) -> Any:
        raise NotImplementedError("catalog.read has no adapter")

    def app_api(self, service: str, path: str) -> Any:
        raise NotImplementedError("app.api.get has no adapter")
