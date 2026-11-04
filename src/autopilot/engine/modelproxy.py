"""Model access decisions.

Runs never hold a model API key. They call a model proxy that authenticates the run
(TokenReview, in the real adapter), checks its model allowlist, network reach and token
budget, then forwards to whichever backend the route names: a hosted provider, or a
Modelplane InferenceGateway. Modelplane's gateway can run "behind another gateway" and trust
an `x-modelplane-caller` header, so the run's session id becomes the caller identity on its
usage records and the two systems join on it.

This module is the decision core. The Gateway owns one (Gateway.model_authorize / model_usage, durable like every
other operation) and server.create_model_app is the HTTP forwarder in front of it.
"""
from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Callable

from .core import Core, CoreUnreachable, outage_verdict, rfc3339
from .session import SessionStore


# What a call's usage costs the session's model_tokens budget, per kind of token, by what the provider charges for it
# relative to an uncached input token (Anthropic: a cache write is 1.25x, a cache read 0.1x; output is counted at 1
# here, because the budget bounds work done, not money). A run that resends a long conversation under prompt caching
# is charged for what it costs, not for every token again; one that does not cache is charged in full, as before.
USAGE_WEIGHTS = {"input_tokens": 1.0, "output_tokens": 1.0, "cache_creation_input_tokens": 1.25,
                 "cache_read_input_tokens": 0.1}


# The usage kinds a model call's outcome record carries (clearance evidence `usage`, revision 3), every one present.
USAGE_KINDS = tuple(USAGE_WEIGHTS)


def charged_tokens(usage: dict) -> int:
    """The budget charge for one call's `usage` (Anthropic's shape), rounded up."""
    total = 0.0
    for k, w in USAGE_WEIGHTS.items():
        v = usage.get(k)
        if isinstance(v, (int, float)) and not isinstance(v, bool) and v > 0:
            total += v * w
    return int(-(-total // 1))


def usage_record(usage: dict | None) -> dict[str, int]:
    """One call's `usage` as its outcome record carries it: each kind a non-negative integer (0 when the provider left
    it out) and `charged_tokens`, the call's weighted charge (charged_tokens: the weights above)."""
    u = usage if isinstance(usage, dict) else {}
    out = {}
    for k in USAGE_KINDS:
        v = u.get(k)
        out[k] = int(v) if isinstance(v, (int, float)) and not isinstance(v, bool) and v > 0 else 0
    out["charged_tokens"] = charged_tokens(out)
    return out


@dataclass(frozen=True)
class ModelRoute:
    alias: str
    backend: str            # "anthropic" | "openai-compatible" | "modelplane"
    upstream: str           # base URL
    upstream_model: str


@dataclass(frozen=True)
class ModelDecision:
    allow: bool
    rule: str | None
    message: str
    route: ModelRoute | None = None
    headers: dict[str, str] | None = None
    # The core's decision record for this call: the proxy reports its outcome against it (Gateway.model_outcome),
    # once the call ran, so a model call is two records like a tool call, the second written as the gateway.
    decision_id: str | None = None


class ModelProxy:
    """The model decision, by the Clearance core (model.call: M001-M007, decided and recorded there), with the
    spend applied to the gateway's session and the route chosen here."""

    def __init__(self, routes: dict[str, ModelRoute], store: SessionStore, core: Core,
                 clock: Callable[[], datetime]):
        self.routes, self.store, self.core, self.now = routes, store, core, clock

    def authorize(self, session_id: str, principal_id: str, model: str, est_tokens: int,
                  acting: str | None = None) -> ModelDecision:
        """`acting` is who the caller acts as (Gateway._acting: a run acts as its session); the record names the
        caller. An unreachable core is a deny (CORE-UNAVAILABLE): a model call is never an outage read."""
        s = self.store.get(session_id)
        props: dict = {"kind": "workload", "tier_ceiling": 1}
        if acting and acting != principal_id:
            props["acting_as"] = acting
        ctx: dict = {"now": rfc3339(self.now()), "session_id": session_id, "models": {"routes": sorted(self.routes)}}
        if s is not None:
            ctx["session"] = s.state()
        req = {"subject": {"type": "principal", "id": principal_id, "properties": props},
               "action": {"name": "model.call", "properties": {"est_tokens": est_tokens}},
               "resource": {"type": "model", "id": model}, "context": ctx}
        try:
            v = self.core.evaluate(req)
        except CoreUnreachable as e:
            ov = outage_verdict(e)
            return ModelDecision(False, ov.rule, ov.message)
        self.store.charge(s, v.spend)
        if not v.allow:
            return ModelDecision(False, v.rule, v.message)
        route = self.routes.get(model)
        if route is None or s is None:   # the core allowed a model it was told is routed
            msg = f"no route for model {model!r}"
            self.core.outcome(v.decision_id, f"refused M006: {msg}", receipt={"stream": False},
                              refusal=("M006", msg))
            return ModelDecision(False, "M006", msg)
        headers = {"x-modelplane-caller": s.id, "x-hangar-task": s.task_id}
        return ModelDecision(True, None, v.message or "allowed", route, headers, v.decision_id)

    def outcome(self, decision_id: str | None, result: str, *, stream: bool, upstream_status: int | None = None,
                usage: dict[str, int] | None = None, model_version: str | None = None) -> int | None:
        """Report an allowed model call's result to the core (POST /v1/decisions/{id}/outcome), written as the gateway's
        provider: `result` is "ok" or "error:<Kind>", and the receipt (flat, claimed) says whether the call streamed
        and what the upstream answered. The flight recorder's fields go on the same outcome (A-4): `usage`
        (usage_record: the four token kinds and charged_tokens, the call's weighted charge) when the call used
        any, and `model_version`, the model that answered. Never raises: an outcome the core cannot take now waits
        in the outbox."""
        receipt: dict = {"stream": bool(stream)}
        if upstream_status is not None:
            receipt["upstream_status"] = int(upstream_status)
        return self.core.outcome(decision_id, result, receipt=receipt, usage=usage, model_version=model_version)

    def record_usage(self, session_id: str, tokens: int, usage: dict[str, int] | None = None) -> None:
        """Charge what the model actually used. Overrunning the budget trips the breaker. `usage` (usage_record) is
        added to the session's running cost (Session.usage: the read API's "on the session", run.finish's cost).

        The two differ only on an overrun, on purpose: the budget (used["model_tokens"]) counts the charge up to its
        limit, as it always has, so `remaining` never goes negative and a parent's reservation is never overdrawn;
        the running cost and the call's outcome record keep the whole charge, because cost accounting (A-4) is what
        the call cost, not what the budget let it count. The breaker trips, so no further model call is allowed."""
        s = self.store.get(session_id)
        if s is None:
            return
        if usage is not None:
            s.add_usage(usage)
        room = s.remaining("model_tokens")
        s.used["model_tokens"] += min(tokens, max(room, 0))
        if tokens > room:
            s.trip("model-token overrun")
