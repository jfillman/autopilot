"""Sessions: identity, budgets, expiry, breaker, and the run tree.

A session is one agent run's authority. It is created narrower than its definition, and a
child session is created narrower than its parent and *reserves* its budget from the
parent, so the sum of a team's spend can never exceed what the root was granted.
"""
from __future__ import annotations

import secrets
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Callable

from .limits import BUDGET_FIELDS, Claim, Limits, narrow
from .tiers import Tier

MAX_DEPTH = 3
DENIAL_TRIP = 5


class SessionError(Exception):
    def __init__(self, code: str, message: str):
        self.code = code
        super().__init__(message)


@dataclass
class Session:
    id: str
    principal: str
    agent: str
    limits: Limits
    created_at: datetime
    expires_at: datetime
    parent_id: str | None = None
    depth: int = 0
    task_id: str = ""
    used: dict[str, int] = field(default_factory=lambda: {f: 0 for f in BUDGET_FIELDS})
    reserved: dict[str, int] = field(default_factory=lambda: {f: 0 for f in BUDGET_FIELDS})
    children: list[str] = field(default_factory=list)
    tripped: bool = False
    trip_reason: str | None = None
    denials: int = 0
    closed: bool = False

    # -- budgets ---------------------------------------------------------
    def remaining(self, f: str) -> int:
        return getattr(self.limits, f) - self.used[f] - self.reserved[f]

    def can_spend(self, f: str, n: int = 1) -> bool:
        return n <= self.remaining(f)

    def spend(self, f: str, n: int = 1) -> None:
        if not self.can_spend(f, n):
            raise SessionError("budget", f"{f} exhausted")
        self.used[f] += n

    def minutes_left(self, now: datetime) -> int:
        return max(0, int((self.expires_at - now).total_seconds() // 60))

    def expired(self, now: datetime) -> bool:
        return self.closed or now >= self.expires_at

    # -- breaker ---------------------------------------------------------
    def trip(self, reason: str) -> None:
        if not self.tripped:
            self.tripped = True
            self.trip_reason = reason

    def record_denial(self) -> None:
        self.denials += 1
        if self.denials >= DENIAL_TRIP:
            self.trip(f"{self.denials} denied calls")


class SessionStore:
    def __init__(self, id_gen: Callable[[], str] | None = None):
        self._s: dict[str, Session] = {}
        self._id = id_gen or (lambda: "s-" + secrets.token_hex(6))

    def get(self, sid: str) -> Session | None:
        return self._s.get(sid)

    def open(self, principal: str, agent: str, base: Limits, claim: Claim | None, now: datetime,
             parent_id: str | None = None, task_id: str | None = None) -> Session:
        parent = self._s.get(parent_id) if parent_id else None
        if parent_id and parent is None:
            raise SessionError("no-parent", f"unknown parent session {parent_id}")
        if parent is not None:
            if parent.expired(now) or parent.tripped:
                raise SessionError("parent-unavailable", "parent is expired or tripped")
            if parent.depth + 1 > MAX_DEPTH:
                raise SessionError("depth", f"run tree deeper than {MAX_DEPTH}")
            if not parent.can_spend("children", 1):
                raise SessionError("budget", "children budget exhausted")
            # The child starts from what the parent has *left*, not what it started with.
            base = Limits(
                tier_ceiling=min(base.tier_ceiling, parent.limits.tier_ceiling),
                ttl_minutes=min(base.ttl_minutes, parent.minutes_left(now)),
                **{f: min(getattr(base, f), parent.remaining(f)) for f in BUDGET_FIELDS},
                tools=base.tools & parent.limits.tools,
                models=base.models & parent.limits.models,
                network=min(base.network, parent.limits.network),
                compute=min(base.compute, parent.limits.compute),
            )
        limits = narrow(base, claim or Claim())
        if limits.ttl_minutes < 1:
            raise SessionError("ttl", "no time left to run")
        expires = now + timedelta(minutes=limits.ttl_minutes)
        if parent is not None:
            expires = min(expires, parent.expires_at)
        sid = self._id()
        s = Session(id=sid, principal=principal, agent=agent, limits=limits,
                    created_at=now, expires_at=expires,
                    parent_id=parent.id if parent else None,
                    depth=(parent.depth + 1) if parent else 0,
                    task_id=(parent.task_id if parent else (task_id or "t-" + sid.removeprefix("s-"))))
        if parent is not None:
            for f in BUDGET_FIELDS:
                if f != "children":
                    parent.reserved[f] += getattr(limits, f)
            parent.used["children"] += 1
            parent.children.append(s.id)
        self._s[s.id] = s
        return s

    def find_live(self, agent: str, task_id: str, now: datetime) -> "Session | None":
        for s in self._s.values():
            if s.agent == agent and s.task_id == task_id and not s.expired(now) and s.parent_id is None:
                return s
        return None

    def close(self, sid: str) -> None:
        s = self._s.get(sid)
        if s is None or s.closed:
            return
        for c in list(s.children):
            self.close(c)
        s.closed = True
        if s.parent_id and (p := self._s.get(s.parent_id)):
            for f in BUDGET_FIELDS:
                if f != "children":
                    p.reserved[f] -= getattr(s.limits, f)
                    p.used[f] += s.used[f]

    def trip(self, sid: str, reason: str) -> None:
        s = self._s.get(sid)
        if s:
            s.trip(reason)
            for c in s.children:
                self.trip(c, f"parent tripped: {reason}")

    def sweep(self, now: datetime) -> list[str]:
        """Close everything past its deadline. The AgentRun composition enforces expiry on
        its own; this only keeps Clearance's own view consistent."""
        gone = [s.id for s in self._s.values() if not s.closed and now >= s.expires_at]
        for sid in gone:
            self.close(sid)
        return gone
