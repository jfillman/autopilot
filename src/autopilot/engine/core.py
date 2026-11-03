"""The seam between the gateway engine and Clearance: the Clearance core's HTTP API, through its generated client.

The engine enforces; the core decides and records (Hangar's boundary rule, 2026-11-02). R-3a moved the gateway here
with Clearance's decision and record still called in-process through this module; R-3b replaced it with the core's
API (`clearance_client`, generated from contract/clearance-core.json), and nothing here imports `clearance`.

  evaluate   POST /access/v1/evaluation before every tool call, model call, session open and spawn. The answer is a
             Verdict: allow or deny with its rule and hint, the spend the gateway applies to the session itself, the
             grant (the session to create) for an open or spawn, and the decision record's seq.
  outcome    POST /v1/decisions/{id}/outcome once an allowed call ran: its result line, effect and receipt.
  record     POST /v1/records for the gateway's own runtime records (run.launch, run.delete, run.finish,
             human.answer, session.freeze, session.end, trigger.hold, trigger.release).
  publish_definitions
             PUT /v1/definitions: the whole cluster-pinned definition set (the Agent XR loader's), which the core
             decides again itself and records once per change (definition.load, .unload, .reject). The gateway
             writes no definition.* record; the core is their one writer.

The gateway writes no evidence. What it cannot hand the core at once waits in its outbox (outbox.py), in its own
database, and is posted in order once the core answers again (Core.drain):
  - a record or outcome the core could not take while it was unreachable;
  - a T0 read allowed during an outage (GATEWAY_OUTAGE_ALLOW_READS), posted as a coverage-unavailable `tool.call`
    record naming the tool;
  - a record of a kind the core does not accept yet (`held`), posted once the client (and so the core) lists the
    kind in RECORD_KINDS. Since clearance v0.28.0 the core takes every kind the gateway writes, so nothing new is
    held; the reason stays for a rolling upgrade, and a row held by an older gateway is posted by drain.
The definition set never waits in the outbox: a publish the core could not take is made again, whole, on the next
poll (Gateway.apply_cluster_definitions).

Outage policy (decided 2026-11-02, Hangar spec R-3). The client's own settings are the policy's: 250 ms per attempt,
one retry, a breaker that opens after 5 consecutive failures. An unreachable core (CoreUnavailable) is a deny for
every T1-and-above call, every session open, every spawn and every model call; a T0 read is denied too unless
the deployment allows reads (Core.allow_reads). A core that answers but refuses the request (a 4xx, an unreadable
answer) is a deny as well, never an allow.

AuthError and Denied are the engine's own (they were clearance.dispatch's until R-3b): the authenticators (auth.py)
raise AuthError, a handler raises Denied for a refusal the core's rules cannot see.
"""
from __future__ import annotations

import hashlib
import json
import logging
import re
import threading
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Mapping, Sequence

from clearance_client import (RECORD_KINDS, BadResponse, ClearanceClientError, CoreClient, CoreRequestError,
                              CoreUnavailable)

from .outbox import MemoryOutbox, Outbox

log = logging.getLogger("autopilot.engine.core")

PROVIDER = "autopilot-gateway"

# The rule a call carries when the core could not decide it (the outage policy), and when the core refused the
# request itself (fail closed: a 4xx or an unreadable answer is no decision).
OUTAGE_RULE = "CORE-UNAVAILABLE"
REFUSED_RULE = "CORE-REFUSED"
OUTAGE_HINT = "Clearance is unreachable, so nothing that needs its decision runs. Retry in a minute."

# A handler refusal's rule as the core takes it on an outcome (`refusal.rule`, internal/server/outcome.go): R004,
# HR-003, ART-002. A rule outside it is sent without the refusal (the result line still names it).
REFUSAL_RULE = re.compile(r"[A-Z][A-Z0-9]*(-[A-Z0-9]+)*")

# A model version as the core takes it (internal/server/cost.go): a provider's model name. One that is not is left
# off the outcome rather than have the core refuse the whole outcome.
MODEL_VERSION = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,199}")

# PUT /v1/definitions carries the whole set (up to 256 pins, 4 MiB), so it gets its own client with a longer
# timeout than the decision path's 250 ms, and its own breaker: a slow publish never opens the decisions' breaker.
DEFINITIONS_TIMEOUT = 5.0


