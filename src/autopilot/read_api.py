"""The gateway's read API (`autopilot-gateway api`, docs/admin "The read API"): the runtime's reads, a separate,
read-only process.

It serves what the gateway keeps as runtime state: agents (with the hold on their triggers), sessions, a run's result,
the run tree, the human inbox and the trigger bridge's history, plus the runtime catalog. Clearance's own reads
(evidence, counts, the chain check, receipts, provenance, the deployment gate's changes and policy, the decision policy
and its explainer, the definitions it decides with) are clearance-api's, the Go `clearance api` (clearance
docs/clearance-api.md); `/v1/contract` names it (CLEARANCE_API_URL). Ported from clearance api.py (v0.26.1) with the
same paths, parameters, answers, status codes and roles (R-3c): the nine operations the Go API left for the gateway.

It reads the gateway's tables through its own read-only DSN, never the writer lock, so reads never queue behind
agents' tool calls and the API can run several replicas. It writes nothing: answering a human request, the person
controls and running an agent stay on the gateway. The tables are clearance v0.26.1's (`clearance_*`, on clearance-pg
until the cutover) or the gateway's own database's (`gateway_*`, R-3b): the same shapes under another prefix, chosen by
GATEWAY_API_TABLES.

Who sees what (`Viewer`):
  auditor    every read (GATEWAY_API_AUDITORS)
  person     their own sessions and sessions of agents whose definition `owner` group they are in (Directory); an
             app's events to the people who own it (Ownership); the human inbox as at the gateway
  delegate   everything the person named in X-Hangar-On-Behalf-Of may (CLEARANCE_DELEGATES, e.g. Tower)
  notifier   the human-inbox feed across everyone, nothing else (GATEWAY_API_NOTIFIERS, e.g. Tower's poller)

ROUTES is the route table and the contract (`contract()`, contract/autopilot-api.json, checked by a test): one entry per
operation, with its parameters and the roles that may call it.
"""
from __future__ import annotations

import queue
import threading
from dataclasses import dataclass, field
from datetime import datetime, timezone
from fnmatch import fnmatchcase
from typing import Any, Callable, Mapping, Protocol, Sequence

from .engine.dispatch import AuthError, Authenticator

API_VERSION = "v1"
MAX_LIMIT = 200
DEFAULT_LIMIT = 50
STATEMENT_TIMEOUT_MS = 5000
TABLE_PREFIXES = ("clearance", "gateway")

PERSON_ROLES = ("person", "delegate", "auditor")
ROUTES: tuple[dict[str, Any], ...] = (
    {"operationId": "listAgents", "method": "GET", "path": "/v1/agents", "roles": PERSON_ROLES, "params": ["full"],
     "summary": "Agent definitions: kind, identity, owner, tier ceiling, tools, scope, limits, triggers"},
    {"operationId": "getAgent", "method": "GET", "path": "/v1/agents/{name}", "roles": PERSON_ROLES, "params": ["full"],
     "summary": "One agent definition"},
    {"operationId": "listSessions", "method": "GET", "path": "/v1/sessions", "roles": PERSON_ROLES,
     "params": ["agent", "principal", "task", "state", "limit", "after"],
     "summary": "Sessions you may see, newest first: limits, budget used, breaker, run, parent, expiry; state is live, closed or expired"},
    {"operationId": "getSession", "method": "GET", "path": "/v1/sessions/{id}", "roles": PERSON_ROLES, "params": [],
     "summary": "One session: with input (what an in-cluster run was asked) and outcome (how it ended) once it has them"},
    {"operationId": "getSessionResult", "method": "GET", "path": "/v1/sessions/{id}/result", "roles": PERSON_ROLES,
     "params": [], "summary": "A finished in-cluster run's answer (results.py), kept 30 days: status, final_text, turns, tool_calls [{tool, ok, rule}], reason; 404 when there is none or it expired"},
    {"operationId": "getTask", "method": "GET", "path": "/v1/tasks/{task_id}", "roles": PERSON_ROLES, "params": [],
     "summary": "A run tree: every session of the task, with parent links and totals"},
    {"operationId": "listHumanRequests", "method": "GET", "path": "/v1/human/requests", "roles": ("person", "delegate", "notifier", "auditor"),
     "params": ["since"],
     "summary": "Pending human requests: a person's, those they may answer; a notifier's, every pending one (with owner and group), created since `since`"},
    {"operationId": "listEvents", "method": "GET", "path": "/v1/events", "roles": PERSON_ROLES,
     "params": ["app", "agent", "source", "key", "subject", "since", "until", "limit", "after"],
     "summary": "The events the trigger bridge received, newest first, each with what it did per agent (launched, deduped, braked, refused): the history a trigger dry run, an agent's trigger statistics and an app's run marks read. Kept 30 days; the cursor is received_at"},
    {"operationId": "listRuntimes", "method": "GET", "path": "/v1/runtimes", "roles": PERSON_ROLES, "params": [],
     "summary": "The deployment's runtime catalog (runtimes.py): the agent images a definition may run (kind runtime) or a code agent may build FROM (kind base), each pinned by digest, with its framework and status"},
    {"operationId": "getContract", "method": "GET", "path": "/v1/contract", "roles": ("any",), "params": [],
     "summary": "This contract, with where the gateway's writes go (gateway) and where Clearance's reads are (clearance)"},
)


