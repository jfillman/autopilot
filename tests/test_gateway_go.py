"""A-9: the Go AI gateway (gateway/), the slice that is built so far. The Python gateway is still the deployed one and
the reference these tests hold the Go one to:

  - the tool registry, the baseline deny lists, the gateway database schema and the bundled agents' parsed scope are
    generated from the Python code (tools/gen_gateway_registry.py), so the two implementations cannot drift;
  - the Go module builds, vets and passes its own tests, including the replay of Clearance's golden corpus through
    the Go gateway against the real core (gateway/internal/replay) and, with a scratch Postgres, its database tests;
  - the conformance harness that ships with the Clearance release drives the Go gateway's control interface, and it
    stays off unless asked.
"""
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

from test_conformance import _assert_green, _free_port, _harness, _keep, _wait

ROOT = Path(__file__).resolve().parents[1]
GATEWAY = ROOT / "gateway"
GO = shutil.which("go")
needs_go = pytest.mark.skipif(GO is None, reason="no Go toolchain")


def test_what_the_go_gateway_takes_from_the_python_one_is_current():
    r = subprocess.run([sys.executable, str(ROOT / "tools" / "gen_gateway_registry.py"), "--check"], capture_output=True, text=True)
    assert r.returncode == 0, ("gateway/internal/{tools,pgstate,definitions} are generated from the Python gateway: run "
                               "`python tools/gen_gateway_registry.py` and commit the result")


def test_the_registry_is_the_python_registry_in_the_python_order():
    from autopilot.engine.tiers import CORE_TOOLS
    from autopilot.hangar_tools import TOOLS
    doc = json.loads((GATEWAY / "internal" / "tools" / "registry.json").read_text())
    assert [t["name"] for t in doc["tools"]] == [t.name for t in (*CORE_TOOLS, *TOOLS)]


def _clearance_src() -> str | None:
    for cand in (os.environ.get("CLEARANCE_SRC"),
                 os.path.join(os.environ["HANGAR_TECH"], "clearance") if os.environ.get("HANGAR_TECH") else None,
                 str(ROOT.parent / "clearance")):
        if cand and (Path(cand) / "testdata" / "golden" / "corpus.jsonl").is_file():
            return cand
    return None


@needs_go
def test_the_go_module_builds_vets_and_passes_its_tests():
    env = {**os.environ, "CGO_ENABLED": "1", "GOFLAGS": "-mod=readonly"}
    env["CLEARANCE_CORE_BIN"] = os.environ["AUTOPILOT_TEST_CORE_BIN"]   # the binary conftest built: the replay's cores
    src = _clearance_src()
    if src:
        env["CLEARANCE_SRC"] = src
    for cmd in (["go", "vet", "./..."], ["go", "test", "-race", "-count=1", "./..."]):
        r = subprocess.run(cmd, cwd=GATEWAY, capture_output=True, text=True, env=env, timeout=900)
        assert r.returncode == 0, f"{' '.join(cmd)}\n{r.stdout[-6000:]}\n{r.stderr[-3000:]}"
    if src:
        r = subprocess.run(["go", "test", "-count=1", "-v", "-run", "TestCorpusReplaysThroughGateway", "./internal/replay"],
                           cwd=GATEWAY, capture_output=True, text=True, env=env, timeout=600)
        assert r.returncode == 0 and "--- PASS: TestCorpusReplaysThroughGateway" in r.stdout, r.stdout[-4000:]


@pytest.fixture(scope="module")
def go_binary(tmp_path_factory):
    if GO is None:
        pytest.skip("no Go toolchain")
    out = tmp_path_factory.mktemp("apgw") / "autopilot-gateway"
    r = subprocess.run([GO, "build", "-trimpath", "-o", str(out), "./cmd/autopilot-gateway"], cwd=GATEWAY,
                       capture_output=True, text=True, env={**os.environ, "CGO_ENABLED": "0"})
    assert r.returncode == 0, r.stderr
    return out


@pytest.fixture
def go_control(go_binary, tmp_path):
    port = _free_port()
    env = {**os.environ, "GATEWAY_CONFORMANCE_CONTROL": "1", "AUTOPILOT_GATEWAY_VERSION": "gateway-v0.0.0-conformance"}
    log = (tmp_path / "control.log").open("w")
    p = subprocess.Popen([str(go_binary), "conformance-control", "--listen", f"127.0.0.1:{port}"], env=env,
                         stdout=log, stderr=subprocess.STDOUT)
    url = f"http://127.0.0.1:{port}"
    try:
        _wait(url + "/conformance/v1/info")
        yield url
    finally:
        p.terminate()
        p.wait(timeout=10)
        log.close()


def test_the_go_gateway_passes_the_runtime_conformance_suite(go_control, tmp_path):
    report = tmp_path / "conformance-runtime.json"
    code, out, rep = _harness(go_control, report)
    _keep(report, "conformance-runtime-go-binary.json")
    _assert_green(code, out, rep)


@pytest.mark.skipif(not os.environ.get("AUTOPILOT_CONFORMANCE_IMAGE"),
                    reason="AUTOPILOT_CONFORMANCE_IMAGE (the released core image; CI) is not set")
def test_the_go_gateway_passes_the_runtime_suite_of_the_core_image(go_control, tmp_path):
    out_dir = tmp_path / "out"
    out_dir.mkdir()
    report = out_dir / "conformance-runtime.json"
    code, out, rep = _harness(go_control, report, image=os.environ["AUTOPILOT_CONFORMANCE_IMAGE"])
    _keep(report, "conformance-runtime-go-image.json")
    _assert_green(code, out, rep)


def test_the_go_control_interface_is_off_without_the_switch_and_loopback_only(go_binary):
    env = {k: v for k, v in os.environ.items() if k != "GATEWAY_CONFORMANCE_CONTROL"}
    assert subprocess.run([str(go_binary), "conformance-control"], env=env, capture_output=True).returncode == 2
    on = {**env, "GATEWAY_CONFORMANCE_CONTROL": "true"}
    assert subprocess.run([str(go_binary), "conformance-control"], env=on, capture_output=True).returncode == 2, \
        "only the exact value 1 turns it on"
    on = {**env, "GATEWAY_CONFORMANCE_CONTROL": "1"}
    r = subprocess.run([str(go_binary), "conformance-control", "--listen", "0.0.0.0:9091"], env=on, capture_output=True, text=True)
    assert r.returncode == 2 and "loopback" in r.stderr


def test_the_go_gateway_refuses_what_it_does_not_carry_yet(go_binary):
    base = {k: v for k, v in os.environ.items() if not k.startswith(("CLEARANCE_", "GATEWAY_", "GITHUB_"))}
    r = subprocess.run([str(go_binary), "api"], env=base, capture_output=True, text=True)
    assert r.returncode == 2 and "not ported yet" in r.stderr
    env = {**base, "GATEWAY_DSN": "postgresql://x", "CLEARANCE_CORE_URL": "http://127.0.0.1:1", "GITHUB_PR_ENABLED": "1"}
    r = subprocess.run([str(go_binary), "serve"], env=env, capture_output=True, text=True)
    assert r.returncode == 2 and "GITHUB_PR_ENABLED" in r.stderr, "it must not start without a feature it was asked for"
