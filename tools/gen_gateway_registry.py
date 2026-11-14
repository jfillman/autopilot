"""Writes what the Go gateway (A-9) takes from the Python one, so the two cannot drift while both exist:
gateway/internal/tools/registry.json (the tool registry: core tools, then the Hangar pack, its T3 tripwires and the
baseline deny lists), gateway/internal/pgstate/schema.sql (the gateway database's schema, engine/pg.py SCHEMA) and
gateway/internal/definitions/testdata/agents.json (each bundled agent definition with the scope and agent the Python
gateway sends the core for it, which the Go parser must reproduce) and gateway/internal/session/testdata/rows.json
(session rows as the Python gateway writes them, Session.to_record, which the Go gateway must read and write back whole)
and gateway/internal/dispatch/testdata/backend_errors.json (what the Python dispatch tells the agent and writes on the
outcome for each kind of backend failure, which the Go dispatch must reproduce) and
gateway/internal/dispatch/testdata/effects.json (the effect, the receipt of a write, Python's dispatch._effect makes of
a tool's result, which the Go dispatch must send the core on the outcome).

tests/test_gateway_go.py fails when the committed file differs from what this prints, so the two
implementations cannot drift while both exist.   python tools/gen_gateway_registry.py [--check]
"""
from __future__ import annotations

import dataclasses
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
from autopilot.hangar_tools import TOOLS as HANGAR  # noqa: E402

OUT = ROOT / "gateway" / "internal" / "tools" / "registry.json"
AGENTS_OUT = ROOT / "gateway" / "internal" / "definitions" / "testdata" / "agents.json"
ERRORS_OUT = ROOT / "gateway" / "internal" / "dispatch" / "testdata" / "backend_errors.json"
EFFECTS_OUT = ROOT / "gateway" / "internal" / "dispatch" / "testdata" / "effects.json"
ROWS_OUT = ROOT / "gateway" / "internal" / "session" / "testdata" / "rows.json"
SCHEMA_OUT = ROOT / "gateway" / "internal" / "pgstate" / "schema.sql"


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


if __name__ == "__main__":
    text, ag, rw, be, ef = build(), agents(), rows(), backend_errors(), effects()
    if "--check" in sys.argv:
        sys.exit(0 if OUT.read_text() == text and SCHEMA_OUT.read_text() == pg.SCHEMA and AGENTS_OUT.read_text() == ag
                 and ROWS_OUT.is_file() and ROWS_OUT.read_text() == rw
                 and ERRORS_OUT.is_file() and ERRORS_OUT.read_text() == be
                 and EFFECTS_OUT.is_file() and EFFECTS_OUT.read_text() == ef else 1)
    OUT.write_text(text)
    SCHEMA_OUT.write_text(pg.SCHEMA)
    AGENTS_OUT.parent.mkdir(parents=True, exist_ok=True)
    AGENTS_OUT.write_text(ag)
    ROWS_OUT.parent.mkdir(parents=True, exist_ok=True)
    ROWS_OUT.write_text(rw)
    ERRORS_OUT.parent.mkdir(parents=True, exist_ok=True)
    ERRORS_OUT.write_text(be)
    EFFECTS_OUT.write_text(ef)