def contract() -> dict[str, Any]:
    return {"api": "autopilot-api", "version": API_VERSION,
            "notes": ("GET only. The Autopilot gateway's read API: the runtime's state. Writes (POST "
                      "/v1/human/requests/{id}/answer, the person controls, POST /v1/agents/{name}/runs) go to the "
                      "gateway; Clearance's reads (evidence, receipts, provenance, clearances, policy, definitions) are "
                      "clearance-api's (clearance contract/clearance-api.json), named in this contract's `clearance`."),
            "operations": [dict(r, roles=list(r["roles"])) for r in ROUTES]}


class ApiError(Exception):
    def __init__(self, status: int, message: str):
        self.status, self.message = status, message
        super().__init__(message)


class Directory(Protocol):
    def groups(self, principal: str) -> set[str]: ...


class Ownership(Protocol):
    """The apps a person owns (directly or through a group), for an app's events."""
    def apps(self, principal: str) -> set[str]: ...


@dataclass(frozen=True)
class Viewer:
    role: str                         # auditor | person | notifier
    person: str | None = None         # user:<login> for a person or a delegate's person
    via: str | None = None            # the delegate, when a delegate asked
    agents: frozenset[str] = field(default_factory=frozenset)   # agents of the person's teams
    apps: frozenset[str] = field(default_factory=frozenset)     # apps the person owns
    groups: frozenset[str] = field(default_factory=frozenset)   # the person's groups

    @property
    def all(self) -> bool:
        return self.role == "auditor"


def _any(principal: str, globs: Sequence[str]) -> bool:
    return any(fnmatchcase(principal, g) for g in globs)


@dataclass
class Access:
    auth: Authenticator
    definitions: Mapping[str, Any]
    delegates: Sequence[str] = ()
    notifiers: Sequence[str] = ()
    auditors: Sequence[str] = ()
    directory: Directory | None = None
    ownership: Ownership | None = None

    def viewer(self, token: str | None, on_behalf_of: str | None) -> Viewer:
        if not token:
            raise ApiError(401, "a bearer token is required")
        try:
            principal = self.auth.authenticate(token)
        except AuthError:
            raise ApiError(401, "token not accepted") from None
        if on_behalf_of:
            if not _any(principal.id, self.delegates):
                raise ApiError(403, f"{principal.id} may not act for a person")
            if not on_behalf_of.startswith("user:") or len(on_behalf_of) > 200:
                raise ApiError(400, "X-Hangar-On-Behalf-Of must name a person (user:<name>)")
            return self._person(on_behalf_of, via=principal.id)
        if _any(principal.id, self.auditors):
            return Viewer("auditor", person=principal.id)
        if _any(principal.id, self.notifiers):
            return Viewer("notifier")
        if principal.kind == "user":
            return self._person(principal.id)
        raise ApiError(403, f"{principal.id} is not a person, delegate, notifier or auditor")

    def _person(self, person: str, via: str | None = None) -> Viewer:
        groups = self.directory.groups(person) if self.directory else set()
        agents = frozenset(n for n, d in self.definitions.items() if getattr(d, "owner", None) in groups)
        apps = frozenset(a.lower() for a in (self.ownership.apps(person) if self.ownership else set()))
        return Viewer("person", person=person, via=via, agents=agents, apps=apps, groups=frozenset(groups))


