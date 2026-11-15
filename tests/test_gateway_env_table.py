"""A-9 S7: gateway/envtable.json (every environment variable the Python gateway and autopilot-api read, against the Go gateway's) is
held to the Python source here and to the Go source by gateway/cmd/autopilot-gateway/envtable_test.go."""
import ast
import json
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src" / "autopilot"
TABLE = {v["name"]: v for v in json.loads((ROOT / "gateway" / "envtable.json").read_text())["vars"]}
ENVISH = re.compile(r"^[A-Z][A-Z0-9]*(_[A-Z0-9]+)+$")

# Constants of the form of an environment variable that are not read from the environment: timeouts and rule ids of the core client, and the
# variables the gateway SETS on the run pods it creates (agentrun_compose.py: what the pod reads, not the gateway).
NOT_READ = {"DECISION_TIMEOUT", "DEFINITIONS_TIMEOUT", "OUTAGE_RULE", "REFUSED_RULE", "AUTOPILOT_URL", "MODEL_PROXY_URL"}
NOT_READ_PREFIX = ("HANGAR_",)


def _python_names() -> dict[str, set[str]]:
    out: dict[str, set[str]] = {}
    for p in SRC.rglob("*.py"):
        for n in ast.walk(ast.parse(p.read_text())):
            if isinstance(n, ast.Constant) and isinstance(n.value, str) and (ENVISH.match(n.value) or n.value == "PORT"):
                out.setdefault(n.value, set()).add(p.name)
    return out


def test_every_variable_the_python_gateway_names_is_in_the_table():
    names = _python_names()
    missing = sorted(n for n in names if n not in TABLE and n not in NOT_READ and not n.startswith(NOT_READ_PREFIX))
    assert not missing, (f"the Python source names {missing}: add each to gateway/envtable.json (what it is to the Go gateway: same, "
                         "refused, or not carried with a reason), then run tools/gen_gateway_env_table.py")


def test_every_variable_the_table_calls_pythons_is_in_the_python_source():
    names = _python_names()
    # the metrics port and a few defaults are read through helper functions, but always by a literal name
    gone = sorted(n for n, v in TABLE.items() if v["python"] and n not in names)
    assert not gone, f"the table says Python reads {gone} and no Python file names it"
    wrong = sorted(n for n, v in TABLE.items() if not v["python"] and n in names and v["class"] != "retired")
    assert not wrong, f"the table says Python does not read {wrong}, and it names them"


def test_the_only_variables_go_does_not_carry_are_two_dev_only_tokens():
    not_carried = sorted(n for n, v in TABLE.items() if v["class"] == "not_carried")
    assert not_carried == ["CLEARANCE_CORE_TOKEN", "CLEARANCE_TOKEN"], not_carried
    for n in not_carried:
        assert TABLE[n]["note"] and not TABLE[n].get("deployed")


def test_the_env_document_is_current():
    r = subprocess.run([sys.executable, str(ROOT / "tools" / "gen_gateway_env_table.py"), "--check"], capture_output=True, text=True)
    assert r.returncode == 0, r.stderr


def test_the_deployed_count_is_recorded_and_matches():
    doc = json.loads((ROOT / "gateway" / "envtable.json").read_text())
    for proc in ("serve", "api"):
        n = sum(1 for v in doc["vars"] if v.get("deployed") == "both" or proc in v.get("deployed", "").split(","))
        assert n == doc["deployed_recorded"][proc] > 0, (proc, n, doc["deployed_recorded"])
