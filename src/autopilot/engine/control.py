"""Person controls: freeze or end a run, hold or release an agent's triggers (the write path).

Decided 2026-10-31 (Autopilot and Clearance in Backstage: UI design): people act through the delegate model, the
same one the human inbox uses. A person signs in to Clearance themselves (people-dex), or a delegate (Tower's
backend) calls as itself and names them in X-Hangar-On-Behalf-Of. Clearance decides who may act:

  - an agent's owner group (the definition's `owner`, a Backstage group the directory resolves),
  - a platform admin (CLEARANCE_ADMINS, principal globs),
  - and for a session, the person whose session it is.

Every control is an evidence record naming the person and the delegate (`session.freeze`, `session.end`,
`trigger.hold`, `trigger.release`), so the chain says who stopped what and why. A hold is an emergency brake with an
expiry (24 h by default, renewable, at most 14 days); anything meant to last is a definition PR, so git stays the
source of truth for what an agent may do. While held, Gateway.open_triggered refuses with R024.
"""
from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timedelta
from fnmatch import fnmatchcase
from typing import Any, Iterable, Protocol, Sequence

DEFAULT_HOLD_HOURS = 24
MAX_HOLD_HOURS = 24 * 14
MAX_REASON = 500


class ControlError(Exception):
    """A refusal the person can act on: `rule`, `message`, `hint`; status is the HTTP status."""

    def __init__(self, rule: str, message: str, hint: str = "", status: int = 400):
        self.rule, self.message, self.hint, self.status = rule, message, hint, status
        super().__init__(message)


@dataclass
class Hold:
    agent: str
    by: str                 # the person (user:<login>)
    via: str | None         # the delegate they acted through, if any
    reason: str
    since: datetime
    until: datetime

    def active(self, now: datetime) -> bool:
        return now < self.until

    def view(self) -> dict[str, Any]:
        return {"agent": self.agent, "by": self.by, "via": self.via, "reason": self.reason,
                "since": self.since.isoformat(), "until": self.until.isoformat()}

    @classmethod
    def from_record(cls, r: dict[str, Any]) -> "Hold":
        return cls(agent=r["agent"], by=r["by"], via=r.get("via"), reason=r["reason"],
                   since=datetime.fromisoformat(r["since"]), until=datetime.fromisoformat(r["until"]))


class HoldStore(Protocol):
    def get(self, agent: str) -> Hold | None: ...
    def put(self, hold: Hold) -> None: ...
    def delete(self, agent: str) -> None: ...
    def all(self) -> list[Hold]: ...


class MemoryHolds:
    def __init__(self) -> None:
        self._h: dict[str, Hold] = {}

    def get(self, agent: str) -> Hold | None:
        return self._h.get(agent)

    def put(self, hold: Hold) -> None:
        self._h[hold.agent] = hold

    def delete(self, agent: str) -> None:
        self._h.pop(agent, None)

    def all(self) -> list[Hold]:
        return list(self._h.values())


def may_manage(person: str, owner: str | None, groups: Iterable[str], admins: Sequence[str]) -> bool:
    """The agent's owner group, or a platform admin."""
    return (owner is not None and owner in set(groups)) or any(fnmatchcase(person, g) for g in admins)


def check_reason(reason: Any) -> str:
    if not isinstance(reason, str) or not reason.strip():
        raise ControlError("CT-REASON", "a reason is required", "Say why, in a sentence; it is recorded.", 400)
    if len(reason) > MAX_REASON:
        raise ControlError("CT-REASON", f"the reason is longer than {MAX_REASON} characters", "", 400)
    return reason.strip()


def check_hours(hours: Any) -> int:
    if hours is None or hours == "":
        return DEFAULT_HOLD_HOURS
    try:
        n = int(hours)
    except (TypeError, ValueError):
        raise ControlError("CT-HOURS", "hours is a whole number", "", 400) from None
    if not 1 <= n <= MAX_HOLD_HOURS:
        raise ControlError("CT-HOURS", f"hours is 1 to {MAX_HOLD_HOURS}", "A hold is a brake, not a policy: anything longer is a definition PR.", 400)
    return n


def hold_until(now: datetime, hours: int) -> datetime:
    return now + timedelta(hours=hours)