# -- the store -------------------------------------------------------------------------------------------------------

class ReadStore(Protocol):
    def sessions(self, f: dict[str, Any], v: Viewer, limit: int, after: str | None) -> list[dict[str, Any]]: ...
    def session(self, sid: str) -> dict[str, Any] | None: ...
    def task(self, task_id: str) -> list[dict[str, Any]]: ...
    def result(self, sid: str, now: datetime) -> dict[str, Any] | None: ...
    def human_pending(self, now: datetime, since: datetime | None) -> list[dict[str, Any]]: ...
    def events(self, f: dict[str, Any], v: Viewer, limit: int, after: str | None) -> list[dict[str, Any]]: ...
    def holds(self, now: datetime) -> dict[str, dict[str, Any]]: ...
    def definitions(self) -> list[dict[str, Any]]: ...
    def ping(self) -> None: ...


class Pool:
    """A few read-only connections, opened lazily and replaced when one fails. Every connection is read only and
    bounded by a statement timeout, server-side, whatever the role allows."""

    def __init__(self, dsn: str, size: int = 5, connect: Callable[..., Any] | None = None):
        if connect is None:
            import psycopg
            connect = psycopg.connect
        self.dsn, self.size, self._connect = dsn, size, connect
        self._free: "queue.Queue[Any]" = queue.Queue()
        self._made, self._lock = 0, threading.Lock()

    def _new(self):
        return self._connect(self.dsn, autocommit=True,
                             options=f"-c statement_timeout={STATEMENT_TIMEOUT_MS} -c default_transaction_read_only=on")

    def run(self, sql: str, params: Sequence[Any] = ()) -> list[tuple]:
        try:
            conn = self._free.get_nowait()
        except queue.Empty:
            with self._lock:
                grow = self._made < self.size
                if grow:
                    self._made += 1
            conn = self._new() if grow else self._free.get(timeout=10)
        try:
            with conn.cursor() as cur:
                cur.execute(sql, params)
                rows = cur.fetchall() if cur.description else []
            self._free.put(conn)
            return rows
        except Exception:
            try:
                conn.close()
            finally:
                with self._lock:
                    self._made -= 1
            raise


def _session_view(r: dict[str, Any], now: datetime) -> dict[str, Any]:
    state = "closed" if r.get("closed") else ("expired" if datetime.fromisoformat(r["expires_at"]) <= now else "live")
    return {**r, "state": state}


