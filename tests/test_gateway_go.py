"""A-9: the Go AI gateway (gateway/), the slice that is built so far. The Python gateway is still the deployed one and
the reference these tests hold the Go one to:

  - the tool registry, the baseline deny lists, the gateway database schema and the bundled agents' parsed scope are
    generated from the Python code (tools/gen_gateway_registry.py), so the two implementations cannot drift;
  - the Go module builds, vets and passes its own tests, including the replay of Clearance's golden corpus through
    the Go gateway against the real core (gateway/internal/replay) and, with a scratch Postgres, its database tests;
  - the drift guard: gateway/PORTED.json lists the Python files the Go gateway ports, with the hash each had when the Go
    code was last brought level with it (tools/gen_gateway_ported.py), and a change to one fails here until it is ported
    or confirmed not to touch what the Go gateway carries;
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
    assert r.returncode == 0, ("gateway/internal/{tools,pgstate,definitions,session,runs,dispatch}'s fixtures are generated from the "
                               "Python gateway: run `python tools/gen_gateway_registry.py` and commit the result")


def test_the_registry_is_the_python_registry_in_the_python_order():
    from autopilot.engine.tiers import CORE_TOOLS
    from autopilot.hangar_tools import TOOLS
    doc = json.loads((GATEWAY / "internal" / "tools" / "registry.json").read_text())
    assert [t["name"] for t in doc["tools"]] == [t.name for t in (*CORE_TOOLS, *TOOLS)]


# -- the drift guard (gateway/PORTED.json) ---------------------------------------------------------------------------
sys.path.insert(0, str(ROOT / "tools"))
import gen_gateway_ported as ported  # noqa: E402

# What the manifest must always list: removing one of these from the tables to silence a change is itself a failure.
MUST_BE_WATCHED = {
    "src/autopilot/engine/dispatch.py", "src/autopilot/engine/core.py", "src/autopilot/engine/session.py",
    "src/autopilot/engine/definitions.py", "src/autopilot/engine/profile.py", "src/autopilot/engine/modelproxy.py",
    "src/autopilot/engine/auth.py", "src/autopilot/engine/server.py", "src/autopilot/engine/backends.py",
    "src/autopilot/engine/pg.py", "src/autopilot/engine/tiers.py", "src/autopilot/hangar_tools.py",
    "src/autopilot/reads.py", "src/autopilot/serve.py", "src/autopilot/settings.py",
    # S2: the run tree
    "src/autopilot/engine/limits.py", "src/autopilot/engine/results.py", "src/autopilot/kube_runs.py",
    "src/autopilot/manifests.py", "src/autopilot/gateway.py",
}


def _manifest() -> dict:
    return json.loads((GATEWAY / "PORTED.json").read_text())


def test_the_python_files_the_go_gateway_ports_have_not_changed_since_it_was_brought_level():
    m = _manifest()
    changed = ported.changed(m)
    assert not changed, (
        "a Python file the Go gateway ports changed: " + ", ".join(changed) + ".\n"
        "Read the change. If it affects what the Go gateway carries (see `carries` in gateway/PORTED.json), port it to the "
        "Go paths listed there in this pull request, with a test that holds the Go result equal to the Python one. If it "
        "does not (it is in what is left for a later slice, or it changes nothing the Go gateway does), say so in the pull "
        "request. Then run `python tools/gen_gateway_ported.py`, which shows what changed and records the new hashes; do "
        "not edit gateway/PORTED.json by hand.")


def test_the_drift_guard_is_not_vacuous():
    m = _manifest()
    assert len(m["ported"]) >= len(MUST_BE_WATCHED) and MUST_BE_WATCHED <= set(m["ported"]), \
        f"unwatched: {sorted(MUST_BE_WATCHED - set(m['ported']))}"
    for rel, e in m["ported"].items():
        assert (ROOT / rel).is_file(), f"{rel} is listed as ported and does not exist: update tools/gen_gateway_ported.py"
        assert len(e["sha256"]) == 64 and e["go"] and e["carries"], rel
        for g in e["go"]:
            assert (ROOT / g).exists(), f"{rel}: the Go path {g} does not exist"
    # the guard can fail: a hash that differs is reported
    first = next(iter(m["ported"]))
    m["ported"][first]["sha256"] = "0" * 64
    assert ported.changed(m) == [first]
    # and the manifest is what the tool's tables produce (the hashes aside): nothing was edited out by hand
    now = ported.build(None)
    assert {k: {f: v for f, v in e.items() if f != "sha256"} for k, e in _manifest()["ported"].items()} == \
        {k: {f: v for f, v in e.items() if f != "sha256"} for k, e in now["ported"].items()}
    assert _manifest()["not_ported"] == now["not_ported"] and _manifest()["out_of_scope"] == now["out_of_scope"]


def test_every_python_gateway_file_is_classified_so_a_new_module_cannot_be_forgotten():
    m = _manifest()
    listed = [*m["ported"], *m["not_ported"], *m["out_of_scope"]]
    assert len(listed) == len(set(listed)), "a file is in two of ported, not_ported and out_of_scope"
    new = sorted(set(ported.python_files()) - set(listed))
    assert not new, (f"new Python file(s) {new}: decide whether the Go gateway ports them (add to PORTED), a later slice does "
                     "(NOT_PORTED, with the slice) or neither (OUT_OF_SCOPE, with the reason) in tools/gen_gateway_ported.py")
    gone = sorted(rel for rel in listed if not (ROOT / rel).is_file())
    assert not gone, f"classified but missing: {gone}"
    assert set(m["not_ported"].values()) <= set(ported.SLICES)


def test_the_go_client_of_clearance_is_the_release_the_python_client_and_the_test_core_are():
    import re
    ci = re.search(r"^  CLEARANCE_REF: (\S+)", (ROOT / ".github" / "workflows" / "test.yaml").read_text(), re.M).group(1)
    py = re.search(r"clearance@(v[\d.]+)#subdirectory=clients/python", (ROOT / "pyproject.toml").read_text()).group(1)
    go = re.search(r"github.com/jfillman/clearance (v[\d.]+)", (GATEWAY / "go.mod").read_text()).group(1)
    assert ci == py == go, f"CI core {ci}, Python client {py}, Go client {go}: bump the three together"


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
    base = {k: v for k, v in os.environ.items() if not k.startswith(("CLEARANCE_", "AUTOPILOT_", "GATEWAY_", "GITHUB_"))}
    r = subprocess.run([str(go_binary), "api"], env=base, capture_output=True, text=True)
    assert r.returncode == 2 and "not ported yet" in r.stderr
    env = {**base, "GATEWAY_DSN": "postgresql://x", "CLEARANCE_CORE_URL": "http://127.0.0.1:1", "GITHUB_PR_ENABLED": "1"}
    r = subprocess.run([str(go_binary), "serve"], env=env, capture_output=True, text=True)
    assert r.returncode == 2 and "GITHUB_PR_ENABLED" in r.stderr, "it must not start without a feature it was asked for"

    # A-9 S3 carries EVENTS_ENABLED (the event bridge); glidepath-api's URL only together with it, and the RabbitMQ source is refused
    # as the Python gateway refuses it.
    env = {**base, "GATEWAY_DSN": "postgresql://x", "CLEARANCE_CORE_URL": "http://127.0.0.1:1", "GLIDEPATH_API_URL": "http://127.0.0.1:1"}
    r = subprocess.run([str(go_binary), "serve"], env=env, capture_output=True, text=True)
    assert r.returncode == 2 and "GLIDEPATH_API_URL" in r.stderr
    r = subprocess.run([str(go_binary), "serve"], env={**env, "EVENTS_ENABLED": "1", "EVENTS_RABBITMQ_ENABLED": "1"}, capture_output=True, text=True)
    assert r.returncode == 2 and "EVENTS_RABBITMQ_ENABLED" in r.stderr
    r = subprocess.run([str(go_binary), "serve"], env={**base, "GATEWAY_DSN": "postgresql://x", "CLEARANCE_CORE_URL": "http://127.0.0.1:1",
                                                          "EVENTS_ENABLED": "1"}, capture_output=True, text=True)
    assert r.returncode == 2 and "needs GLIDEPATH_API_URL" in r.stderr
