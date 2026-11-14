"""Writes what the Go gateway (A-9) takes from the Python one, so the two cannot drift while both exist:
gateway/internal/tools/registry.json (the tool registry: core tools, then the Hangar pack, its T3 tripwires and the
baseline deny lists), gateway/internal/pgstate/schema.sql (the gateway database's schema, engine/pg.py SCHEMA) and
gateway/internal/definitions/testdata/agents.json (each bundled agent definition with the scope and agent the Python
gateway sends the core for it, which the Go parser must reproduce) and gateway/internal/session/testdata/rows.json
(session rows as the Python gateway writes them, Session.to_record, which the Go gateway must read and write back whole)
and gateway/internal/dispatch/testdata/backend_errors.json (what the Python dispatch tells the agent and writes on the
outcome for each kind of backend failure, which the Go dispatch must reproduce) and
gateway/internal/dispatch/testdata/effects.json (the effect, the receipt of a write, Python's dispatch._effect makes of
a tool's result, which the Go dispatch must send the core on the outcome) and the run slice's (A-9 S2): the limit rules
(gateway/internal/session/testdata/limits.json: engine/limits.py narrow and SessionStore.open), the AgentRun manifests and
the run identity (gateway/internal/runs/testdata/manifests.json: manifests.agentrun_manifest, AgentRunBackend.principal),
who a caller acts as (gateway/internal/dispatch/testdata/acting.json: Gateway._acting), the run results helpers and the
operations that end a run (gateway/internal/dispatch/testdata/results.json and runs.json: results.observe/warnings/clip_text, Gateway.finish_run,
close_session, expire, _spawn) and the Preflight candidate clamp (gateway/internal/dispatch/testdata/candidates.json) and the
S3 slice's: the human inbox, the person controls and the rows they write (gateway/internal/dispatch/testdata/people.json:
engine/inbox.py, control.py, events.py, Gateway.freeze_session, end_session, hold_triggers, release_triggers, human_answer) and
the triggers (gateway/internal/triggers/testdata/triggers.json: triggers.py, trigger_ledger.py, event_bridge.py, reconcile.py,
scheduler.py driven over a fake gateway and the in-memory ledger).

tests/test_gateway_go.py fails when the committed file differs from what this prints, so the two
implementations cannot drift while both exist.   python tools/gen_gateway_registry.py [--check]
"""
from __future__ import annotations

import dataclasses
import hashlib
import json
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

import yaml  # noqa: E402
from autopilot.engine import dispatch, pg, profile  # noqa: E402
from autopilot.engine.limits import Limits  # noqa: E402
from autopilot.engine.session import Session  # noqa: E402
from autopilot.engine.tiers import CORE_TOOLS, TRIPWIRES  # noqa: E402
from autopilot.hangar_tools import HANGAR as HANGAR_PACK, TOOLS as HANGAR  # noqa: E402

OUT = ROOT / "gateway" / "internal" / "tools" / "registry.json"
AGENTS_OUT = ROOT / "gateway" / "internal" / "definitions" / "testdata" / "agents.json"
ERRORS_OUT = ROOT / "gateway" / "internal" / "dispatch" / "testdata" / "backend_errors.json"
EFFECTS_OUT = ROOT / "gateway" / "internal" / "dispatch" / "testdata" / "effects.json"
ROWS_OUT = ROOT / "gateway" / "internal" / "session" / "testdata" / "rows.json"
SCHEMA_OUT = ROOT / "gateway" / "internal" / "pgstate" / "schema.sql"
LIMITS_OUT = ROOT / "gateway" / "internal" / "session" / "testdata" / "limits.json"
MANIFESTS_OUT = ROOT / "gateway" / "internal" / "runs" / "testdata" / "manifests.json"
ACTING_OUT = ROOT / "gateway" / "internal" / "dispatch" / "testdata" / "acting.json"
RUNS_OUT = ROOT / "gateway" / "internal" / "dispatch" / "testdata" / "results.json"
RUN_OPS_OUT = ROOT / "gateway" / "internal" / "dispatch" / "testdata" / "runs.json"
CANDIDATES_OUT = ROOT / "gateway" / "internal" / "dispatch" / "testdata" / "candidates.json"
PEOPLE_OUT = ROOT / "gateway" / "internal" / "dispatch" / "testdata" / "people.json"
TRIGGERS_OUT = ROOT / "gateway" / "internal" / "triggers" / "testdata" / "triggers.json"


def spec(t) -> dict:
    d = dataclasses.asdict(t)
    d.pop("extra")
    d["tier"] = int(t.tier)
    d["required_args"], d["receipt"] = list(t.required_args), list(t.receipt)
    return d


def build() -> str:
    doc = {"tools": [spec(t) for t in (*CORE_TOOLS, *HANGAR)], "tripwires": sorted(TRIPWIRES),
           "baseline_deny_paths": list(profile.BASELINE_DENY_PATHS),
           "baseline_deny_fields": list(profile.BASELINE_DENY_FIELDS)}
    return json.dumps(doc, indent=1, sort_keys=True) + "\n"


def agents() -> str:
    out = []
    for f in sorted((ROOT / "agents").glob("*.yaml")):
        doc = yaml.safe_load(f.read_text())
        d = profile.parse(doc)
        out.append({"name": d.name, "doc": doc, "scope": dispatch._scope(d), "agent": dispatch._agent(d),
                    "runs_in": d.runs_in})
    return json.dumps(out, indent=1, sort_keys=True, default=str) + "\n"


def backend_errors() -> str:
    """What dispatch makes of each failure a backend can raise: the agent's error text (_backend_error) and the outcome's
    result (_error_result), with the exception's evidence and agent message the Go side's error types are built from."""
    from autopilot.argo_sync import ArgoSyncError
    from autopilot.github_pr import GitHubError
    from autopilot.glidepath_api import GlidepathError
    from autopilot.reads import ReadError
    long = "LogQL backend unreachable: " + "x" * 200
    errs = [
        GitHubError.of("GitHub token broker", 502, {"error": "the agents' GitHub App is not installed on jfillman/parachute"}, key="error"),
        GlidepathError("glidepath-api unreachable: URLError(ConnectionRefusedError)"),
        ArgoSyncError("API server 409 patching parachute-lower: b'{\"token\":\"ghp_SECRET\"}'"),
        ReadError("PromQL backend unreachable: URLError"), ReadError("LogQL backend 502"), ReadError(long),
        RuntimeError("boom: internal detail"),
    ]
    out = []
    for e in errs:
        out.append({"class": type(e).__name__, "text": str(e), "evidence": getattr(e, "evidence", None),
                    "agent_message": getattr(e, "agent_message", None),
                    "agent_error": dispatch._backend_error(e), "outcome_result": dispatch._error_result(e)})
    return json.dumps(out, indent=1, sort_keys=True) + "\n"