class PgReadStore:
    """ReadStore on the gateway's tables (clearance pg.py SCHEMA at v0.26.1, or the gateway's own database's: the same
    shapes, `prefix` clearance or gateway). Every query is scoped by the viewer in SQL."""

    def __init__(self, pool: Pool, clock: Callable[[], datetime] = lambda: datetime.now(timezone.utc),
                 prefix: str = "clearance"):
        if prefix not in TABLE_PREFIXES:
            raise ValueError(f"tables are one of {', '.join(TABLE_PREFIXES)}")
        self.pool, self.now = pool, clock
        # A fixed identifier from TABLE_PREFIXES, never caller input, so it is safe to place in the SQL.
        p = prefix
        self.t = {k: f"{p}_{k}" for k in ("sessions", "results", "human_requests", "events", "holds", "definitions")}

    @staticmethod
    def _session_scope(v: Viewer, alias: str = "s") -> tuple[str, list[Any]]:
        if v.all:
            return "TRUE", []
        return (f"({alias}.record->>'principal' = %s OR {alias}.record->>'agent' = ANY(%s))", [v.person, list(v.agents)])

    def sessions(self, f, v, limit, after):
        where, params = [], []
        scope, sp = self._session_scope(v)
        where.append(scope); params += sp
        for key, col in (("agent", "agent"), ("principal", "principal"), ("task", "task_id")):
            if f.get(key):
                where.append(f"s.record->>'{col}' = %s"); params.append(f[key])
        state, now = f.get("state"), self.now()
        if state == "live":
            where.append("NOT s.closed AND s.expires_at > %s"); params.append(now)
        elif state == "closed":
            where.append("s.closed")
        elif state == "expired":
            where.append("NOT s.closed AND s.expires_at <= %s"); params.append(now)
        if after:
            where.append("s.record->>'created_at' < %s"); params.append(after)
        rows = self.pool.run(f"SELECT s.record FROM {self.t['sessions']} s WHERE " + " AND ".join(where)
                             + " ORDER BY s.record->>'created_at' DESC LIMIT %s", params + [limit])
        return [_session_view(r[0], now) for r in rows]

    def session(self, sid):
        rows = self.pool.run(f"SELECT record FROM {self.t['sessions']} WHERE id = %s", [sid])
        return _session_view(rows[0][0], self.now()) if rows else None

    def result(self, sid, now):
        rows = self.pool.run(f"SELECT record FROM {self.t['results']} WHERE session = %s AND expires_at > %s", [sid, now])
        return rows[0][0] if rows else None

    def task(self, task_id):
        now = self.now()
        rows = self.pool.run(f"SELECT record FROM {self.t['sessions']} WHERE record->>'task_id' = %s "
                             "ORDER BY record->>'created_at'", [task_id])
        return [_session_view(r[0], now) for r in rows]

    def human_pending(self, now, since):
        sql, params = (f"SELECT record FROM {self.t['human_requests']} WHERE status = 'pending' AND expires_at > %s", [now])
        if since:
            sql += " AND record->>'created_at' > %s"; params.append(since.isoformat())
        return [r[0] for r in self.pool.run(sql + " ORDER BY record->>'created_at'", params)]

    def events(self, f, v, limit, after):
        """The trigger bridge's history (PgEvents): an app's events to its owners, an agent's to its team, all to an auditor."""
        where, params = [], []
        if not v.all:
            where.append("(e.app = ANY(%s) OR EXISTS (SELECT 1 FROM jsonb_array_elements(e.record->'outcomes') o "
                         "WHERE o->>'agent' = ANY(%s)))")
            params += [list(v.apps), list(v.agents)]
        for key, col in (("app", "e.app"), ("source", "e.source"), ("key", "e.routing_key"),
                         ("subject", "e.record->'subject'->>'id'")):
            if f.get(key):
                where.append(f"{col} = %s"); params.append(f[key])
        if f.get("agent"):
            where.append("EXISTS (SELECT 1 FROM jsonb_array_elements(e.record->'outcomes') o WHERE o->>'agent' = %s)")
            params.append(f["agent"])
        if f.get("since"):
            where.append("e.received_at >= %s"); params.append(f["since"])
        if f.get("until"):
            where.append("e.received_at < %s"); params.append(f["until"])
        if after:
            where.append("e.received_at < %s"); params.append(after)
        sql = f"SELECT e.record FROM {self.t['events']} e" + (" WHERE " + " AND ".join(where) if where else "")
        rows = self.pool.run(sql + " ORDER BY e.received_at DESC LIMIT %s", params + [limit])
        return [r[0] for r in rows]

    def holds(self, now):
        """The active holds on agents' triggers (PgHolds), by agent."""
        rows = self.pool.run(f"SELECT record FROM {self.t['holds']} WHERE until > %s", [now])
        return {r[0]["agent"]: r[0] for r in rows}

    def definitions(self):
        """The cluster-pinned definitions the gateway loaded (PgDefinitions): rows for Definitions.apply_records."""
        rows = self.pool.run(f"SELECT record FROM {self.t['definitions']} ORDER BY state, name")
        return [r[0] for r in rows]

    def ping(self):
        self.pool.run(f"SELECT 1 FROM {self.t['sessions']} LIMIT 1")


# -- the HTTP app -------------------------------------------------------------------------------------------------

