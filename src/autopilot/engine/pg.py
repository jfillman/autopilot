"""Postgres: the gateway's own database (GATEWAY_DSN). Runtime state, not evidence.

Sessions, the human inbox, run results, artifacts, trigger history, holds, the cluster-pinned definition set and the
outbox (what the gateway owes the Clearance core) live here. The evidence chain does not: the core writes every
record (core.py), in its own database, which the gateway never opens. Moved from clearance pg.py (v0.26.1) with the
same table shapes, renamed clearance_* -> gateway_*, less clearance_evidence and clearance_changes (the core's) and
plus gateway_outbox. tools/cutover/copy_runtime_tables.py copies the runtime tables across at the R-4 cutover.

One connection, one transaction per gateway operation: an operation's writes (an inbox row, a hold, an outbox
entry) are made on it, and store.flush() saves the sessions it changed and commits once, so they land together.

One writer only. Opening a GatewayState takes a session-level advisory lock and fails if another gateway holds it.
"""
from __future__ import annotations

import re
from datetime import datetime, timedelta, timezone
from typing import Any, Iterable

import psycopg
from psycopg.types.json import Jsonb

from .core import Denied
from .outbox import REASONS

LOCK_KEY = 0x61706730  # "apg0": one gateway writer per database (the core's is "clr0", in its own database)
RECENT = timedelta(days=1)  # closed or expired sessions kept in memory this long, for the run tree's lookups

SCHEMA = """
CREATE TABLE IF NOT EXISTS gateway_sessions (
  id         text PRIMARY KEY,
  record     jsonb NOT NULL,
  expires_at timestamptz NOT NULL,
  closed     boolean NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS gateway_sessions_live ON gateway_sessions (closed, expires_at);
CREATE TABLE IF NOT EXISTS gateway_artifacts (
  task_id    text        NOT NULL,
  name       text        NOT NULL,
  content    text        NOT NULL,
  bytes      integer     NOT NULL,
  expires_at timestamptz NOT NULL,
  written_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, name)
);
CREATE INDEX IF NOT EXISTS gateway_artifacts_expiry ON gateway_artifacts (expires_at);
CREATE TABLE IF NOT EXISTS gateway_human_requests (
  id         text        PRIMARY KEY,
  session    text        NOT NULL,
  status     text        NOT NULL,
  expires_at timestamptz NOT NULL,
  record     jsonb       NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS gateway_human_requests_pending ON gateway_human_requests (status, expires_at);
CREATE TABLE IF NOT EXISTS gateway_events (
  id          text        PRIMARY KEY,
  received_at timestamptz NOT NULL,
  source      text        NOT NULL,
  app         text,
  routing_key text,
  record      jsonb       NOT NULL,
  written_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS gateway_events_received ON gateway_events (received_at DESC);
CREATE INDEX IF NOT EXISTS gateway_events_app ON gateway_events (app, received_at DESC);
CREATE TABLE IF NOT EXISTS gateway_holds (
  agent      text        PRIMARY KEY,
  until      timestamptz NOT NULL,
  record     jsonb       NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS gateway_results (
  session    text        PRIMARY KEY,
  task_id    text        NOT NULL,
  record     jsonb       NOT NULL,
  expires_at timestamptz NOT NULL,
  written_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS gateway_results_expiry ON gateway_results (expires_at);
CREATE TABLE IF NOT EXISTS gateway_definitions (
  name       text        NOT NULL,
  state      text        NOT NULL,
  record     jsonb       NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (name, state)
);
CREATE TABLE IF NOT EXISTS gateway_outbox (
  id          bigserial   PRIMARY KEY,
  op          text        NOT NULL CHECK (op IN ('record', 'outcome')),
  kind        text,
  decision_id text,
  body        jsonb       NOT NULL,
  reason      text        NOT NULL CHECK (reason IN ('outage', 'core-unavailable', 'held', 'refused')),
  state       text        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'done', 'refused')),
  error       text,
  seq         bigint,
  created_at  timestamptz NOT NULL DEFAULT now(),
  posted_at   timestamptz
);
CREATE INDEX IF NOT EXISTS gateway_outbox_pending ON gateway_outbox (id) WHERE state = 'pending';
-- clearance v0.28.0: the core writes definition.* records itself when the gateway publishes its set (PUT
-- /v1/definitions), and POST /v1/records refuses them. A definition.* row an older gateway held can never be posted,
-- and the set it describes is published whole, so it goes.
DELETE FROM gateway_outbox WHERE state = 'pending' AND reason = 'held' AND kind LIKE 'definition.%';
"""


