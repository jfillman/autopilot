"""A run's result: what an in-cluster run answered when it finished (v0.25.0).

The runtime writes /run/output/result.json inside its pod, and the pod is gone with its namespace minutes later, so
before it exits it hands the same result to Clearance (the MCP tool run_finish, Gateway.finish_run). The answer is
kept here for RETENTION, by session, for the read API (GET /v1/sessions/{id}/result) and Tower's run detail. The
session record keeps only the outcome (status, turns, counts, and the answer's size and sha256), and the evidence
record run.finish only the decision, never the text: the chain records what was decided, not what an agent said.

A record: {session, task_id, agent, principal, status, final_text, truncated, turns, tool_calls: [{tool, ok, rule}],
reason, written_at, expires_at}.
"""
from __future__ import annotations

from datetime import datetime, timedelta
from typing import Any, Protocol

RETENTION = timedelta(days=30)
MAX_TEXT_BYTES = 16 << 10      # the answer, cut at this many bytes (truncated: true)
MAX_TOOL_CALLS = 200           # the per-call summary kept with the answer
MAX_REASON = 500
STATUSES = ("done", "budget", "expired", "error")


class ResultStore(Protocol):
    def put(self, record: dict[str, Any], now: datetime) -> None: ...
    def get(self, session_id: str, now: datetime) -> dict[str, Any] | None: ...


class MemoryResults:
    def __init__(self):
        self.rows: dict[str, dict[str, Any]] = {}

    def put(self, record, now):
        self.rows = {k: r for k, r in self.rows.items() if datetime.fromisoformat(r["expires_at"]) > now}
        self.rows[record["session"]] = record

    def get(self, session_id, now):
        r = self.rows.get(session_id)
        return r if r and datetime.fromisoformat(r["expires_at"]) > now else None


def clip_text(text: str) -> tuple[str, bool]:
    """The answer cut to MAX_TEXT_BYTES on a character boundary, and whether it was cut."""
    raw = text.encode()
    if len(raw) <= MAX_TEXT_BYTES:
        return text, False
    return raw[:MAX_TEXT_BYTES].decode(errors="ignore"), True


def tool_call_summary(calls: Any) -> list[dict[str, Any]]:
    """The runtime's per-call list, reduced to what it may claim: a tool name, ok, and the refusing rule."""
    out = []
    for c in calls if isinstance(calls, list) else []:
        if isinstance(c, dict) and isinstance(c.get("tool"), str):
            rule = c.get("rule")
            out.append({"tool": c["tool"][:100], "ok": bool(c.get("ok")),
                        "rule": rule[:40] if isinstance(rule, str) else None})
        if len(out) >= MAX_TOOL_CALLS:
            break
    return out
