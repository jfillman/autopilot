"""The Clearance core stands alone (D16): it installs where there is no Airframe and no Glidepath.

Core is every module under src/clearance except packs/hangar. It may not import autopilot (Hangar's
backends) or the Hangar pack, directly or through anything it imports, and a Gateway built with no
packs is a working Clearance for the core tools.
"""
import ast
import asyncio
import json
import subprocess
import sys
from pathlib import Path

from clearance import profile
from clearance.audit import AuditLog
from clearance.backends import FakeHuman, FakeRead, FakeRunBackend, core_handlers
from clearance.dispatch import Gateway, Principal
from clearance.packs.hangar import HANGAR
from clearance.policy import RULES, Policy
from clearance.server import create_server
from clearance.session import SessionStore
from clearance.tiers import CORE_TOOLS, TOOLS, Tier

from conftest import ROOT, StaticAuth

SRC = ROOT / "src"
PKG = SRC / "clearance"
HANGAR_PACK = PKG / "packs" / "hangar"
FORBIDDEN = ("autopilot", "clearance.packs.hangar")


def _core_files() -> list[Path]:
    return sorted(p for p in PKG.rglob("*.py") if HANGAR_PACK not in p.parents)


def _module(path: Path) -> str:
    parts = list(path.relative_to(SRC).with_suffix("").parts)
    return ".".join(parts[:-1] if parts[-1] == "__init__" else parts)


def _imports(path: Path) -> set[str]:
    """Every module an import statement in `path` names, relative imports resolved."""
    mod = _module(path)
    package = mod if path.name == "__init__.py" else mod.rpartition(".")[0]
    out: set[str] = set()
    for node in ast.walk(ast.parse(path.read_text())):
        if isinstance(node, ast.Import):
            out.update(a.name for a in node.names)
        elif isinstance(node, ast.ImportFrom):
            if node.level:
                base = package.split(".")
                base = base[: len(base) - (node.level - 1)]
                target = ".".join(base + ([node.module] if node.module else []))
            else:
                target = node.module or ""
            out.add(target)
            out.update(f"{target}.{a.name}" for a in node.names)
    return out


def _forbidden(name: str) -> bool:
    return any(name == f or name.startswith(f + ".") for f in FORBIDDEN)


def test_the_core_is_not_empty_and_excludes_the_hangar_pack():
    mods = {_module(p) for p in _core_files()}
    assert {"clearance.dispatch", "clearance.policy", "clearance.tiers", "clearance.server", "clearance.pack"} <= mods
    assert not any(m.startswith("clearance.packs.hangar") for m in mods)


def test_no_core_module_names_autopilot_or_the_hangar_pack():
    bad = {_module(p): sorted(i for i in _imports(p) if _forbidden(i)) for p in _core_files()}
    bad = {m: i for m, i in bad.items() if i}
    assert bad == {}, f"core modules importing Hangar code: {bad}"


def test_importing_the_whole_core_loads_nothing_from_hangar():
    """The AST scan sees import statements; this sees what actually loads, in a fresh interpreter."""
    mods = [_module(p) for p in _core_files()]
    assert len(mods) > 10
    code = ("import importlib, json, sys\n"
            f"for m in {mods!r}: importlib.import_module(m)\n"
            "print(json.dumps(sorted(sys.modules)))")
    out = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, check=True,
                         env={"PYTHONPATH": str(SRC)}, cwd=str(ROOT))
    loaded = json.loads(out.stdout)
    assert "clearance.dispatch" in loaded
    assert [m for m in loaded if _forbidden(m)] == []


def _bare_gateway(clock, **kw):
    auth = StaticAuth({"alice": Principal("user:alice", "user", Tier.T1)})
    return Gateway(profile.load_dir(ROOT / "agents"), SessionStore(), AuditLog(), auth, clock, **kw)


def test_a_gateway_with_no_packs_opens_a_session_and_calls_a_core_tool(clock):
    read = FakeRead()
    gw = _bare_gateway(clock, handlers=core_handlers(read=read))
    assert gw.tools == TOOLS and set(gw.tools) == {t.name for t in CORE_TOOLS}
    sid = gw.open_session("alice", "coding-agent").data["session"]
    r = gw.call("alice", sid, "catalog.read", {"query": "flight"})
    assert r.ok and read.names() == ["read.catalog"]
    # A Hangar tool is simply unknown without the pack, and a tripwire is still a tripwire.
    assert gw.call("alice", sid, "glidepath.runs.list", {"app": "gate-api"}).decision["rule"] == "R003"
    assert gw.call("alice", sid, "k8s.exec", {}).decision["rule"] == "R002"


def test_a_registered_tool_with_no_handler_is_refused_and_charged_nothing(clock):
    gw = _bare_gateway(clock, handlers=core_handlers(read=FakeRead()))     # no HumanBackend
    sid = gw.open_session("alice", "coding-agent").data["session"]
    r = gw.call("alice", sid, "human.request", {"question": "ship it?"})
    assert not r.ok and gw.audit.records[-1]["result"] == "error:NoHandler"
    assert gw.store.get(sid).used["tool_calls"] == 0


def test_the_run_tree_works_with_any_run_backend(clock):
    runs = FakeRunBackend()
    gw = _bare_gateway(clock, handlers=core_handlers(human=FakeHuman()), runs=runs)
    sid = gw.open_session("alice", "orchestrator").data["session"]
    r = gw.call("alice", sid, "run.spawn", {"agent": "researcher", "limits": {"tool_calls": 10}})
    assert r.ok, r.as_dict()
    assert r.data["run"] == runs.live[r.data["session"]]
    assert gw.call("alice", sid, "run.close", {"run_id": r.data["session"]}).ok and runs.live == {}


def test_tools_list_is_the_gateways_registry(clock):
    def listed(gw):
        mcp = create_server(gw, lambda: "alice")
        res = asyncio.run(mcp.call_tool("tools_list", {}))
        sc = getattr(res, "structured_content", None) or getattr(res, "structuredContent", None)
        return {t["name"] for t in (sc.get("result", sc) if sc else json.loads(res.content[0].text))}
    bare = listed(_bare_gateway(clock))
    full = listed(_bare_gateway(clock, packs=(HANGAR,)))
    assert bare == set(TOOLS) and full == bare | {t.name for t in HANGAR.tools}


def test_handlers_and_packs_are_checked_when_the_gateway_is_built(clock):
    import pytest
    with pytest.raises(ValueError, match="not registered"):
        _bare_gateway(clock, handlers={"glidepath.runs.list": lambda s, a: None})
    with pytest.raises(ValueError, match="owns"):
        _bare_gateway(clock, handlers={"run.spawn": lambda s, a: None})
    with pytest.raises(ValueError, match="twice"):
        Policy((HANGAR, HANGAR))


def test_rule_order_with_the_hangar_pack_is_the_order_before_the_split():
    """First match wins, so the order is behaviour. This is the order RULES had when R021-R023 were core."""
    assert [r.id for r in RULES] == ["R001", "R002", "R003", "R004", "R005", "R006", "R007", "R008", "R009",
                                     "R010", "R011", "R012", "R013", "R014", "R015", "R016", "R017",
                                     "R019", "R018", "R020"]
    assert [r.id for r in Policy((HANGAR,)).rules] == [
        "R001", "R002", "R003", "R004", "R005", "R006", "R007", "R008", "R009", "R010", "R011", "R012",
        "R013", "R014", "R015", "R016", "R017", "R022", "R023", "R019", "R018", "R020", "R021"]
