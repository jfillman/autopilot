import json
import os
import subprocess
import sys
import tempfile
import urllib.request
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

import pytest

from autopilot.engine import profile
from autopilot.backends import fake_backends
from autopilot.gateway import hangar_gateway
from autopilot.engine.dispatch import Principal
from autopilot.engine.session import SessionStore
from autopilot.engine.tiers import Tier

ROOT = Path(__file__).resolve().parents[1]
T0 = datetime(2026, 9, 26, 12, 0, 0, tzinfo=timezone.utc)


class Clock:
    def __init__(self):
        self.t = T0

    def __call__(self):
        return self.t

    def advance(self, **kw):
        self.t += timedelta(**kw)


class StaticAuth:
    def __init__(self, table):
        self.table = table

    def authenticate(self, token):
        return self.table[token]


@pytest.fixture
def clock():
    return Clock()


@pytest.fixture
def defs():
    return profile.load_dir(ROOT / "agents")


@pytest.fixture
def backends():
    return fake_backends()


# -- the Clearance core under test --------------------------------------------------------------------------------
# Every gateway in these tests asks a real Clearance core: the unmodified `clearance` binary, run as `clearance core
# --memory --insecure-dev-auth --max-clock-skew 0` (tests/clearance_core/coreenv.py), which serves the chain it
# wrote at GET /v1/dev/records. CLEARANCE_CORE_BIN names a built binary; else CLEARANCE_SRC (or
# $HANGAR_TECH/clearance, or ../clearance) is a checkout to `go build ./cmd/clearance` from. CI checks clearance out
# at the ref pyproject.toml pins the client by, so the core under test is the core the client was generated from.

def _core_binary(tmp: Path) -> Path:
    if os.environ.get("CLEARANCE_CORE_BIN"):
        return Path(os.environ["CLEARANCE_CORE_BIN"])
    for cand in (os.environ.get("CLEARANCE_SRC"),
                 os.path.join(os.environ["HANGAR_TECH"], "clearance") if os.environ.get("HANGAR_TECH") else None,
                 str(ROOT.parent / "clearance")):
        if cand and (Path(cand) / "cmd" / "clearance").is_dir():
            out = tmp / "clearance"
            subprocess.run(["go", "build", "-trimpath", "-o", str(out), "./cmd/clearance"], cwd=cand, check=True,
                           env={**os.environ, "CGO_ENABLED": "0"})
            return out
    pytest.exit("no Clearance core to test against: set CLEARANCE_CORE_BIN, or CLEARANCE_SRC to a clearance checkout",
                returncode=4)


sys.path.insert(0, str(ROOT / "tests" / "clearance_core"))
from coreenv import CORE_ENV, CoreProcess, tap_core  # noqa: E402

_CORE: dict[str, Any] = {}


import coreenv  # noqa: E402


def pytest_configure(config):
    """Start the test core once, before collection, so a module's environment (coreenv.core_env) can name it."""
    tmp = Path(tempfile.mkdtemp(prefix="autopilot-core-"))
    binary = _core_binary(tmp)
    os.environ["AUTOPILOT_TEST_CORE_BIN"] = str(binary)
    c = CoreProcess(binary).start()
    tok = tmp / "core-token"
    tok.write_text(coreenv.GATEWAY_PRINCIPAL)   # --insecure-dev-auth: the bearer token is the principal
    os.environ["AUTOPILOT_TEST_CORE_URL"], os.environ["AUTOPILOT_TEST_CORE_TOKEN_FILE"] = c.url, str(tok)
    _CORE["core"] = c


def pytest_unconfigure(config):
    c = _CORE.pop("core", None)
    if c is not None:
        c.stop()


@pytest.fixture(scope="session")
def test_core():
    return _CORE["core"]


@pytest.fixture(autouse=True)
def _serve_reads_its_chain(monkeypatch):
    """A gateway serve.build_gateway builds keeps its real wiring (client_from_env, the token file, the outbox) and
    is a TapCore, so the test can read what it recorded."""
    import coreenv
    from autopilot import serve
    monkeypatch.setattr(serve, "Core", coreenv.TapCore)


@pytest.fixture
def make_core(test_core):
    """make_core(allow_reads=..., url=..., outbox=..., record_kinds=..., timeout=...): coreenv.tap_core."""
    return tap_core


@pytest.fixture
def core(make_core):
    return make_core()


@pytest.fixture
def gw(defs, backends, clock, core):
    counter = iter(range(1, 1000))
    store = SessionStore(id_gen=lambda: f"s-{next(counter):06x}")
    auth = StaticAuth({
        "alice": Principal("user:alice", "user", Tier.T1),
        "bob": Principal("user:bob", "user", Tier.T1),
        "readonly": Principal("user:ro", "user", Tier.T0),
        "holmes": Principal("holmesgpt/holmes", "workload", Tier.T1),
        "triage": Principal("autopilot-runs/triage-agent", "workload", Tier.T1),
    })
    return hangar_gateway(defs, store, core, backends, auth, clock)
