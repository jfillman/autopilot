"""The gateway core, independent of any transport.

authenticate -> find session -> ask the Clearance core -> apply its spend -> backend -> report the outcome.
Clearance decides and records (core.py, the core's HTTP API); this module enforces. Every call, allowed or denied,
is a decision record in the core's chain, so "what did the agent try to do" is answerable from the evidence alone.
An allowed call's result is a second record, its outcome, linked to the decision.

A deployment builds its Gateway from parts it names: the packs whose tools it registers (tiers.Pack), a handler per
tool (backends.core_handlers plus its packs' handlers), a RunBackend for the run tree, and the core. Nothing is found
by import.

The outage policy (core.py): when the core is unreachable, every T1-and-above call, every session open, every spawn
and every model call is denied with CORE-UNAVAILABLE; a T0 read is denied too unless the deployment allows reads
(GATEWAY_OUTAGE_ALLOW_READS), and then only an unscoped one (_outage_call), recorded later as coverage-unavailable.
"""
from __future__ import annotations

import dataclasses
import functools
import hashlib
import json
import threading
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Callable, Mapping, Protocol, Sequence

from fnmatch import fnmatchcase

from . import control as ctl, definitions as defn, events as evh, inbox as hr
from . import results as res, runtimes as runtimes_mod
from .backends import Handler, RunBackend
# Clearance's half (the decision and the record), through the core's API (core.py).
from .core import (MODEL_VERSION, OUTAGE_RULE, REFUSED_RULE, AuthError, Core, CoreUnreachable, Denied, Verdict,  # noqa: F401
                   args_digest, iso, outage_verdict, rfc3339, sha256_hex)
from .modelproxy import ModelDecision, ModelProxy, ModelRoute, usage_record
from .profile import AgentDefinition
from .scope import BadPath, normalize
from .session import Session, SessionStore
from .tiers import Pack, Tier, registry, CORE_TOOLS

# A Preflight result's values (clearance evidence `preflight.result`).
PREFLIGHT_RESULTS = ("pass", "fail", "not-run")


@dataclass(frozen=True)
class Principal:
    id: str
    kind: str                 # "user" (delegated agent) or "workload"
    tier_ceiling: Tier = Tier.T1
    # Verified facts an authenticator vouches for beyond the id, e.g. a GitHub Actions token's `repository`.
    attrs: Mapping[str, str] = field(default_factory=dict, compare=False)


class Authenticator(Protocol):
    def authenticate(self, token: str) -> Principal: ...


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


# A Preflight run of a candidate definition (A-6; Gateway.run_agent with `document`) is clamped to this envelope, and
# a budget in the request can only lower it. The candidate runs as the person, so it can do no more than they may.
PREFLIGHT_CAP = {"ttl_minutes": 10, "tool_calls": 60, "model_tokens": 150_000, "children": 3}
PREFLIGHT_TIER = Tier.T1
PREFLIGHT_KEEP = 500          # candidate runs remembered for scoring (session -> definition fingerprint)
MAX_RUN_INPUT = 2000          # a person's task input to an in-cluster run (an AgentRun's input is 4 KiB in all)

# During an outage the gateway cannot check scope (repos.allow, an app allowlist, an API allowlist): a read whose
# arguments name a repo, an app or a service is denied even when reads are allowed.
OUTAGE_SCOPED_ARGS = ("repo", "app", "service")

LOWER_ENVS = frozenset({"dev", "development", "sandbox", "scratch"})


def _durable(op):
    """End every public operation with store.flush(), whatever path it returned by: the sessions it changed, its
    inbox, holds, results and outbox rows commit together in the gateway's database."""
    @functools.wraps(op)
    def run(self, *a, **k):
        try:
            return op(self, *a, **k)
        finally:
            self.store.flush()
    return run


def _subject(pid: str, kind: str, tier: int, acting: str | None = None) -> dict[str, Any]:
    props: dict[str, Any] = {"kind": kind, "tier_ceiling": int(tier)}
    if acting and acting != pid:
        props["acting_as"] = acting
    return {"type": "principal", "id": pid, "properties": props}


def _agent(d: AgentDefinition | None) -> dict[str, Any] | None:
    if d is None:
        return None
    return {"name": d.name, "kind": d.kind, "identity_type": d.identity_type, "service_account": d.service_account,
            "limits": d.limits.as_dict()}


def _scope(d: AgentDefinition) -> dict[str, Any]:
    out = {"agent_kind": d.kind, "deny_paths": list(d.deny_paths), "repos_allow": list(d.repos_allow),
           "api_allow": list(d.api_allow), "field_deny": list(d.field_deny), "field_allow": list(d.field_allow)}
    if getattr(d, "raw_profile", None):
        out["profile"] = d.raw_profile
    return out


def env_tier_of(spec, args: dict[str, Any]) -> str:
    """Which environment tier these arguments point at (the record's `target`; the core computes the same)."""
    if spec.target == "none":
        return "none"
    project = args.get("project")
    if isinstance(project, str):
        return "lower" if project.endswith("-lower") else "upper"
    env = args.get("env")
    if isinstance(env, str):
        return "lower" if (env in LOWER_ENVS or env.startswith(("pr-", "agent-"))) else "upper"
    repo = args.get("repo")
    if isinstance(repo, str):
        return "upper" if repo.split("/")[-1].startswith("gitops-") else "lower"
    return spec.target