def effects() -> str:
    from autopilot.engine.tiers import registry
    reg = registry((*CORE_TOOLS, *HANGAR))
    pr = {"pr": 7, "commit": "a" * 40, "branch": "agent/x", "ignored": "not in the receipt"}
    cases = [
        ("repo.pr.open", {"repo": "Owner/App", "files": [{"path": "./a//b.txt"}, {"path": "a/b.txt"}, {"path": "../x"}]}, pr),
        ("repo.pr.open", {"repo": "owner/app", "files": []}, {"pr": 1}),
        ("repo.pr.open", {"repo": "owner/app", "files": [{"path": "z"}]}, "not a dict"),
        ("glidepath.cicd.propose", {"app": "a", "repo": "Owner/App", "patch": {}, "title": "t"},
         {**pr, "merge": "person", "needsPerson": ["cicd.yaml"], "needsPersonFields": ["/deploy/environments/prod"],
          "paths": ["cicd.yaml"], "repo": "Owner/App"}),
        ("glidepath.cicd.propose", {"app": "a", "repo": "owner/app", "patch": {}, "title": "t"},
         {**pr, "paths": ["./cicd.yaml"]}),
        ("airframe.apply", {"spec": {}}, {"pr": 3, "merge": "person", "needsPerson": ["gitops-app/values.yaml"], "state": "awaiting-merge"}),
        ("airframe.apply", {"spec": {}}, {"state": "agent-steps-merged"}),
        ("catalog.read", {"query": "x"}, {"pr": 1}),
    ]
    out = []
    for tool, args, data in cases:
        spec = reg[tool]
        out.append({"tool": tool, "args": args, "data": data, "effect": dispatch._effect(spec, args, data)})
    return json.dumps(out, indent=1, sort_keys=True) + "\n"


def rows() -> str:
    """Session rows from the Python Session itself, so a field it adds (signals, A-11) reaches the Go round-trip test."""
    t0 = datetime(2026, 9, 26, 12, 0, tzinfo=timezone.utc)
    lim = Limits.from_dict({"tier_ceiling": "T1", "ttl_minutes": 30, "tool_calls": 200, "github_calls": 300, "open_prs": 3,
                            "children": 2, "model_tokens": 500000, "tools": ["catalog.read", "run.spawn"],
                            "models": ["claude-sonnet-5"], "network": "clearance+model", "compute": "small"})
    plain = Session("s-1a6aaf485880", "user:alice", "coding-agent", lim, t0, t0 + timedelta(minutes=30), task_id="t-1a6aaf485880")
    busy = Session("s-2b7bbf596991", "user:alice", "parachute", lim, t0 + timedelta(microseconds=123456),
                   t0 + timedelta(minutes=30, microseconds=123456), parent_id="s-1a6aaf485880", depth=1, task_id="t-1a6aaf485880",
                   used={**plain.used, "tool_calls": 3, "github_calls": 4, "open_prs": 1}, children=["s-c1"], tripped=True,
                   trip_reason="5 denied calls", denials=5, run="agent-x", input={"prompt": "hi"},
                   outcome={"status": "done", "turns": 2, "warnings": ["airframe.plan was called 4 times"]},
                   signals={"plan_calls": 4, "apply_ok": 1, "apply_pr": 1, "apply_state": "agent-steps-merged"})
    busy.add_usage({"input_tokens": 1, "output_tokens": 2, "charged_tokens": 3})
    return json.dumps([plain.to_record(), busy.to_record()], indent=1, sort_keys=True) + "\n"



# -- A-9 S2: runs and the run tree ------------------------------------------------------------------------------------
T0 = datetime(2026, 9, 26, 12, 0, tzinfo=timezone.utc)
LIM = {"tier_ceiling": "T2", "ttl_minutes": 60, "tool_calls": 100, "github_calls": 50, "open_prs": 3, "model_tokens": 400000,
       "children": 4, "tools": ["argo.app.get", "catalog.read", "metrics.query", "run.close", "run.spawn"],
       "models": ["claude-sonnet-5", "claude-haiku-5"], "network": "clearance+model", "compute": "medium"}


def _session(sid="s-0000a1", *, limits=None, parent=None, depth=0, minutes=60, used=None, reserved=None, closed=False,
             tripped=False, run=None, principal="user:alice", agent="orchestrator", task="t-0000a1", **kw):
    from autopilot.engine.session import Session
    lim = Limits.from_dict({**LIM, **(limits or {})})
    s = Session(sid, principal, agent, lim, T0, T0 + timedelta(minutes=minutes), parent_id=parent, depth=depth, task_id=task,
                closed=closed, tripped=tripped, run=run, **kw)
    s.used.update(used or {})
    s.reserved.update(reserved or {})
    return s


def limits_cases() -> str:
    """The limit rules: LimitRequest + narrow (a request can only narrow) and SessionStore.open's cut of a child (never wider than
    the parent has left, never deeper than MAX_DEPTH, never past its deadline), for the Go gateway's session.CutChild."""
    from autopilot.engine.limits import LimitRequest, WideningError
    from autopilot.engine.session import SessionError, SessionStore
    base = dict(LIM, tier_ceiling="T1", ttl_minutes=30, tool_calls=40, github_calls=0, open_prs=0, model_tokens=100000, children=1,
                tools=["argo.app.get", "catalog.read", "logs.query"], models=["claude-sonnet-5"], network="clearance+model",
                compute="small")
    parents = {
        "fresh": _session(),
        "spent": _session(used={"tool_calls": 70, "model_tokens": 350000, "children": 1}, reserved={"tool_calls": 20, "model_tokens": 40000}),
        "low-tier": _session(limits={"tier_ceiling": "T0", "network": "clearance", "compute": "small"}),
        "ending": _session(minutes=12),
        "seconds-left": _session(minutes=0),
        "deep": _session(depth=3), "one-from-deep": _session(depth=2),
        "closed": _session(closed=True), "tripped": _session(tripped=True),
        "no-children": _session(used={"children": 4}),
        "narrow-tools": _session(limits={"tools": ["catalog.read"], "models": []}),
    }
    requests = [None, {}, {"tool_calls": 5}, {"tool_calls": 40}, {"tool_calls": 41}, {"ttl_minutes": 10}, {"ttl_minutes": 31},
                {"tier_ceiling": "T0"}, {"tier_ceiling": "T2"}, {"tools": ["catalog.read"]}, {"tools": ["catalog.read", "repo.pr.open"]},
                {"models": []}, {"models": ["claude-opus-9"]}, {"network": "clearance"}, {"network": "allowlist"},
                {"network": "CLEARANCE-MODEL"}, {"compute": "small"}, {"compute": "gpu"},
                {"children": 0}, {"children": 9}, {"github_calls": 1, "open_prs": 1, "model_tokens": 10},
                {"tool_calls": 99, "tier_ceiling": "T3", "tools": ["x.y"], "network": "allowlist"},
                {"bogus": 1}, {"tier_ceiling": "T9"}, {"network": "wifi"}, {"compute": "tpu"}, {"tools": None, "ttl_minutes": None}]
    out = []
    for pname, parent in parents.items():
        for req in requests:
            from autopilot.engine.session import Session
            store = SessionStore(id_gen=lambda: "s-0000c1")
            live = Session.from_record(parent.to_record())     # a fresh copy per case: open mutates the parent
            store._s[live.id] = live
            entry = {"parent": pname, "request": req}
            try:
                lr = LimitRequest.from_dict(req)
                child = store.open("user:alice", "researcher", dataclasses.replace(Limits.from_dict(base)), lr, T0, parent_id=live.id)
                entry["result"] = {"limits": child.limits.as_dict(), "expires_at": child.expires_at.isoformat(), "depth": child.depth}
            except WideningError as e:
                entry["result"] = {"widening": e.fields}
            except SessionError as e:
                entry["result"] = {"error": e.code}
            except (ValueError, KeyError):
                entry["result"] = {"bad_request": True}
            out.append(entry)
    doc = {"parents": {n: p.to_record() for n, p in parents.items()}, "base": base, "now": T0.isoformat(), "cases": out}
    return json.dumps(doc, indent=1, sort_keys=True) + "\n"


