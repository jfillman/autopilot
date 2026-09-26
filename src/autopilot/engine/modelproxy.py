"""Model access decisions.

Runs never hold a model API key. They call a Hangar model proxy that authenticates the run
(TokenReview, in the real adapter), checks its model allowlist, network reach and token
budget, then forwards to whichever backend the route names: a hosted provider, or a
Modelplane InferenceGateway. Modelplane's gateway can run "behind another gateway" and trust
an `x-modelplane-caller` header, so the run's session id becomes the caller identity on its
usage records and the two systems join on it.

This module is the decision core only. The HTTP forwarder is not built.
"""
from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Callable

from . import manifests
from .audit import AuditLog, args_digest
from .limits import Network
from .session import SessionStore


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


class ModelProxy:
    def __init__(self, routes: dict[str, ModelRoute], store: SessionStore, audit: AuditLog,
                 clock: Callable[[], datetime]):
        self.routes, self.store, self.audit, self.now = routes, store, audit, clock

    def authorize(self, session_id: str, principal_id: str, model: str, est_tokens: int) -> ModelDecision:
        s = self.store.get(session_id)
        d = self._decide(s, principal_id, model, est_tokens)
        self.audit.append({
            "ts": manifests.iso(self.now()), "task_id": s.task_id if s else None, "session": session_id,
            "parent": s.parent_id if s else None, "principal": principal_id, "agent": s.agent if s else None,
            "tool": "model.call", "tier": None, "decision": "allow" if d.allow else "deny", "rule": d.rule,
            "target": model, "args_sha256": args_digest({"model": model, "est": est_tokens}),
            "result": "ok" if d.allow else d.message})
        if not d.allow and s is not None:
            s.record_denial()
        return d

    def _decide(self, s, principal_id, model, est) -> ModelDecision:
        if s is None or s.expired(self.now()):
            return ModelDecision(False, "M001", "no live session")
        if s.principal != principal_id:
            return ModelDecision(False, "M002", "not this session's principal")
        if s.tripped:
            return ModelDecision(False, "M003", "session breaker is tripped")
        if s.limits.network < Network.CLEARANCE_MODEL:
            return ModelDecision(False, "M004", "this run's network mode does not include the model proxy")
        if model not in s.limits.models:
            return ModelDecision(False, "M005", f"model {model!r} is not on this session's allowlist")
        route = self.routes.get(model)
        if route is None:
            return ModelDecision(False, "M006", f"no route for model {model!r}")
        if est < 0 or not s.can_spend("model_tokens", est):
            return ModelDecision(False, "M007", "model-token budget would be exceeded")
        headers = {"x-modelplane-caller": s.id, "x-hangar-task": s.task_id}
        return ModelDecision(True, None, "allowed", route, headers)

    def record_usage(self, session_id: str, tokens: int) -> None:
        """Charge what the model actually used. Overrunning the budget trips the breaker."""
        s = self.store.get(session_id)
        if s is None:
            return
        room = s.remaining("model_tokens")
        s.used["model_tokens"] += min(tokens, max(room, 0))
        if tokens > room:
            s.trip("model-token overrun")
