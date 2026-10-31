"""Event history: what the trigger bridge received and what it did with each event.

The bridge (autopilot event_bridge.py) reads glidepath-api's event stream and, per event, starts the agents whose
Glidepath trigger matches, under each one's storm brake. Until now that left only log lines. This module keeps one
row per event for RETENTION: the event's summary (the same one a run is given: id, app, routing key, type, subject)
and the outcome per agent (launched, deduped, braked, refused, launch-failed), so that

  - a trigger dry run (Autopilot's New agent profile, board A3) can replay the last days' events against a draft
    trigger without the bridge knowing about drafts,
  - an agent's profile (board A2) can say how many events its trigger matched, started, deduplicated or braked,
  - Tower's Pipelines tab (board T1) can mark the run an event named ("triage-agent investigating") by the
    event's subject.

This is history, not evidence: nothing is decided or spent here, so it is not in the hash chain. clearance-api
serves it (`GET /v1/events`) scoped like everything else (an app's events to its owners, an agent's to its team).
"""
from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any, Protocol

RETENTION = timedelta(days=30)
MAX_SUBJECT_BYTES = 2000
MAX_OUTCOMES = 50

OUTCOME_KEYS = ("agent", "result", "session", "run", "rule")


@dataclass
class EventRecord:
    id: str
    received_at: datetime
    source: str
    routing_key: str | None
    type: str | None
    app: str | None
    subject: dict[str, Any] = field(default_factory=dict)
    outcomes: list[dict[str, Any]] = field(default_factory=list)

    def view(self) -> dict[str, Any]:
        return {"id": self.id, "received_at": self.received_at.isoformat(), "source": self.source,
                "routing_key": self.routing_key, "type": self.type, "app": self.app, "subject": self.subject,
                "outcomes": self.outcomes}


def _parse_time(s: Any, fallback: datetime) -> datetime:
    if not isinstance(s, str) or not s:
        return fallback
    try:
        t = datetime.fromisoformat(s.replace("Z", "+00:00"))
    except ValueError:
        return fallback
    return t if t.tzinfo else t.replace(tzinfo=timezone.utc)


def from_summary(summary: dict[str, Any], outcomes: list[dict[str, Any]], now: datetime,
                 source: str = "glidepath") -> EventRecord:
    """The record for one handled event: the bridge's summary of it (event_bridge.summarize) and what was done per
    agent. The subject is trimmed to MAX_SUBJECT_BYTES, the outcomes to their known keys."""
    eid = summary.get("id")
    if not isinstance(eid, str) or not eid:
        raise ValueError("an event needs an id")
    subject = dict(summary.get("subject") or {})
    content = dict(subject.get("content") or {}) if isinstance(subject.get("content"), dict) else {}
    subject["content"] = content
    while len(json.dumps(subject, separators=(",", ":"))) > MAX_SUBJECT_BYTES and content:
        content.popitem()
    kept = [{k: o[k] for k in OUTCOME_KEYS if k in o} for o in outcomes if isinstance(o, dict) and o.get("agent")]
    app = summary.get("app")
    return EventRecord(id=eid, received_at=_parse_time(summary.get("receivedAt"), now), source=source,
                       routing_key=summary.get("routingKey") if isinstance(summary.get("routingKey"), str) else None,
                       type=summary.get("type") if isinstance(summary.get("type"), str) else None,
                       app=app.lower() if isinstance(app, str) and app else None,
                       subject=subject, outcomes=kept[:MAX_OUTCOMES])


class EventsStore(Protocol):
    def record(self, ev: EventRecord) -> None: ...


class MemoryEvents:
    """The ephemeral store: a bounded list, newest last."""

    def __init__(self, keep: int = 5000):
        self.keep = keep
        self.records: dict[str, EventRecord] = {}

    def record(self, ev: EventRecord) -> None:
        self.records.pop(ev.id, None)
        self.records[ev.id] = ev
        while len(self.records) > self.keep:
            self.records.pop(next(iter(self.records)))

    def recent(self, limit: int = 100) -> list[EventRecord]:
        return sorted(self.records.values(), key=lambda e: e.received_at, reverse=True)[:limit]
