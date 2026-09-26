"""Triggers for scheduled and event-driven agents. Definitions are durable (git); each
firing becomes an ephemeral run."""
from __future__ import annotations

import re
from datetime import datetime, timedelta

from .profile import AgentDefinition

_INTERVAL = re.compile(r"^(\d+)([mh])$")


def parse_interval(s: str) -> timedelta:
    m = _INTERVAL.match(s)
    if not m:
        raise ValueError(f"bad interval {s!r}")
    n, unit = int(m.group(1)), m.group(2)
    if n < 1:
        raise ValueError("interval must be positive")
    return timedelta(minutes=n) if unit == "m" else timedelta(hours=n)


def due(defs: dict[str, AgentDefinition], last_run: dict[str, datetime], now: datetime) -> list[str]:
    """Agents with an interval trigger whose interval has elapsed since their last run."""
    out = []
    for name, d in sorted(defs.items()):
        for t in d.triggers:
            if t["type"] != "interval":
                continue
            last = last_run.get(name)
            if last is None or now - last >= parse_interval(t["every"]):
                out.append(name)
                break
    return out


def match_alert(defs: dict[str, AgentDefinition], labels: dict[str, str]) -> list[str]:
    """Agents whose alert trigger's `match` is entirely satisfied by the alert's labels."""
    out = []
    for name, d in sorted(defs.items()):
        for t in d.triggers:
            if t["type"] == "alert" and all(labels.get(k) == v for k, v in t["match"].items()):
                out.append(name)
                break
    return out


def amqp_topic_match(pattern: str, key: str) -> bool:
    """RabbitMQ topic semantics: words split on '.', '*' is exactly one word, '#' is zero or more."""
    def go(p: list[str], k: list[str]) -> bool:
        if not p:
            return not k
        if p[0] == "#":
            return any(go(p[1:], k[i:]) for i in range(len(k) + 1))
        if not k:
            return False
        return (p[0] == "*" or p[0] == k[0]) and go(p[1:], k[1:])
    return go(pattern.split("."), key.split("."))


def match_event(defs: dict[str, AgentDefinition], exchange: str, routing_key: str) -> list[str]:
    """Agents whose event trigger binds this exchange and routing key."""
    out = []
    for name, d in sorted(defs.items()):
        for t in d.triggers:
            if t["type"] == "event" and t["exchange"] == exchange and amqp_topic_match(t["bindingKey"], routing_key):
                out.append(name)
                break
    return out


def event_task_id(agent: str, message_id: str) -> str:
    """A stable task id per (agent, message), so a redelivered message maps to the same run."""
    import hashlib
    return "t-" + hashlib.sha256(f"{agent}:{message_id}".encode()).hexdigest()[:12]


class RateLimiter:
    """Sliding one-hour window per key: the brake on an event storm."""
    def __init__(self, per_hour: int):
        self.per_hour = per_hour
        self._hits: list[datetime] = []

    def allow(self, now: datetime) -> bool:
        self._hits = [h for h in self._hits if now - h < timedelta(hours=1)]
        if len(self._hits) >= self.per_hour:
            return False
        self._hits.append(now)
        return True
