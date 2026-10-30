"""The gateway core, independent of any transport.

authenticate -> find session -> policy -> audit -> budget -> backend -> audit result.
Every call, allowed or denied, produces exactly one decision record, so "what did the
agent try to do" is answerable from the audit log alone.

A deployment builds its Gateway from parts it names: the packs whose policy it runs (pack.py),
a handler per tool (backends.core_handlers plus its packs' handlers), and a RunBackend for the run
tree. Nothing is found by import: a Gateway with no packs is a complete Clearance for the core tools.
"""
from __future__ import annotations

import functools
import threading
from dataclasses import dataclass
from datetime import datetime
from typing import Any, Callable, Mapping, Protocol, Sequence

from fnmatch import fnmatchcase

from . import policy, provenance
from .audit import AuditLog, args_digest, iso
from .backends import Handler, RunBackend
from .limits import LimitRequest, WideningError
from .modelproxy import ModelDecision, ModelProxy, ModelRoute
from .pack import Pack
from .profile import AgentDefinition
from .session import Session, SessionError, SessionStore
from .tiers import Tier


@dataclass(frozen=True)
class Principal:
    id: str
    kind: str                 # "user" (delegated agent) or "workload"
    tier_ceiling: Tier = Tier.T1


class Authenticator(Protocol):
    def authenticate(self, token: str) -> Principal: ...


class AuthError(Exception):
    pass


class Denied(Exception):
    """Raised by a handler that must refuse for a reason the CEL rules cannot see."""
    def __init__(self, rule: str, message: str, hint: str = ""):
        self.rule, self.message, self.hint = rule, message, hint
        super().__init__(message)


@dataclass
class Result:
    ok: bool
    decision: dict[str, Any]
    data: Any = None
    error: str | None = None
    audit_seq: int | None = None
    session: str | None = None

    def as_dict(self) -> dict[str, Any]:
        return {"ok": self.ok, "decision": self.decision, "data": self.data,
                "error": self.error, "audit_seq": self.audit_seq, "session": self.session}


# Handlers the Gateway owns because they need the session store; a deployment cannot replace them.
CORE_OWNED = frozenset({"run.spawn", "run.close"})


def _durable(op):
    """End every public operation with store.flush(), whatever path it returned by: the sessions it changed, and
    (with a durable audit sink sharing the store's transaction) its audit records, commit together."""
    @functools.wraps(op)
    def run(self, *a, **k):
        try:
            return op(self, *a, **k)
        finally:
            self.store.flush()
    return run


