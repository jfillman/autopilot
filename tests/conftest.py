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
from clearance_client import CoreClient

from autopilot.engine.core import Core
from autopilot.engine.outbox import MemoryOutbox
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
# Every gateway in these tests asks a real Clearance core: `clearance autopilot-testcore` (tests/clearance_core), the
# core `clearance core --memory --insecure-dev-auth` runs plus a records read and no clock-skew check, built from a
# clearance checkout. CLEARANCE_CORE_BIN names a built binary; else CLEARANCE_SRC (or $HANGAR_TECH/clearance, or
# ../clearance) is a checkout to build it from. CI checks clearance out at the SHA pyproject.toml pins.

GATEWAY_PRINCIPAL = "autopilot-system/autopilot-gateway"
CORE_ENV = {
    "CLEARANCE_PROVIDERS": json.dumps([{"name": "autopilot-gateway", "version": "test",
                                        "principals": [GATEWAY_PRINCIPAL]}]),
    "CLEARANCE_PACKS": "hangar",
}


def _core_binary(tmp: Path) -> Path:
    if os.environ.get("CLEARANCE_CORE_BIN"):
        return Path(os.environ["CLEARANCE_CORE_BIN"])
    for cand in (os.environ.get("CLEARANCE_SRC"),
                 os.path.join(os.environ["HANGAR_TECH"], "clearance") if os.environ.get("HANGAR_TECH") else None,
                 str(ROOT.parent / "clearance")):
        if cand and (Path(cand) / "cmd" / "clearance").is_dir():
            from build import build
            return build(Path(cand), tmp / "clearance")
    pytest.exit("no Clearance core to test against: set CLEARANCE_CORE_BIN, or CLEARANCE_SRC to a clearance checkout",
                returncode=4)


class CoreProcess:
    """One running test core: start() it, read its chain, stop() it."""

    def __init__(self, binary: Path, env: dict[str, str] | None = None):
        self.binary, self.env = binary, {**CORE_ENV, **(env or {})}
        self.proc = None
        self.url = ""

    def start(self) -> "CoreProcess":
        env = {k: v for k, v in os.environ.items() if not k.startswith("CLEARANCE_")}
        self.proc = subprocess.Popen([str(self.binary), "autopilot-testcore"], env={**env, **self.env},
                                     stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        line = self.proc.stdout.readline().strip()
        if not line:
            raise RuntimeError("the test core did not start")
        self.url = "http://" + line
        return self

    def stop(self) -> None:
        if self.proc is not None:
            self.proc.terminate()
            self.proc.wait(timeout=10)
            self.proc = None

    def records(self, since: int = 0) -> list[dict[str, Any]]:
        with urllib.request.urlopen(f"{self.url}/testcore/v1/records?from={since}", timeout=10) as r:
            return json.load(r)

    def head(self) -> int:
        recs = self.records(max(self._last, 0))
        if recs:
            self._last = recs[-1]["seq"]
        return self._last + 1

    _last = -1


class ChainView:
    """The records the core wrote since a test began (its seq mark): what the tests read where the in-process engine
    had its AuditLog. An allowed call is two records (its decision, then its outcome)."""

    def __init__(self, core: CoreProcess):
        self.core, self.start = core, core.head()

    @property
    def records(self) -> list[dict[str, Any]]:
        return self.core.records(self.start)

    def tools(self) -> list[str]:
        return [r["tool"] for r in self.records]


sys.path.insert(0, str(ROOT / "tests" / "clearance_core"))
_CORE: dict[str, Any] = {}


def pytest_configure(config):
    """Start the test core once, before collection, so a module's environment (coreenv.core_env) can name it."""
    tmp = Path(tempfile.mkdtemp(prefix="autopilot-core-"))
    c = CoreProcess(_core_binary(tmp)).start()
    tok = tmp / "core-token"
    tok.write_text(GATEWAY_PRINCIPAL)   # --insecure-dev-auth: the bearer token is the principal
    os.environ["AUTOPILOT_TEST_CORE_URL"], os.environ["AUTOPILOT_TEST_CORE_TOKEN_FILE"] = c.url, str(tok)
    _CORE["core"] = c


def pytest_unconfigure(config):
    c = _CORE.pop("core", None)
    if c is not None:
        c.stop()


@pytest.fixture(scope="session")
def test_core():
    return _CORE["core"]


@pytest.fixture
def make_core(test_core):
    """A gateway's Core on the test core: make_core(allow_reads=..., url=..., timeout=...)."""
    def make(*, allow_reads: bool = False, url: str | None = None, outbox=None, record_kinds=None, **client_kw):
        client = CoreClient(url or test_core.url, token=GATEWAY_PRINCIPAL, provider="autopilot-gateway", **client_kw)
        kw = {"record_kinds": record_kinds} if record_kinds is not None else {}
        return Core(client, outbox if outbox is not None else MemoryOutbox(), allow_reads=allow_reads, **kw)
    return make


@pytest.fixture
def core(make_core):
    return make_core()


@pytest.fixture
def chain(test_core):
    return ChainView(test_core)


@pytest.fixture
def gw(defs, backends, clock, core, chain):
    counter = iter(range(1, 1000))
    store = SessionStore(id_gen=lambda: f"s-{next(counter):06x}")
    auth = StaticAuth({
        "alice": Principal("user:alice", "user", Tier.T1),
        "bob": Principal("user:bob", "user", Tier.T1),
        "readonly": Principal("user:ro", "user", Tier.T0),
        "holmes": Principal("holmesgpt/holmes", "workload", Tier.T1),
        "triage": Principal("autopilot-runs/triage-agent", "workload", Tier.T1),
    })
    g = hangar_gateway(defs, store, core, backends, auth, clock)
    g.audit = chain   # the core's records since the test began (ChainView)
    return g