def _agents_docs() -> dict:
    return {f.stem: yaml.safe_load(f.read_text()) for f in sorted((ROOT / "agents").glob("*.yaml"))}


def manifests_cases() -> str:
    from autopilot import manifests
    from autopilot.backends import FakeRuns  # noqa: F401  (the Python fake the gateway's tests use)
    docs = _agents_docs()
    hard = json.loads(json.dumps(docs["researcher"]))
    hard["runtime"].update({"sandbox": "hardened", "framework": "langgraph",
                            "sidecars": [{"name": "proxy", "image": "ghcr.io/x/y@sha256:" + "a" * 64}]})
    docs["hardened-sidecars"] = hard
    cases = []
    for name, doc in docs.items():
        d = profile.parse(doc)
        if not d.image:
            continue
        for label, sess, inp in (
                ("top-level", _session(agent=name, limits=d.limits.as_dict() | {"tier_ceiling": d.limits.tier_ceiling.name}), None),
                ("child", _session("s-00beef", parent="s-0000a1", depth=1, agent=name, minutes=20, task="t-0000a1"),
                 {"prompt": "go", "started_by": "user:root", "instructions": "do the thing é"}),
                ("empty-input", _session("s-00cafe", agent=name), {})):
            m = manifests.agentrun_manifest(sess, d, run_input=inp)
            cases.append({"label": f"{name}/{label}", "doc": doc, "session": sess.to_record(), "input": inp, "manifest": m,
                          "run_name": manifests.run_name(sess)})
    noimg = json.loads(json.dumps(docs["researcher"]))
    noimg["runtime"].pop("image")
    d = profile.parse(noimg)
    sess = _session(agent="researcher")
    try:
        manifests.agentrun_manifest(sess, d)
        err = None
    except ValueError as e:
        err = str(e)
    cases.append({"label": "no-image", "doc": noimg, "session": sess.to_record(), "input": None, "manifest": None, "error": err})
    return json.dumps(cases, indent=1, sort_keys=True) + "\n"


def _python_gateway(defs=None, runs=None, results=None, core=None, clock=None, auth=None):
    from autopilot.engine.dispatch import Gateway
    from autopilot.engine.session import SessionStore
    defs = defs if defs is not None else profile.load_dir(ROOT / "agents")
    return Gateway(defs, SessionStore(), core, auth, clock or (lambda: T0), packs=(HANGAR_PACK,), runs=runs, results=results)


class _Store:
    """A Python AgentRunStore that records, and fails or ends runs when told."""
    def __init__(self, fail=(), phases=None):
        self.created, self.deleted, self.fail, self.phases = [], [], set(fail), dict(phases or {})

    def create(self, manifest):
        self.created.append(manifest["metadata"]["name"])
        return manifest

    def delete(self, name):
        from autopilot.kube_runs import KubeRunError
        if name in self.fail:
            raise KubeRunError(f"API server 500 deleting AgentRun {name}")
        self.deleted.append(name)

    def outcome(self, name):
        return self.phases.get(name)


class _Core:
    """What the run operations ask the core: records (kept), the outbox (nothing)."""
    def __init__(self):
        self.records, self.n = [], 0

    def record(self, kind, fields, required=False):
        self.n += 1
        self.records.append({"kind": kind, **fields})
        return self.n

    def drain(self):
        return 0


class _Auth:
    def authenticate(self, token):
        from autopilot.engine.dispatch import Principal
        return Principal(token, "workload" if token.startswith("agent-") else "user", Tier.T1)


from autopilot.engine.tiers import Tier  # noqa: E402


def acting_cases() -> str:
    from autopilot.engine.dispatch import Principal
    from autopilot.gateway import AgentRunBackend
    gw = _python_gateway(runs=AgentRunBackend(_Store()))
    out = []
    for run, runs_label in (("r-0000a1", "r-0000a1"), (None, "no run"), ("r-" + "ab" * 16, "long"), ("r-AB", "bad hex"),
                            ("r-12", "short"), ("x", "not a run name"), ("", "empty run")):
        s = _session(run=run or None)
        for caller, kind in ((s.principal, "user"), ("agent-r-0000a1/run", "workload"), ("agent-r-0000a1/run", "user"),
                             ("agent-r-0000b2/run", "workload"), ("agent-r-0000a1/default", "workload"),
                             ("agent-r-0000a1/run/x", "workload"), ("other/run", "workload"), ("agent-r-0000a1", "workload"),
                             ("agent-" + (run or "") + "/run", "workload"), ("user:bob", "user"), ("", "workload")):
            got = gw._acting(s, caller)
            ok_name = bool(run) and bool(__import__("re").fullmatch(r"r-[a-f0-9]{6,32}", run))
            out.append({"session_principal": s.principal, "run": run or None, "caller": caller, "kind": kind, "python": got,
                        "go": got if (kind == "workload" and (ok_name or got == caller)) else caller})
    return json.dumps(out, indent=1, sort_keys=True) + "\n"


def results_cases() -> str:
    from autopilot.engine import results as res
    obs = []
    steps = [("airframe.plan", None), ("airframe.plan", None), ("airframe.apply", {"pr": 7, "state": "awaiting-merge"}),
             ("airframe.apply", {"state": "x" * 60}), ("airframe.apply", {"pr": 0}), ("airframe.apply", "not a dict"),
             ("catalog.read", {"pr": 1}), ("airframe.plan", None), ("airframe.plan", None)]
    sig: dict = {}
    for tool, data in steps:
        res.observe(sig, tool, data)
        obs.append({"tool": tool, "data": data, "signals": json.loads(json.dumps(sig)), "warnings": res.warnings(sig)})
    warn = []
    for sig in ({}, {"plan_calls": 3}, {"plan_calls": 4}, {"apply_ok": 1}, {"apply_ok": 2, "apply_state": "agent-steps-merged"},
                {"apply_ok": 1, "apply_pr": 1}, {"plan_calls": 9, "apply_ok": 1, "apply_state": "failed"}):
        warn.append({"signals": sig, "warnings": res.warnings(sig)})
    clips = []
    for text in ("", "short", "é" * 20000, "a" * 16384, "a" * 16385, "é" * 8192, "é" * 8193, "😀" * 5000):
        t, cut = res.clip_text(text)
        clips.append({"len": len(text), "text_sha": hashlib.sha256(text.encode()).hexdigest(), "clipped_len_bytes": len(t.encode()),
                      "clipped_sha": hashlib.sha256(t.encode()).hexdigest(), "truncated": cut})
    calls_in = [{"tool": "catalog.read", "ok": True, "rule": None}, {"tool": "x" * 150, "ok": 0, "rule": "R" * 60},
                {"tool": 5, "ok": True}, "junk", {"tool": "t", "rule": 7}, {"tool": "u", "ok": "yes", "rule": "R001"}]
    summary = {"in": calls_in, "out": res.tool_call_summary(calls_in),
               "many": len(res.tool_call_summary([{"tool": "t"}] * 300))}
    return json.dumps({"observe": obs, "warnings": warn, "clips": clips, "summary": summary}, indent=1, sort_keys=True) + "\n"