class AuthError(Exception):
    pass


class Denied(Exception):
    """Raised by a handler that must refuse for a reason the core's rules cannot see."""
    def __init__(self, rule: str, message: str, hint: str = ""):
        self.rule, self.message, self.hint = rule, message, hint
        super().__init__(message)


class CoreUnreachable(Exception):
    """No decision: the core is unreachable (the outage policy applies) or refused the request (refused=True)."""
    def __init__(self, message: str, *, refused: bool = False):
        self.refused = refused
        super().__init__(message)

    @property
    def rule(self) -> str:
        return REFUSED_RULE if self.refused else OUTAGE_RULE


# -- the record helpers (canonical digests and timestamps, as the core computes them: internal/canon) ----------------

def canonical(obj: Any) -> bytes:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def sha256_hex(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def args_digest(args: Any) -> str:
    return sha256_hex(canonical(args))


def iso(dt: datetime) -> str:
    """The one timestamp format records (and anything that names a session's expiry) use."""
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def rfc3339(dt: datetime) -> str:
    """A datetime as the evaluation request carries it (aware; a naive one is UTC)."""
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.isoformat()


# -- the answer -----------------------------------------------------------------------------------------------------

@dataclass(frozen=True)
class Verdict:
    """One evaluation's answer, as the engine uses it."""
    allow: bool
    rule: str | None
    name: str | None
    message: str
    hint: str
    tier: int | None
    spend: Mapping[str, Any] = field(default_factory=dict)
    grant: Mapping[str, Any] | None = None
    problems: tuple[dict[str, Any], ...] = ()
    decision_id: str | None = None
    seq: int | None = None

    def as_dict(self) -> dict[str, Any]:
        """The decision as the agent sees it (unchanged from the in-process engine's Decision.as_dict)."""
        out = {"allow": self.allow, "rule": self.rule, "name": self.name, "message": self.message,
               "hint": self.hint, "tier": self.tier}
        if self.problems:
            out["problems"] = [dict(p) for p in self.problems]
        return out


def _verdict(res) -> Verdict:
    c = res.context
    spend = {k: v for k, v in (c.spend.to_dict() if c.spend is not None else {}).items() if v not in (None, 0, False, "")}
    grant = c.grant.to_dict() if c.grant is not None else None
    problems = tuple(p.to_dict() for p in (c.problems or ()))
    return Verdict(bool(res.decision), c.rule or None, c.name or None, c.message or "", c.hint or "", c.tier,
                   spend, grant, problems, res.decision_id, res.record.seq if res.record is not None else None)


def outage_verdict(e: CoreUnreachable, tier: int | None = None) -> Verdict:
    msg = ("Clearance refused the request: " + str(e)) if e.refused else "Clearance is unreachable: no decision"
    return Verdict(False, e.rule, "core-unreachable" if not e.refused else "core-refused", msg, OUTAGE_HINT, tier)


# -- the client -----------------------------------------------------------------------------------------------------

class Core:
    """The gateway's Clearance core: one client (one breaker, one connection pool), the outbox, the outage switch."""

    def __init__(self, client: CoreClient, outbox: Outbox | None = None, *, allow_reads: bool = False,
                 provider: str = PROVIDER, record_kinds: frozenset[str] | tuple[str, ...] = tuple(RECORD_KINDS),
                 definitions_client: CoreClient | None = None):
        self.client, self.outbox = client, outbox if outbox is not None else MemoryOutbox()
        # The definition set's publish (publish_definitions): its own client when given, else the decisions' one.
        self.definitions_client = definitions_client if definitions_client is not None else client
        self.allow_reads, self.provider, self.record_kinds = allow_reads, provider, frozenset(record_kinds)
        self._drain_lock = threading.Lock()

    # -- decisions --------------------------------------------------------------------------------------------------
    def evaluate(self, request: Mapping[str, Any]) -> Verdict:
        """Decide one evaluation. Raises CoreUnreachable when there is no decision (the caller denies)."""
        try:
            res = self.client.evaluate(request)
        except CoreUnavailable as e:
            log.warning("clearance core unavailable: %s", e.reason)
            raise CoreUnreachable(e.reason) from None
        except (CoreRequestError, BadResponse) as e:
            log.error("clearance core refused an evaluation: %s", e)
            raise CoreUnreachable(str(getattr(e, "message", "") or e), refused=True) from None
        except ClearanceClientError as e:   # TokenUnavailable: nothing was sent
            log.error("clearance core: %s", type(e).__name__)
            raise CoreUnreachable(type(e).__name__) from None
        return _verdict(res)

    # -- outcomes and records ---------------------------------------------------------------------------------------
    def outcome(self, decision_id: str | None, result: str, effect: dict[str, Any] | None = None,
                receipt: dict[str, Any] | None = None, *, refusal: tuple[str, str] | None = None,
                usage: dict[str, int] | None = None, model_version: str | None = None) -> int | None:
        """Report an allowed decision's result. Never raises: an outcome the core cannot take now waits in the
        outbox (the call already ran). `refusal` is (rule, message) when a handler refused the call after the core
        allowed it (Denied): the record keeps the decision's rule and adds the handler's as `refusal` (claimed). The
        outcome record's seq, or None when it is queued. `usage` and `model_version` are a model call's (A-4): what it
        used and was charged (modelproxy.usage_record), and the model that answered."""
        if not decision_id:
            return None
        body: dict[str, Any] = {"result": result}
        if effect:
            body["effect"] = effect
        if receipt:
            body["receipt"] = receipt
        if usage is not None:
            body["usage"] = dict(usage)
        if model_version and MODEL_VERSION.fullmatch(model_version):
            body["model_version"] = model_version
        if refusal is not None:
            rule, message = refusal
            if len(rule) <= 64 and REFUSAL_RULE.fullmatch(rule) and message:
                body["refusal"] = {"rule": rule, "message": message.encode()[:4000].decode(errors="ignore")}
                body.pop("effect", None)   # a refused call made no write
        return self._send({"op": "outcome", "decision_id": decision_id, "body": body}, required=False)

    def record(self, kind: str, fields: Mapping[str, Any], *, required: bool = False) -> int | None:
        """One of the gateway's own records. A kind the core does not accept yet is held in the outbox. When the
        core is unreachable: `required` raises CoreUnreachable (the caller fails closed and undoes what it did), else
        the record waits in the outbox. The record's seq, or None when it is queued or held."""
        entry = {"op": "record", "kind": kind, "body": {k: v for k, v in fields.items() if v is not None}}
        if kind not in self.record_kinds:
            self.outbox.add(entry, "held")
            log.info("record %s held: the core does not accept this kind yet", kind)
            return None
        return self._send(entry, required=required)

    def publish_definitions(self, loaded: Sequence[Any], rejected: Sequence[Any] = ()) -> bool:
        """Hand the core the whole cluster-pinned definition set (PUT /v1/definitions): each definitions.Loaded as
        its source, its XR owner and its document, each definitions.Rejected as its name, source and reason. The
        core decides every pin again and records what changed. Never raises: False when the core did not take it
        (unreachable, or it refused), and the caller publishes the whole set again later. Nothing is queued."""
        pins = [{"source": _pin_source(l.source), "owner": l.definition.owner, "doc": dict(l.doc)} for l in loaded]
        rej = [{"name": r.name, "source": _pin_source(r.source), "reason": r.reason} for r in rejected]
        try:
            res = self.definitions_client.publish_definitions(pins, rej, provider=self.provider)
        except CoreUnavailable as e:
            log.warning("clearance core unavailable: the definition set waits for the next poll (%s)", e.reason)
            return False
        except ClearanceClientError as e:
            log.error("clearance core refused the definition set: %s", e)
            return False
        log.info("definitions published: +%d ~%d -%d rejected %d cleared %d (the core holds %d)",
                 len(res.added), len(res.changed), len(res.removed), len(res.rejected), len(res.cleared), res.cluster)
        return True

    def outage_read(self, fields: Mapping[str, Any]) -> None:
        """A T0 read allowed while the core was unreachable: posted once it answers, as coverage-unavailable."""
        body = {k: v for k, v in fields.items() if v is not None}
        body["coverage"] = "coverage-unavailable"
        self.outbox.add({"op": "record", "kind": "tool.call", "body": body}, "outage")

    def _send(self, entry: dict[str, Any], *, required: bool) -> int | None:
        if self.outbox.has_pending(self.record_kinds):
            # Keep the order: what waits goes first. If the core is still down, this one waits behind it.
            self.drain()
            if self.outbox.has_pending(self.record_kinds):
                if required:
                    raise CoreUnreachable("the core has records still to take")
                self.outbox.add(entry, "core-unavailable")
                return None
        try:
            return self._post(entry)
        except CoreUnavailable as e:
            if required:
                raise CoreUnreachable(e.reason) from None
            self.outbox.add(entry, "core-unavailable")
            log.warning("clearance core unavailable: %s queued", entry.get("kind") or "outcome")
            return None
        except (CoreRequestError, BadResponse, ClearanceClientError) as e:
            # The core answered and refused: retrying it unchanged gets the same answer. Kept, with why, for a person.
            log.error("clearance core refused %s: %s", entry.get("kind") or "an outcome", e)
            if required:
                raise CoreUnreachable(str(e), refused=True) from None
            self.outbox.add(entry, "refused", error=str(e)[:500])
            return None

    def _post(self, entry: dict[str, Any]) -> int:
        if entry["op"] == "outcome":
            return self.client.outcome(entry["decision_id"], entry["body"]).seq
        return self.client.record(entry["kind"], entry["body"], provider=self.provider).seq

    def drain(self, limit: int = 500) -> int:
        """Post what waits in the outbox, oldest first, stopping at the first one the core cannot take now. Held
        records of a kind the core does not accept stay held. Returns how many were posted."""
        if not self._drain_lock.acquire(blocking=False):
            return 0
        try:
            sent = 0
            for oid, entry in self.outbox.pending(self.record_kinds, limit):
                try:
                    seq = self._post(entry)
                except CoreUnavailable:
                    break
                except CoreRequestError as e:
                    if entry["op"] == "outcome" and e.status == 409:
                        # It landed before (the answer was lost): done, nothing to post.
                        self.outbox.done(oid, None)
                        continue
                    log.error("clearance core refused a queued %s: %s", entry.get("kind") or "outcome", e)
                    self.outbox.refused(oid, str(e)[:500])
                    continue
                except ClearanceClientError as e:
                    self.outbox.refused(oid, str(e)[:500])
                    continue
                self.outbox.done(oid, seq)
                sent += 1
            return sent
        finally:
            self._drain_lock.release()

    @property
    def breaker_state(self) -> str:
        return self.client.breaker_state


def _pin_source(source: Any) -> dict[str, Any]:
    """A definitions.Source as PUT /v1/definitions takes it (PinSource, strictly decoded): the view less `kind`
    (always cluster here) and `pin` (derived)."""
    return {k: v for k, v in source.view().items() if k not in ("kind", "pin")}


def client_from_env(url: str, token_file: str | None, *, timeout: float | None = None) -> CoreClient:
    """The core's client with the outage policy's defaults (the client's own: 250 ms, one retry, breaker at 5)."""
    kw: dict[str, Any] = {"provider": PROVIDER}
    if token_file:
        kw["token_file"] = token_file
    if timeout is not None:
        kw["timeout"] = timeout
    return CoreClient(url, **kw)


def local_core() -> Core:
    """A Core on a local development core (`clearance core --memory --insecure-dev-auth`, where the bearer token is
    the principal): CLEARANCE_CORE_URL (default http://127.0.0.1:8090), CLEARANCE_CORE_TOKEN (default the gateway's
    principal). For the live smoke tools (tools/*_smoke.py), never a deployment."""
    import os
    return Core(CoreClient(os.environ.get("CLEARANCE_CORE_URL") or "http://127.0.0.1:8090",
                           token=os.environ.get("CLEARANCE_CORE_TOKEN") or "autopilot-system/autopilot-gateway",
                           provider=PROVIDER))


__all__ = ["AuthError", "Core", "CoreUnreachable", "DEFINITIONS_TIMEOUT", "Denied", "OUTAGE_RULE", "PROVIDER", "REFUSED_RULE", "Verdict",
           "args_digest", "canonical", "client_from_env", "iso", "local_core", "outage_verdict", "rfc3339", "sha256_hex"]
