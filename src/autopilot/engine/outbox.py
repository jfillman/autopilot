"""The gateway's outbox: what it owes the Clearance core and could not hand it yet, kept in order (core.Core.drain).

An entry is {"op": "record", "kind", "body"} (POST /v1/records) or {"op": "outcome", "decision_id", "body"}
(POST /v1/decisions/{id}/outcome), with why it waits:

  outage            a T0 read allowed while the core was unreachable (GATEWAY_OUTAGE_ALLOW_READS), posted as a
                    coverage-unavailable `tool.call` record
  core-unavailable  a record or outcome the core could not take when it was written
  held              a record of a kind the core does not accept yet; posted once it does
  refused           the core answered and refused it (kept with the error, for a person; never retried)

States: pending (outage, core-unavailable, held), done (posted, with the record's seq), refused. Not evidence: the
core writes the record when the entry is posted; until then this is the gateway's own state, like a session.
"""
from __future__ import annotations

import itertools
import threading
from datetime import datetime, timezone
from typing import Any, Iterable, Protocol

REASONS = ("outage", "core-unavailable", "held", "refused")


def sendable(entry: dict[str, Any], kinds: Iterable[str]) -> bool:
    """An outcome always; a record when the core accepts its kind."""
    return entry["op"] == "outcome" or entry.get("kind") in kinds


class Outbox(Protocol):
    def add(self, entry: dict[str, Any], reason: str, error: str | None = None) -> int: ...
    def pending(self, kinds: frozenset[str], limit: int) -> list[tuple[int, dict[str, Any]]]: ...
    def has_pending(self, kinds: frozenset[str]) -> bool: ...
    def done(self, oid: int, seq: int | None) -> None: ...
    def refused(self, oid: int, error: str) -> None: ...
    def all(self) -> list[dict[str, Any]]: ...


class MemoryOutbox:
    """The outbox in memory: an ephemeral gateway and tests."""

    def __init__(self):
        self._rows: dict[int, dict[str, Any]] = {}
        self._ids = itertools.count(1)
        self._lock = threading.Lock()

    def add(self, entry: dict[str, Any], reason: str, error: str | None = None) -> int:
        if reason not in REASONS:
            raise ValueError(f"outbox reason {reason!r}")
        with self._lock:
            oid = next(self._ids)
            self._rows[oid] = {"id": oid, "entry": entry, "reason": reason,
                               "state": "refused" if reason == "refused" else "pending", "error": error, "seq": None,
                               "created_at": datetime.now(timezone.utc).isoformat(), "posted_at": None}
            return oid

    def pending(self, kinds: frozenset[str], limit: int) -> list[tuple[int, dict[str, Any]]]:
        with self._lock:
            rows = [r for r in self._rows.values() if r["state"] == "pending" and sendable(r["entry"], kinds)]
        return [(r["id"], r["entry"]) for r in sorted(rows, key=lambda r: r["id"])[:limit]]

    def has_pending(self, kinds: frozenset[str]) -> bool:
        with self._lock:
            return any(r["state"] == "pending" and sendable(r["entry"], kinds) for r in self._rows.values())

    def done(self, oid: int, seq: int | None) -> None:
        with self._lock:
            self._rows[oid].update(state="done", seq=seq, posted_at=datetime.now(timezone.utc).isoformat())

    def refused(self, oid: int, error: str) -> None:
        with self._lock:
            self._rows[oid].update(state="refused", error=error)

    def all(self) -> list[dict[str, Any]]:
        with self._lock:
            return [dict(r) for r in sorted(self._rows.values(), key=lambda r: r["id"])]
