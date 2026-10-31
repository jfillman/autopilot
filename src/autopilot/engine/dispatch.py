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

from . import control as ctl, definitions as defn, events as evh, gate, inbox as hr, policy, provenance
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


class AppOwners(Protocol):
    """Who owns an app (a group ref), for the deployment gate when the check does not say: on Hangar, the Backstage
    Component's spec.owner."""
    def owner_of(self, app: str) -> str | None: ...


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
                 agent_logins: Sequence[str] = (), inbox: hr.InboxStore | None = None,
                 directory: hr.Directory | None = None, delegates: Sequence[str] = (),
                 events: evh.EventsStore | None = None, holds: ctl.HoldStore | None = None,
                 admins: Sequence[str] = (), definitions_store: defn.DefinitionsStore | None = None,
                 changes: gate.ChangeStore | None = None, gate_policy: gate.GatePolicy | None = None,
                 owners: "AppOwners | None" = None):
        self.defs, self.store, self.audit, self.auth, self.now = definitions, store, audit, auth, clock
        # The deployment gate (gate.py): the changes registered by the clearance-gate check, the platform's tier
        # table, and where an app's owner group comes from when the check does not say. None = the gate is off.
        self.changes, self.gate_policy, self.owners = changes, gate_policy or gate.GatePolicy(), owners
        # Cluster-pinned definitions (definitions.py): where the loaded set is kept for the read API and restarts.
        self.definitions_store = definitions_store
        # Person controls (control.py): the holds on agents' triggers, and the platform admins (principal globs) who
        # may freeze, end, hold and release beside each agent's owner group.
        self.holds, self.admins = holds, tuple(admins)
        # The trigger bridge's event history (events.py): what it received and did, for the read API. None = not kept.
        self.events = events
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
        # human.request's inbox (inbox.py): the Gateway's own handlers, since a request needs its session. delegates are
        # the principals (globs) that may answer for a person they name, e.g. Tower's backend.
        self.inbox, self.directory, self.delegates = inbox, directory, tuple(delegates)
        if inbox is not None:
            self._handlers.update({"human.request": self._human_request, "human.request.get": self._human_get})

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
        held = self.hold_for(agent)
        if held is not None:
            return self._deny_open(p, agent, "R024", f"triggers held by {held.by} until {iso(held.until)}: {held.reason}")
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

    # -- human.request (inbox.py) ------------------------------------------
    def _human_request(self, s: Session, a: dict[str, Any]) -> dict[str, Any]:
        try:
            question, options = hr.check_question(a.get("question"), a.get("options"))
        except hr.InboxError as e:
            raise Denied(e.rule, e.message, e.hint) from e
        now = self.now()
        if self.inbox.count_pending(s.id, now) >= hr.MAX_PENDING:
            raise Denied("HR-FLOOD", f"this session already has {hr.MAX_PENDING} unanswered requests",
                         "Read their answers with human.request.get before asking more.")
        r = hr.HumanRequest(hr.new_id(), s.task_id, s.id, s.agent, s.principal,
                            getattr(self.defs.get(s.agent), "owner", None), question, options, now, s.expires_at)
        self.inbox.add(r)
        return {"request": r.id, "status": "pending", "expires_at": r.expires_at.isoformat(),
                "hint": "Not answered yet. Read the answer with human.request.get whenever you choose; "
                        "this call does not wait."}

    def _human_get(self, s: Session, a: dict[str, Any]) -> dict[str, Any]:
        rid = a.get("request")
        r = self.inbox.get(rid) if isinstance(rid, str) and hr.REQUEST_ID.match(rid) else None
        if r is None or r.task_id != s.task_id:
            raise Denied("HR-UNKNOWN", f"no request {rid!r} in this task", "Pass an id human.request returned.")
        return r.view(self.now())

    def _person(self, token: str, on_behalf_of: str | None) -> tuple[str, str | None]:
        """Who is answering, and through which delegate: a person signed in themselves, or a delegate naming one."""
        principal = self.auth.authenticate(token)
        if on_behalf_of:
            if not any(fnmatchcase(principal.id, g) for g in self.delegates):
                raise hr.InboxError("HR-DELEGATE", f"{principal.id} may not act for a person", "", 403)
            if not on_behalf_of.startswith("user:") or len(on_behalf_of) > 200:
                raise hr.InboxError("HR-PERSON", "on-behalf-of must name a person (user:<name>)", "", 400)
            return on_behalf_of, principal.id
        if principal.kind != "user":
            raise hr.InboxError("HR-PERSON", f"{principal.id} is not a person", "", 403)
        return principal.id, None

    @_durable
    def record_event(self, summary: dict[str, Any], outcomes: list[dict[str, Any]]) -> bool:
        """What the trigger bridge received and did with one event (events.py), for the read API's history. Not a
        decision and not evidence: nothing is denied or spent, so it is not in the chain. False when this deployment
        keeps no history."""
        if self.events is None:
            return False
        self.events.record(evh.from_summary(summary, outcomes, self.now()))
        return True

    # -- person controls (control.py): freeze or end a run, hold or release an agent's triggers ---------------------
    def hold_for(self, agent: str) -> ctl.Hold | None:
        """The active hold on an agent's triggers, if any; an expired one is dropped."""
        if self.holds is None:
            return None
        h = self.holds.get(agent)
        if h is None:
            return None
        if not h.active(self.now()):
            self.holds.delete(agent)
            return None
        return h

    def _manager(self, token: str, on_behalf_of: str | None, agent: str, session: Session | None = None) -> tuple[str, str | None]:
        """Who is acting (person, via), and that they may: the agent's owner group, a platform admin, or for a
        session, the person whose session it is."""
        try:
            person, via = self._person(token, on_behalf_of)
        except hr.InboxError as e:
            raise ctl.ControlError(e.rule.replace("HR-", "CT-"), e.message, e.hint, e.status) from None
        d = self.defs.get(agent)
        groups = self.directory.groups(person) if self.directory is not None else set()
        own = session is not None and session.principal == person
        if not own and not ctl.may_manage(person, getattr(d, "owner", None), groups, self.admins):
            raise ctl.ControlError("CT-FORBIDDEN", f"{person} may not manage {agent}",
                                   "Only the agent's owner group, a platform admin, or the person whose session it is may.", 403)
        return person, via

    def _control_record(self, person: str, via: str | None, tool: str, s: Session | None, agent: str, target: str,
                        args: dict[str, Any], result: str) -> dict[str, Any]:
        return self.audit.append({
            "ts": iso(self.now()), "task_id": s.task_id if s else None, "session": s.id if s else None,
            "parent": s.parent_id if s else None, "principal": person, "agent": agent, "tool": tool, "tier": None,
            "decision": "allow", "rule": None, "target": target, "args_sha256": args_digest(args),
            "result": result + (f" via {via}" if via else "")})

    # -- the deployment gate (gate.py) ---------------------------------------------------------------------------
    def _gate_record(self, principal: str, via: str | None, tool: str, c: gate.Change, decision: str, rule: str | None,
                     result: str, args: Any) -> dict[str, Any]:
        return self.audit.append({
            "ts": iso(self.now()), "task_id": None, "session": None, "parent": None, "principal": principal,
            "agent": c.agent, "tool": tool, "tier": None, "decision": decision, "rule": rule, "target": c.target,
            "args_sha256": args_digest(args), "result": result + (f" via {via}" if via else ""), "app": c.app,
            "boundary": "deployment", "change": c.summary()})

    def _gate_on(self) -> gate.ChangeStore:
        if self.changes is None:
            raise gate.GateError("DG-OFF", "this Clearance keeps no changes (the deployment gate is off)", "", 404)
        return self.changes

    @_durable
    def register_change(self, token: str, body: Any) -> dict[str, Any]:
        """The clearance-gate check registers (or re-evaluates) a change to a protected environment. The caller must
        be a verifier. The change is classified (gate.classify); tiers 0 and 1 are cleared by policy at once, 2 and 3
        hold for people, an unknown proposer is denied, and a loosening clearance.yaml is denied too. Idempotent by
        (repo, pr, head): a second call with new check results re-evaluates a holding change; a decided one is
        returned as it stands. A new head on the same PR supersedes the heads still holding. Every step is a
        deployment-boundary record. Raises AuthError, provenance.NotVerifier, gate.BadChange."""
        principal = self.auth.authenticate(token)
        if not any(fnmatchcase(principal.id, g) for g in self.verifiers):
            raise provenance.NotVerifier(f"{principal.id} is not a verifier")
        store = self._gate_on()
        req = gate.ChangeRequest.from_dict(body)
        if not req.owner and self.owners is not None:
            req = gate.ChangeRequest(**{**req.__dict__, "owner": self.owners.owner_of(req.app) or ""})
        if not req.owner:
            req = gate.ChangeRequest(**{**req.__dict__, "owner": (self.gate_policy.outside_groups or ("group:default/admins",))[0]})
        now = self.now()
        existing = store.get(req.id)
        if existing is not None and existing.state != "holding":
            return existing.view(now)
        try:
            c = gate.classify(req, self.gate_policy)
        except gate.GateError as e:
            # A clearance.yaml that loosens the baseline: the change is denied, not the file ignored.
            ch = existing or gate.Change.open(req, gate.Classification(3, (gate.Reason("clearance.yaml", "policy:loosens", 3),), "deny", e.rule, e.message, None, self.gate_policy), now, principal.id)
            ch.state, ch.deny_reason, ch.denied_by, ch.decided_at, ch.updated_at = "denied", e.message, "policy", now, now
            store.put(ch)
            if existing is None:
                self._gate_record(principal.id, None, "deploy.request", ch, "allow", None, f"tier {ch.tier}: denied ({e.rule})", body)
            self._gate_record("policy", None, "deploy.deny", ch, "deny", e.rule, e.message, {"rule": e.rule})
            return ch.view(now)
        if existing is None:
            for old in store.for_pr(req.repo, req.pr):
                if old.state == "holding" and old.head != req.head:
                    old.state, old.superseded_by, old.decided_at, old.updated_at = "superseded", req.head, now, now
                    store.put(old)
                    self._gate_record("policy", None, "deploy.expire", old, "allow", None, f"superseded by {req.head[:7]}", {"by": req.head})
            ch = gate.Change.open(req, c, now, principal.id)
            self._gate_record(principal.id, None, "deploy.request", ch, "allow", None, c.why, body)
        else:
            ch = existing
            ch.checks, ch.tier, ch.reasons, ch.updated_at = dict(req.checks), c.tier, [r.view() for r in c.reasons], now
            ch.requirement = c.requirement.view() if c.requirement else None
        if c.decision == "deny":
            ch.state, ch.deny_reason, ch.denied_by, ch.decided_at = "denied", c.why, "policy", now
            self._gate_record("policy", None, "deploy.deny", ch, "deny", c.rule, c.why, {"rule": c.rule})
        elif c.decision == "clear":
            ch.state, ch.cleared_by, ch.decided_at = "cleared", "policy", now
            self._gate_record("policy", None, "deploy.clear", ch, "allow", None, c.why, {"tier": c.tier})
        store.put(ch)
        return ch.view(now)

    def _gate_person(self, token: str, on_behalf_of: str | None) -> tuple[str, str | None, set[str]]:
        try:
            person, via = self._person(token, on_behalf_of)
        except hr.InboxError as e:
            raise gate.GateError(e.rule.replace("HR-", "DG-"), e.message, e.hint, e.status) from None
        groups = self.directory.groups(person) if self.directory is not None else set()
        return person, via, set(groups)

    @_durable
    def clear_change(self, token: str, change_id: str, on_behalf_of: str | None = None) -> dict[str, Any]:
        """A person clears a holding change: one approval in the role they qualify for (gate.role_for). The change is
        cleared when the requirement is met. Recorded as deploy.clear naming person and delegate."""
        store = self._gate_on()
        c = store.get(change_id)
        if c is None:
            raise gate.GateError("DG-UNKNOWN", "no such change", "", 404)
        person, via, groups = self._gate_person(token, on_behalf_of)
        now = self.now()
        if c.state == "holding" and now >= c.expires_at:
            self._expire_change(c, now)
            raise gate.GateError("DG-STATE", "this change has expired; a new push registers it again", "", 409)
        role = gate.role_for(person, groups, c)
        c.approvals.append({"person": person, "via": via, "role": role, "at": iso(now)})
        c.updated_at = now
        req = c.requirement_obj()
        done = req is not None and req.satisfied(c.approvals)
        if done:
            c.state, c.cleared_by, c.decided_at = "cleared", "people", now
        store.put(c)
        rec = self._gate_record(person, via, "deploy.clear", c, "allow", None,
                                f"cleared as {role}" + (" (requirement met)" if done else f" ({len(c.approvals)} of the people needed)"),
                                {"role": role})
        return {**c.view(now), "audit_seq": rec["seq"]}

    @_durable
    def deny_change(self, token: str, change_id: str, reason: Any, on_behalf_of: str | None = None) -> dict[str, Any]:
        """A person who could clear a holding change denies it instead, with a reason. Recorded as deploy.deny."""
        store = self._gate_on()
        c = store.get(change_id)
        if c is None:
            raise gate.GateError("DG-UNKNOWN", "no such change", "", 404)
        person, via, groups = self._gate_person(token, on_behalf_of)
        text = gate.check_reason(reason)
        if c.state != "holding":
            raise gate.GateError("DG-STATE", f"this change is {c.state}", "", 409)
        req = c.requirement_obj()
        eligible = req is not None and (req.owner_group in groups or any(g in groups for g in req.outside_groups)
                                        or any(fnmatchcase(person, g) for g in self.admins))
        if not eligible:
            raise gate.GateError("DG-FORBIDDEN", f"{person} may not decide this change", "Only the owner group, the outside group or a platform admin may.", 403)
        now = self.now()
        c.state, c.deny_reason, c.denied_by, c.decided_at, c.updated_at = "denied", text, person, now, now
        store.put(c)
        rec = self._gate_record(person, via, "deploy.deny", c, "deny", "DG-DENIED", text, {"reason": text})
        return {**c.view(now), "audit_seq": rec["seq"]}

    def _expire_change(self, c: gate.Change, now: datetime) -> None:
        c.state, c.decided_at, c.updated_at = "expired", now, now
        self.changes.put(c)
        self._gate_record("policy", None, "deploy.expire", c, "allow", None, f"not cleared within {self.gate_policy.ttl_days} days", {"expires_at": iso(c.expires_at)})

    def expire_changes(self, now: datetime | None = None) -> list[str]:
        """Holding changes past their TTL expire, each a deploy.expire record. Called from expire()."""
        if self.changes is None:
            return []
        now = now or self.now()
        done = []
        for c in self.changes.holding(now):
            if now >= c.expires_at:
                self._expire_change(c, now)
                done.append(c.id)
        return done

    def gate_policy_view(self) -> dict[str, Any]:
        return self.gate_policy.view()

    # -- cluster-pinned definitions (definitions.py) -----------------------------------------------------------
    def _definition_record(self, tool: str, name: str, source: defn.Source, decision: str, rule: str | None,
                           result: str) -> dict[str, Any]:
        principal = f"pin:{source.namespace}/{source.xr}" if source.kind == "cluster" else "platform"
        return self.audit.append({
            "ts": iso(self.now()), "task_id": None, "session": None, "parent": None, "principal": principal,
            "agent": name, "tool": tool, "tier": None, "decision": decision, "rule": rule, "target": source.pin,
            "args_sha256": args_digest(source.view()), "result": result})

    @_durable
    def apply_cluster_definitions(self, loaded: Sequence[defn.Loaded], rejected: Sequence[defn.Rejected] = ()) -> defn.Changes:
        """The loader's hand-off: make `loaded` + `rejected` the cluster set (definitions.Definitions.replace_cluster),
        record what changed as evidence (definition.load for a new or re-pinned definition, definition.unload for a
        pin that is gone, definition.reject for a pin that did not load, once each), and persist the set for the read
        API. Nothing is recorded when the poll found the same set."""
        if not isinstance(self.defs, defn.Definitions):
            raise TypeError("cluster definitions need a definitions.Definitions mapping")
        with self.lock:
            ch = self.defs.replace_cluster(list(loaded), list(rejected))
            if not ch:
                return ch
            for n in ch.added:
                self._definition_record("definition.load", n, ch.sources[n], "allow", None, f"loaded {ch.sources[n].pin}")
            for n in ch.changed:
                self._definition_record("definition.load", n, ch.sources[n], "allow", None, f"re-pinned {ch.sources[n].pin}")
            for n in ch.removed:
                self._definition_record("definition.unload", n, ch.sources[n], "allow", None, f"pin removed {ch.sources[n].pin}")
            for r in ch.rejected:
                self._definition_record("definition.reject", r.name, r.source, "deny", "DF-PIN", r.reason)
            if self.definitions_store is not None:
                self.definitions_store.replace(self.defs.records())
            return ch

    @_durable
    def freeze_session(self, token: str, session_id: str, reason: Any, on_behalf_of: str | None = None) -> dict[str, Any]:
        """Trip a live session's breaker by hand: every further call of the run (and its children) is refused; the
        run stays for a person to read. Recorded as session.freeze."""
        s = self.store.get(session_id)
        if s is None:
            raise ctl.ControlError("CT-UNKNOWN", "no such session", "", 404)
        person, via = self._manager(token, on_behalf_of, s.agent, s)
        text = ctl.check_reason(reason)
        if s.closed:
            raise ctl.ControlError("CT-CLOSED", "this session is already closed", "", 409)
        if s.tripped:
            raise ctl.ControlError("CT-TRIPPED", f"this session is already frozen: {s.trip_reason}", "", 409)
        self.store.trip(s.id, f"frozen by {person}: {text}")
        rec = self._control_record(person, via, "session.freeze", s, s.agent, s.id, {"reason": text}, f"frozen: {text}")
        return {"session": s.id, "agent": s.agent, "tripped": True, "trip_reason": s.trip_reason, "audit_seq": rec["seq"]}

    @_durable
    def end_session(self, token: str, session_id: str, reason: Any, on_behalf_of: str | None = None) -> dict[str, Any]:
        """Close a session (and its children) by hand: its budget is settled, every further call is refused. The run's
        own pod ends with its AgentRun's TTL. Recorded as session.end."""
        s = self.store.get(session_id)
        if s is None:
            raise ctl.ControlError("CT-UNKNOWN", "no such session", "", 404)
        person, via = self._manager(token, on_behalf_of, s.agent, s)
        text = ctl.check_reason(reason)
        if s.closed:
            raise ctl.ControlError("CT-CLOSED", "this session is already closed", "", 409)
        self.store.close(s.id)
        rec = self._control_record(person, via, "session.end", s, s.agent, s.id, {"reason": text}, f"ended: {text}")
        return {"session": s.id, "agent": s.agent, "closed": True, "audit_seq": rec["seq"]}

    @_durable
    def hold_triggers(self, token: str, agent: str, reason: Any, hours: Any = None, on_behalf_of: str | None = None) -> dict[str, Any]:
        """Stop an agent's triggers starting runs, for `hours` (24 by default, 336 at most); renewing replaces the
        hold. open_triggered refuses with R024 while it lasts. Recorded as trigger.hold."""
        if self.holds is None:
            raise ctl.ControlError("CT-OFF", "this deployment keeps no holds", "", 404)
        if agent not in self.defs:
            raise ctl.ControlError("CT-UNKNOWN", "no such agent", "", 404)
        person, via = self._manager(token, on_behalf_of, agent)
        text, n = ctl.check_reason(reason), ctl.check_hours(hours)
        now = self.now()
        hold = ctl.Hold(agent=agent, by=person, via=via, reason=text, since=now, until=ctl.hold_until(now, n))
        self.holds.put(hold)
        rec = self._control_record(person, via, "trigger.hold", None, agent, agent, {"reason": text, "hours": n},
                                   f"held until {iso(hold.until)}: {text}")
        return {**hold.view(), "audit_seq": rec["seq"]}

    @_durable
    def release_triggers(self, token: str, agent: str, on_behalf_of: str | None = None) -> dict[str, Any]:
        """Lift a hold before it expires. Recorded as trigger.release."""
        hold = self.hold_for(agent)
        if hold is None:
            raise ctl.ControlError("CT-NOHOLD", f"{agent} is not held", "", 404)
        person, via = self._manager(token, on_behalf_of, agent)
        self.holds.delete(agent)
        rec = self._control_record(person, via, "trigger.release", None, agent, agent, {},
                                   f"released (held by {hold.by} since {iso(hold.since)}: {hold.reason})")
        return {"agent": agent, "released": True, "audit_seq": rec["seq"]}

    def human_requests(self, token: str, on_behalf_of: str | None = None) -> list[dict[str, Any]]:
        """The pending requests this person may answer, oldest first."""
        if self.inbox is None:
            return []
        person, _ = self._person(token, on_behalf_of)
        groups = self.directory.groups(person) if self.directory is not None else set()
        now = self.now()
        return [r.view(now) for r in self.inbox.pending(now) if hr.may_answer(r, person, groups)]

    @_durable
    def human_answer(self, token: str, request_id: str, answer: Any, on_behalf_of: str | None = None) -> dict[str, Any]:
        """Answer one request, as the person (or a delegate naming them). Recorded as human.answer."""
        if self.inbox is None:
            raise hr.InboxError("HR-OFF", "this deployment has no human inbox", "", 404)
        person, via = self._person(token, on_behalf_of)
        r = self.inbox.get(request_id) if isinstance(request_id, str) and hr.REQUEST_ID.match(request_id) else None
        groups = self.directory.groups(person) if self.directory is not None else set()
        if r is None or not hr.may_answer(r, person, groups):
            raise hr.InboxError("HR-UNKNOWN", "no such request for you", "", 404)
        now = self.now()
        text = hr.check_answer(r, answer, now)
        r.status, r.answer, r.answered_by, r.answered_via, r.answered_at = "answered", text, person, via, now
        self.inbox.save(r)
        rec = self.audit.append({
            "ts": iso(now), "task_id": r.task_id, "session": r.session, "parent": None, "principal": person,
            "agent": r.agent, "tool": "human.answer", "tier": None, "decision": "allow", "rule": None,
            "target": r.id, "args_sha256": args_digest({"answer": text}),
            "result": "answered" + (f" via {via}" if via else "")})
        return {**r.view(now), "audit_seq": rec["seq"]}

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
        self.expire_changes(now)
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
        app = self._app_of(tool, args)
        if app:
            event["app"] = app
        return self.audit.append(event)

    def _app_of(self, tool: str, args: Any) -> str | None:
        """The application this call acts on, from the first pack that names one (Pack.app_of)."""
        if not isinstance(args, dict):
            return None
        for p in self.policy.packs:
            fn = getattr(p, "app_of", None)
            app = fn(tool, args) if fn else None
            if isinstance(app, str) and app:
                return app.lower()[:100]
        return None


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
    elif isinstance(data.get("paths"), list):
        # A tool whose files are its own, not an argument (glidepath.cicd.propose writes cicd.yaml): its result says
        # which repo and paths its commit carried, and the receipt records them the same way.
        e["repo"] = str(data.get("repo") or args.get("repo", "")).lower()
        e["paths"] = sorted({provenance.normal(p) for p in data["paths"] if isinstance(p, str)})
    return e