def _definition_view(d: Any, full: bool, hold: dict[str, Any] | None = None,
                     source: Any = None) -> dict[str, Any]:
    lim = d.limits.as_dict()
    out = {"name": d.name, "description": getattr(d, "description", "") or None, "kind": d.kind,
           "runsIn": getattr(d, "runs_in", "cluster" if d.image else "client"), "owner": getattr(d, "owner", None),
           "identity": {"type": d.identity_type, "serviceAccount": d.service_account},
           "tierCeiling": lim["tier_ceiling"], "tools": lim["tools"], "models": lim["models"],
           "limits": {k: lim[k] for k in ("ttl_minutes", "tool_calls", "github_calls", "open_prs", "model_tokens", "children")},
           "network": lim["network"], "compute": lim["compute"], "repos": {"allow": list(d.repos_allow)},
           "triggers": list(d.triggers), "runtime": {"framework": d.framework, "sandbox": d.sandbox, "image": d.image},
           "scopes": {k: v for k, v in (d.raw_profile or {}).items() if k in ("glidepath", "airframe")},
           # The active hold on its triggers (control.py), or None: by, via, reason, since, until.
           "hold": hold,
           # Where the definition came from (definitions.py): {kind: bundled} or {kind: cluster, repo, path, commit,
           # namespace, xr, loaded_at, pin}. None when the deployment's definitions are a plain directory.
           "source": source.view() if source is not None else None}
    if full:
        out["instructions"] = d.instructions
    return out


def _limit(q: Mapping[str, str]) -> int:
    try:
        n = int(q.get("limit") or DEFAULT_LIMIT)
    except ValueError:
        raise ApiError(400, "limit must be a number") from None
    return max(1, min(n, MAX_LIMIT))