class WriterLocked(RuntimeError):
    pass


class GatewayState:
    def __init__(self, dsn: str, connect=psycopg.connect):
        self.conn = connect(dsn, autocommit=False)
        with self.conn.cursor() as cur:
            cur.execute("SELECT pg_try_advisory_lock(%s)", (LOCK_KEY,))
            if not cur.fetchone()[0]:
                self.conn.close()
                raise WriterLocked("another gateway is writing to this database")
        self.migrate()

    def migrate(self) -> None:
        with self.conn.cursor() as cur:
            cur.execute(SCHEMA)
        self.conn.commit()

    def commit(self) -> None:
        self.conn.commit()

    def close(self) -> None:
        self.conn.close()


class PgSessions:
    """SessionStore persistence on gateway_sessions. save() upserts and commits the operation's transaction (its
    inbox, hold, result and outbox rows included); a failed commit rolls it back and the error reaches the caller."""

    def __init__(self, state: GatewayState, now=lambda: datetime.now(timezone.utc)):
        self.state, self.now = state, now

    def load(self) -> Iterable[dict[str, Any]]:
        with self.state.conn.cursor() as cur:
            cur.execute("SELECT record FROM gateway_sessions WHERE NOT closed OR expires_at > %s",
                        (self.now() - RECENT,))
            rows = [r[0] for r in cur.fetchall()]
        self.state.commit()
        return rows

    def save(self, records: list[dict[str, Any]]) -> None:
        try:
            with self.state.conn.cursor() as cur:
                for r in records:
                    cur.execute(
                        "INSERT INTO gateway_sessions (id, record, expires_at, closed) VALUES (%s, %s, %s, %s) "
                        "ON CONFLICT (id) DO UPDATE SET record = EXCLUDED.record, expires_at = EXCLUDED.expires_at, "
                        "closed = EXCLUDED.closed, updated_at = now()",
                        (r["id"], Jsonb(r), datetime.fromisoformat(r["expires_at"]), r["closed"]))
            self.state.commit()
        except Exception:
            self.state.conn.rollback()
            raise


ARTIFACT_NAME = re.compile(r"^[A-Za-z0-9._-]{1,100}$")


