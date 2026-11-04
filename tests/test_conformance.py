"""A-1: the gateway is a conformant runtime provider (Clearance's provider protocol, docs/provider-protocol.md).

`clearance conformance --provider <url> --role runtime`, the harness that ships in the Clearance release under test,
drives the gateway's test-only control interface (autopilot.conformance): the plain binary here (the one conftest
built or CLEARANCE_CORE_BIN names), and the released core image when AUTOPILOT_CONFORMANCE_IMAGE names one (CI). The
harness starts its own in-memory core and fake target; the gateway runs its own tool-call path against them. A broken
variant (acting on a deny) must fail, so a pass is not vacuous. And the switch stays off everywhere it could be set.
"""
import json
import os
import shutil
import socket
import subprocess
import sys
import threading
import time
import urllib.request
from pathlib import Path

import pytest

from autopilot import conformance as cf

ROOT = Path(__file__).resolve().parents[1]
RUNTIME_ASSERTIONS = ["control.info", "runtime.deny-not-acted", "runtime.allow-acted-once",
                      "runtime.outcome-references-decision", "runtime.spend-applied", "runtime.outage-fails-closed",
                      "chain.verify"]


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _wait(url: str, timeout: float = 15) -> None:
    deadline = time.monotonic() + timeout
    while True:
        try:
            with urllib.request.urlopen(url, timeout=1):
                return
        except OSError:
            assert time.monotonic() < deadline, f"{url} did not answer"
            time.sleep(0.1)


@pytest.fixture
def control_cli(tmp_path):
    """The real CLI: `autopilot-gateway conformance-control` (serve.main), as a process, with the switch on."""
    port = _free_port()
    env = {**os.environ, cf.ENABLE_ENV: "1", "AUTOPILOT_GATEWAY_VERSION": "gateway-v0.0.0-conformance"}
    log = (tmp_path / "control.log").open("w")
    p = subprocess.Popen([sys.executable, "-m", "autopilot.serve", "conformance-control", "--listen", f"127.0.0.1:{port}"],
                         env=env, stdout=log, stderr=subprocess.STDOUT)
    url = f"http://127.0.0.1:{port}"
    try:
        _wait(url + "/conformance/v1/info")
        yield url
    finally:
        p.terminate()
        p.wait(timeout=10)
        log.close()


def _harness(provider: str, report: Path, *, image: str | None = None) -> tuple[int, str, dict]:
    if image:
        report.parent.chmod(0o777)
        cmd = ["docker", "run", "--rm", "--network", "host", "-v", f"{report.parent}:/out", image,
               "conformance", "--provider", provider, "--role", "runtime", "--report", f"/out/{report.name}"]
    else:
        cmd = [os.environ["AUTOPILOT_TEST_CORE_BIN"], "conformance", "--provider", provider, "--role", "runtime",
               "--report", str(report)]
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=180)
    print(r.stdout, r.stderr)
    return r.returncode, r.stdout, json.loads(report.read_text()) if report.exists() else {}


def _keep(report: Path, name: str) -> None:
    """CI keeps the report as an artifact (CONFORMANCE_REPORT_DIR)."""
    d = os.environ.get("CONFORMANCE_REPORT_DIR")
    if d and report.exists():
        Path(d).mkdir(parents=True, exist_ok=True)
        shutil.copy(report, Path(d) / name)


def _assert_green(code: int, out: str, rep: dict) -> None:
    assert code == 0, out
    assert rep["pass"] is True and rep["suite"] == "clearance-conformance/v1" and rep["role"] == "runtime"
    assert rep["provider"]["name"] == "autopilot-gateway" and rep["provider"]["version"] == "gateway-v0.0.0-conformance"
    assert [a["id"] for a in rep["assertions"]] == RUNTIME_ASSERTIONS, "every runtime assertion ran"
    assert all(a["pass"] for a in rep["assertions"])
    assert rep["chain"]["ok"] is True and rep["chain"]["records"] > 0


def test_the_gateway_passes_the_runtime_suite(control_cli, tmp_path):
    report = tmp_path / "conformance-runtime.json"
    code, out, rep = _harness(control_cli, report)
    _keep(report, "conformance-runtime-binary.json")
    _assert_green(code, out, rep)


@pytest.mark.skipif(not os.environ.get("AUTOPILOT_CONFORMANCE_IMAGE"),
                    reason="AUTOPILOT_CONFORMANCE_IMAGE (the released core image; CI) is not set")
def test_the_gateway_passes_the_runtime_suite_of_the_core_image(control_cli, tmp_path):
    out_dir = tmp_path / "out"
    out_dir.mkdir()
    report = out_dir / "conformance-runtime.json"
    code, out, rep = _harness(control_cli, report, image=os.environ["AUTOPILOT_CONFORMANCE_IMAGE"])
    _keep(report, "conformance-runtime-image.json")
    _assert_green(code, out, rep)


@pytest.fixture
def control_thread():
    """The control app in this process (so a test can break it), on a real socket."""
    import uvicorn
    server = uvicorn.Server(uvicorn.Config(cf.create_control_app(), host="127.0.0.1", port=0, log_level="warning"))
    t = threading.Thread(target=server.run, daemon=True)
    t.start()
    deadline = time.monotonic() + 10
    while not server.started:
        assert time.monotonic() < deadline, "uvicorn did not start"
        time.sleep(0.02)
    yield f"http://127.0.0.1:{server.servers[0].sockets[0].getsockname()[1]}"
    server.should_exit = True
    t.join(timeout=10)