class Gateway:
    def __init__(self, definitions: dict[str, AgentDefinition], store: SessionStore,
                 core: Core, auth: Authenticator, clock: Callable[[], datetime], *,
                 packs: Sequence[Pack] = (), handlers: Mapping[str, Handler] | None = None,
                 runs: RunBackend | None = None, models: Mapping[str, ModelRoute] | None = None,
                 model_auth: Authenticator | None = None, inbox: hr.InboxStore | None = None,
                 directory: hr.Directory | None = None, delegates: Sequence[str] = (),
                 events: evh.EventsStore | None = None, holds: ctl.HoldStore | None = None,
                 admins: Sequence[str] = (), definitions_store: defn.DefinitionsStore | None = None,
                 runtime_images: Sequence[str] = (), runtimes: Sequence[Any] = (),
                 results: res.ResultStore | None = None,
                 preflight: Callable[[AgentDefinition], Mapping[str, Any] | None] | None = None,
                 preflight_cases: Mapping[str, Any] | None = None, preflight_ledger: Any = None):
        self.defs, self.store, self.core, self.auth, self.now = definitions, store, core, auth, clock
        # Preflight (A-6): the cases a candidate definition is scored against (preflight.Case by id), and the ledger
        # a score lands in (preflight.PreflightLedger; its `of` is the `preflight` callable above when none is given).
        # Candidates: definitions under Preflight, by their run name; never in `defs`, so no trigger or pin sees them.
        self.preflight_cases = dict(preflight_cases or {})
        self.preflight_ledger = preflight_ledger
        if preflight is None and preflight_ledger is not None:
            preflight = preflight_ledger.of
        self.candidates: dict[str, AgentDefinition] = {}
        self._candidate_runs: dict[str, tuple[str, str, str]] = {}   # session -> (agent, run name, fingerprint)
        # The Preflight result of a definition (A-6 scores them; AP-C1 puts it on every run record): a callable from
        # the definition to {result: pass|fail|not-run, definition, cases}, or None for not-run.
        self.preflight = preflight
        # Where a definition's runtime.image may come from (prefix globs), and the deployment's runtime catalog
        # (runtimes.py): the read API shows them.
        self.runtimes = tuple(runtimes)
        self.runtime_images = tuple(runtime_images) + tuple(runtimes_mod.allowed_images(self.runtimes))
        # Cluster-pinned definitions (definitions.py): where the loaded set is kept for the read API and restarts.
        self.definitions_store = definitions_store
        # Whether the core has the cluster set yet (apply_cluster_definitions): not until the first publish.
        self._definitions_unpublished = True
        self._definitions_publish_lock = threading.Lock()
        # Person controls (control.py): the holds on agents' triggers, and the platform admins (principal globs) who
        # may freeze, end, hold and release beside each agent's owner group.
        self.holds, self.admins = holds, tuple(admins)
        # The trigger bridge's event history (events.py): what it received and did, for the read API. None = not kept.
        self.events = events
        # The session store is single-writer: every transport (the MCP server, the model proxy) takes this lock
        # around a gateway operation.
        self.lock = threading.RLock()
        # The model proxy's decisions (modelproxy.py), on this Gateway's store and core, so a model call is charged
        # to the same session budget and recorded in the same chain as the tool calls. model_auth verifies the run's
        # model-proxy token (its own audience); without models, no model call is ever allowed.
        self.models = ModelProxy(dict(models), store, core, clock) if models else None
        self.model_auth = model_auth
        self.packs = tuple(packs)
        self.tools = registry(CORE_TOOLS, *(p.tools for p in self.packs))
        self.runs = runs
        # Finished runs' answers (results.py), kept results.RETENTION for the read API. None = only the outcome is kept.
        self.results = results
        handlers = dict(handlers or {})
        unknown = sorted(set(handlers) - set(self.tools))
        if unknown:
            raise ValueError(f"handlers for tools that are not registered: {unknown}")
        owned = sorted(set(handlers) & CORE_OWNED)
        if owned:
            raise ValueError(f"handlers for tools the Gateway owns: {owned}")
        self._handlers: dict[str, Handler] = handlers
        if runs is not None:
            self._handlers.update({"run.spawn": self._spawn_unreached, "run.close": self._close})
        # human.request's inbox (inbox.py): the Gateway's own handlers, since a request needs its session. delegates are
        # the principals (globs) that may answer for a person they name, e.g. Tower's backend.
        self.inbox, self.directory, self.delegates = inbox, directory, tuple(delegates)
        if inbox is not None:
            self._handlers.update({"human.request": self._human_request, "human.request.get": self._human_get})

    # -- sessions --------------------------------------------------------
    def _open_request(self, subject: dict[str, Any], agent: str, props: dict[str, Any], now: datetime,
                      **ctx: Any) -> dict[str, Any]:
        c: dict[str, Any] = {"now": rfc3339(now), "new_session_id": self.store.new_id()}
        d = self._def(agent)
        if d is not None:
            c["agent"] = _agent(d)
        c.update({k: v for k, v in ctx.items() if v is not None})
        return {"subject": subject, "action": {"name": "session.open", "properties": props},
                "resource": {"type": "agent", "id": agent}, "context": c}

    def _evaluate_open(self, req: dict[str, Any]) -> Verdict:
        """An open's decision; the session id it named goes back to the store unless the core granted it."""
        try:
            v = self.core.evaluate(req)
        except CoreUnreachable as e:
            v = outage_verdict(e)
        if not v.allow:
            self.store.give_back(req["context"]["new_session_id"])
        return v

    def _deny_open(self, v: Verdict) -> Result:
        return Result(False, {"allow": False, "rule": v.rule, "message": v.message}, error=v.message, audit_seq=v.seq)

    @_durable
    def open_session(self, token: str, agent: str, limits: dict | None = None,
                     task_id: str | None = None) -> Result:
        principal = self.auth.authenticate(token)
        now = self.now()
        props: dict[str, Any] = {}
        if limits is not None:
            props["limits"] = limits
        if task_id:
            props["task_id"] = task_id
        req = self._open_request(_subject(principal.id, principal.kind, principal.tier_ceiling), agent, props, now)
        v = self._evaluate_open(req)
        if not v.allow:
            return self._deny_open(v)
        s = self.store.adopt(dict(v.grant), now)
        return Result(True, {"allow": True}, {"session": s.id, "task_id": s.task_id,
                                              "expires_at": iso(s.expires_at),
                                              "limits": s.limits.as_dict()}, audit_seq=v.seq, session=s.id)

    @_durable
    def open_triggered(self, agent: str, trigger: str, task_id: str | None = None) -> Result:
        """Start a scheduled or event-driven agent. The run's identity is the agent's own
        workload identity; the trigger that started it is recorded, not trusted."""
        now = self.now()
        held = self.hold_for(agent) if agent in self.defs else None
        live = self.store.find_live(agent, task_id, now) if task_id else None
        props: dict[str, Any] = {"trigger": trigger}
        if task_id:
            props["task_id"] = task_id
        req = self._open_request(_subject(f"trigger:{trigger}", "workload", 1), agent, props, now,
                                 hold={"by": held.by, "until": rfc3339(held.until), "reason": held.reason} if held else None,
                                 live=live.state() if live else None)
        v = self._evaluate_open(req)
        if not v.allow:
            return self._deny_open(v)
        g = dict(v.grant)
        if g.get("deduped"):
            self.store.give_back(req["context"]["new_session_id"])
            s = self.store.get(g["session"])
            return Result(True, {"allow": True}, {"session": s.id, "task_id": s.task_id,
                                                  "expires_at": iso(s.expires_at), "deduped": True},
                          audit_seq=v.seq, session=s.id)
        s = self.store.adopt(g, now)
        return Result(True, {"allow": True}, {"session": s.id, "task_id": s.task_id,
                                              "expires_at": iso(s.expires_at), "deduped": False},
                      audit_seq=v.seq, session=s.id)

    @_durable
    def launch(self, session_id: str, run_input: dict | None = None) -> Result:
        """Start the run for a top-level session (a triggered or delegated in-cluster run; on Hangar, an
        AgentRun XR). Children are created by run.spawn; service agents are deployed as applications and have no run.
        The run.launch record must reach the core: when it cannot, the run is deleted again (fail closed)."""
        s = self.store.get(session_id)
        if s is None or s.closed:
            return Result(False, {"allow": False, "rule": "R004"}, error="no live session")
        d = self._def(s.agent)
        if d.runs_in != "cluster":
            return Result(False, {"allow": False, "rule": "R001"}, session=s.id,
                          error=f"{d.name} has no runtime image: it runs in a person's MCP client, not as an in-cluster run")
        if d.kind == "service":
            return Result(False, {"allow": False, "rule": "R001"}, session=s.id,
                          error="service agents are deployed as applications, not launched as runs")
        if self.runs is None:
            return Result(False, {"allow": False, "rule": "R001"}, session=s.id,
                          error="this deployment has no run backend")
        run = self.runs.create(s, d, run_input)
        s.run = run
        s.input = _input_view(run_input)
        try:
            seq = self._record("run.launch", s, s.principal, {"run": run}, "created", required=True)
        except CoreUnreachable as e:
            try:
                self.runs.delete(s)
            except Exception:  # noqa: BLE001 - the session keeps no run; the sweep's backstop finds a leftover
                pass
            s.run, s.input = None, None
            v = outage_verdict(e)
            return Result(False, {"allow": False, "rule": v.rule}, session=s.id, error=v.message)
        return Result(True, {"allow": True}, {"run": run, "session": s.id}, audit_seq=seq, session=s.id)

    # -- tool calls ------------------------------------------------------
    def _tool_request(self, principal: Principal, session_id: str, s: Session | None, tool: str,
                      args: dict[str, Any], now: datetime) -> dict[str, Any]:
        acting = self._acting(s, principal.id) if s is not None else None
        req: dict[str, Any] = {
            "subject": _subject(principal.id, principal.kind, principal.tier_ceiling, acting),
            "action": {"name": "tool.call", "properties": {"args": args}},
            "resource": {"type": "tool", "id": tool},
            "context": {"now": rfc3339(now), "session_id": session_id}}
        if s is not None:
            req["context"]["session"] = s.state()
            req["context"]["scope"] = _scope(self._def(s.agent))
        return req

    @_durable
    def call(self, token: str, session_id: str, tool: str, args: dict[str, Any]) -> Result:
        principal = self.auth.authenticate(token)
        s = self.store.get(session_id)
        now = self.now()
        req = self._tool_request(principal, session_id, s, tool, args, now)
        spawn = s is not None and tool == "run.spawn" and self.runs is not None and tool in self._handlers
        child_def = None
        if spawn:
            req["action"]["name"] = "session.spawn"
            child_def = self.defs.get(args.get("agent")) if isinstance(args.get("agent"), str) else None
            if child_def is not None:
                req["context"]["agent"] = _agent(child_def)
            req["context"]["new_session_id"] = self.store.new_id()
        try:
            v = self.core.evaluate(req)
        except CoreUnreachable as e:
            if spawn:
                self.store.give_back(req["context"]["new_session_id"])
            return self._outage_call(principal, session_id, s, tool, args, now, e)
        if spawn and not v.allow:
            self.store.give_back(req["context"]["new_session_id"])
        if s is None or not v.allow:
            self.store.charge(s, v.spend)
            return Result(False, v.as_dict(), error=v.message, audit_seq=v.seq, session=s.id if s else session_id)
        spec = self.tools[tool]
        handler = self._handlers.get(tool)
        if handler is None:
            # Registered (its policy is known) but nothing in this deployment backs it. Charged nothing.
            seq = self.core.outcome(v.decision_id, "error:NoHandler")
            return Result(False, v.as_dict(), error="no backend for this tool in this deployment",
                          audit_seq=seq or v.seq, session=s.id)
        self.store.charge(s, v.spend)
        try:
            data = self._spawn(s, child_def, v, args) if spawn else handler(s, args)
        except Denied as e:
            s.record_denial()
            # The handler's rule goes on the outcome as its refusal (claimed), beside the decision's own rule.
            seq = self.core.outcome(v.decision_id, f"refused {e.rule}: {e.message}"[:4000],
                                    refusal=(e.rule, e.message))
            dd = Verdict(False, e.rule, "refused", e.message, e.hint, int(spec.tier))
            return Result(False, dd.as_dict(), error=e.message, audit_seq=seq or v.seq, session=s.id)
        except Exception as e:  # noqa: BLE001 - never leak backend internals to the agent
            seq = self.core.outcome(v.decision_id, f"error:{type(e).__name__}")
            return Result(False, v.as_dict(), error="backend error", audit_seq=seq or v.seq, session=s.id)
        seq = self.core.outcome(v.decision_id, "ok", effect=_effect(spec, args, data))
        return Result(True, v.as_dict(), data, audit_seq=seq or v.seq, session=s.id)

    def _outage_call(self, principal: Principal, session_id: str, s: Session | None, tool: str,
                     args: dict[str, Any], now: datetime, err: CoreUnreachable) -> Result:
        """No decision from the core. Denied, unless the deployment allows reads and this is one the gateway can
        stand behind on its own: a T0 tool with a handler, in a live, untripped session of the caller's, granted to
        it, with budget left, whose arguments name no repo, app or service (scope only the core can check). Such a
        read runs, is charged, and is written to the outbox as a coverage-unavailable record."""
        spec = self.tools.get(tool)
        tier = int(spec.tier) if spec is not None else None
        handler = self._handlers.get(tool)
        ok = (not err.refused and self.core.allow_reads and spec is not None and spec.tier == Tier.T0
              and handler is not None and tool not in CORE_OWNED and not tool.startswith("chat.")
              and s is not None and not s.expired(now) and not s.tripped
              and self._acting(s, principal.id) == s.principal and tool in s.limits.tools
              and isinstance(args, dict) and all(a in args for a in spec.required_args)
              and not any(a in args for a in OUTAGE_SCOPED_ARGS)
              and s.can_spend("tool_calls") and s.can_spend("github_calls", spec.github_cost))
        if not ok:
            v = outage_verdict(err, tier)
            return Result(False, v.as_dict(), error=v.message, session=s.id if s else session_id)
        s.used["tool_calls"] += 1
        if spec.github_cost:
            s.used["github_calls"] += spec.github_cost
        allowed = Verdict(True, None, None, "allowed during a Clearance outage (GATEWAY_OUTAGE_ALLOW_READS)", "", tier)
        fields = {"task_id": s.task_id, "session": s.id, "parent": s.parent_id, "principal": principal.id,
                  "agent": s.agent, "tool": tool, "tier": tier, "decision": "allow", "target": env_tier_of(spec, args),
                  "args_sha256": args_digest(args),
                  "reason": f"the core was unreachable at {iso(now)}; allowed as an unscoped T0 read "
                            "(GATEWAY_OUTAGE_ALLOW_READS)"}
        try:
            data = handler(s, args)
        except Denied as e:
            s.record_denial()
            self.core.outage_read({**fields, "result": f"refused {e.rule}: {e.message}"[:4000]})
            dd = Verdict(False, e.rule, "refused", e.message, e.hint, tier)
            return Result(False, dd.as_dict(), error=e.message, session=s.id)
        except Exception as e:  # noqa: BLE001
            self.core.outage_read({**fields, "result": f"error:{type(e).__name__}"})
            return Result(False, allowed.as_dict(), error="backend error", session=s.id)
        self.core.outage_read({**fields, "result": "ok"})
        return Result(True, allowed.as_dict(), data, session=s.id)

    @_durable
    def finish_run(self, token: str, session_id: str, body: Any) -> dict[str, Any]:
        """A run hands back its result as it exits (the MCP tool run_finish; autopilot's runtime calls it after writing
        result.json): the answer goes to the result store for results.RETENTION, the outcome onto the session, a
        run.finish record to the core (the status, never the text), and the session closes, so Tower shows the run
        finished and the next sweep deletes its run. Only the session's own run, or its principal, may finish it, and
        only once. Raises ControlError."""
        principal = self.auth.authenticate(token)
        s = self.store.get(session_id)
        if s is None or s.closed:
            raise ctl.ControlError("R004", "no live session", "The session already ended: its outcome is kept.", 409)
        if not s.run or self._acting(s, principal.id) != s.principal:
            raise ctl.ControlError("R001", "only this session's own run may finish it", "", 403)
        b = body if isinstance(body, dict) else {}
        status = b.get("status")
        if status not in res.STATUSES:
            raise ctl.ControlError("R008", f"status is one of {', '.join(res.STATUSES)}", "", 400)
        text = b.get("final_text") or ""
        reason = b.get("reason") or ""
        turns = b.get("turns") or 0
        if not isinstance(text, str) or not isinstance(reason, str) or not isinstance(turns, int) or isinstance(turns, bool):
            raise ctl.ControlError("R008", "final_text and reason are strings, turns an integer", "", 400)
        text, truncated = res.clip_text(text)
        calls = res.tool_call_summary(b.get("tool_calls"))
        now = self.now()
        kept = None
        if self.results is not None:
            expires = now + res.RETENTION
            self.results.put({"session": s.id, "task_id": s.task_id, "agent": s.agent, "principal": s.principal,
                              "status": status, "final_text": text, "truncated": truncated, "turns": turns,
                              "tool_calls": calls, "reason": reason[:res.MAX_REASON], "written_at": iso(now),
                              "expires_at": iso(expires)}, now)
            kept = {"bytes": len(text.encode()), "sha256": sha256_hex(text.encode()), "truncated": truncated,
                    "expires_at": iso(expires)}
        s.outcome = {"status": status, "finished_at": iso(now), "turns": turns, "tool_calls": len(calls),
                     "not_ok": sum(1 for c in calls if not c["ok"]), "reason": reason[:res.MAX_REASON],
                     "result": kept, "via": "runtime"}
        seq = self._record("run.finish", s, principal.id, {"status": status, "turns": turns}, status)
        self.store.close(s.id)
        return {"session": s.id, "status": status, "closed": True, "kept": kept, "audit_seq": seq}

    def _backstop_outcomes(self, now: datetime) -> None:
        """Sessions whose run ended without run_finish (an older runtime, or a crash): the run backend says how the
        workload ended (RunBackend.ended, optional), and the session closes with that as its outcome. Sessions that
        reach their deadline first are given the outcome "expired"."""
        ended = getattr(self.runs, "ended", None)
        for s in self.store.all():
            if s.closed or s.outcome is not None:
                continue
            if now >= s.expires_at:
                s.outcome = {"status": "expired", "finished_at": iso(s.expires_at), "result": None, "via": "deadline"}
                continue
            if not s.run or ended is None:
                continue
            try:
                phase = ended(s)
            except Exception:  # noqa: BLE001 - asked again on the next sweep
                continue
            if phase is None:
                continue
            s.outcome = {"status": "no-result", "run_phase": str(phase)[:40], "finished_at": iso(now), "result": None,
                         "via": "run"}
            self._record("run.finish", s, s.principal, {"run": s.run, "phase": str(phase)[:40]}, "no-result")
            self.store.close(s.id)

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
                            getattr(self._def(s.agent), "owner", None), question, options, now, s.expires_at)
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

    def _def(self, name: str) -> AgentDefinition | None:
        """A live definition, else a candidate under Preflight (by its run name)."""
        d = self.defs.get(name)
        return d if d is not None else self.candidates.get(name)

    # -- Preflight (A-6) -----------------------------------------------------------------------------------------
    def _candidate(self, agent: str, document: Any, budget: Any) -> tuple[AgentDefinition, str]:
        """The sandboxed definition a PR's candidate document runs as, and its fingerprint. The document is parsed by
        the loader like any pin (a DefinitionError is a 400 naming the problem), must name `agent`, and must pin its
        runtime image by digest (and be on the deployment's image list when it has one). The run is then a task as
        the calling person with the budget's envelope: tier at most T1 and only the tools at or under it that open no PR and spend no GitHub calls, no GitHub
        calls or PRs, time, calls, tokens and children capped (PREFLIGHT_CAP; `budget` can only lower them), no
        triggers. It is named pf-<agent>-<fingerprint> so it never shadows the live definition."""
        from . import profile as prof
        from ..preflight import fingerprint
        if not isinstance(document, dict):
            raise ctl.ControlError("R008", "document is an agent definition (an object)", "", 400)
        try:
            cand = prof.parse(document)
        except prof.DefinitionError as e:
            raise ctl.ControlError("R008", f"the candidate definition would not load: {e}", "", 400) from None
        if cand.name != agent:
            raise ctl.ControlError("R008", f"the document names {cand.name!r}, not {agent!r}", "", 400)
        if cand.runs_in != "cluster":
            raise ctl.ControlError("R001", f"{agent} runs in a person's MCP client; Preflight runs in-cluster agents", "", 409)
        if "@sha256:" not in (cand.image or ""):
            raise ctl.ControlError("R008", "the candidate's runtime image must be pinned by digest", "", 400)
        if self.runtime_images and not any(fnmatchcase(cand.image, g) or cand.image.startswith(g.rstrip("*")) for g in self.runtime_images):
            raise ctl.ControlError("R008", "the candidate's runtime image is not on this deployment's list", "", 400)
        b = budget if isinstance(budget, dict) else {}
        def cap(key: str, own: int) -> int:
            want = b.get(key)
            want = want if isinstance(want, int) and not isinstance(want, bool) and want > 0 else PREFLIGHT_CAP[key]
            return max(1, min(want, PREFLIGHT_CAP[key], own)) if own > 0 else 0
        lim = cand.limits
        keep = frozenset(t for t in lim.tools if t in self.tools and self.tools[t].tier <= PREFLIGHT_TIER
                         and not self.tools[t].propose_only and not self.tools[t].github_cost)
        limits = dataclasses.replace(
            lim, tier_ceiling=min(lim.tier_ceiling, PREFLIGHT_TIER), tools=keep, github_calls=0, open_prs=0,
            ttl_minutes=cap("ttl_minutes", lim.ttl_minutes), tool_calls=cap("tool_calls", lim.tool_calls),
            model_tokens=cap("model_tokens", lim.model_tokens), children=cap("children", lim.children))
        fp = fingerprint(cand)
        name = f"pf-{agent[:28]}-{fp[:8]}"
        if name in self.defs:
            raise ctl.ControlError("R001", f"{name} is a live definition", "", 409)
        return dataclasses.replace(cand, name=name, kind="task", identity_type="delegated", service_account=None,
                                   triggers=({"type": "manual"},), limits=limits), fp

    def preflight_case_list(self, token: str, agent: Any, on_behalf_of: str | None = None) -> dict[str, Any]:
        """The Preflight cases of an agent, for the PR check that launches their runs: id, title, the run's input and
        how many runs the case reads. Any authenticated caller; the cases are in the repo."""
        self.auth.authenticate(token)
        if not isinstance(agent, str) or not agent:
            raise ctl.ControlError("R008", "agent is the definition's name", "", 400)
        return {"agent": agent, "cases": [{"id": c.id, "title": c.title, "input": c.input, "runs": c.runs}
                                          for c in sorted(self.preflight_cases.values(), key=lambda c: c.id)
                                          if c.agent == agent]}

    def preflight_score(self, token: str, body: Any, on_behalf_of: str | None = None) -> dict[str, Any]:
        """Score the Preflight runs of a candidate: {"agent": name, "cases": [{"case": id, "sessions": [sid, ...]}]}
        (one session per run the case reads, in order). Each case's result(s) are scored (preflight.py) with the
        session's model usage as the evidence; when every case has finished, the definition's result lands in the
        ledger (`pass` only if every case passed) and so on the run records of that definition while it is unchanged.
        Answers {finished, passed, scores, cases} (finished false: some run has not ended; nothing is recorded).
        Raises ControlError."""
        from .. import preflight as pf
        b = body if isinstance(body, dict) else {}
        agent, asked = b.get("agent"), b.get("cases")
        if not isinstance(agent, str) or not isinstance(asked, list) or not asked or len(asked) > 50:
            raise ctl.ControlError("R008", 'body is {"agent": name, "cases": [{"case": id, "sessions": [id, ...]}]}', "", 400)
        self._manager(token, on_behalf_of, agent)
        scores: list[dict[str, Any]] = []
        finished, fps = True, set()
        for item in asked:
            cid = item.get("case") if isinstance(item, dict) else None
            sids = item.get("sessions") if isinstance(item, dict) else None
            case = self.preflight_cases.get(cid) if isinstance(cid, str) else None
            if case is None or case.agent != agent:
                raise ctl.ControlError("R003", f"no Preflight case {cid!r} for {agent}", "", 404)
            if not isinstance(sids, list) or not sids or not all(isinstance(x, str) for x in sids):
                raise ctl.ControlError("R008", "sessions is the list of the case's run sessions", "", 400)
            results, ev, unfinished = [], [], []
            for sid in sids:
                made = self._candidate_runs.get(sid)
                if made is None or made[0] != agent:
                    raise ctl.ControlError("R003", f"{sid} is not a Preflight run of {agent}", "", 404)
                fps.add(made[2])
                r = self.results.get(sid, self.now()) if self.results is not None else None
                sess = self.store.get(sid)
                if r is None and sess is not None and sess.outcome:      # ended without handing a result back
                    r = {"status": sess.outcome.get("status"), "final_text": "", "tool_calls": []}
                if r is None:
                    unfinished.append(sid)
                    continue
                results.append(r)
                # The run's refused calls are denials in the evidence (a refused call to a TRIPWIRES tool is a
                # tripwire attempt): without them the usage record alone would hide them from the score.
                for c in r.get("tool_calls") or []:
                    if isinstance(c, dict) and not c.get("ok") and c.get("rule"):
                        ev.append({"tool": c.get("tool"), "decision": "deny", "rule": c.get("rule")})
                if sess is not None and isinstance(sess.usage.get("charged_tokens"), int):
                    ev.append({"tool": "model.call", "decision": "allow", "usage": {"charged_tokens": sess.usage["charged_tokens"]}})
            if unfinished or len(results) < case.runs:
                finished = False
                scores.append({"case": cid, "finished": False, "waiting_on": unfinished})
                continue
            rec, sc = pf.score_runs(case, results, ev)
            scores.append({**pf.score_view(sc, rec), "finished": True})
        out: dict[str, Any] = {"agent": agent, "finished": finished, "scores": scores}
        if finished:
            passed = all(x["passed"] for x in scores)
            out["passed"] = passed
            if self.preflight_ledger is not None and len(fps) == 1:
                out["record"] = self.preflight_ledger.put(agent, next(iter(fps)), passed, len(scores))
        return out

    @_durable
    def run_agent(self, token: str, agent: str, run_input: Any = None, on_behalf_of: str | None = None,
                  document: Any = None, budget: Any = None) -> dict[str, Any]:
        """A person starts an in-cluster run of a task agent (Tower's Run, or their own token): a session opened as the
        person, then launch() with their input as the run's task. Only a task agent with a runtime image; only the
        agent's owner group or a platform admin (the person controls' rule). The session is the person's, so the run
        acts with their delegated authority inside the definition's envelope. Recorded as session.open (via the
        delegate) and run.launch. Raises ControlError."""
        d = self.defs.get(agent)
        fp = None
        if document is not None:
            # A candidate under Preflight: the person must manage the live agent (or be an admin, for a new one).
            person, via = self._manager(token, on_behalf_of, agent)
            d, fp = self._candidate(agent, document, budget)
            self.candidates[d.name] = d
        elif d is None:
            raise ctl.ControlError("R003", "no such agent", "", 404)
        else:
            person, via = self._manager(token, on_behalf_of, agent)
        if d.kind != "task" or d.identity_type != "delegated":
            raise ctl.ControlError("R001", f"{agent} is a {d.kind} agent; only task agents are run by a person",
                                   "Event and scheduled agents start from their triggers.", 409)
        if d.runs_in != "cluster":
            raise ctl.ControlError("R001", f"{agent} runs in a person's MCP client, not in the cluster",
                                   "Open a session from your MCP client (session_open), or give the definition a runtime image.", 409)
        if run_input is not None and (not isinstance(run_input, str) or len(run_input) > MAX_RUN_INPUT):
            raise ctl.ControlError("R008", f"input is a string of at most {MAX_RUN_INPUT} characters", "", 400)
        if not (run_input or "").strip() and not d.instructions:
            raise ctl.ControlError("R008", f"{agent} has no instructions of its own: say what it should do", "", 400)
        held = self.hold_for(agent) if document is None else None
        if held is not None:
            raise ctl.ControlError("R024", f"{agent} is held until {iso(held.until)}: {held.reason}", "Release the hold first.", 409)
        # The session is opened by the core's evaluation, as the person (decided like any open; recorded by the core
        # as session.open). `run: true` marks a person's run, and `via` the delegate they acted through: the core's
        # record is clearance v0.26.1's run_agent record (args_sha256 of {"via": ...}, `opened to run[ via X]`).
        now = self.now()
        req = self._open_request(_subject(person, "user", int(Tier.T1)), d.name,
                                 {"run": True, **({"via": via} if via else {})}, now)
        v = self._evaluate_open(req)
        if not v.allow and v.rule in (OUTAGE_RULE, REFUSED_RULE):
            raise ctl.ControlError(v.rule, v.message, v.hint, 503)
        if not v.allow:
            raise ctl.ControlError(v.rule or "R008", v.message, v.hint, 409)
        s = self.store.adopt(dict(v.grant), now)
        # The runtime's input (autopilot runtime hangar_agent.loop): the person's words are the prompt, the
        # definition's instructions its system prompt, as the trigger bridge passes them for an event.
        task = {"prompt": (run_input or "").strip() or "Carry out your instructions.", "started_by": person}
        if d.instructions:
            task["instructions"] = d.instructions
        out = self.launch(s.id, task)
        if not out.ok:
            raise ctl.ControlError(out.decision.get("rule", "R001"), out.error or "the run was not launched", "", 409)
        view = {"agent": agent, "session": s.id, "task_id": s.task_id, "run": out.data.get("run"), "by": person, "via": via}
        if fp is not None:
            self._candidate_runs[s.id] = (agent, d.name, fp)
            for old in list(self._candidate_runs)[:-PREFLIGHT_KEEP]:
                self._candidate_runs.pop(old, None)
            kept = {v[1] for v in self._candidate_runs.values()}
            for n in [n for n in self.candidates if n not in kept]:
                self.candidates.pop(n, None)
            view.update({"preflight": True, "definition": d.name, "fingerprint": fp, "limits": d.limits.as_dict()})
        return view

    def agents_for(self, token: str) -> list[dict[str, Any]]:
        """The agent definitions as a person sees them from an MCP client: what each is for, where it runs, its kind
        and tools. Any authenticated caller; nothing here is secret (instructions are never included)."""
        self.auth.authenticate(token)
        return [{"name": d.name, "description": d.description, "kind": d.kind, "identity": d.identity_type,
                 "runs_in": d.runs_in, "tier_ceiling": d.limits.tier_ceiling.name, "tools": sorted(d.limits.tools),
                 "can_open_session": d.identity_type == "delegated"}
                for d in sorted(self.defs.values(), key=lambda d: d.name)]

    def _control_record(self, person: str, via: str | None, tool: str, s: Session | None, agent: str, target: str,
                        args: dict[str, Any], result: str) -> int | None:
        """A person control's record (session.freeze, session.end, trigger.hold, trigger.release). The control has
        already taken effect: an unreachable core queues the record, it never undoes a freeze or a hold."""
        return self.core.record(tool, {
            "task_id": s.task_id if s else None, "session": s.id if s else None,
            "parent": s.parent_id if s else None, "principal": person, "agent": agent,
            "decision": "allow", "target": target, "args_sha256": args_digest(args),
            "result": result + (f" via {via}" if via else "")})

    # -- cluster-pinned definitions (definitions.py) -----------------------------------------------------------
    @_durable
    def apply_cluster_definitions(self, loaded: Sequence[defn.Loaded], rejected: Sequence[defn.Rejected] = ()) -> defn.Changes:
        """The loader's hand-off: make `loaded` + `rejected` the cluster set (definitions.Definitions.replace_cluster),
        persist it for the read API, and publish the whole set to the core (Core.publish_definitions, PUT
        /v1/definitions), which decides each pin again and records what changed (definition.load, .unload,
        .reject), once each. The gateway writes no definition.* record itself.

        Published after a change, and on the first call whatever it found (the core keeps the set across a gateway
        restart; a set that emptied while the gateway was down must still reach it). A publish the core did not take
        is made again on the next call, whole: the set is state, so there is nothing to queue. The publish runs
        outside the gateway's lock, so a slow core never holds up tool calls."""
        if not isinstance(self.defs, defn.Definitions):
            raise TypeError("cluster definitions need a definitions.Definitions mapping")
        with self.lock:
            ch = self.defs.replace_cluster(list(loaded), list(rejected))
            if ch and self.definitions_store is not None:
                self.definitions_store.replace(self.defs.records())
                self.store.flush()   # commit the set before the publish: no transaction stays open across the call
            if ch:
                self._definitions_unpublished = True
        self._publish_definitions()
        return ch

    def publish_definitions(self) -> bool:
        """Publish the definition set (the bundled agents and the cluster pins) if the core does not have it yet.
        The loader does this on every poll; a gateway with no loader calls it at start and until the core takes it."""
        return self._publish_definitions()

    def _publish_definitions(self) -> bool:
        """Publish the current cluster set if the core does not have it yet. One publish at a time, of the set as it
        is when the publish starts, so a later set is never overwritten by an earlier one."""
        with self._definitions_publish_lock:
            if not self._definitions_unpublished:
                return True
            with self.lock:
                version = self.defs.version
                pins, rejected = self.defs.cluster_set()
                bundled = self.defs.bundled_set()
            ok = self.core.publish_definitions(pins, rejected, bundled)
            with self.lock:
                if ok and self.defs.version == version:
                    self._definitions_unpublished = False
            return ok

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
        return {"session": s.id, "agent": s.agent, "tripped": True, "trip_reason": s.trip_reason, "audit_seq": rec}

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
        rec = self._end(s, person, via, text)
        return {"session": s.id, "agent": s.agent, "closed": True, "audit_seq": rec}

    def _end(self, s: Session, person: str, via: str | None, text: str) -> int | None:
        """End a live session and its children, and write session.end through the core (end_session's path, shared
        with close_session). The control has taken effect before the record: an unreachable core queues it."""
        self.store.close(s.id)
        return self._control_record(person, via, "session.end", s, s.agent, s.id, {"reason": text}, f"ended: {text}")

    @_durable
    def close_session(self, token: str, session_id: str, reason: Any = None) -> dict[str, Any]:
        """The MCP tool session_close: the session's own caller ends it (a person's MCP client, or the session's own
        in-cluster run), instead of leaving it live until its TTL. Owner-only: anyone else is refused, the agent's
        owner group and platform admins included (they use the person control, end_session: Tower's End). The
        session and its children close, session.end is written through the core (end_session's path), and its
        run and its children's runs are deleted as the expiry sweep deletes them, each with a run.delete record.
        Raises ControlError."""
        principal = self.auth.authenticate(token)
        s = self.store.get(session_id)
        if s is None or self._acting(s, principal.id) != s.principal:
            # The same answer for an unknown session and another's: a caller learns nothing of sessions not its own.
            raise ctl.ControlError("CT-FORBIDDEN", "no such session of yours",
                                   "session_close ends only your own session; a person ends another's from Tower.", 403)
        if s.closed:
            raise ctl.ControlError("CT-CLOSED", "this session is already closed", "", 409)
        text = ctl.check_reason(reason) if reason not in (None, "") else "closed by its owner (session_close)"
        rec = self._end(s, principal.id, None, text)
        runs = [sid for sid in [s.id, *self._descendants(s)] if (x := self.store.get(sid)) is not None and x.run]
        self._delete_runs(s, s, "closed")
        return {"session": s.id, "agent": s.agent, "closed": True, "audit_seq": rec,
                "runs_deleted": sorted(sid for sid in runs if not self.store.get(sid).run)}

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
        return {**hold.view(), "audit_seq": rec}

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
        return {"agent": agent, "released": True, "audit_seq": rec}

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
        # The answer stands only once its record does: an unreachable core refuses it (the person retries).
        try:
            rec = self.core.record("human.answer", {
                "task_id": r.task_id, "session": r.session, "principal": person, "agent": r.agent,
                "decision": "allow", "target": r.id, "args_sha256": args_digest({"answer": text}),
                "result": "answered" + (f" via {via}" if via else "")}, required=True)
        except CoreUnreachable as e:
            raise hr.InboxError(e.rule, "Clearance is unreachable: the answer was not recorded, so it is not given",
                                "Answer again in a minute.", 503) from None
        r.status, r.answer, r.answered_by, r.answered_via, r.answered_at = "answered", text, person, via, now
        self.inbox.save(r)
        return {**r.view(now), "audit_seq": rec}

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
    def model_usage(self, session_id: str, usage: dict[str, Any] | int | None) -> dict[str, int] | None:
        """Charge what an allowed model call actually used (an overrun trips the session's breaker) and add it to the
        session's running cost (A-4). `usage` is the provider's usage block; an int is a bare charge. Returns the
        usage as its outcome carries it (modelproxy.usage_record), which model_outcome then reports: the record's
        charged_tokens is this call's whole charge (the budget counts it up to its limit: ModelProxy.record_usage)."""
        if self.models is None:
            return None
        if isinstance(usage, dict):
            rec = usage_record(usage)
            self.models.record_usage(session_id, rec["charged_tokens"], rec)
            return rec
        if isinstance(usage, int) and not isinstance(usage, bool):
            # A bare charge: no token breakdown, but the same one call in the session's running cost as in its budget.
            rec = {**usage_record(None), "charged_tokens": max(usage, 0)}
            self.models.record_usage(session_id, rec["charged_tokens"], rec)
            return rec
        return None

    @_durable
    def model_outcome(self, decision_id: str | None, result: str, *, stream: bool,
                      upstream_status: int | None = None, usage: dict[str, int] | None = None,
                      model_version: str | None = None) -> int | None:
        """The outcome of an allowed model call, once it ran (modelproxy.ModelProxy.outcome): one per allowed
        decision, written as this gateway's provider, with `stream` on its receipt and, from A-4, the call's usage and
        charge (model_usage's answer) and the model that answered."""
        if self.models is None:
            return None
        return self.models.outcome(decision_id, result, stream=stream, upstream_status=upstream_status, usage=usage,
                                   model_version=model_version)

    # -- run tree --------------------------------------------------------
    def _spawn_unreached(self, s: Session, a: dict[str, Any]) -> dict[str, Any]:
        raise AssertionError("run.spawn is decided as session.spawn in Gateway.call")

    def _spawn(self, parent: Session, d: AgentDefinition | None, v: Verdict, a: dict[str, Any]) -> dict[str, Any]:
        """An allowed session.spawn: the child is the core's grant, cut from what the parent has left after this
        call's charge, and the parent reserves its budget. A run that fails to start closes the child again."""
        if d is None or not v.grant:
            self.store.give_back((v.grant or {}).get("session"))
            raise Denied("R003", "unknown agent definition", "Spawn an agent that exists in the catalog.")
        child = self.store.adopt(dict(v.grant), self.now(), reserve=dict(v.spend.get("reserve") or {}),
                                 children=int(v.spend.get("children") or 0))
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
        """Close every session past its deadline and delete its run, and its children's, each with a record; then
        post what waits in the outbox, if the core answers.

        The run itself already ended at its deadline (the run backend enforces that without Clearance); this is the
        cleanup. A deletion that fails is recorded and retried by the next sweep, because the session keeps its run."""
        now = now or self.now()
        self._backstop_outcomes(now)
        self.store.sweep(now)
        pending = [s for s in self.store.all() if s.run and s.closed]
        done = []
        for s in pending:
            if s.run and self.runs is not None:
                self._delete_runs(s, s, "finished" if (s.outcome or {}).get("via") in ("runtime", "run") else "expired")
                done.append(s.id)
        self.core.drain()
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
                self._record("run.delete", s, actor.principal, {"run": s.run, "why": why}, f"error:{type(e).__name__}")
                continue
            self._record("run.delete", s, actor.principal, {"run": s.run, "why": why}, "deleted")
            s.run = None

    def _descendants(self, s: Session) -> list[str]:
        out = []
        for c in s.children:
            out.append(c)
            child = self.store.get(c)
            if child is not None:
                out.extend(self._descendants(child))
        return out

    # -- the gateway's own records -----------------------------------------------------------------------------------
    def _record(self, kind: str, s: Session, principal_id: str, args: Any, result: str, *,
                required: bool = False) -> int | None:
        """A runtime record of the session's (run.launch, run.delete, run.finish) through the core's record API:
        the fields the in-process engine wrote (dispatch.py _audit before R-3b), less the ones the core sets, and the
        flight recorder's (A-4): the run's pinned model version and its definition's Preflight result on every one,
        and on run.finish the run's cost."""
        fields: dict[str, Any] = {
            "task_id": s.task_id, "session": s.id, "parent": s.parent_id, "principal": principal_id,
            "agent": s.agent, "decision": "allow", "args_sha256": args_digest(args), "result": result}
        d = self._def(s.agent)
        fields["model_version"] = self.pinned_model(d)
        fields["preflight"] = self.preflight_of(d, s.agent)
        if kind == "run.finish":
            fields["cost"] = self.run_cost(s, d)
        return self.core.record(kind, fields, required=required)

    # -- the flight recorder (A-4: AP-C1, AP-C2) ---------------------------
    def pinned_model(self, d: AgentDefinition | None) -> str | None:
        """The model version a run of `d` runs on: its first listed model (the one its runtime asks for) through this
        deployment's route to the upstream model, or the alias itself when there is no route. None without a model."""
        models = (d.raw_profile.get("models") if d is not None else None) or []
        alias = models[0] if isinstance(models, list) and models and isinstance(models[0], str) else None
        if alias is None:
            return None
        route = self.models.routes.get(alias) if self.models is not None else None
        version = route.upstream_model if route is not None else alias
        return version if MODEL_VERSION.fullmatch(version) else None

    def preflight_of(self, d: AgentDefinition | None, name: str) -> dict[str, Any]:
        """The Preflight result of the run's definition (self.preflight), else not-run."""
        got = self.preflight(d) if (self.preflight is not None and d is not None) else None
        out: dict[str, Any] = {"result": "not-run", "definition": name}
        if isinstance(got, Mapping) and got.get("result") in PREFLIGHT_RESULTS:
            out["result"] = got["result"]
            if isinstance(got.get("cases"), int) and not isinstance(got.get("cases"), bool):
                out["cases"] = got["cases"]
        return out

    def run_cost(self, s: Session, d: AgentDefinition | None) -> dict[str, Any]:
        """A run's cost when it ends (run.finish `cost`): its model usage as the proxy charged it, its model and tool
        calls, its wall seconds from open to now, and the compute class its session was granted."""
        u = s.usage
        created = s.created_at if s.created_at.tzinfo else s.created_at.replace(tzinfo=timezone.utc)
        now = self.now()
        now = now if now.tzinfo else now.replace(tzinfo=timezone.utc)
        cost: dict[str, Any] = {k: int(u.get(k) or 0) for k in ("charged_tokens", "input_tokens", "output_tokens",
                                                                 "cache_creation_input_tokens",
                                                                 "cache_read_input_tokens", "model_calls")}
        cost["tool_calls"] = int(s.used.get("tool_calls") or 0)
        cost["wall_seconds"] = max(0, int((now - created).total_seconds()))
        cost["compute"] = s.limits.compute.name.lower()   # the class the session was granted (narrowed at open)
        return cost


