"""The test core's settings as a gateway reads them (tests/conftest.py starts the core in pytest_configure): what
a test's environment for serve.config_from_env adds, so the gateway it builds asks the real test core."""
import json
import os
import subprocess
import urllib.request
from pathlib import Path
from typing import Any


def core_env() -> dict[str, str]:
    return {"CLEARANCE_CORE_URL": os.environ["AUTOPILOT_TEST_CORE_URL"],
            "CLEARANCE_CORE_TOKEN_FILE": os.environ["AUTOPILOT_TEST_CORE_TOKEN_FILE"]}


GATEWAY_PRINCIPAL = "autopilot-system/autopilot-gateway"
CORE_ENV = {
    "CLEARANCE_PROVIDERS": json.dumps([{"name": "autopilot-gateway", "version": "test",
                                        "principals": [GATEWAY_PRINCIPAL]}]),
    "CLEARANCE_PACKS": "hangar",
    # Provenance is the core's (tests/test_glidepath_tools.py asks it about an agent's PR): the CI gate's verifier
    # identity, and the platform's bundled definitions, which name the agents' trailers it checks.
    "CLEARANCE_VERIFIERS": "app-*-cicd/pipeline-runner",
    "CLEARANCE_AGENTS_DIR": str(Path(__file__).resolve().parents[2] / "agents"),
}


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


def chain_records(since: int = 0) -> list[dict]:
    with urllib.request.urlopen(f"{os.environ['AUTOPILOT_TEST_CORE_URL']}/testcore/v1/records?from={since}",
                                timeout=10) as r:
        return json.load(r)


def chain_head() -> int:
    """The seq the next record gets."""
    recs = chain_records(max(_last[0], 0))
    if recs:
        _last[0] = recs[-1]["seq"]
    return _last[0] + 1


_last = [-1]


from autopilot.engine.core import Core  # noqa: E402


class TapCore(Core):
    """A gateway's Core that also reads back what the test core recorded since it was made (`.records`, where the
    in-process engine had AuditLog.records). An allowed call is two records: its decision and its outcome."""

    def __init__(self, *a, **k):
        super().__init__(*a, **k)
        self.start = chain_head()

    @property
    def records(self) -> list[dict]:
        return chain_records(self.start)

    def at(self, seq: int) -> dict:
        """The core's record `seq`."""
        [r] = [r for r in chain_records(seq) if r["seq"] == seq]
        return r

    def held(self, kind: str | None = None) -> list[dict]:
        """What the outbox holds for a kind the core does not take yet (run.finish, session.end, ...), oldest first:
        each as the record it becomes, with `tool` set to its kind."""
        return [{**r["entry"]["body"], "tool": r["entry"]["kind"]} for r in self.outbox.all()
                if r["reason"] == "held" and r["state"] == "pending" and (kind is None or r["entry"]["kind"] == kind)]


def tap_core(*, allow_reads: bool = False, url: str | None = None, outbox=None, record_kinds=None, **client_kw):
    """A TapCore on the test core (or `url`), as the gateway's client: the dev-auth token is the gateway principal."""
    from clearance_client import CoreClient

    from autopilot.engine.outbox import MemoryOutbox

    client = CoreClient(url or os.environ["AUTOPILOT_TEST_CORE_URL"], token=GATEWAY_PRINCIPAL,
                        provider="autopilot-gateway", **client_kw)
    kw = {"record_kinds": record_kinds} if record_kinds is not None else {}
    return TapCore(client, outbox if outbox is not None else MemoryOutbox(), allow_reads=allow_reads, **kw)


def verify_chain() -> tuple[bool, int | None]:
    """The whole test chain, from genesis: each record's hash is sha256 of its canonical JSON without `hash`, and
    each prev_hash is the record before's hash (docs/evidence.md's chain rule, the same for every version)."""
    import hashlib
    import json
    prev = "0" * 64
    for r in chain_records(0):
        body = {k: v for k, v in r.items() if k != "hash"}
        h = hashlib.sha256(json.dumps(body, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()).hexdigest()
        if r["prev_hash"] != prev or h != r["hash"]:
            return False, r["seq"]
        prev = r["hash"]
    return True, None
