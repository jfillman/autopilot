"""The gateway core, independent of any transport.

authenticate -> find session -> policy -> audit -> budget -> backend -> audit result.
Every call, allowed or denied, produces exactly one decision record, so "what did the
agent try to do" is answerable from the audit log alone.
"""
from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Any, Callable, Protocol

from . import manifests, policy
from .audit import AuditLog, args_digest
from .backends import Backends
from .limits import LimitRequest, WideningError
from .profile import AgentDefinition
from .session import Session, SessionError, SessionStore
from .tiers import TOOLS, Tier


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


class Gateway:
    def __init__(self, definitions: dict[str, AgentDefinition], store: SessionStore,
                 audit: AuditLog, backends: Backends, auth: Authenticator,
                 clock: Callable[[], datetime]):
        self.defs, self.store, self.audit, self.b, self.auth, self.now = definitions, store, audit, backends, auth, clock
        self._handlers: dict[str, Callable[[Session, dict], Any]] = {
            "catalog.read": lambda s, a: self.b.read.catalog(a["query"]),
            "metrics.query": lambda s, a: self.b.read.promql(a["promql"]),
            "logs.query": lambda s, a: self.b.read.logql(a["logql"]),
            "argo.app.get": lambda s, a: self.b.argo.get_app(a["app"]),
            "app.api.get": lambda s, a: self.b.read.app_api(a["service"], a["path"]),
            "artifact.put": lambda s, a: self.b.artifacts.put(s.task_id, a["name"], a["content"]),
            "artifact.get": lambda s, a: self.b.artifacts.get(s.task_id, a["name"]),
            "chat.recv": lambda s, a: self.b.human.recv(s.id),
            "chat.send": lambda s, a: self.b.human.send(s.id, a["text"]),
            "repo.pr.open": lambda s, a: self.b.git.open_pr(a["repo"], a["branch"], a["title"], a["files"], a.get("base", "main")),
            "repo.pr.open_upper": lambda s, a: self.b.git.open_pr(a["repo"], a["branch"], a["title"], a["files"], a.get("base", "main")),
            "xr.request": lambda s, a: self.b.git.commit_xr(a["app"], a["env"], a["kind"], a["spec"]),
            "argo.sync.lower": lambda s, a: self.b.argo.sync(a["app"], a["project"]),
            "pipeline.rerun": lambda s, a: self.b.pipelines.rerun(a["app"], a["pipelinerun"]),
            "human.request": lambda s, a: self.b.human.request(a["question"], a.get("options")),
            "run.spawn": self._spawn,
            "run.close": self._close,
        }

    # -- sessions --------------------------------------------------------
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
                                              "expires_at": manifests.iso(s.expires_at),
                                              "limits": s.limits.as_dict()}, audit_seq=rec["seq"], session=s.id)

    def _deny_open(self, principal: Principal, agent: str, rule: str, why: str) -> Result:
        rec = self.audit.append({"ts": manifests.iso(self.now()), "session": None, "task_id": None,
                                 "principal": principal.id, "agent": agent, "tool": "session.open",
                                 "tier": None, "decision": "deny", "rule": rule,
                                 "target": None, "args_sha256": args_digest({}), "result": why})
        return Result(False, {"allow": False, "rule": rule, "message": why}, error=why, audit_seq=rec["seq"])

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
                                                      "expires_at": manifests.iso(live.expires_at), "deduped": True},
                              audit_seq=rec["seq"], session=live.id)
        s = self.store.open(d.service_account, agent, d.limits, None, self.now(), task_id=task_id)
        rec = self._audit(s, Principal(d.service_account, "workload"), "session.open", None, "allow", None,
                          {"trigger": trigger}, "opened")
        return Result(True, {"allow": True}, {"session": s.id, "task_id": s.task_id,
                                              "expires_at": manifests.iso(s.expires_at), "deduped": False},
                      audit_seq=rec["seq"], session=s.id)

    def launch(self, session_id: str, run_input: dict | None = None) -> Result:
        """Create the AgentRun XR for a top-level session (a triggered or delegated in-cluster run).
        Children are created by run.spawn; service agents are deployed as applications and have no run."""
        s = self.store.get(session_id)
        if s is None or s.closed:
            return Result(False, {"allow": False, "rule": "R004"}, error="no live session")
        d = self.defs[s.agent]
        if d.kind == "service":
            return Result(False, {"allow": False, "rule": "R001"}, session=s.id,
                          error="service agents are deployed as applications, not launched as runs")
        manifest = manifests.agentrun_manifest(s, d, run_input=run_input)
        self.b.runs.create(manifest)
        rec = self._audit(s, Principal(s.principal, "workload"), "run.launch", None, "allow", None,
                          {"run": manifest["metadata"]["name"]}, "created")
        return Result(True, {"allow": True}, {"run": manifest["metadata"]["name"], "session": s.id},
                      audit_seq=rec["seq"], session=s.id)

    # -- tool calls ------------------------------------------------------
    def call(self, token: str, session_id: str, tool: str, args: dict[str, Any]) -> Result:
        principal = self.auth.authenticate(token)
        s = self.store.get(session_id)
        if s is None:
            d = policy.Decision(False, "R004", "session-expired", "no such session", "Open a new session.")
            rec = self.audit.append({"ts": manifests.iso(self.now()), "session": session_id, "task_id": None,
                                     "principal": principal.id, "agent": None, "tool": tool, "tier": None,
                                     "decision": "deny", "rule": "R004", "target": None,
                                     "args_sha256": args_digest(args), "result": "no such session"})
            return Result(False, d.as_dict(), error=d.message, audit_seq=rec["seq"], session=session_id)
        d = self.defs[s.agent]
        now = self.now()
        ctx = policy.build_context(s, principal.id, principal.tier_ceiling, tool, args,
                                   list(d.deny_paths), list(d.repos_allow), now,
                                   api_allow=list(d.api_allow), agent_kind=d.kind,
                                   field_deny=list(d.field_deny), field_allow=list(d.field_allow))
        decision = policy.evaluate(ctx)
        if not decision.allow:
            s.record_denial()
            if decision.rule == "R002":
                self.store.trip(s.id, f"tripwire: {tool}")
            rec = self._audit(s, principal, tool, decision.tier, "deny", decision.rule, args, decision.message)
            return Result(False, decision.as_dict(), error=decision.message, audit_seq=rec["seq"], session=s.id)
        spec = TOOLS[tool]
        s.spend("tool_calls", 1)
        if spec.github_cost:
            s.spend("github_calls", spec.github_cost)
        if ctx["cost"]["prs"]:
            s.spend("open_prs", 1)
        try:
            data = self._handlers[tool](s, args)
        except Denied as e:
            s.record_denial()
            rec = self._audit(s, principal, tool, int(spec.tier), "deny", e.rule, args, e.message)
            dd = policy.Decision(False, e.rule, "refused", e.message, e.hint, int(spec.tier))
            return Result(False, dd.as_dict(), error=e.message, audit_seq=rec["seq"], session=s.id)
        except Exception as e:  # noqa: BLE001 - never leak backend internals to the agent
            rec = self._audit(s, principal, tool, int(spec.tier), "allow", None, args, f"error:{type(e).__name__}")
            return Result(False, decision.as_dict(), error="backend error", audit_seq=rec["seq"], session=s.id)
        rec = self._audit(s, principal, tool, int(spec.tier), "allow", None, args, "ok")
        return Result(True, decision.as_dict(), data, audit_seq=rec["seq"], session=s.id)

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
            manifest = manifests.agentrun_manifest(child, d, run_input=a.get("input"))
            self.b.runs.create(manifest)
        except Exception:
            self.store.close(child.id)
            raise
        return {"session": child.id, "run": manifests.run_name(child), "expires_at": manifests.iso(child.expires_at)}

    def _close(self, s: Session, a: dict[str, Any]) -> dict[str, Any]:
        target = self.store.get(a["run_id"])
        if target is None or (target.id != s.id and target.id not in s.children):
            raise Denied("R001", "not one of this session's runs", "You may only close your own run or its children.")
        self.store.close(target.id)
        self.b.runs.delete(manifests.run_name(target))
        return {"closed": target.id}

    # -- audit -----------------------------------------------------------
    def _audit(self, s: Session, principal: Principal, tool: str, tier, decision: str, rule, args, result: str) -> dict:
        spec = TOOLS.get(tool)
        target = policy.env_tier_of(spec, args) if spec and isinstance(args, dict) else None
        return self.audit.append({
            "ts": manifests.iso(self.now()), "task_id": s.task_id, "session": s.id,
            "parent": s.parent_id, "principal": principal.id, "agent": s.agent,
            "tool": tool, "tier": tier, "decision": decision, "rule": rule,
            "target": target, "args_sha256": args_digest(args), "result": result,
        })