def _input_view(run_input: Any) -> dict[str, Any] | None:
    """What a run was asked, kept on its session for the read API: the prompt (clipped to MAX_RUN_INPUT) and who
    started it. Never the definition's instructions (the definition has them) or an event's payload."""
    if not isinstance(run_input, dict):
        return None
    out = {k: run_input[k][:MAX_RUN_INPUT] for k in ("prompt", "started_by") if isinstance(run_input.get(k), str)}
    return out or None


def _effect(spec, args: dict[str, Any], data: Any) -> dict[str, Any] | None:
    """What a successful write made, from its result (spec.receipt), with the repo and the paths it wrote: the
    receipt a PR's provenance is checked against. None for a tool without a receipt."""
    if not spec.receipt or not isinstance(data, dict):
        return None
    e: dict[str, Any] = {k: data[k] for k in spec.receipt if k in data}
    if spec.writes_files:
        e["repo"] = str(args.get("repo", "")).lower()
        e["paths"] = sorted({_normal(f.get("path", "")) for f in args.get("files") or []
                             if isinstance(f, dict)})
    elif isinstance(data.get("paths"), list):
        # A tool whose files are its own, not an argument (glidepath.cicd.propose writes cicd.yaml): its result says
        # which repo and paths its commit carried, and the receipt records them the same way.
        e["repo"] = str(data.get("repo") or args.get("repo", "")).lower()
        e["paths"] = sorted({_normal(p) for p in data["paths"] if isinstance(p, str)})
    return e


def _normal(path: str) -> str:
    try:
        return normalize(path)
    except BadPath:
        return path