def _final(gw, sessions):
    return {sid: gw.store.get(sid).to_record() for sid in sessions}


def run_ops_cases() -> str:
    """finish_run, close_session, expire and _spawn driven on the Python gateway with a recording core and an AgentRun store,
    so the Go gateway's answers, sessions and records can be held equal."""
    from autopilot.engine import control as ctl
    from autopilot.engine import results as res
    from autopilot.engine.dispatch import Verdict
    from autopilot.engine.session import Session
    from autopilot.gateway import AgentRunBackend
    out: dict = {}

    def fresh(*sessions, store=None, results=True):
        store = store or _Store()
        core = _Core()
        gw = _python_gateway(runs=AgentRunBackend(store), results=res.MemoryResults() if results else None, core=core, auth=_Auth())
        for s in sessions:
            gw.store._s[s.id] = Session.from_record(s.to_record())
        return gw, core, store

    def ctl_error(e):
        return {"rule": e.rule, "message": e.message, "status": e.status}

    # -- finish_run
    for label, body_in in (("done", {"status": "done", "final_text": "Python and Go.", "turns": 2,
                                     "tool_calls": [{"tool": "catalog.read", "ok": True, "rule": None}, {"tool": "x", "ok": False, "rule": "R001"}]}),
                           ("long", {"status": "budget", "final_text": "é" * 20000, "turns": 9, "reason": "r" * 700}),
                           ("empty", {"status": "error"}),
                           ("bad-status", {"status": "great"}), ("bad-turns", {"status": "done", "turns": "2"}),
                           ("bool-turns", {"status": "done", "turns": True}), ("bad-text", {"status": "done", "final_text": 5})):
        s = _session(run="r-0000a1", signals={"plan_calls": 5, "apply_ok": 1, "apply_state": "x"}, agent="researcher")
        gw, core, _ = fresh(s)
        steps = []
        for caller in ("user:bob", "agent-r-0000b2/run", "agent-r-0000a1/run", "agent-r-0000a1/run"):
            try:
                steps.append({"caller": caller, "result": gw.finish_run(caller, s.id, body_in)})
            except ctl.ControlError as e:
                steps.append({"caller": caller, "error": ctl_error(e)})
            if "error" in steps[-1] and steps[-1]["error"]["rule"] == "R008":
                break
        out[f"finish/{label}"] = {"session": s.to_record(), "steps": steps, "final": _final(gw, [s.id]), "results": gw.results.rows,
                                  "records": core.records}
    gw, core, _ = fresh(_session(), results=False)
    s2 = _session("s-0000a2", run="r-0000a2")
    gw, core, _ = fresh(s2, results=False)
    out["finish/no-result-store"] = {"session": s2.to_record(), "steps": [{"caller": "agent-r-0000a2/run",
        "result": gw.finish_run("agent-r-0000a2/run", s2.id, {"status": "done", "final_text": "ok"})}], "final": _final(gw, [s2.id]),
        "results": None, "records": core.records}
    s3 = _session("s-0000a3")      # no run: nobody may finish it
    gw, core, _ = fresh(s3)
    try:
        gw.finish_run("user:alice", s3.id, {"status": "done"})
        err = None
    except ctl.ControlError as e:
        err = ctl_error(e)
    out["finish/no-run"] = {"session": s3.to_record(), "steps": [{"caller": "user:alice", "error": err}], "final": _final(gw, [s3.id]),
                            "results": {}, "records": core.records}

    # -- close_session
    parent = _session("s-0000a1", run="r-0000a1", children=["s-0000c1"], reserved={"tool_calls": 10, "model_tokens": 1000}, agent="orchestrator")
    child = _session("s-0000c1", parent="s-0000a1", depth=1, run="r-0000c1", children=["s-0000d1"], agent="researcher",
                     limits={"tool_calls": 10, "model_tokens": 1000}, used={"tool_calls": 3})
    grand = _session("s-0000d1", parent="s-0000c1", depth=2, run="r-0000d1", agent="researcher")
    for label, caller, reason, fail in (("by its run", "agent-r-0000a1/run", None, ()), ("by its person", "user:alice", "done here", ()),
                                         ("another", "user:bob", None, ()), ("a child's run", "agent-r-0000c1/run", None, ()),
                                         ("bad reason", "user:alice", "x" * 600, ()), ("failed delete", "user:alice", None, ("r-0000c1",))):
        gw, core, store = fresh(parent, child, grand, store=_Store(fail=fail))
        try:
            r, err = gw.close_session(caller, "s-0000a1", reason), None
        except ctl.ControlError as e:
            r, err = None, ctl_error(e)
        out[f"close_session/{label}"] = {"sessions": [x.to_record() for x in (parent, child, grand)], "caller": caller,
                                         "reason": reason, "fail": list(fail), "result": r, "error": err,
                                         "final": _final(gw, ["s-0000a1", "s-0000c1", "s-0000d1"]), "records": core.records,
                                         "deleted": store.deleted}
    gw, core, store = fresh(parent, child, grand)
    gw.close_session("agent-r-0000a1/run", "s-0000a1", None)
    try:
        gw.close_session("agent-r-0000a1/run", "s-0000a1", None)
        again = None
    except ctl.ControlError as e:
        again = ctl_error(e)
    out["close_session/twice"] = {"again": again}

    # -- expire (backstop outcomes, the deletion of finished runs, a failed delete retried)
    now = T0 + timedelta(minutes=10)
    live = _session("s-000e01", run="r-000e01", minutes=60, agent="researcher")
    crashed = _session("s-000e02", run="r-000e02", minutes=60, agent="researcher")
    gone = _session("s-000e03", run="r-000e03", minutes=60, agent="researcher")
    late = _session("s-000e04", run="r-000e04", minutes=5, agent="researcher")
    finished = _session("s-000e05", run="r-000e05", minutes=60, closed=True, agent="researcher",
                        outcome={"status": "done", "via": "runtime"})
    stuck = _session("s-000e06", run="r-000e06", minutes=60, closed=True, agent="researcher", outcome={"status": "done", "via": "runtime"})
    norun = _session("s-000e07", minutes=5, agent="researcher")
    store = _Store(fail=("r-000e06",), phases={"r-000e02": "Failed", "r-000e03": "Gone", "r-000e04": "Running"})
    sess = [live, crashed, gone, late, finished, stuck, norun]
    gw, core, store = fresh(*sess, store=store)
    ids = [x.id for x in sess]
    first = gw.expire(now)
    store.fail.clear()
    second = gw.expire(now)
    out["expire"] = {"sessions": [x.to_record() for x in sess], "now": now.isoformat(), "phases": store.phases,
                     "fail": ["r-000e06"], "first": sorted(first), "second": sorted(second), "final": _final(gw, ids),
                     "records": core.records, "deleted": store.deleted}

    # -- _spawn: the child is the grant, the parent takes the spend, the run is created
    d = profile.load_dir(ROOT / "agents")["researcher"]
    parent = _session("s-0000a1", agent="orchestrator", used={"tool_calls": 5, "children": 1}, reserved={"tool_calls": 3}, children=["s-0000b9"],
                      limits={"tools": sorted(d.limits.tools | {"run.spawn", "run.close"}), "models": sorted(d.limits.models)})
    gw, core, store = fresh(parent)
    d = gw.defs["researcher"]
    grant = {"session": "s-0000b1", "principal": "user:alice", "agent": "researcher", "task_id": "t-0000a1", "parent": "s-0000a1",
             "depth": 1, "expires_at": (T0 + timedelta(minutes=20)).isoformat(),
             "limits": d.limits.as_dict() | {"tool_calls": 10, "model_tokens": 20000, "github_calls": 0, "open_prs": 0, "children": 0,
                                              "tier_ceiling": "T0"}}
    v = Verdict(True, None, None, "allowed", "", 1, spend={"tool_calls": 1, "children": 1, "reserve": {
        "tool_calls": 10, "model_tokens": 20000, "github_calls": 0, "open_prs": 0}}, grant=grant)
    data = gw._spawn(gw.store.get("s-0000a1"), d, v, {"agent": "researcher", "limits": {}, "input": {"prompt": "hi"}})
    out["spawn"] = {"parent": parent.to_record(), "grant": grant, "spend": v.spend, "result": data,
                    "final": _final(gw, ["s-0000a1", "s-0000b1"]), "created": store.created}
    return json.dumps(out, indent=1, sort_keys=True, default=str) + "\n"


