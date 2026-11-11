"""The trigger ledger: what makes a trigger launch once, across restarts, replays and the reconciliation sweep (A-5).

Every trigger firing has a key. One row per (agent, key) is claimed before anything is opened, and the row's state
is the decision made for it: `launched`, `deduped`, `braked`, `held`, `refused`, `launch-failed` or, for an interval
slot that passed with no gateway to fire it, `missed`. A second claim of the same (agent, key) loses, whoever makes
it: the event stream after a restart, the `/v1/events/recent` catch-up, the reconciliation sweep or the scheduler.
The one exception is a claim left `claimed` (the gateway died between the claim and the decision): it may be claimed
again once it is older than STALE or than the claiming process itself (`stale_before`: one writer holds the
database, so a claim from before this process started is an orphan), and Gateway.open_triggered answers a still-live
run of the same task as a dedupe.

Keys (trigger_key, slot_key):
  - `<app>.pipelinerun.<predicate>/<pipelinerun name>` for a pipelinerun event: the routing key and the CDEvent's
    subject id (the PipelineRun's name), both documented in glidepath docs/admin/api.md. A finished run read back
    from glidepath-api's run records (GET /v1/runs) gives the same key, so the reconciliation sweep and the stream
    agree on what "this run's pipelinerun.finished" is without knowing how glidepath-api forms its event ids.
  - `event:<id>` for any other Glidepath event: the glidepath-api event id, the same on every replica.
  - `slot:<every>:<n>` for an interval slot (scheduler.py).

The cursor (one row per source) is the newest event id the bridge has handled, saved after each event, so a restart
resumes from it with `/v1/events/recent?since=` and then `Last-Event-ID`. Its `floor` is when the ledger first ran for
that source: the reconciliation sweep and the missed-slot check never reach back before it, so turning A-5 on does not
launch a day of old failures.

MemoryTriggers is the ephemeral store (AUTOPILOT_EPHEMERAL, tests); pg.py PgTriggers is the durable one
(gateway_trigger_launches, gateway_event_cursors).
"""
from __future__ import annotations

import threading
from dataclasses import dataclass
from datetime import datetime, timedelta
from typing import Any, Protocol

STALE = timedelta(minutes=5)        # a claim with no decision after this may be claimed again
RETENTION = timedelta(days=30)      # rows kept this long (the run records reach back 30 days at most)

STATES = ("claimed", "launched", "deduped", "braked", "held", "refused", "launch-failed", "missed")
VIA = ("stream", "recent", "reconcile", "schedule", "rabbitmq")


def trigger_key(envelope: dict[str, Any]) -> str | None:
    """The ledger key of a glidepath-api event envelope (see the module docstring); None without an id."""
    eid = envelope.get("id")
    rk, app = envelope.get("routingKey"), envelope.get("app")
    subject = ((envelope.get("event") or {}).get("subject") or {}) if isinstance(envelope.get("event"), dict) else {}
    sid = subject.get("id") if isinstance(subject, dict) else None
    if (isinstance(rk, str) and isinstance(app, str) and app and rk.startswith(app + ".pipelinerun.")
            and isinstance(sid, str) and sid):
        return f"{rk}/{sid}"
    if isinstance(eid, str) and eid:
        return f"event:{eid}"
    return None


def slot_key(every: str, slot: int) -> str:
    return f"slot:{every}:{slot}"


@dataclass
class Cursor:
    source: str
    last_id: str | None
    floor: datetime


class TriggerStore(Protocol):
    def claim(self, agent: str, key: str, via: str, event_id: str | None, now: datetime,
              stale_before: datetime | None = None) -> bool: ...
    def decide(self, agent: str, key: str, state: str, session: str | None = None, run: str | None = None) -> None: ...
    def get(self, agent: str, key: str) -> dict[str, Any] | None: ...
    def cursor(self, source: str, now: datetime) -> Cursor: ...
    def advance(self, source: str, last_id: str) -> None: ...
    def prune(self, now: datetime) -> int: ...


def stale_before(started: datetime, now: datetime) -> datetime:
    """Claims older than this are orphans: those from before this process started, and any older than STALE."""
    return max(started, now - STALE)


def check(via: str | None = None, state: str | None = None) -> None:
    if via is not None and via not in VIA:
        raise ValueError(f"trigger via {via!r}")
    if state is not None and state not in STATES:
        raise ValueError(f"trigger state {state!r}")


class MemoryTriggers:
    """The ephemeral ledger and cursors: what PgTriggers keeps, in dicts."""

    def __init__(self):
        self.rows: dict[tuple[str, str], dict[str, Any]] = {}
        self.cursors: dict[str, Cursor] = {}
        self._lock = threading.Lock()

    def claim(self, agent: str, key: str, via: str, event_id: str | None, now: datetime,
              stale_before: datetime | None = None) -> bool:
        check(via)
        stale = stale_before or now - STALE
        with self._lock:
            row = self.rows.get((agent, key))
            if row is not None and not (row["state"] == "claimed" and row["claimed_at"] <= stale):
                return False
            self.rows[(agent, key)] = {"agent": agent, "key": key, "via": via, "event_id": event_id,
                                       "state": "claimed", "session": None, "run": None, "claimed_at": now}
            return True

    def decide(self, agent: str, key: str, state: str, session: str | None = None, run: str | None = None) -> None:
        check(state=state)
        with self._lock:
            row = self.rows.get((agent, key))
            if row is not None:
                row.update(state=state, session=session, run=run)

    def get(self, agent: str, key: str) -> dict[str, Any] | None:
        row = self.rows.get((agent, key))
        return dict(row) if row else None

    def cursor(self, source: str, now: datetime) -> Cursor:
        with self._lock:
            return self.cursors.setdefault(source, Cursor(source, None, now))

    def advance(self, source: str, last_id: str) -> None:
        with self._lock:
            c = self.cursors[source]
            if c.last_id is None or last_id > c.last_id:   # ids order by receive time as plain strings
                c.last_id = last_id

    def prune(self, now: datetime) -> int:
        with self._lock:
            old = [k for k, r in self.rows.items() if now - r["claimed_at"] > RETENTION]
            for k in old:
                del self.rows[k]
            return len(old)
