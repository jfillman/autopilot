"""The boundary after R-3b: the gateway speaks to Clearance only through the core's API (engine/core.py and the
generated client), and imports nothing from the `clearance` package. The one exception is the read API
(`autopilot-gateway api`, api_serve.py), which R-3c moves off it. Hangar's policy is a bundle the core loads
(policy/hangar/bundle.json, R-5), not a clearance package."""
import ast
from pathlib import Path

SRC = Path(__file__).resolve().parents[2] / "src" / "autopilot"

# file -> why it may still import clearance (R-3c, R-5)
ALLOWED = {"api_serve.py": "the read API, R-3c"}


def clearance_imports(path: Path) -> list[str]:
    out = []
    for node in ast.walk(ast.parse(path.read_text())):
        if isinstance(node, ast.Import):
            out += [a.name for a in node.names if a.name == "clearance" or a.name.startswith("clearance.")]
        elif isinstance(node, ast.ImportFrom) and node.level == 0 and node.module and \
                (node.module == "clearance" or node.module.startswith("clearance.")):
            out.append(node.module)
    return out


def test_nothing_but_the_read_api_imports_clearance():
    found = {str(p.relative_to(SRC)): clearance_imports(p) for p in SRC.rglob("*.py")}
    found = {k: v for k, v in found.items() if v}
    assert set(found) <= set(ALLOWED), {k: v for k, v in found.items() if k not in ALLOWED}


def test_the_engine_reaches_the_core_only_through_core_py():
    users = [p.name for p in (SRC / "engine").glob("*.py")
             if any(m.startswith("clearance_client") for m in _modules(p))]
    assert users == ["core.py"]


def _modules(path: Path) -> list[str]:
    out = []
    for node in ast.walk(ast.parse(path.read_text())):
        if isinstance(node, ast.Import):
            out += [a.name for a in node.names]
        elif isinstance(node, ast.ImportFrom) and node.module:
            out.append(node.module)
    return out