def candidates_cases() -> str:
    """Gateway._candidate's clamp: the sandboxed definition a Preflight candidate runs as."""
    import copy
    gw = _python_gateway()
    docs = _agents_docs()
    out = []
    for name in ("researcher", "orchestrator", "coding-agent", "parachute"):
        for budget in (None, {}, {"tool_calls": 5}, {"model_tokens": 1000, "ttl_minutes": 3, "children": 1}, {"tool_calls": 10**6},
                       {"tool_calls": 0}, {"tool_calls": -3}, {"tool_calls": True}, {"tool_calls": "9"}, {"children": 99}):
            doc = copy.deepcopy(docs[name])
            try:
                d, fp = gw._candidate(name, doc, budget)
            except Exception as e:  # noqa: BLE001
                out.append({"agent": name, "doc": doc, "budget": budget, "error": str(e)})
                continue
            out.append({"agent": name, "doc": doc, "budget": budget, "name": d.name, "fingerprint": fp, "kind": d.kind,
                        "identity_type": d.identity_type, "limits": d.limits.as_dict(), "triggers": [dict(t) for t in d.triggers]})
    return json.dumps(out, indent=1, sort_keys=True, default=str) + "\n"


# -- A-9 S3: the people's side and the triggers ---------------------------------------------------------------------------------


class _PeopleAuth:
    """Tokens are principal ids; kind by id: user:... is a person, the rest a workload (a run, a delegate)."""
    def authenticate(self, token):
        from autopilot.engine.dispatch import AuthError, Principal
        if token == "nobody":
            raise AuthError("token not accepted")
        return Principal(token, "user" if token.startswith("user:") else "workload", Tier.T1)


class _Dir:
    def __init__(self, groups):
        self.g = groups

    def groups(self, principal):
        return set(self.g.get(principal, ()))


def _err(e):
    return {"rule": e.rule, "message": e.message, "status": e.status}


