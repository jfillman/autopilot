"""Writes what the Go gateway (A-9) takes from the Python one, so the two cannot drift while both exist:
gateway/internal/tools/registry.json (the tool registry: core tools, then the Hangar pack, its T3 tripwires and the
baseline deny lists), gateway/internal/pgstate/schema.sql (the gateway database's schema, engine/pg.py SCHEMA) and
gateway/internal/definitions/testdata/agents.json (each bundled agent definition with the scope and agent the Python
gateway sends the core for it, which the Go parser must reproduce).

tests/test_gateway_go_registry.py fails when the committed file differs from what this prints, so the two
implementations cannot drift while both exist.   python tools/gen_gateway_registry.py [--check]
"""
from __future__ import annotations

import dataclasses
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

import yaml  # noqa: E402
from autopilot.engine import dispatch, pg, profile  # noqa: E402
from autopilot.engine.tiers import CORE_TOOLS, TRIPWIRES  # noqa: E402
from autopilot.hangar_tools import TOOLS as HANGAR  # noqa: E402

OUT = ROOT / "gateway" / "internal" / "tools" / "registry.json"
AGENTS_OUT = ROOT / "gateway" / "internal" / "definitions" / "testdata" / "agents.json"
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


if __name__ == "__main__":
    text, ag = build(), agents()
    if "--check" in sys.argv:
        sys.exit(0 if OUT.read_text() == text and SCHEMA_OUT.read_text() == pg.SCHEMA and AGENTS_OUT.read_text() == ag else 1)
    OUT.write_text(text)
    SCHEMA_OUT.write_text(pg.SCHEMA)
    AGENTS_OUT.parent.mkdir(parents=True, exist_ok=True)
    AGENTS_OUT.write_text(ag)
