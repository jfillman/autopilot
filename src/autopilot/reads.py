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

catalog.read searches Tower's Backstage catalog (GET /api/catalog/entities/by-query) as Clearance's own workload
identity: a projected service-account token for the `backstage` audience, re-read on every call, that Backstage checks
against this cluster's public issuer (backend.auth.externalAccess type jwks, restricted to catalog.entity.read). No
stored secret. Only a summary of each entity is returned (kind, ref, title, description, type, owner, lifecycle,
system), at most MAX_ENTITIES of them, never annotations or the rest of spec.

app.api.get has no adapter yet; serve.build_handlers drops it.
"""
from __future__ import annotations

import json
import re
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
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
    """ArgoBackend over the Application objects. get reads; sync (argo.sync.lower, T1) is argo_sync.ArgoSync's, and
    only when one is given (serve: ARGO_SYNC_ENABLED), else the tool has no handler."""

    def __init__(self, reader: KubeReader, namespaces: tuple[str, ...] = ("argocd",), syncer: Any = None):
        self.kube, self.namespaces, self.syncer = reader, namespaces, syncer

    def get_app(self, app: str) -> dict[str, Any]:
        if not isinstance(app, str) or not _NAME.match(app):
            raise Denied("READ-ARGO-NAME", f"{app!r} is not an Application name",
                         "Pass the Argo CD Application's name, e.g. flight-api-dev.")
        for ns in self.namespaces:
            obj = self.kube.get(argo_path("applications", ns, app))
            if obj is not None:
                return app_summary(obj)
        return {"app": app, "found": False, "searched": list(self.namespaces)}

    def sync(self, app: str, project: str, who: dict[str, str] | None = None) -> dict[str, Any]:
        if self.syncer is None:
            raise NotImplementedError("argo.sync.lower has no adapter in this deployment")
        return self.syncer.sync(app, project, who or {})


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


# -- Backstage ------------------------------------------------------------------------------------------------------

MAX_ENTITIES = 25
CATALOG_FIELDS = ("kind", "metadata.namespace", "metadata.name", "metadata.title", "metadata.description",
                  "spec.type", "spec.owner", "spec.lifecycle", "spec.system")
SEARCH_FIELDS = ("metadata.name", "metadata.title", "metadata.description")


class BackstageCatalog:
    """catalog.read: a full-text search, or (a query with `=`) a Backstage filter such as
    `kind=component,spec.owner=group:default/skyport`."""

    def __init__(self, base_url: str, token_file: str | Path, timeout: float = 30.0, opener: Opener = _urlopen):
        self.base, self.token_file = base_url.rstrip("/"), Path(token_file)
        self.timeout, self._open = timeout, opener

    def catalog(self, query: str) -> dict[str, Any]:
        if not isinstance(query, str) or not query.strip():
            raise Denied("READ-QUERY", "empty catalog query", "Pass search words, or a filter like kind=component.")
        if len(query) > 500:
            raise Denied("READ-QUERY", "catalog query longer than 500 characters", "Shorten the query.")
        q: list[tuple[str, str]] = [("limit", str(MAX_ENTITIES))] + [("fields", f) for f in CATALOG_FIELDS]
        if "=" in query:
            q.append(("filter", query.strip()))
        else:
            # Without fullTextFilterFields, Backstage's full-text search matches nothing (live, 2026-10-30).
            q += [("fullTextFilterTerm", query.strip())] + [("fullTextFilterFields", f) for f in SEARCH_FIELDS]
        token = self.token_file.read_text().strip()
        req = urllib.request.Request(f"{self.base}/api/catalog/entities/by-query?{urllib.parse.urlencode(q)}",
                                     method="GET", headers={"Authorization": f"Bearer {token}",
                                                            "Accept": "application/json"})
        try:
            status, body = self._open(req, self.timeout)
        except OSError as e:
            raise ReadError(f"catalog unreachable: {type(e).__name__}") from e
        if status in (401, 403):
            raise Denied("READ-CATALOG-403", f"Backstage refused Clearance's identity ({status})",
                         "Clearance may read the catalog only; ask a human to check Backstage's externalAccess.")
        if status == 400:
            return {"error": "bad-query", "message": _clip(body.decode(errors="replace"))}
        if status != 200:
            raise ReadError(f"catalog {status}")
        doc = json.loads(body)
        items = doc.get("items") or []
        return {"entities": [_entity(e) for e in items[:MAX_ENTITIES]], "totalItems": doc.get("totalItems"),
                "truncated": bool((doc.get("pageInfo") or {}).get("nextCursor")) or len(items) > MAX_ENTITIES}


def _entity(e: dict[str, Any]) -> dict[str, Any]:
    md, spec = e.get("metadata") or {}, e.get("spec") or {}
    out = {"ref": f"{str(e.get('kind', '')).lower()}:{md.get('namespace', 'default')}/{md.get('name')}",
           "kind": e.get("kind"), "title": md.get("title"), "description": _clip(md.get("description")),
           **{k: spec.get(k) for k in ("type", "owner", "lifecycle", "system")}}
    return {k: v for k, v in out.items() if v is not None}


class BackstageDirectory:
    """clearance.inbox.Directory on Tower's catalog: a person's groups are their User entity's memberOf relations.
    A Clearance person `user:<login>` is the catalog's `user:default/<login>` (Tower signs people in by GitHub login,
    usernameMatchingUserEntityName). Same token as catalog.read. Cached for `ttl` seconds; any failure, or an unknown
    person, is no groups (so only a request's session owner may answer it), never an error."""

    def __init__(self, catalog: BackstageCatalog, ttl: float = 60.0, clock: Callable[[], float] | None = None):
        import time
        self.catalog, self.ttl, self.clock = catalog, ttl, clock or time.monotonic
        self._cache: dict[str, tuple[float, set[str]]] = {}

    def groups(self, principal: str) -> set[str]:
        name = principal.removeprefix("user:") if isinstance(principal, str) and principal.startswith("user:") else None
        if not name or not _NAME.match(name.lower()):
            return set()
        hit = self._cache.get(name)
        if hit and self.clock() - hit[0] < self.ttl:
            return hit[1]
        groups = self._fetch(name)
        self._cache[name] = (self.clock(), groups)
        return groups

    def _fetch(self, name: str) -> set[str]:
        c = self.catalog
        try:
            token = c.token_file.read_text().strip()
            req = urllib.request.Request(f"{c.base}/api/catalog/entities/by-name/user/default/{urllib.parse.quote(name)}",
                                         method="GET", headers={"Authorization": f"Bearer {token}",
                                                                "Accept": "application/json"})
            status, body = c._open(req, c.timeout)
            if status != 200:
                return set()
            rels = json.loads(body).get("relations") or []
        except (OSError, ValueError):
            return set()
        return {r["targetRef"] for r in rels
                if r.get("type") == "memberOf" and str(r.get("targetRef", "")).startswith("group:")}


class BackstageOwnership:
    """clearance.api.Ownership on Tower's catalog: the apps a person owns are the Components whose spec.owner is the
    person (user:default/<login>) or one of their groups (BackstageDirectory). On Hangar a Component's name is its
    app's. Cached like the Directory; a failure is no apps, never an error."""

    def __init__(self, catalog: BackstageCatalog, directory: "BackstageDirectory", ttl: float = 60.0,
                 clock: Callable[[], float] | None = None):
        import time
        self.catalog, self.directory, self.ttl, self.clock = catalog, directory, ttl, clock or time.monotonic
        self._cache: dict[str, tuple[float, set[str]]] = {}

    def apps(self, principal: str) -> set[str]:
        name = principal.removeprefix("user:") if isinstance(principal, str) and principal.startswith("user:") else None
        if not name or not _NAME.match(name.lower()):
            return set()
        hit = self._cache.get(name)
        if hit and self.clock() - hit[0] < self.ttl:
            return hit[1]
        owners = sorted({f"user:default/{name}"} | self.directory.groups(principal))
        apps: set[str] = set()
        try:
            c = self.catalog
            token = c.token_file.read_text().strip()
            q = [("filter", f"kind=component,spec.owner={o}") for o in owners] + [("fields", "metadata.name"), ("limit", "500")]
            req = urllib.request.Request(f"{c.base}/api/catalog/entities/by-query?{urllib.parse.urlencode(q)}",
                                         method="GET", headers={"Authorization": f"Bearer {token}", "Accept": "application/json"})
            status, body = c._open(req, c.timeout)
            if status == 200:
                apps = {e["metadata"]["name"].lower() for e in json.loads(body).get("items") or [] if e.get("metadata")}
        except (OSError, ValueError, KeyError):
            apps = set()
        self._cache[name] = (self.clock(), apps)
        return apps


class BackstageAppOwners:
    """clearance.dispatch.AppOwners on Tower's catalog, for the deployment gate: an app's owner group is its Component's
    spec.owner (`component:default/<app>`). A bare owner name is the catalog's group:default/<name>. Cached like the
    Directory; a failure or an unknown app is None (the gate then lets the outside group clear), never an error."""

    def __init__(self, catalog: BackstageCatalog, ttl: float = 300.0, clock: Callable[[], float] | None = None):
        import time
        self.catalog, self.ttl, self.clock = catalog, ttl, clock or time.monotonic
        self._cache: dict[str, tuple[float, str | None]] = {}

    def owner_of(self, app: str) -> str | None:
        if not isinstance(app, str) or not _NAME.match(app.lower()):
            return None
        hit = self._cache.get(app)
        if hit and self.clock() - hit[0] < self.ttl:
            return hit[1]
        owner = None
        try:
            c = self.catalog
            token = c.token_file.read_text().strip()
            req = urllib.request.Request(f"{c.base}/api/catalog/entities/by-name/component/default/{urllib.parse.quote(app)}",
                                         method="GET", headers={"Authorization": f"Bearer {token}", "Accept": "application/json"})
            status, body = c._open(req, c.timeout)
            if status == 200:
                raw = str((json.loads(body).get("spec") or {}).get("owner") or "")
                if raw:
                    owner = raw if raw.startswith("group:") else (None if ":" in raw else f"group:default/{raw}")
                    if owner and "/" not in owner:
                        owner = owner.replace("group:", "group:default/", 1)
        except (OSError, ValueError):
            owner = None
        self._cache[app] = (self.clock(), owner)
        return owner


class Reads:
    """ReadBackend for the core read tools: PromQL, LogQL and the catalog, each when configured. app_api has no
    adapter (serve.build_handlers drops app.api.get, so it is never called)."""

    def __init__(self, prometheus: PrometheusAPI | None = None, loki: LokiAPI | None = None,
                 backstage: BackstageCatalog | None = None):
        self.prometheus, self.loki, self.backstage = prometheus, loki, backstage

    def promql(self, q: str) -> Any:
        return self.prometheus.promql(q)

    def logql(self, q: str) -> Any:
        return self.loki.logql(q)

    def catalog(self, query: str) -> Any:
        return self.backstage.catalog(query)

    def app_api(self, service: str, path: str) -> Any:
        raise NotImplementedError("app.api.get has no adapter")