def people_cases() -> str:
    from autopilot.engine import control as ctl
    from autopilot.engine import events as evh
    from autopilot.engine import inbox as hr
    from autopilot.engine.session import Session
    out: dict = {}
    now = T0

    # -- the pure checks
    def attempt(fn, *a):
        try:
            return {"ok": fn(*a)}
        except (hr.InboxError, ctl.ControlError) as e:
            return {"error": _err(e)}

    qs = [("Which?", None), ("  Which?  ", ["a", " b "]), ("", None), ("   ", None), (5, None), ("x" * 2001, None), ("x" * 2000, None),
          ("q", "a"), ("q", ["a", "a"]), ("q", ["a", " "]), ("q", ["a" * 201]), ("q", [str(i) for i in range(11)]), ("q", [str(i) for i in range(10)]),
          ("q", [1]), ("q", []), ("q", {"a": 1})]
    out["check_question"] = [{"question": q, "options": o, **attempt(hr.check_question, q, o)} for q, o in qs]
    req = hr.HumanRequest("h-000000000001", "t-1", "s-1", "a", "user:alice", "group:default/team", "q?", ["a", "b"], now, now + timedelta(hours=1))
    free = hr.HumanRequest("h-000000000002", "t-1", "s-1", "a", "user:alice", None, "q?", [], now, now + timedelta(hours=1))
    answered = hr.HumanRequest("h-000000000003", "t-1", "s-1", "a", "user:alice", None, "q?", [], now, now + timedelta(hours=1), status="answered",
                               answered_by="user:bob")
    cases = []
    for r, label in ((req, "options"), (free, "free"), (answered, "answered")):
        for at in (now, now + timedelta(hours=1), now + timedelta(hours=2)):
            for ans in ("a", " b ", "c", "", "  ", None, 5, "x" * 4001, "x" * 4000):
                cases.append({"request": label, "at": at.isoformat(), "answer": ans, **attempt(hr.check_answer, r, ans, at)})
    out["check_answer"] = cases
    out["may_answer"] = [{"person": p, "groups": g, "owner": hr.may_answer(req, p, g), "free": hr.may_answer(free, p, g)}
                         for p in ("user:alice", "user:bob") for g in ([], ["group:default/team"], ["group:default/other"])]
    out["check_hours"] = [{"hours": h, **attempt(ctl.check_hours, h)} for h in (None, "", 1, 336, 337, 0, -1, "7", " 7 ", "x", 2.5, 2.9, True, False, [], {}, "1e2", "٣")]
    out["check_reason"] = [{"reason": r, **attempt(ctl.check_reason, r)} for r in ("x", "  x  ", "", "  ", None, 5, "x" * 500, "x" * 501)]
    out["may_manage"] = [{"person": p, "owner": o, "groups": g, "admins": a, "ok": ctl.may_manage(p, o, g, a)}
                         for p in ("user:alice", "user:root") for o in (None, "group:default/team") for g in ([], ["group:default/team"])
                         for a in ([], ["user:root"], ["user:*"], ["user:[a-b]*"])]
    # -- the rows the Python gateway writes
    from dataclasses import asdict
    rec = asdict(answered)
    for k in ("created_at", "expires_at", "answered_at"):
        rec[k] = rec[k].isoformat() if rec[k] else None
    full = hr.HumanRequest("h-000000000004", "t-1", "s-1", "a", "user:alice", "group:default/team", "q?", ["a"], now + timedelta(microseconds=5),
                           now + timedelta(hours=1), status="answered", answer="a", answered_by="user:alice", answered_via="hangar/tower/backend",
                           answered_at=now + timedelta(minutes=1, microseconds=120))
    rec2 = asdict(full)
    for k in ("created_at", "expires_at", "answered_at"):
        rec2[k] = rec2[k].isoformat() if rec2[k] else None
    out["inbox_rows"] = [rec, rec2, asdict(req) | {"created_at": now.isoformat(), "expires_at": (now + timedelta(hours=1)).isoformat(), "answered_at": None}]
    out["inbox_views"] = [{"view": r.view(now + timedelta(hours=1)), "at": (now + timedelta(hours=1)).isoformat()} for r in (req, answered, full)]
    out["holds"] = [ctl.Hold("a", "user:root", "tower", "storm", now, now + timedelta(hours=24)).view(),
                    ctl.Hold("a", "user:root", None, "storm", now + timedelta(microseconds=7), now + timedelta(hours=1)).view()]
    ev = evh.from_summary({"id": "e1", "app": "Gate-API", "routingKey": "gate-api.pipelinerun.finished", "receivedAt": "2026-09-26T11:59:00Z", "type": "t",
                           "subject": {"id": "pr-1", "content": {"outcome": "failure", "n": 3}}},
                          [{"agent": "triage-agent", "result": "launched", "session": "s-1", "run": "r-1", "via": "stream", "extra": 1},
                           {"agent": "", "result": "x"}, {"result": "no agent"}], now)
    out["events"] = [ev.view()]
    # -- the operations, on a Python gateway
    from autopilot.engine.dispatch import Gateway
    from autopilot.engine.session import SessionStore
    from autopilot.engine.inbox import MemoryInbox
    from autopilot.engine.control import MemoryHolds

    def gw(*sessions):
        core = _Core()
        defs = profile.load_dir(ROOT / "agents")
        g = Gateway(defs, SessionStore(), core, _PeopleAuth(), lambda: T0, packs=(HANGAR_PACK,), inbox=MemoryInbox(),
                    directory=_Dir({"user:grace": ["group:default/team-platform"]}), delegates=["hangar-prod/tower/*"], holds=MemoryHolds(),
                    admins=["user:root"])
        for x in sessions:
            g.store._s[x.id] = Session.from_record(x.to_record())
        return g, core

    def step(g, fn, *a, **k):
        try:
            return {"result": fn(*a, **k)}
        except (hr.InboxError, ctl.ControlError) as e:
            return {"error": _err(e)}

    parent = _session("s-0000a1", agent="orchestrator", children=["s-0000c1"], reserved={"tool_calls": 10},
                      limits={"tools": ["catalog.read", "human.request", "human.request.get"]})
    child = _session("s-0000c1", parent="s-0000a1", depth=1, agent="researcher", limits={"tool_calls": 10}, used={"tool_calls": 4})
    callers = [("user:alice", None), ("user:bob", None), ("user:root", None), ("user:grace", None), ("agent-r-0000a1/run", None),
               ("agent-r-0000a1/run", "user:alice"), ("hangar-prod/tower/backend", None), ("hangar-prod/tower/backend", "user:alice"),
               ("hangar-prod/tower/backend", "user:bob"), ("hangar-prod/tower/backend", "user:grace"), ("hangar-prod/tower/backend", "alice"),
               ("hangar-prod/tower/backend", "user:"), ("hangar-prod/other", "user:root"), ("user:bob", "user:root")]
    out["initial_sessions"] = [parent.to_record(), child.to_record()]
    ops = []
    for who, on in callers:
        for name, fn in (("freeze", lambda g, w, o: g.freeze_session(w, "s-0000a1", "it opens bad PRs", on_behalf_of=o)),
                         ("end", lambda g, w, o: g.end_session(w, "s-0000a1", "enough", on_behalf_of=o)),
                         ("hold", lambda g, w, o: g.hold_triggers(w, "triage-agent", "storm", 2, on_behalf_of=o)),
                         ("release", lambda g, w, o: (g.holds.put(ctl.Hold("triage-agent", "user:x", None, "r", T0, T0 + timedelta(hours=1))),
                                                       g.release_triggers(w, "triage-agent", on_behalf_of=o))[1]),
                         ("answer", lambda g, w, o: (g.inbox.add(hr.HumanRequest("h-000000000001", "t-0000a1", "s-0000a1", "orchestrator", "user:alice",
                                                                                 "group:default/team-platform", "q?", [], T0, T0 + timedelta(hours=1))),
                                                      g.human_answer(w, "h-000000000001", "yes", on_behalf_of=o))[1])):
            g, core = gw(parent, child)
            r = step(g, fn, g, who, on)
            ops.append({"op": name, "caller": who, "on_behalf_of": on, **r,
                        "sessions": {x: g.store.get(x).to_record() for x in ("s-0000a1", "s-0000c1")}, "records": core.records,
                        "holds": [h.view() for h in g.holds.all()], "inbox": [x.view(T0) for x in g.inbox._by_id.values()]})
    out["operations"] = ops
    # the control checks that come after who: reasons, hours, unknown things, state
    g, core = gw(parent, child)
    seq = [("freeze", "user:alice", "s-0000a1", "  "), ("freeze", "user:alice", "s-nope", "x"), ("freeze", "user:alice", "s-0000a1", "go"),
           ("freeze", "user:alice", "s-0000a1", "again"), ("end", "user:alice", "s-0000c1", "bye"), ("end", "user:alice", "s-0000c1", "again"),
           ("freeze", "user:alice", "s-0000c1", "closed"), ("end", "user:alice", "s-0000a1", "x" * 501)]
    steps = []
    for op, who, sid, reason in seq:
        fn = g.freeze_session if op == "freeze" else g.end_session
        steps.append({"op": op, "caller": who, "session": sid, "reason": reason, **step(g, fn, who, sid, reason)})
    out["control_sequence"] = {"steps": steps, "sessions": {x: g.store.get(x).to_record() for x in ("s-0000a1", "s-0000c1")}, "records": core.records}
    g, core = gw(parent, child)
    hs = []
    for hours in (None, "", 0, 337, "x", 2.5, True, "7", 336):
        hs.append({"hours": hours, **step(g, g.hold_triggers, "user:root", "triage-agent", "storm", hours)})
    hs.append(step(g, g.hold_triggers, "user:root", "no-such-agent", "storm", 1))
    hs.append(step(g, g.hold_triggers, "user:root", "triage-agent", "", 1))
    hs.append(step(g, g.release_triggers, "user:root", "triage-agent"))
    hs.append(step(g, g.release_triggers, "user:root", "triage-agent"))
    out["hold_sequence"] = {"steps": hs, "records": core.records}
    # the inbox through a session's own handler, then a person's answer (the ids are a counter here: the real ones are random)
    counter = iter(range(1, 1000))
    real_new_id = hr.new_id
    hr.new_id = lambda: f"h-{next(counter):012x}"
    g, core = gw(parent, child)
    sess = g.store.get("s-0000a1")
    inb = []
    for a in ({"question": "Which?", "options": ["a", "b"]}, {"question": ""}, {"question": "q", "options": ["a", "a"]}):
        try:
            inb.append({"args": a, "result": g._human_request(sess, a)})
        except Exception as e:  # noqa: BLE001 - a Denied
            inb.append({"args": a, "denied": {"rule": e.rule, "message": e.message, "hint": e.hint}})
    ids = [r.id for r in g.inbox._by_id.values()]
    for rid in ("h-nope", ids[0], 5):
        try:
            inb.append({"get": "id" if rid == ids[0] else rid, "result": {k: v for k, v in g._human_get(sess, {"request": rid}).items() if k not in ("id",)}})
        except Exception as e:  # noqa: BLE001
            inb.append({"get": rid if rid != ids[0] else "id", "denied": {"rule": e.rule, "message": e.message, "hint": e.hint}})
    for _ in range(6):
        try:
            g._human_request(sess, {"question": "flood"})
        except Exception as e:  # noqa: BLE001
            inb.append({"flood": {"rule": e.rule, "message": e.message, "hint": e.hint}})
    out["inbox_handlers"] = inb
    hr.new_id = real_new_id
    return json.dumps(out, indent=1, sort_keys=True, default=str) + "\n"