class Gateway:
    def __init__(self, definitions: dict[str, AgentDefinition], store: SessionStore,
                 audit: AuditLog, auth: Authenticator, clock: Callable[[], datetime], *,
                 packs: Sequence[Pack] = (), handlers: Mapping[str, Handler] | None = None,
                 runs: RunBackend | None = None, models: Mapping[str, ModelRoute] | None = None,
                 model_auth: Authenticator | None = None, verifiers: Sequence[str] = (),
                 agent_logins: Sequence[str] = ()):
        self.defs, self.store, self.audit, self.auth, self.now = definitions, store, audit, auth, clock
        # The session store and the audit chain are single-writer: every transport (the MCP server, the model proxy)
        # takes this lock around a gateway operation.
        self.lock = threading.RLock()
        # The model proxy's decisions (modelproxy.py), on this Gateway's store and audit chain, so a model call is
        # charged to the same session budget and recorded in the same chain as the tool calls. model_auth verifies
        # the run's model-proxy token (its own audience); without models, no model call is ever allowed.
        self.models = ModelProxy(dict(models), store, audit, clock) if models else None
        self.model_auth = model_auth
        # Who may ask for a PR's provenance (principal globs, e.g. "app-*-cicd/pipeline-runner"), and the GitHub
        # logins agents open PRs as (an agents' App's bot login): provenance.py.
        self.verifiers, self.agent_logins = tuple(verifiers), tuple(agent_logins)
        self.policy = policy.Policy(packs)
        self.tools = self.policy.tools
        self.runs = runs
        handlers = dict(handlers or {})
        unknown = sorted(set(handlers) - set(self.tools))
        if unknown:
            raise ValueError(f"handlers for tools that are not registered: {unknown}")
        owned = sorted(set(handlers) & CORE_OWNED)
        if owned:
            raise ValueError(f"handlers for tools the Gateway owns: {owned}")
        self._handlers: dict[str, Handler] = handlers
        if runs is not None:
            self._handlers.update({"run.spawn": self._spawn, "run.close": self._close})

    # -- sessions --------------------------------------------------------
    @_durable
    def open_session(self, token: str, agent: str, limits: dict | None = None,
                     task_id: str | None = None) -> Result:
        principal = self.auth.authenticate(token)
        d = self.defs.get(agent)
        if d is None:
            return self._deny_open(principal, agent, "R003", "unknown agent definition")
        if d.identity_type == "workload" and principal.id != d.service_account:
            return self._deny_open(principal, agent, "R001", "principal does not match the definition's workload identity")
        if d.identity_type == "delegated" and principal.kind != "user":
            return self._deny_open(principal, agent, "R001", "definition is for a delegated agent")
        try:
            s = self.store.open(principal.id, agent, d.limits, LimitRequest.from_dict(limits), self.now(), task_id=task_id)
        except WideningError as e:
            return self._deny_open(principal, agent, "R007", str(e))
        except (SessionError, ValueError) as e:
            return self._deny_open(principal, agent, "R008", str(e))
        rec = self._audit(s, principal, "session.open", None, "allow", None, {"limits": limits or {}}, "opened")
        return Result(True, {"allow": True}, {"session": s.id, "task_id": s.task_id,
                                              "expires_at": iso(s.expires_at),
                                              "limits": s.limits.as_dict()}, audit_seq=rec["seq"], session=s.id)

    def _deny_open(self, principal: Principal, agent: str, rule: str, why: str) -> Result:
        rec = self.audit.append({"ts": iso(self.now()), "session": None, "task_id": None,
                                 "principal": principal.id, "agent": agent, "tool": "session.open",
                                 "tier": None, "decision": "deny", "rule": rule,
                                 "target": None, "args_sha256": args_digest({}), "result": why})
        return Result(False, {"allow": False, "rule": rule, "message": why}, error=why, audit_seq=rec["seq"])

    @_durable
    def open_triggered(self, agent: str, trigger: str, task_id: str | None = None) -> Result:
        """Start a scheduled or event-driven agent. The run's identity is the agent's own
        workload identity; the trigger that started it is recorded, not trusted."""
        d = self.defs.get(agent)
        p = Principal(f"trigger:{trigger}", "workload")
        if d is None:
            return self._deny_open(p, agent, "R003", "unknown agent definition")
        if d.identity_type != "workload" or d.kind not in ("scheduled", "event", "service"):
            return self._deny_open(p, agent, "R001", "only workload agents of kind scheduled, event or service can be triggered")
        if task_id:
            # At-least-once delivery: the same task id must not start a second run.
            live = self.store.find_live(agent, task_id, self.now())
            if live is not None:
                rec = self._audit(live, Principal(d.service_account, "workload"), "session.open", None, "allow",
                                  None, {"trigger": trigger, "deduped": True}, "deduped")
                return Result(True, {"allow": True}, {"session": live.id, "task_id": live.task_id,
                                                      "expires_at": iso(live.expires_at), "deduped": True},
                              audit_seq=rec["seq"], session=live.id)
        s = self.store.open(d.service_account, agent, d.limits, None, self.now(), task_id=task_id)
        rec = self._audit(s, Principal(d.service_account, "workload"), "session.open", None, "allow", None,
                          {"trigger": trigger}, "opened")
        return Result(True, {"allow": True}, {"session": s.id, "task_id": s.task_id,
                                              "expires_at": iso(s.expires_at), "deduped": False},
                      audit_seq=rec["seq"], session=s.id)

    @_durable
    def launch(self, session_id: str, run_input: dict | None = None) -> Result:
        """Start the run for a top-level session (a triggered or delegated in-cluster run; on Hangar, an
        AgentRun XR). Children are created by run.spawn; service agents are deployed as applications and have no run."""
        s = self.store.get(session_id)
        if s is None or s.closed:
            return Result(False, {"allow": False, "rule": "R004"}, error="no live session")
        d = self.defs[s.agent]
        if d.kind == "service":
            return Result(False, {"allow": False, "rule": "R001"}, session=s.id,
                          error="service agents are deployed as applications, not launched as runs")
        if self.runs is None:
            return Result(False, {"allow": False, "rule": "R001"}, session=s.id,
                          error="this deployment has no run backend")
        run = self.runs.create(s, d, run_input)
        s.run = run
        rec = self._audit(s, Principal(s.principal, "workload"), "run.launch", None, "allow", None,
                          {"run": run}, "created")
        return Result(True, {"allow": True}, {"run": run, "session": s.id},
                      audit_seq=rec["seq"], session=s.id)

    # -- tool calls ------------------------------------------------------
    @_durable
    def call(self, token: str, session_id: str, tool: str, args: dict[str, Any]) -> Result:
        principal = self.auth.authenticate(token)
        s = self.store.get(session_id)
        if s is None:
            d = policy.Decision(False, "R004", "session-expired", "no such session", "Open a new session.")
            rec = self.audit.append({"ts": iso(self.now()), "session": session_id, "task_id": None,
                                     "principal": principal.id, "agent": None, "tool": tool, "tier": None,
                                     "decision": "deny", "rule": "R004", "target": None,
                                     "args_sha256": args_digest(args), "result": "no such session"})
            return Result(False, d.as_dict(), error=d.message, audit_seq=rec["seq"], session=session_id)
        d = self.defs[s.agent]
        now = self.now()
        ctx = self.policy.build_context(s, self._acting(s, principal.id), principal.tier_ceiling, tool, args,
                                        list(d.deny_paths), list(d.repos_allow), now,
                                        api_allow=list(d.api_allow), agent_kind=d.kind,
                                        field_deny=list(d.field_deny), field_allow=list(d.field_allow),
                                        definition=d)
        decision = self.policy.evaluate(ctx)
        if not decision.allow:
            s.record_denial()
            if decision.rule == "R002":
                self.store.trip(s.id, f"tripwire: {tool}")
            rec = self._audit(s, principal, tool, decision.tier, "deny", decision.rule, args, decision.message)
            return Result(False, decision.as_dict(), error=decision.message, audit_seq=rec["seq"], session=s.id)
        spec = self.tools[tool]
        handler = self._handlers.get(tool)
        if handler is None:
            # Registered (its policy is known) but nothing in this deployment backs it. Charged nothing.
            rec = self._audit(s, principal, tool, int(spec.tier), "allow", None, args, "error:NoHandler")
            return Result(False, decision.as_dict(), error="no backend for this tool in this deployment",
                          audit_seq=rec["seq"], session=s.id)
        s.spend("tool_calls", 1)
        if spec.github_cost:
            s.spend("github_calls", spec.github_cost)
        if ctx["cost"]["prs"]:
            s.spend("open_prs", 1)
        try:
            data = handler(s, args)
        except Denied as e:
            s.record_denial()
            rec = self._audit(s, principal, tool, int(spec.tier), "deny", e.rule, args, e.message)
            dd = policy.Decision(False, e.rule, "refused", e.message, e.hint, int(spec.tier))
            return Result(False, dd.as_dict(), error=e.message, audit_seq=rec["seq"], session=s.id)
        except Exception as e:  # noqa: BLE001 - never leak backend internals to the agent
            rec = self._audit(s, principal, tool, int(spec.tier), "allow", None, args, f"error:{type(e).__name__}")
            return Result(False, decision.as_dict(), error="backend error", audit_seq=rec["seq"], session=s.id)
        rec = self._audit(s, principal, tool, int(spec.tier), "allow", None, args, "ok", effect=_effect(spec, args, data))
        return Result(True, decision.as_dict(), data, audit_seq=rec["seq"], session=s.id)

    def session_tools(self, token: str, session_id: str) -> list[str] | None:
        """The tools a caller may use in its own session that this deployment can run: the session's granted tools
        (its definition's, narrowed at open), less any without a handler here. None when the session is unknown or
        not the caller's. A read of the caller's own grant: no record, nothing spent, and every call is still decided
        on its own; this only spares an agent tools it would be refused or find unbacked."""
        principal = self.auth.authenticate(token)
        s = self.store.get(session_id)
        if s is None or s.closed or self._acting(s, principal.id) != s.principal:
            return None
        return sorted(t for t in s.limits.tools if t in self.tools and t in self._handlers)

    def _acting(self, s: Session, principal_id: str) -> str:
        """Who the caller acts as in session `s`: the session's principal when the caller is the session's own run
        (RunBackend.principal), else the caller. Records always name the caller itself."""
        run_principal = getattr(self.runs, "principal", None)
        if s.run and run_principal is not None and principal_id != s.principal and run_principal(s) == principal_id:
            return s.principal
        return principal_id

    # -- PR provenance (provenance.py) ---------------------------------------
    @_durable
    def provenance(self, token: str, body: Any) -> dict[str, Any]:
        """The agent-identity and agent-scope verdicts for one PR, for a CI gate. The caller must be a verifier;
        each verdict is recorded. Raises AuthError (unauthenticated), provenance.NotVerifier, provenance.BadRequest."""
        principal = self.auth.authenticate(token)
        if not any(fnmatchcase(principal.id, g) for g in self.verifiers):
            raise provenance.NotVerifier(f"{principal.id} is not a provenance verifier")
        req = provenance.Request.from_dict(body)
        verdict = provenance.verify(req, self.audit.receipts(req.repo, req.pr, provenance.PR_TOOLS), self.defs,
                                    self.agent_logins)
        r = verdict["record"] or {}
        rec = self.audit.append({
            "ts": iso(self.now()), "task_id": r.get("task_id"), "session": r.get("session"), "parent": None,
            "principal": principal.id, "agent": r.get("agent"), "tool": "provenance.verify", "tier": None,
            "decision": "allow", "rule": verdict["identity"]["rule"] or verdict["scope"]["rule"],
            "target": f"{req.repo}#{req.pr}", "args_sha256": args_digest(body),
            "result": f"identity:{verdict['identity']['status']} scope:{verdict['scope']['status']}"})
        return {**verdict, "audit_seq": rec["seq"]}

    # -- model calls (the proxy's half lives in server.create_model_app) ---
    @_durable
    def model_authorize(self, token: str, session_id: str, model: str, est_tokens: int) -> ModelDecision:
        """Authenticate a run's model-proxy token and decide one model call (modelproxy.ModelProxy.authorize)."""
        if self.models is None or self.model_auth is None:
            return ModelDecision(False, "M006", "this deployment has no model routes")
        principal = self.model_auth.authenticate(token)
        s = self.store.get(session_id)
        acting = self._acting(s, principal.id) if s is not None else principal.id
        return self.models.authorize(session_id, principal.id, model, est_tokens, acting=acting)

    @_durable
    def model_usage(self, session_id: str, tokens: int) -> None:
        """Charge what an allowed model call actually used; an overrun trips the session's breaker."""
        if self.models is not None:
            self.models.record_usage(session_id, tokens)

    # -- run tree --------------------------------------------------------
    def _spawn(self, parent: Session, a: dict[str, Any]) -> dict[str, Any]:
        d = self.defs.get(a["agent"])
        if d is None:
            raise Denied("R003", "unknown agent definition", "Spawn an agent that exists in the catalog.")
        try:
            child = self.store.open(parent.principal, d.name, d.limits, LimitRequest.from_dict(a.get("limits")),
                                    self.now(), parent_id=parent.id)
        except WideningError as e:
            raise Denied("R007", str(e), "A child run may only narrow what its parent has left.") from e
        except SessionError as e:
            raise Denied("R016", str(e), "Spawn fewer or smaller child runs, or finish the ones running.") from e
        except ValueError as e:
            raise Denied("R008", str(e), "Fix the requested limits; see the tool schema.") from e
        try:
            run = self.runs.create(child, d, a.get("input"))
            child.run = run
        except Exception:
            self.store.close(child.id)
            raise
        return {"session": child.id, "run": run, "expires_at": iso(child.expires_at)}

    def _close(self, s: Session, a: dict[str, Any]) -> dict[str, Any]:
        target = self.store.get(a["run_id"])
        if target is None or (target.id != s.id and target.id not in s.children):
            raise Denied("R001", "not one of this session's runs", "You may only close your own run or its children.")
        self.store.close(target.id)
        self._delete_runs(target, s, "closed")
        return {"closed": target.id}

    # -- expiry (Hangar D20) ---------------------------------------------
    @_durable
    def expire(self, now: datetime | None = None) -> list[str]:
        """Close every session past its deadline and delete its run, and its children's, each with a record.

        The run itself already ended at its deadline (the run backend enforces that without Clearance); this is the
        cleanup after the evidence is written: the record of the deletion is in the same commit as the closed
        session. A deletion that fails is recorded and retried by the next sweep, because the session keeps its run."""
        now = now or self.now()
        self.store.sweep(now)
        pending = [s for s in self.store.all() if s.run and s.closed]
        done = []
        for s in pending:
            if s.run and self.runs is not None:
                self._delete_runs(s, s, "expired")
                done.append(s.id)
        return done

    def _delete_runs(self, target: Session, actor: Session, why: str) -> None:
        self.store.close(target.id)
        for sid in [target.id, *self._descendants(target)]:
            s = self.store.get(sid)
            if s is None or not s.run or self.runs is None:
                continue
            try:
                self.runs.delete(s)
            except Exception as e:  # noqa: BLE001 - recorded and retried on the next sweep
                self._audit(s, Principal(actor.principal, "workload"), "run.delete", None, "allow", None,
                            {"run": s.run, "why": why}, f"error:{type(e).__name__}")
                continue
            self._audit(s, Principal(actor.principal, "workload"), "run.delete", None, "allow", None,
                        {"run": s.run, "why": why}, "deleted")
            s.run = None

    def _descendants(self, s: Session) -> list[str]:
        out = []
        for c in s.children:
            out.append(c)
            child = self.store.get(c)
            if child is not None:
                out.extend(self._descendants(child))
        return out

    # -- audit -----------------------------------------------------------
    def _audit(self, s: Session, principal: Principal, tool: str, tier, decision: str, rule, args, result: str,
               effect: dict[str, Any] | None = None) -> dict:
        spec = self.tools.get(tool)
        target = policy.env_tier_of(spec, args) if spec and isinstance(args, dict) else None
        event = {
            "ts": iso(self.now()), "task_id": s.task_id, "session": s.id,
            "parent": s.parent_id, "principal": principal.id, "agent": s.agent,
            "tool": tool, "tier": tier, "decision": decision, "rule": rule,
            "target": target, "args_sha256": args_digest(args), "result": result,
        }
        if effect:
            event["effect"] = effect
        return self.audit.append(event)


def _effect(spec, args: dict[str, Any], data: Any) -> dict[str, Any] | None:
    """What a successful write made, from its result (spec.receipt), with the repo and the paths it wrote: the
    receipt a PR's provenance is checked against. None for a tool without a receipt."""
    if not spec.receipt or not isinstance(data, dict):
        return None
    e: dict[str, Any] = {k: data[k] for k in spec.receipt if k in data}
    if spec.writes_files:
        e["repo"] = str(args.get("repo", "")).lower()
        e["paths"] = sorted({provenance.normal(f.get("path", "")) for f in args.get("files") or []
                             if isinstance(f, dict)})
    return e
