"""The test core's settings as a gateway reads them (tests/conftest.py starts the core in pytest_configure): what
a test's environment for serve.config_from_env adds, so the gateway it builds asks the real test core."""
import json
import os
import subprocess
import tempfile
import time
import urllib.request
from pathlib import Path
from typing import Any


def core_env() -> dict[str, str]:
    return {"CLEARANCE_CORE_URL": os.environ["AUTOPILOT_TEST_CORE_URL"],
            "CLEARANCE_CORE_TOKEN_FILE": os.environ["AUTOPILOT_TEST_CORE_TOKEN_FILE"]}


GATEWAY_PRINCIPAL = "autopilot-system/autopilot-gateway"
CORE_ENV = {
    # The gateway publishes its cluster-pinned definitions (PUT /v1/definitions): "definitions": true.
    "CLEARANCE_PROVIDERS": json.dumps([{"name": "autopilot-gateway", "version": "test",
                                        "principals": [GATEWAY_PRINCIPAL], "definitions": True}]),
    "CLEARANCE_PACKS": "hangar",
    # Provenance is the core's (tests/test_glidepath_tools.py asks it about an agent's PR): the CI gate's verifier
    # identity, and the platform's bundled definitions, which name the agents' trailers it checks.
    "CLEARANCE_VERIFIERS": "app-*-cicd/pipeline-runner",
    "CLEARANCE_AGENTS_DIR": str(Path(__file__).resolve().parents[2] / "agents"),
}

# The unmodified core: an in-memory store, `Bearer <principal>` as that principal (which also serves the chain at
# GET /v1/dev/records), and no clock-skew check, because the tests run on a fixed clock (2026-09-26).
CORE_ARGS = ("core", "--memory", "--insecure-dev-auth", "--max-clock-skew", "0", "--listen", "127.0.0.1:0")
DEV_RECORDS_PAGE = 10000   # the core's limit per GET /v1/dev/records


def read_records(url: str, since: int = 0) -> list[dict[str, Any]]:
    """The core's chain from seq `since` (GET /v1/dev/records, page by page). Any bearer token will do."""
    out: list[dict[str, Any]] = []
    while True:
        req = urllib.request.Request(f"{url}/v1/dev/records?from={since}&limit={DEV_RECORDS_PAGE}",
                                     headers={"Authorization": f"Bearer {GATEWAY_PRINCIPAL}"})
        with urllib.request.urlopen(req, timeout=10) as r:
            page = json.load(r)
        out.extend(page)
        if len(page) < DEV_RECORDS_PAGE:
            return out
        since = page[-1]["seq"] + 1


class CoreProcess:
    """One running test core (`clearance core --memory --insecure-dev-auth --max-clock-skew 0`): start() it, read its
    chain, stop() it. Its log goes to a file (a pipe nobody drains would block it); the address it listens on is the
    `addr` of its "clearance core serving" line."""

    def __init__(self, binary: Path, env: dict[str, str] | None = None):
        self.binary, self.env = binary, {**CORE_ENV, **(env or {})}
        self.proc = None
        self.url = ""
        self.log_path: Path | None = None

    def start(self, timeout: float = 30.0) -> "CoreProcess":
        env = {k: v for k, v in os.environ.items() if not k.startswith("CLEARANCE_")}
        fd, name = tempfile.mkstemp(prefix="clearance-core-", suffix=".log")
        self.log_path = Path(name)
        with os.fdopen(fd, "w") as log:
            self.proc = subprocess.Popen([str(self.binary), *CORE_ARGS], env={**env, **self.env},
                                         stdout=subprocess.DEVNULL, stderr=log, text=True)
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            for line in self.log_path.read_text().splitlines():
                try:
                    msg = json.loads(line)
                except ValueError:
                    continue
                if msg.get("msg") == "clearance core serving":
                    self.url = "http://" + msg["addr"]
                    return self
            if self.proc.poll() is not None:
                break
            time.sleep(0.05)
        tail = self.log_path.read_text()[-2000:]
        self.stop()
        raise RuntimeError(f"the test core did not start:\n{tail}")

    def stop(self) -> None:
        if self.proc is not None:
            self.proc.terminate()
            self.proc.wait(timeout=10)
            self.proc = None

    def records(self, since: int = 0) -> list[dict[str, Any]]:
        return read_records(self.url, since)


def chain_records(since: int = 0) -> list[dict]:
    return read_records(os.environ["AUTOPILOT_TEST_CORE_URL"], since)


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