class _TrigGW:
    """What the bridge and the scheduler need of a gateway: a fake whose session half is a map from (agent, task) to a session."""
    def __init__(self, defs, hold=None):
        from autopilot.engine.dispatch import Result
        self.Result, self.defs, self.hold, self.n, self.sessions, self.run = Result, defs, dict(hold or {}), 0, {}, {}
        self.opens, self.launches, self.events, self.fail = [], [], [], set()

    def open_triggered(self, agent, trigger, task_id=None):
        self.opens.append([agent, trigger, task_id])
        if agent in self.fail:
            raise RuntimeError("boom")
        if self.hold.get(agent):
            return self.Result(False, {"allow": False, "rule": self.hold[agent]}, error="held")
        if (agent, task_id) in self.sessions:
            sid = self.sessions[(agent, task_id)]
            return self.Result(True, {"allow": True}, {"session": sid, "deduped": True}, session=sid)
        self.n += 1
        sid = f"s-{self.n:06d}"
        self.sessions[(agent, task_id)] = sid
        return self.Result(True, {"allow": True}, {"session": sid, "deduped": False}, session=sid)

    def launch(self, sid, run_input=None):
        self.launches.append(sid)
        self.run[sid] = True
        return self.Result(True, {"allow": True}, {"run": "r-" + sid[2:], "session": sid}, session=sid)

    def record_event(self, summary, outcomes):
        from autopilot.engine import events as evh
        self.events.append(evh.from_summary(summary, outcomes, T0).view())
        return True