class PgArtifacts:
    """ArtifactBackend on gateway_artifacts: text a task's sessions keep for each other (a child's report, a plan),
    keyed by task, so a whole run tree shares them and nothing else sees them. Bounded and expiring: each artifact at
    most `max_bytes`, a task at most `max_count` and `max_total` bytes, gone `ttl` after its last write. A put with
    an existing name replaces it. Writes join the call's transaction."""

    def __init__(self, state: GatewayState, now=lambda: datetime.now(timezone.utc), ttl: timedelta = timedelta(days=7),
                 max_bytes: int = 256 << 10, max_count: int = 32, max_total: int = 2 << 20):
        self.state, self.now, self.ttl = state, now, ttl
        self.max_bytes, self.max_count, self.max_total = max_bytes, max_count, max_total

    def put(self, task_id: str, name: str, content: str) -> dict[str, Any]:
        if not isinstance(name, str) or not ARTIFACT_NAME.match(name):
            raise Denied("ART-NAME", f"{name!r} is not an artifact name", "Use letters, digits, '.', '_' or '-'.")
        if not isinstance(content, str):
            raise Denied("ART-CONTENT", "an artifact's content is text", "Pass a string (JSON-encode structured data).")
        size = len(content.encode())
        if size > self.max_bytes:
            raise Denied("ART-SIZE", f"{size} bytes is over the {self.max_bytes}-byte limit for one artifact",
                         "Store a summary, or split it.")
        now = self.now()
        with self.state.conn.cursor() as cur:
            cur.execute("DELETE FROM gateway_artifacts WHERE expires_at <= %s", (now,))
            cur.execute("SELECT count(*), coalesce(sum(bytes), 0) FROM gateway_artifacts WHERE task_id = %s AND name <> %s",
                        (task_id, name))
            count, total = cur.fetchone()
            if count + 1 > self.max_count or total + size > self.max_total:
                raise Denied("ART-QUOTA", f"this task may keep {self.max_count} artifacts and {self.max_total} bytes",
                             "Replace an existing artifact by its name instead of adding one.")
            expires = now + self.ttl
            cur.execute("INSERT INTO gateway_artifacts (task_id, name, content, bytes, expires_at) "
                        "VALUES (%s, %s, %s, %s, %s) ON CONFLICT (task_id, name) DO UPDATE SET "
                        "content = EXCLUDED.content, bytes = EXCLUDED.bytes, expires_at = EXCLUDED.expires_at, "
                        "written_at = now()", (task_id, name, content, size, expires))
        return {"stored": name, "bytes": size, "expires_at": expires.isoformat()}

    def get(self, task_id: str, name: str) -> str | None:
        with self.state.conn.cursor() as cur:
            cur.execute("SELECT content FROM gateway_artifacts WHERE task_id = %s AND name = %s AND expires_at > %s",
                        (task_id, name, self.now()))
            row = cur.fetchone()
        return row[0] if row else None


class PgResults:
    """ResultStore on gateway_results: a finished run's answer, by session, until it expires (results.py). A put
    joins the run_finish call's transaction, so the answer commits with the closed session; each put first deletes
    what has expired."""

    def __init__(self, state: GatewayState):
        self.state = state

    def put(self, record: dict[str, Any], now: datetime) -> None:
        with self.state.conn.cursor() as cur:
            cur.execute("DELETE FROM gateway_results WHERE expires_at <= %s", (now,))
            cur.execute("INSERT INTO gateway_results (session, task_id, record, expires_at) VALUES (%s, %s, %s, %s) "
                        "ON CONFLICT (session) DO NOTHING",
                        (record["session"], record["task_id"], Jsonb(record), datetime.fromisoformat(record["expires_at"])))

    def get(self, session_id: str, now: datetime) -> dict[str, Any] | None:
        with self.state.conn.cursor() as cur:
            cur.execute("SELECT record FROM gateway_results WHERE session = %s AND expires_at > %s", (session_id, now))
            row = cur.fetchone()
        return row[0] if row else None


class PgInbox:
    """InboxStore on gateway_human_requests. Writes join the operation's transaction."""

    def __init__(self, state: GatewayState):
        self.state = state

    @staticmethod
    def _load(rec: dict[str, Any]):
        from .inbox import HumanRequest
        d = dict(rec)
        for k in ("created_at", "expires_at", "answered_at"):
            d[k] = datetime.fromisoformat(d[k]) if d.get(k) else None
        return HumanRequest(**d)

    def _put(self, r) -> None:
        from dataclasses import asdict
        rec = asdict(r)
        for k in ("created_at", "expires_at", "answered_at"):
            rec[k] = rec[k].isoformat() if rec[k] else None
        with self.state.conn.cursor() as cur:
            cur.execute("INSERT INTO gateway_human_requests (id, session, status, expires_at, record) "
                        "VALUES (%s, %s, %s, %s, %s) ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, "
                        "record = EXCLUDED.record, updated_at = now()",
                        (r.id, r.session, r.status, r.expires_at, Jsonb(rec)))

    add = save = _put

    def get(self, request_id: str):
        with self.state.conn.cursor() as cur:
            cur.execute("SELECT record FROM gateway_human_requests WHERE id = %s", (request_id,))
            row = cur.fetchone()
        return self._load(row[0]) if row else None

    def pending(self, now: datetime) -> list:
        with self.state.conn.cursor() as cur:
            cur.execute("SELECT record FROM gateway_human_requests WHERE status = 'pending' AND expires_at > %s "
                        "ORDER BY record->>'created_at'", (now,))
            return [self._load(r[0]) for r in cur.fetchall()]

    def count_pending(self, session: str, now: datetime) -> int:
        with self.state.conn.cursor() as cur:
            cur.execute("SELECT count(*) FROM gateway_human_requests WHERE session = %s AND status = 'pending' "
                        "AND expires_at > %s", (session, now))
            return cur.fetchone()[0]


