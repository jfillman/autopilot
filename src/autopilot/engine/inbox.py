"""human.request's inbox: an agent's question to a person, held by Clearance until someone allowed answers it.

Non-blocking by design. human.request records the question and returns its id at once; the agent reads the answer
with human.request.get whenever it chooses (polling, or after other work): whether to wait is the agent's and its
developers' decision, not the gateway's.

Who may answer: the session's owner (its principal), and the members of the agent definition's `owner` group (a
Backstage group ref, e.g. group:default/skyport), so a request from a triggered run, which no person opened, still
has someone to answer it. Group membership comes from a Directory (the deployment's: on Hangar, Tower's catalog);
without one, only the owner may answer. A person answers directly (their own sign-in) or through a delegate (Tower's
backend) that names them; either way the evidence records who answered and through what.

A request expires with its session: an expired one is never listed and cannot be answered.
"""
from __future__ import annotations

import re
import secrets
from dataclasses import asdict, dataclass, field
from datetime import datetime
from typing import Any, Callable, Iterable, Protocol

MAX_QUESTION = 2000
MAX_OPTIONS = 10
MAX_OPTION = 200
MAX_ANSWER = 4000
MAX_PENDING = 5          # per session: a run cannot flood its people
REQUEST_ID = re.compile(r"^h-[0-9a-f]{12}$")


class InboxError(Exception):
    """A refusal the agent or the person can act on: `rule`, `message`, `hint`. status is the HTTP status for people."""
    def __init__(self, rule: str, message: str, hint: str = "", status: int = 400):
        self.rule, self.message, self.hint, self.status = rule, message, hint, status
        super().__init__(message)


@dataclass
class HumanRequest:
    id: str
    task_id: str
    session: str
    agent: str
    owner: str                    # the session's principal
    group: str | None             # the definition's owner group, if any
    question: str
    options: list[str]
    created_at: datetime
    expires_at: datetime
    status: str = "pending"       # pending | answered (expired is computed)
    answer: str | None = None
    answered_by: str | None = None
    answered_via: str | None = None   # the delegate a person answered through, or None
    answered_at: datetime | None = None
    extra: dict[str, Any] = field(default_factory=dict)

    def state(self, now: datetime) -> str:
        return "expired" if self.status == "pending" and now >= self.expires_at else self.status

    def view(self, now: datetime) -> dict[str, Any]:
        d = {k: v for k, v in asdict(self).items() if k != "extra"}
        for k in ("created_at", "expires_at", "answered_at"):
            d[k] = d[k].isoformat() if d[k] else None
        d["status"] = self.state(now)
        return d


class InboxStore(Protocol):
    def add(self, r: HumanRequest) -> None: ...
    def get(self, request_id: str) -> HumanRequest | None: ...
    def save(self, r: HumanRequest) -> None: ...
    def pending(self, now: datetime) -> list[HumanRequest]: ...
    def count_pending(self, session: str, now: datetime) -> int: ...


class Directory(Protocol):
    """The groups a person is in (Backstage group refs), for the owner-group rule. Empty when unknown."""
    def groups(self, principal: str) -> set[str]: ...


class MemoryInbox:
    def __init__(self):
        self._by_id: dict[str, HumanRequest] = {}

    def add(self, r: HumanRequest) -> None:
        self._by_id[r.id] = r

    def get(self, request_id: str) -> HumanRequest | None:
        return self._by_id.get(request_id)

    def save(self, r: HumanRequest) -> None:
        self._by_id[r.id] = r

    def pending(self, now: datetime) -> list[HumanRequest]:
        return sorted((r for r in self._by_id.values() if r.state(now) == "pending"), key=lambda r: r.created_at)

    def count_pending(self, session: str, now: datetime) -> int:
        return sum(1 for r in self._by_id.values() if r.session == session and r.state(now) == "pending")


def new_id() -> str:
    return "h-" + secrets.token_hex(6)


def check_question(question: Any, options: Any) -> tuple[str, list[str]]:
    if not isinstance(question, str) or not question.strip() or len(question) > MAX_QUESTION:
        raise InboxError("HR-QUESTION", f"a question is 1 to {MAX_QUESTION} characters",
                         "Ask one clear question; put detail in an artifact and name it.")
    opts = [] if options is None else options
    if (not isinstance(opts, list) or len(opts) > MAX_OPTIONS
            or not all(isinstance(o, str) and o.strip() and len(o) <= MAX_OPTION for o in opts)
            or len(set(opts)) != len(opts)):
        raise InboxError("HR-OPTIONS", f"options are up to {MAX_OPTIONS} distinct strings of up to {MAX_OPTION} characters",
                         "Offer a short list of choices, or none for a free-text answer.")
    return question.strip(), [o.strip() for o in opts]


def may_answer(r: HumanRequest, person: str, groups: Iterable[str]) -> bool:
    return person == r.owner or (r.group is not None and r.group in set(groups))


def check_answer(r: HumanRequest, answer: Any, now: datetime) -> str:
    state = r.state(now)
    if state == "expired":
        raise InboxError("HR-EXPIRED", "this request expired with its session", "The agent is gone; nothing to answer.", 410)
    if state != "pending":
        raise InboxError("HR-ANSWERED", f"already answered by {r.answered_by}", "Each request takes one answer.", 409)
    if not isinstance(answer, str) or not answer.strip() or len(answer) > MAX_ANSWER:
        raise InboxError("HR-ANSWER", f"an answer is 1 to {MAX_ANSWER} characters", "Write the answer.")
    if r.options and answer.strip() not in r.options:
        raise InboxError("HR-ANSWER", "the answer must be one of the request's options",
                         "Pick one of: " + ", ".join(r.options))
    return answer.strip()


Clock = Callable[[], datetime]
