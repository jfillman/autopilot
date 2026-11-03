"""The boundary after R-3: the gateway speaks to Clearance only through the core's API (engine/core.py and the
generated client), and nothing here, source, tools or tests, imports the `clearance` package (which left clearance
in v0.30.0, R-3d). Hangar's policy is a bundle the core loads (policy/hangar/bundle.json, R-5), not a clearance
package. What the tests compare with clearance v0.26.1 is vendored (tests/fixtures/clearance-v0.26.1-*)."""
import ast
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SRC = ROOT / "src" / "autopilot"


def clearance_imports(path: Path) -> list[str]:
    out = []
    for node in ast.walk(ast.parse(path.read_text())):
        if isinstance(node, ast.Import):
            out += [a.name for a in node.names if a.name == "clearance" or a.name.startswith("clearance.")]
        elif isinstance(node, ast.ImportFrom) and node.level == 0 and node.module and \
                (node.module == "clearance" or node.module.startswith("clearance.")):
            out.append(node.module)
    return out


def test_nothing_imports_clearance():
    files = [p for d in ("src", "tools", "tests", "runtime") for p in (ROOT / d).rglob("*.py")
             if not {".venv", "site-packages", "node_modules"} & set(p.parts)]
    assert len(files) > 100
    found = {str(p.relative_to(ROOT)): clearance_imports(p) for p in files}
    assert {k: v for k, v in found.items() if v} == {}


def test_nothing_depends_on_the_clearance_package():
    """Only the generated client is a dependency (clearance-core-client), in every extra."""
    import tomllib
    project = tomllib.loads((ROOT / "pyproject.toml").read_text())["project"]
    deps = project["dependencies"] + [d for ds in project.get("optional-dependencies", {}).values() for d in ds]
    assert not [d for d in deps if d.split("@")[0].split("[")[0].strip() == "clearance"], deps
    assert any(d.startswith("clearance-core-client @") for d in project["dependencies"])


def test_the_import_check_sees_an_import(tmp_path):
    f = tmp_path / "x.py"
    f.write_text("import clearance.pg\nfrom clearance import review\nfrom clearance_client import CoreClient\n")
    assert clearance_imports(f) == ["clearance.pg", "clearance"]


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