class PgEvents:
    """EventsStore on gateway_events (events.py): one row per event the trigger bridge handled, kept
    events.RETENTION. Every PRUNE_EVERY writes, rows past retention are deleted."""

    PRUNE_EVERY = 500

    def __init__(self, state: GatewayState):
        self.state = state
        self._writes = 0

    def record(self, ev) -> None:
        from .events import RETENTION
        with self.state.conn.cursor() as cur:
            cur.execute("INSERT INTO gateway_events (id, received_at, source, app, routing_key, record) "
                        "VALUES (%s, %s, %s, %s, %s, %s) ON CONFLICT (id) DO UPDATE SET record = EXCLUDED.record, "
                        "written_at = now()",
                        (ev.id, ev.received_at, ev.source, ev.app, ev.routing_key, Jsonb(ev.view())))
            self._writes += 1
            if self._writes % self.PRUNE_EVERY == 0:
                cur.execute("DELETE FROM gateway_events WHERE received_at < %s",
                            (datetime.now(timezone.utc) - RETENTION,))


class PgHolds:
    """HoldStore on gateway_holds (control.py): one row per held agent, written in the control's transaction."""

    def __init__(self, state: GatewayState):
        self.state = state

    def get(self, agent: str):
        from .control import Hold
        with self.state.conn.cursor() as cur:
            cur.execute("SELECT record FROM gateway_holds WHERE agent = %s", (agent,))
            row = cur.fetchone()
        return Hold.from_record(row[0]) if row else None

    def put(self, hold) -> None:
        with self.state.conn.cursor() as cur:
            cur.execute("INSERT INTO gateway_holds (agent, until, record) VALUES (%s, %s, %s) "
                        "ON CONFLICT (agent) DO UPDATE SET until = EXCLUDED.until, record = EXCLUDED.record, "
                        "updated_at = now()", (hold.agent, hold.until, Jsonb(hold.view())))

    def delete(self, agent: str) -> None:
        with self.state.conn.cursor() as cur:
            cur.execute("DELETE FROM gateway_holds WHERE agent = %s", (agent,))

    def all(self) -> list:
        from .control import Hold
        with self.state.conn.cursor() as cur:
            cur.execute("SELECT record FROM gateway_holds ORDER BY agent")
            return [Hold.from_record(r[0]) for r in cur.fetchall()]


class PgDefinitions:
    """DefinitionsStore on gateway_definitions (definitions.py): the cluster-pinned set, replaced whole in the
    loader's transaction (a few rows), for restarts and the read API."""

    def __init__(self, state: GatewayState):
        self.state = state

    def replace(self, records: list) -> None:
        with self.state.conn.cursor() as cur:
            cur.execute("DELETE FROM gateway_definitions")
            for r in records:
                cur.execute("INSERT INTO gateway_definitions (name, state, record) VALUES (%s, %s, %s)",
                            (r["name"], r["state"], Jsonb(r)))

    def all(self) -> list:
        with self.state.conn.cursor() as cur:
            cur.execute("SELECT record FROM gateway_definitions ORDER BY state, name")
            return [row[0] for row in cur.fetchall()]