def create_api_app(store: ReadStore, access: Access, *, clock: Callable[[], datetime] = lambda: datetime.now(timezone.utc),
                   gateway_url: str | None = None, clearance_api_url: str | None = None,
                   runtimes: Sequence[Any] = (), custom_images: Sequence[str] = ()):
    """The Starlette app for `autopilot-gateway api`. `gateway_url` (where writes go) and `clearance_api_url` (where
    Clearance's reads are) are named in /v1/contract."""
    from starlette.applications import Starlette
    from starlette.requests import Request
    from starlette.responses import JSONResponse
    from starlette.routing import Route
    import anyio

    allowed = {r["operationId"]: set(r["roles"]) for r in ROUTES}

    def viewer_for(request: Request, op: str) -> Viewer:
        auth = request.headers.get("authorization", "")
        token = auth[7:].strip() if auth.lower().startswith("bearer ") else None
        v = access.viewer(token, request.headers.get("x-hangar-on-behalf-of") or None)
        role = "delegate" if v.via else v.role
        if role not in allowed[op] and not (role == "auditor" and "person" in allowed[op]):
            raise ApiError(403, f"a {role} may not call {op}")
        return v

    def endpoint(op: str, fn: Callable[[Request, Viewer], Any]):
        async def handle(request: Request) -> JSONResponse:
            try:
                v = viewer_for(request, op)
                out = await anyio.to_thread.run_sync(fn, request, v)
                return JSONResponse(out)
            except ApiError as e:
                return JSONResponse({"error": e.message}, status_code=e.status)
            except Exception as e:  # noqa: BLE001 - a database failure is the API's, never a leak of internals
                return JSONResponse({"error": f"read failed: {type(e).__name__}"}, status_code=503)
        return handle

    def page(items: list[dict[str, Any]], limit: int, cursor_key: str) -> dict[str, Any]:
        nxt = items[-1][cursor_key] if len(items) == limit and items else None
        return {"items": items, "next": nxt}

    def source_of(name):
        src = getattr(access.definitions, "source", None)
        return src(name) if src else None

    def list_agents(req, v):
        full = v.all and req.query_params.get("full") == "1"
        holds = store.holds(clock())
        # `rejected`: pins (Agent XRs) the gateway could not load, with the reason, for the teams that own them (and
        # everyone: a rejected pin names no instructions, only where it points and why it failed).
        rejected = [r.view() for r in getattr(access.definitions, "rejected", ())]
        return {"items": [_definition_view(d, full, holds.get(n), source_of(n)) for n, d in sorted(access.definitions.items())],
                "rejected": rejected}

    def get_agent(req, v):
        name = req.path_params["name"]
        d = access.definitions.get(name)
        if d is None:
            raise ApiError(404, "no such agent")
        return _definition_view(d, v.all and req.query_params.get("full") == "1", store.holds(clock()).get(name), source_of(name))

    def visible(s: dict[str, Any], v: Viewer) -> bool:
        return v.all or s.get("principal") == v.person or s.get("agent") in v.agents

    def list_sessions(req, v):
        q, limit = req.query_params, _limit(req.query_params)
        f = {k: q.get(k) for k in ("agent", "principal", "task", "state") if q.get(k)}
        if f.get("state") and f["state"] not in ("live", "closed", "expired"):
            raise ApiError(400, "state is live, closed or expired")
        return page(store.sessions(f, v, limit, q.get("after")), limit, "created_at")

    def list_events(req, v):
        q, limit = req.query_params, _limit(req.query_params)
        f = {k: q.get(k) for k in ("app", "agent", "source", "key", "subject", "since", "until") if q.get(k)}
        for k in ("since", "until"):
            if f.get(k):
                try:
                    datetime.fromisoformat(f[k].replace("Z", "+00:00"))
                except ValueError:
                    raise ApiError(400, f"{k} is an ISO time") from None
        if f.get("app"):
            f["app"] = f["app"].lower()
        return page(store.events(f, v, limit, q.get("after")), limit, "received_at")

    def get_session(req, v):
        s = store.session(req.path_params["id"])
        if s is None or not visible(s, v):
            raise ApiError(404, "no such session")
        return s

    def get_session_result(req, v):
        s = store.session(req.path_params["id"])
        if s is None or not visible(s, v):
            raise ApiError(404, "no such session")
        r = store.result(s["id"], clock())
        if r is None:
            raise ApiError(404, "this run kept no result, or it expired" if s.get("outcome") else "the run has not finished")
        return r

    def get_task(req, v):
        # The run tree is the task's sessions (their parent links and budgets used); it needs no evidence, so this
        # reads no Clearance table and calls no Clearance API.
        sessions = [s for s in store.task(req.path_params["task_id"]) if visible(s, v)]
        if not sessions:
            raise ApiError(404, "no such task")
        used: dict[str, int] = {}
        for s in sessions:
            for k, n in (s.get("used") or {}).items():
                used[k] = used.get(k, 0) + int(n)
        return {"task_id": req.path_params["task_id"], "sessions": sessions, "used": used}

    def human(req, v):
        since_s = req.query_params.get("since")
        try:
            since = datetime.fromisoformat(since_s) if since_s else None
        except ValueError:
            raise ApiError(400, "since is an ISO time") from None
        pending = store.human_pending(clock(), since)
        if v.role == "notifier" or v.all:
            return {"items": pending}
        groups = access.directory.groups(v.person) if access.directory else set()
        return {"items": [r for r in pending if r.get("owner") == v.person or (r.get("group") and r["group"] in groups)]}

    def list_runtimes(req, v):
        # custom_images: the globs an image built for an agent's own repo must match (the gateway's RUNTIME_IMAGES_EXTRA
        # on Hangar); empty = only the catalog's runtimes are allowed.
        return {"items": [r.view() for r in runtimes], "custom_images": list(custom_images)}

    async def get_contract(_: Request) -> JSONResponse:
        return JSONResponse({**contract(), **({"gateway": gateway_url} if gateway_url else {}),
                             **({"clearance": clearance_api_url} if clearance_api_url else {})})

    async def healthz(_: Request) -> JSONResponse:
        return JSONResponse({"ok": True})

    async def readyz(_: Request) -> JSONResponse:
        try:
            await anyio.to_thread.run_sync(store.ping)
            return JSONResponse({"ok": True})
        except Exception as e:  # noqa: BLE001
            return JSONResponse({"ok": False, "error": type(e).__name__}, status_code=503)

    fns = {"listAgents": list_agents, "getAgent": get_agent, "listSessions": list_sessions, "getSession": get_session,
           "getSessionResult": get_session_result, "getTask": get_task, "listHumanRequests": human,
           "listEvents": list_events, "listRuntimes": list_runtimes}
    # Literal paths before parameterised ones.
    order = sorted((r for r in ROUTES if r["operationId"] in fns), key=lambda r: "{" in r["path"])
    routes = [Route(r["path"], endpoint(r["operationId"], fns[r["operationId"]]), methods=["GET"]) for r in order]
    routes += [Route("/v1/contract", get_contract, methods=["GET"]), Route("/healthz", healthz, methods=["GET"]),
               Route("/readyz", readyz, methods=["GET"])]
    return Starlette(routes=routes)