def test_a_gateway_that_acts_on_a_deny_fails_the_suite(control_thread, tmp_path, monkeypatch):
    """The broken variant: the handler runs whatever the core answered. The harness must catch it."""
    from autopilot.engine import dispatch

    real_call = dispatch.Gateway.call

    def act_anyway(self, token, session_id, tool, args):
        r = real_call(self, token, session_id, tool, args)
        if r.decision.get("allow") is not True and r.decision.get("rule") not in (cf.OUTAGE_RULE, cf.REFUSED_RULE):
            self._handlers[tool](self.store.get(session_id), args)
        return r
    monkeypatch.setattr(cf.ConformanceGateway, "call", act_anyway)
    monkeypatch.setenv("AUTOPILOT_GATEWAY_VERSION", "gateway-v0.0.0-broken")
    code, out, rep = _harness(control_thread, tmp_path / "broken.json")
    assert code == 1, out
    failed = {a["id"] for a in rep["assertions"] if not a["pass"]}
    assert failed == {"runtime.deny-not-acted"}, out


# -- the control interface itself -------------------------------------------------------------------------------------

def _apply(**over):
    a = {"id": "x-1", "role": "runtime", "provider": "autopilot-gateway",
         "core": {"url": "http://127.0.0.1:1", "bearer": "t"}, "target": {"url": "http://127.0.0.1:2", "bearer": "u"},
         "poll_interval_ms": 50,
         "evaluation": {"subject": {"type": "principal", "id": "user:p"}, "action": {"name": "tool.call"},
                        "resource": {"type": "tool", "id": "repo.pr.open"}, "context": {}}}
    a.update(over)
    return json.dumps(a).encode()


@pytest.mark.parametrize("raw, why", [
    (_apply(extra=1), "unknown fields"),
    (_apply(role="deployment"), "runtime role only"),
    (_apply(provider="argo-gate"), "provider is"),
    (_apply(id="Bad_ID"), "id is"),
    (_apply(core={"url": "http://u:p@127.0.0.1:1"}), "without credentials"),
    (_apply(core={"url": "http://127.0.0.1:1", "token": "x"}), "is {url, bearer}"),
    (_apply(change={"app": "a"}), "no change or manifest"),
    (_apply(manifest={}), "no change or manifest"),
    (_apply(manifest="kind: Deployment"), "no change or manifest"),
    (_apply(evaluation={"subject": {}, "action": {"name": "session.open"}, "resource": {}, "context": {}}), "tool.call"),
    (b"[1]", "an object"),
    (b"{" + b" " * cf.MAX_BODY + b"}", "over its cap"),
])
def test_an_apply_is_strictly_decoded(raw, why):
    with pytest.raises(cf.BadApply, match=why):
        cf.decode_apply(raw)


def test_a_duplicate_apply_is_refused():
    c = cf.Control()
    c._run = lambda a: None
    assert c.start(_apply())[0] == 202
    assert c.start(_apply())[0] == 409


@pytest.mark.parametrize("value", ["0.0.0.0:9091", "10.0.0.5:9091", "[::]:9091", "example.com:9091"])
def test_it_binds_loopback_only(value):
    with pytest.raises(ValueError, match="loopback"):
        cf.listen_address(value)


def test_loopback_addresses():
    assert cf.listen_address("127.0.0.1:9091") == ("127.0.0.1", 9091)
    assert cf.listen_address("[::1]:9091") == ("::1", 9091)
    assert cf.listen_address("localhost:9092") == ("localhost", 9092)


def test_it_is_off_without_the_switch(caplog):
    assert cf.main([], env={}) == 2
    assert cf.main([], env={cf.ENABLE_ENV: "true"}) == 2, "only the exact value 1 turns it on"
    assert "test-only and off" in caplog.text


def test_no_manifest_turns_it_on():
    """The switch is never set in anything that deploys: no YAML, Dockerfile or workflow env in this repo names it
    except the test workflow, which runs the suite."""
    hits = []
    for p in ROOT.rglob("*"):
        if (p.is_file() and ".git" not in p.parts and ".venv" not in p.parts
                and (p.suffix in (".yaml", ".yml", ".json") or p.name.startswith("Dockerfile"))
                and p != ROOT / ".github" / "workflows" / "test.yaml"):
            try:
                if cf.ENABLE_ENV in p.read_text(errors="ignore"):
                    hits.append(str(p.relative_to(ROOT)))
            except OSError:
                continue
    assert hits == []


def test_the_version_comes_from_the_image():
    from autopilot.version import gateway_version
    assert gateway_version({}) == "dev"
    assert gateway_version({"AUTOPILOT_GATEWAY_VERSION": "gateway-v0.4.2"}) == "gateway-v0.4.2"
    dockerfile = (ROOT / "Dockerfile").read_text()
    assert "ENV AUTOPILOT_GATEWAY_VERSION=${GATEWAY_VERSION}" in dockerfile
    assert "GATEWAY_VERSION=${{ github.ref_name }}" in (ROOT / ".github" / "workflows" / "gateway-image.yaml").read_text()