class PgOutbox:
    """outbox.Outbox on gateway_outbox. Rows join the operation's transaction (an entry commits with the session
    state of the call that queued it). has_pending is answered from memory while nothing has been queued since the
    last drain found the box empty, so a call with nothing waiting costs no query."""

    def __init__(self, state: GatewayState):
        self.state = state
        with state.conn.cursor() as cur:
            cur.execute("SELECT EXISTS (SELECT 1 FROM gateway_outbox WHERE state = 'pending')")
            self._maybe = bool(cur.fetchone()[0])
        state.commit()

    def add(self, entry: dict[str, Any], reason: str, error: str | None = None) -> int:
        if reason not in REASONS:
            raise ValueError(f"outbox reason {reason!r}")
        with self.state.conn.cursor() as cur:
            cur.execute("INSERT INTO gateway_outbox (op, kind, decision_id, body, reason, state, error) "
                        "VALUES (%s, %s, %s, %s, %s, %s, %s) RETURNING id",
                        (entry["op"], entry.get("kind"), entry.get("decision_id"), Jsonb(entry["body"]), reason,
                         "refused" if reason == "refused" else "pending", error))
            oid = cur.fetchone()[0]
        if reason != "refused":
            self._maybe = True
        return oid

    def _rows(self, kinds: frozenset[str], limit: int) -> list[tuple[int, dict[str, Any]]]:
        with self.state.conn.cursor() as cur:
            cur.execute("SELECT id, op, kind, decision_id, body FROM gateway_outbox WHERE state = 'pending' "
                        "AND (op = 'outcome' OR kind = ANY(%s)) ORDER BY id LIMIT %s", (sorted(kinds), limit))
            rows = cur.fetchall()
        out = []
        for oid, op, kind, decision_id, body in rows:
            e: dict[str, Any] = {"op": op, "body": body}
            if op == "outcome":
                e["decision_id"] = decision_id
            else:
                e["kind"] = kind
            out.append((oid, e))
        return out

    def pending(self, kinds: frozenset[str], limit: int) -> list[tuple[int, dict[str, Any]]]:
        if not self._maybe:
            return []
        rows = self._rows(kinds, limit)
        if not rows:
            self._maybe = False
        return rows

    def has_pending(self, kinds: frozenset[str]) -> bool:
        if not self._maybe:
            return False
        return bool(self._rows(kinds, 1))

    def done(self, oid: int, seq: int | None) -> None:
        with self.state.conn.cursor() as cur:
            cur.execute("UPDATE gateway_outbox SET state = 'done', seq = %s, posted_at = now() WHERE id = %s", (seq, oid))

    def refused(self, oid: int, error: str) -> None:
        with self.state.conn.cursor() as cur:
            cur.execute("UPDATE gateway_outbox SET state = 'refused', error = %s WHERE id = %s", (error, oid))

    def all(self) -> list[dict[str, Any]]:
        with self.state.conn.cursor() as cur:
            cur.execute("SELECT id, op, kind, decision_id, body, reason, state, error, seq FROM gateway_outbox ORDER BY id")
            return [{"id": r[0], "entry": {"op": r[1], "kind": r[2], "decision_id": r[3], "body": r[4]},
                     "reason": r[5], "state": r[6], "error": r[7], "seq": r[8]} for r in cur.fetchall()]


def open_durable(dsn: str, id_gen=None):
    """A GatewayState and a SessionStore loaded from it: what a deployment passes to Gateway."""
    from .session import SessionStore
    state = GatewayState(dsn)
    store = SessionStore(id_gen=id_gen, persist=PgSessions(state))
    return state, store


def table_rows(state: GatewayState, table: str) -> int:   # for the cutover script's checks and tests
    if not re.fullmatch(r"gateway_[a-z_]+", table):
        raise ValueError(table)
    with state.conn.cursor() as cur:
        cur.execute(f"SELECT count(*) FROM {table}")  # noqa: S608 - the name is checked above
        return cur.fetchone()[0]

