"""R-3a's two copies: the gateway engine (autopilot.engine) moved from clearance v0.26.1, and the clearance package keeps
its own copy of each moved module until the removal PR (R-3b), because Clearance modules that stay (policy, pg, auth,
api, definitions, review, packs.hangar) still import them. Some of that copy is still exercised at runtime here:

  clearance.tiers      Policy builds the registry the Gateway dispatches against (Gateway.tools = policy.tools) from
                       clearance.tiers.CORE_TOOLS and the Hangar pack's specs.
  clearance.profile    cluster-pinned definitions (clearance.definitions.from_pin) and definition review
  + limits             (clearance.review) are parsed by Clearance's profile, schema and Limits; the engine opens and
                       narrows sessions on them.
  clearance.session    clearance.api's policy dry-run builds a Session; clearance.policy reads the engine's sessions.
  clearance.inbox      clearance.pg's PgInbox and PgHolds build HumanRequest and Hold; PgEvents reads events.RETENTION.
  + control + events
  clearance.dispatch   clearance.auth's authenticators return its Principal and raise its AuthError; PgArtifacts raises
                       its Denied. AuthError and Denied are therefore one class (autopilot.engine.core); Principal is
                       used by attribute only.

So the two copies must stay identical until then. These tests fail when they diverge; delete this file with the
removal PR, when the engine is the only copy."""
from __future__ import annotations

import ast
import importlib
import importlib.util
import json
from dataclasses import asdict
from pathlib import Path

import pytest

import autopilot.engine as engine
from autopilot.engine import core

ENGINE = Path(engine.__file__).resolve().parent
SRC = ENGINE.parent
MOVED = ("server", "dispatch", "session", "limits", "tiers", "modelproxy", "backends", "profile", "runtimes", "inbox",
         "control", "results", "events")
# The classes the engine takes from clearance.dispatch instead of defining (core.py says why).
SHARED = {"dispatch": ("AuthError", "Denied")}


def _clearance_source(mod: str) -> str:
    return Path(importlib.util.find_spec(f"clearance.{mod}").origin).read_text()


class _NoImports(ast.NodeTransformer):
    """Drops every import (they differ by design: relative in clearance, the engine's own or the seam's here) and
    the named top-level classes."""

    def __init__(self, drop: tuple[str, ...] = ()):
        self.drop = drop

    def visit_Import(self, node):
        return None

    visit_ImportFrom = visit_Import

    def visit_ClassDef(self, node):
        return None if node.name in self.drop else self.generic_visit(node)


def _normal(src: str, drop: tuple[str, ...] = ()) -> str:
    tree = _NoImports(drop).visit(ast.parse(src))
    return ast.dump(tree, include_attributes=False)


@pytest.mark.parametrize("mod", MOVED)
def test_the_moved_module_is_clearances_copy_but_for_its_imports(mod):
    ours = (ENGINE / f"{mod}.py").read_text()
    theirs = _clearance_source(mod)
    assert _normal(ours) == _normal(theirs, SHARED.get(mod, ())), (
        f"autopilot/engine/{mod}.py has diverged from clearance.{mod}: change both, or finish R-3b's removal first")


def test_the_agent_definition_schema_is_clearances():
    ours = json.loads((ENGINE / "schemas" / "agent-definition.schema.json").read_text())
    theirs = json.loads((Path(importlib.util.find_spec("clearance.profile").origin).parent / "schemas"
                         / "agent-definition.schema.json").read_text())
    assert ours == theirs


def test_the_shared_classes_are_one_class():
    import clearance.dispatch
    from autopilot.engine import dispatch
    for name in SHARED["dispatch"]:
        assert getattr(dispatch, name) is getattr(clearance.dispatch, name) is getattr(core, name)


def test_the_registry_the_gateway_dispatches_against_is_the_engines():
    """Gateway.tools is clearance.policy's registry (core plus pack); built from the engine's own tiers it is the same,
    spec for spec, and the tier vocabulary and tripwires are the same."""
    import clearance.tiers
    from autopilot.engine import tiers
    from autopilot.gateway import PACKS, TOOLS
    ours = tiers.registry(tiers.CORE_TOOLS, *(p.tools for p in PACKS))
    assert list(ours) == list(TOOLS)
    assert {k: asdict(v) for k, v in ours.items()} == {k: asdict(v) for k, v in TOOLS.items()}
    assert tiers.TRIPWIRES == clearance.tiers.TRIPWIRES
    assert [(t.name, int(t)) for t in tiers.Tier] == [(t.name, int(t)) for t in clearance.tiers.Tier]


def _imports(path: Path) -> list[str]:
    out = []
    for node in ast.walk(ast.parse(path.read_text())):
        if isinstance(node, ast.Import):
            out += [a.name for a in node.names]
        elif isinstance(node, ast.ImportFrom) and node.level == 0 and node.module:
            out += [node.module] + [f"{node.module}.{a.name}" for a in node.names]
    return out


def test_only_the_seam_imports_clearance_inside_the_engine():
    offenders = {p.name: [m for m in _imports(p) if m.split(".")[0] == "clearance"]
                 for p in ENGINE.glob("*.py") if p.name != "core.py"}
    assert {k: v for k, v in offenders.items() if v} == {}


def test_nothing_in_autopilot_uses_clearances_copy_of_a_moved_module():
    """Every importer in autopilot (src, tests, tools) takes a moved module from the engine; the one exception is the
    seam's AuthError and Denied, and this file, which compares the copies."""
    root = SRC.parents[1]
    moved = {f"clearance.{m}" for m in MOVED}
    allowed = {("src/autopilot/engine/core.py", "clearance.dispatch"),
               ("src/autopilot/engine/core.py", "clearance.dispatch.AuthError"),
               ("src/autopilot/engine/core.py", "clearance.dispatch.Denied")}
    found = set()
    for base in ("src", "tests", "tools"):
        for p in (root / base).rglob("*.py"):
            if p.resolve() == Path(__file__).resolve():
                continue
            rel = p.relative_to(root).as_posix()
            found |= {(rel, m) for m in _imports(p) if m in moved or any(m.startswith(x + ".") for x in moved)}
    assert found - allowed == set()


def test_the_engine_modules_import():
    for mod in MOVED + ("core",):
        importlib.import_module(f"autopilot.engine.{mod}")