def triggers_cases() -> str:
    import re as _re
    from autopilot import event_bridge as eb
    from autopilot import reconcile as rc
    from autopilot import scheduler as sch
    from autopilot import triggers as trg
    from autopilot.engine import trigger_ledger as tl
    from autopilot.metrics import TriggerMetrics
    out: dict = {}
    pats = ["gate-api.pipelinerun.finished", "*.pipelinerun.finished", "#.finished", "#", "a.#", "a.#.b", "a.*", "*", "", "a.b", "a.*.c", "#.#.#", "#.a.#", "a.#.#.b", "*.*", "a.b.#"]
    keys = ["gate-api.pipelinerun.finished", "", "a", "a.b", "a.x.y.b", "a.b.c", "x.finished", "a.c.c", "a.b.b.b", "a..b"]
    out["amqp"] = [{"pattern": p, "key": k, "match": trg.amqp_topic_match(p, k)} for p in pats for k in keys]
    envs = [{"id": "e1", "app": "gate-api", "routingKey": "gate-api.pipelinerun.finished", "event": {"subject": {"id": "pr-1"}}},
            {"id": "e2", "app": "other", "routingKey": "gate-api.pipelinerun.finished", "event": {"subject": {"id": "pr-1"}}},
            {"id": "e3", "app": "gate-api", "routingKey": "gate-api.pipelinerun.started", "event": {"subject": {"id": "pr-1"}}},
            {"id": "e4", "app": "gate-api", "routingKey": "gate-api.pipelinerun.finished", "event": {"subject": {}}},
            {"id": "e5", "app": "", "routingKey": "gate-api.pipelinerun.finished"}, {"app": "gate-api", "routingKey": "gate-api.pipelinerun.finished", "event": {"subject": {"id": "p"}}},
            {"id": "", "routingKey": "x"}, {"id": 5}, {"id": "e6", "app": "gate-api", "routingKey": "gate-api.pipelinerun.finished", "event": "x"}]
    out["keys"] = [{"envelope": e, "key": tl.trigger_key(e)} for e in envs]
    out["task_ids"] = [{"agent": a, "key": k, "id": trg.event_task_id(a, k)} for a in ("triage-agent", "x") for k in ("gate-api.pipelinerun.finished/pr-1", "event:e1")] + \
                      [{"agent": a, "slot": n, "id": sch.slot_task_id(a, n)} for a in ("nightly-reviewer", "x") for n in (0, 1, 20406, 99999)]
    out["slots"] = [{"now": t.isoformat(), "every": e, "slot": sch.slot_of(t, trg.parse_interval(e))[0], "start": sch.slot_of(t, trg.parse_interval(e))[1].isoformat()}
                    for t in (T0, T0 + timedelta(hours=5, minutes=3), datetime(2026, 11, 14, 0, 0, tzinfo=timezone.utc), datetime(2026, 11, 14, 23, 59, 59, tzinfo=timezone.utc))
                    for e in ("30m", "1h", "6h", "24h")]
    # summaries
    sums = []
    big = {f"k{i}": "v" * 290 for i in range(40)}
    for subject in ({"id": "pr-1", "content": {"outcome": "failure", "pipelineName": "build", "n": 3, "f": 1.5, "b": True, "nested": {"a": 1}, "long": "z" * 301}},
                    {"id": "pr-2", "content": {}}, {"id": "pr-3"}, {}):
        env = {"id": "e1", "app": "gate-api", "routingKey": "gate-api.pipelinerun.finished", "receivedAt": "2026-09-26T11:59:00Z",
               "event": {"context": {"type": "dev.cdevents.pipelinerun.finished.0.3.0"}, "subject": subject}}
        sums.append({"envelope": env, "summary": eb.summarize(env)})
    out["summaries"] = sums
    out["id_times"] = [{"id": i, "time": (eb.id_time(i).isoformat() if eb.id_time(i) else None)} for i in
                       ("1794000000000000-ab", "17940000000000-ab", "1794000000000000", "x-y", "", None, "179400000000000a-bb")]
    rows = [{"name": "pr-1", "app": "gate-api", "namespace": "ns", "status": "failed", "pipeline": "build", "endTime": "2026-09-26T11:50:00.123456789Z"},
            {"name": "pr-2", "app": "gate-api", "status": "succeeded"}, {"name": "pr-3", "app": "gate-api", "status": "cancelled", "pipeline": ""},
            {"name": "pr-4", "app": "gate-api", "status": "weird"}, {"app": "gate-api"}, {"name": "pr-5"}]
    out["run_envelopes"] = [{"row": r, "envelope": rc.envelope_from_run(r)} for r in rows]
    out["times"] = [{"s": s, "time": (rc._time(s).isoformat() if rc._time(s) else None)} for s in
                    ("2026-09-26T11:50:00.123456789Z", "2026-09-26T11:50:00Z", "2026-09-26T11:50:00+02:00", "x", "", None, "2026-09-26T11:50:00.5Z")]
    # the bridge, over a fake gateway and the in-memory ledger
    def tdef(name, trigs, kind="event", ident="workload", instr="do it"):
        return profile.AgentDefinition.__new__(profile.AgentDefinition) if False else None
    defs = {k: v for k, v in profile.load_dir(ROOT / "agents").items() if k in ("triage-agent", "nightly-reviewer")}
    d2 = dataclasses.replace(defs["triage-agent"], name="strict-agent", triggers=({"type": "event", "source": "glidepath", "bindingKey": "gate-api.pipelinerun.finished", "match": {"subject.content.outcome": "failure"}, "maxPerHour": 2},))
    d3 = dataclasses.replace(defs["triage-agent"], name="any-agent", triggers=({"type": "event", "source": "glidepath", "bindingKey": "#.finished"},))
    defs = {"triage-agent": defs["triage-agent"], "strict-agent": d2, "any-agent": d3, "nightly-reviewer": defs["nightly-reviewer"]}

    def fin(eid, pr, outcome, app="gate-api"):
        return {"id": eid, "app": app, "routingKey": f"{app}.pipelinerun.finished", "receivedAt": "2026-09-26T11:59:00Z",
                "event": {"context": {"type": "t"}, "subject": {"id": pr, "content": {"outcome": outcome}}}}

    clock = [T0]
    gwb = _TrigGW(defs, hold={"any-agent": ""})
    ledger = tl.MemoryTriggers()
    m = TriggerMetrics()
    bridge = eb.EventBridge(gwb, "http://glidepath.invalid", "/dev/null", clock=lambda: clock[0], ledger=ledger, metrics=m)
    steps = []

    def handle(env, via="stream", at=0):
        clock[0] = T0 + timedelta(seconds=at)
        done = bridge.handle(env, via)
        steps.append({"envelope": env, "via": via, "at": at, "done": done})

    handle(fin("e1", "pr-1", "failure"))
    handle(fin("e1", "pr-1", "failure"))
    handle(fin("e2", "pr-2", "success"), at=1)
    handle(fin("e3", "pr-3", "failure"), at=2)
    handle(fin("e4", "pr-4", "failure"), at=3)
    handle(fin("reconcile:gate-api/pr-1", "pr-1", "failure"), "reconcile", at=4)
    handle(fin("e5", "pr-5", "failure", app="other"), at=5)
    gwb.hold["any-agent"] = "R024"
    handle(fin("e6", "pr-6", "failure"), at=6)
    gwb.hold["any-agent"] = "R001"
    handle(fin("e7", "pr-7", "failure"), at=7)
    gwb.hold["any-agent"] = ""
    handle(fin("e8", "pr-8", "failure"), "recent", at=3700)
    handle({"id": "e9", "routingKey": "x.y.z"}, at=3701)
    out["bridge"] = {"steps": steps, "rows": sorted(({k: (v.isoformat() if hasattr(v, "isoformat") else v) for k, v in r.items()} for r in ledger.rows.values()),
                                                    key=lambda r: (r["agent"], r["key"])),
                     "opens": gwb.opens, "launches": gwb.launches, "events": gwb.events,
                     "counters": {"outcomes": {f"{k[0]}/{k[1]}/{k[2]}": v for k, v in m.outcomes._values.items()},
                                  "missed": {f"{k[0]}/{k[1]}": v for k, v in m.missed._values.items()}}}
    # the sweep's pure parts
    out["watched"] = []
    for label, ds in (("triage", {"t": defs["triage-agent"]}), ("strict", {"s": d2}), ("wild", {"w": d3}), ("none", {"n": defs["nightly-reviewer"]})):
        found, apps = rc.watched_apps(ds)
        out["watched"].append({"set": label, "found": found, "apps": sorted(apps)})
    # the scheduler: a clock sequence over the in-memory ledger
    clock = [datetime(2026, 11, 14, 0, 2, tzinfo=timezone.utc)]
    gws = _TrigGW({"nightly-reviewer": defs["nightly-reviewer"]})
    ledger = tl.MemoryTriggers()
    ledger.cursor("schedule:nightly-reviewer", datetime(2026, 11, 10, 6, 0, tzinfo=timezone.utc))
    ms = TriggerMetrics()
    s = sch.Scheduler(gws, clock=lambda: clock[0], ledger=ledger, metrics=ms)
    ticks = []
    for label, t in (("first", datetime(2026, 11, 14, 0, 2, tzinfo=timezone.utc)), ("again", datetime(2026, 11, 14, 0, 2, 30, tzinfo=timezone.utc)),
                     ("after grace", datetime(2026, 11, 14, 0, 11, tzinfo=timezone.utc)), ("next day", datetime(2026, 11, 15, 0, 1, tzinfo=timezone.utc)),
                     ("two days later", datetime(2026, 11, 17, 3, 0, tzinfo=timezone.utc)), ("the day after", datetime(2026, 11, 18, 0, 5, tzinfo=timezone.utc))):
        clock[0] = t
        ticks.append({"label": label, "at": t.isoformat(), "done": s.tick()})
    out["scheduler"] = {"ticks": ticks, "rows": sorted(({k: (v.isoformat() if hasattr(v, "isoformat") else v) for k, v in r.items()} for r in ledger.rows.values()),
                                                        key=lambda r: (r["agent"], r["key"])), "opens": gws.opens,
                        "counters": {"launches": {f"{k[0]}/{k[1]}": v for k, v in ms.slot_launches._values.items()},
                                     "missed": {k[0]: v for k, v in ms.slot_missed._values.items()}}}
    return json.dumps(out, indent=1, sort_keys=True, default=str) + "\n"


if __name__ == "__main__":
    text, ag, rw, be, ef = build(), agents(), rows(), backend_errors(), effects()
    runs = {PEOPLE_OUT: people_cases(), TRIGGERS_OUT: triggers_cases(), LIMITS_OUT: limits_cases(), MANIFESTS_OUT: manifests_cases(), ACTING_OUT: acting_cases(), RUNS_OUT: results_cases() + "",
            CANDIDATES_OUT: candidates_cases()}
    runs_ops = run_ops_cases()
    if "--check" in sys.argv:
        ok = (OUT.read_text() == text and SCHEMA_OUT.read_text() == pg.SCHEMA and AGENTS_OUT.read_text() == ag
              and ROWS_OUT.is_file() and ROWS_OUT.read_text() == rw
              and ERRORS_OUT.is_file() and ERRORS_OUT.read_text() == be
              and EFFECTS_OUT.is_file() and EFFECTS_OUT.read_text() == ef
              and all(p.is_file() and p.read_text() == t for p, t in runs.items())
              and RUN_OPS_OUT.is_file() and RUN_OPS_OUT.read_text() == runs_ops)
        sys.exit(0 if ok else 1)
    OUT.write_text(text)
    SCHEMA_OUT.write_text(pg.SCHEMA)
    AGENTS_OUT.parent.mkdir(parents=True, exist_ok=True)
    AGENTS_OUT.write_text(ag)
    ROWS_OUT.parent.mkdir(parents=True, exist_ok=True)
    ROWS_OUT.write_text(rw)
    ERRORS_OUT.parent.mkdir(parents=True, exist_ok=True)
    ERRORS_OUT.write_text(be)
    EFFECTS_OUT.write_text(ef)
    for p, t in {**runs, RUN_OPS_OUT: runs_ops}.items():
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(t)
