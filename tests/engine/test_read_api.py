"""The gateway's read API (read_api.py, `autopilot-gateway api`): who may call what, the scoped reads, and the committed
contract. Ported one-for-one from clearance tests/test_api.py at v0.26.1 for the nine operations the gateway serves
(R-3c); the tests of Clearance's own reads (evidence, counts, the chain check, receipts, the policy and its explainer,
the Hangar pack's app_of) stay with clearance-api. The SQL is exercised against a real Postgres, over both table sets
(GATEWAY_TEST_DSN or CLEARANCE_TEST_DSN; skipped without one)."""
import json
import os
import subprocess
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest
from starlette.testclient import TestClient

from autopilot import read_api as api
from autopilot.engine import runtimes

from .api_helpers import Auth, Groups, MemStore, Owns, TOWER, get

ROOT = Path(__file__).resolve().parents[2]
DSN = os.environ.get("GATEWAY_TEST_DSN") or os.environ.get("CLEARANCE_TEST_DSN")
NOW = datetime(2026, 10, 30, 12, 30, tzinfo=timezone.utc)

# Clearance's reads (clearance v0.27.0 contract/clearance-api.json): clearance-api serves them, the gateway does not.
CLEARANCE_OPS = {"listEvidence", "getEvidence", "verifyEvidence", "countEvidence", "getPolicy", "explainPolicy",
                 "listReceipts", "listProvenance", "listClearances", "countClearances", "getClearance",
                 "getClearancePolicy", "listDefinitions", "getContract"}
GATEWAY_OPS = {"listAgents", "getAgent", "listSessions", "getSession", "getSessionResult", "getTask",
               "listHumanRequests", "listEvents", "listRuntimes"}


@pytest.fixture
def access(defs):
    return api.Access(Auth(), defs, delegates=(TOWER,), notifiers=(TOWER,), auditors=("user:auditor",),
                      directory=Groups(), ownership=Owns())


def test_the_committed_contract_is_current():
    assert json.loads((ROOT / "contract" / "autopilot-api.json").read_text()) == api.contract()
    out = subprocess.run([sys.executable, str(ROOT / "tools" / "gen_api_contract.py"), "--check"], capture_output=True, text=True)
    assert out.returncode == 0, out.stdout
    ops = {o["operationId"] for o in api.contract()["operations"]}
    assert ops == GATEWAY_OPS | {"getContract"}
    assert not (ops - {"getContract"}) & CLEARANCE_OPS, "Clearance's reads are clearance-api's"


def test_every_clearance_v0_26_1_read_is_served_by_one_of_the_two_apis():
    """The split loses nothing: each api.py operation (clearance v0.26.1, vendored in tests/fixtures) is the gateway's or
    clearance-api's, at the same path."""
    old = json.loads((ROOT / "tests" / "fixtures" / "clearance-v0.26.1-api-operations.json").read_text())["operations"]
    assert len(old) == 22
    ours = {o["operationId"]: o for o in api.contract()["operations"]}
    for o in old:
        assert o["operationId"] in ours or o["operationId"] in CLEARANCE_OPS, o["operationId"]
        if o["operationId"] in ours and o["operationId"] != "getContract":
            mine = ours[o["operationId"]]
            assert (mine["path"], mine["params"], mine["roles"], mine["summary"]) == (o["path"], o["params"], o["roles"], o["summary"])


def test_viewers(access):
    with pytest.raises(api.ApiError) as e:
        access.viewer(None, None)
    assert e.value.status == 401
    with pytest.raises(api.ApiError) as e:
        access.viewer("nope", None)
    assert e.value.status == 401
    bob = access.viewer("bob", None)
    assert bob.role == "person" and "orchestrator" in bob.agents and bob.apps == frozenset()
    assert access.viewer("carol", None).apps == {"flight-api"}
    assert access.viewer("audit", None).all
    assert access.viewer("tower", None).role == "notifier"
    t = access.viewer("tower", "user:bob")
    assert (t.role, t.person, t.via) == ("person", "user:bob", TOWER) and "orchestrator" in t.agents
    for tok, who, status in (("svc", None, 403), ("svc", "user:bob", 403), ("tower", "bob", 400)):
        with pytest.raises(api.ApiError) as e:
            access.viewer(tok, who)
        assert e.value.status == status


SESSIONS = [{"id": f"s-{i}", "principal": p, "agent": a, "task_id": f"t-{i}", "created_at": f"2026-10-30T12:0{i}:00Z",
             "expires_at": "2099-01-01T00:00:00+00:00", "closed": False, "used": {"tool_calls": i}}
            for i, (p, a) in enumerate([("user:alice", "coding-agent"), ("user:bob", "orchestrator"), ("user:dave", "coding-agent")])]
HUMAN = [{"id": "h-000000000001", "owner": "user:alice", "group": None},
         {"id": "h-000000000002", "owner": "user:x", "group": "group:default/platform"}]


@pytest.fixture
def client(access):
    app = api.create_api_app(MemStore(SESSIONS, [], HUMAN), access, gateway_url="http://autopilot.example",
                             clearance_api_url="http://clearance-api.example")
    with TestClient(app) as c:
        yield c


def test_http_scopes_by_viewer(client):
    assert get(client, "/v1/sessions").status_code == 401
    assert [s["id"] for s in get(client, "/v1/sessions", "alice").json()["items"]] == ["s-0"]
    assert [s["id"] for s in get(client, "/v1/sessions", "tower", "user:bob").json()["items"]] == ["s-1"]
    assert len(get(client, "/v1/sessions", "audit").json()["items"]) == 3
    assert get(client, "/v1/sessions?limit=1", "audit").json()["next"] == "2026-10-30T12:02:00Z"
    assert [s["id"] for s in get(client, "/v1/sessions?after=2026-10-30T12:02:00Z", "audit").json()["items"]] == ["s-1", "s-0"]
    assert get(client, "/v1/sessions?state=odd", "audit").status_code == 400
    assert get(client, "/v1/sessions?limit=x", "audit").status_code == 400
    assert get(client, "/v1/sessions/s-1", "alice").status_code == 404, "another person's session is not found"
    assert get(client, "/v1/sessions/s-1", "tower", "user:bob").json()["agent"] == "orchestrator"
    assert get(client, "/v1/tasks/t-1", "bob").json()["used"] == {"tool_calls": 1}
    assert get(client, "/v1/tasks/t-1", "alice").status_code == 404, "another person's run tree is not found"


def test_http_roles(client):
    assert get(client, "/v1/sessions", "tower").status_code == 403, "a notifier reads the inbox feed only"
    assert len(get(client, "/v1/human/requests", "tower").json()["items"]) == 2
    assert [r["id"] for r in get(client, "/v1/human/requests", "bob").json()["items"]] == ["h-000000000002"]
    assert [r["id"] for r in get(client, "/v1/human/requests", "alice").json()["items"]] == ["h-000000000001"]
    assert get(client, "/v1/human/requests?since=yesterday", "alice").status_code == 400
    c = get(client, "/v1/contract").json()
    assert c["gateway"] == "http://autopilot.example" and c["clearance"] == "http://clearance-api.example"
    agents = get(client, "/v1/agents", "alice").json()["items"]
    assert any(a["name"] == "orchestrator" and a["owner"] == "group:default/platform" for a in agents)
    assert all("instructions" not in a for a in agents)
    assert all("instructions" not in a for a in get(client, "/v1/agents?full=1", "alice").json()["items"]), "full is an auditor's"
    assert all("instructions" in a for a in get(client, "/v1/agents?full=1", "audit").json()["items"])
    assert get(client, "/v1/agents/nope", "alice").status_code == 404
    assert get(client, "/healthz").json() == {"ok": True}
    assert get(client, "/readyz").json() == {"ok": True}


def test_clearances_reads_are_not_served_here(client):
    """Evidence, receipts, the gate's changes and the policy are clearance-api's: this API does not answer them."""
    for path in ("/v1/evidence", "/v1/evidence/verify", "/v1/evidence/counts", "/v1/receipts?repo=a/b", "/v1/policy",
                 "/v1/clearances", "/v1/clearance-policy", "/v1/definitions"):
        assert get(client, path, "audit").status_code == 404, path


def test_a_finished_runs_result_is_read_like_its_session(client):
    assert get(client, "/v1/sessions/s-0/result", "alice").json()["final_text"] == "Python and Go."
    assert get(client, "/v1/sessions/s-1/result", "alice").status_code == 404, "another person's run's result is not found"
    r = get(client, "/v1/sessions/s-2/result", "audit")
    assert r.status_code == 404 and r.json()["error"] == "the run has not finished"


def test_runtimes_say_which_custom_images_are_allowed(access):
    app = api.create_api_app(MemStore([], [], []), access, custom_images=("ghcr.io/jfillman/*@sha256:*",))
    with TestClient(app) as c:
        assert get(c, "/v1/runtimes", "alice").json() == {"items": [], "custom_images": ["ghcr.io/jfillman/*@sha256:*"]}
    img = "ghcr.io/jfillman/autopilot-agent@sha256:" + "a" * 64
    cat = runtimes.load([{"name": "generic", "image": img}])
    with TestClient(api.create_api_app(MemStore([], [], []), access, runtimes=cat)) as c:
        assert [r["name"] for r in get(c, "/v1/runtimes", "alice").json()["items"]] == ["generic"]
        assert get(c, "/v1/runtimes", "tower").status_code == 403


def test_a_store_failure_is_503_without_internals(access):
    class Broken(MemStore):
        def sessions(self, *a):
            raise RuntimeError("password=hunter2 at db")

        def ping(self):
            raise OSError("down")

    with TestClient(api.create_api_app(Broken([], [], []), access)) as c:
        r = get(c, "/v1/sessions", "alice")
        assert r.status_code == 503 and r.json() == {"error": "read failed: RuntimeError"}
        assert get(c, "/readyz").status_code == 503


def test_a_failed_connect_gives_its_pool_slot_back():
    calls = []

    def connect(dsn, **kw):
        calls.append(kw["options"])
        if len(calls) <= 3:
            raise OSError("postgres restarting")

        class Conn:
            def cursor(self):
                class Cur:
                    description = None
                    def __enter__(self): return self
                    def __exit__(self, *a): return False
                    def execute(self, sql, params): pass
                return Cur()
        return Conn()

    pool = api.Pool("postgresql://x", size=1, connect=connect)
    for _ in range(3):
        with pytest.raises(OSError):
            pool.run("SELECT 1")
    assert pool._made == 0
    assert pool.run("SELECT 1") == [] and pool._made == 1, "the pool still connects after the database is back"
    assert "default_transaction_read_only=on" in calls[-1] and "statement_timeout=5000" in calls[-1]


def test_the_store_reads_only_the_gateways_tables():
    """R-4: clearance v0.26.1's clearance_* runtime tables are no longer a choice."""
    store = api.PgReadStore(None)
    assert set(store.t.values()) == {f"gateway_{t}" for t in ("sessions", "results", "human_requests", "events", "holds",
                                                              "definitions")}
    with pytest.raises(TypeError):
        api.PgReadStore(None, prefix="clearance")


# -- Postgres ----------------------------------------------------------------------------------------------------

@pytest.fixture
def pg():
    if not DSN:
        pytest.skip("set GATEWAY_TEST_DSN to a scratch Postgres")
    psycopg = pytest.importorskip("psycopg")
    from psycopg.types.json import Jsonb
    from autopilot.engine.pg import SCHEMA  # the table set as its writer creates it (R-3b)
    p = "gateway"
    tables = [f"{p}_{t}" for t in ("sessions", "results", "human_requests", "events", "holds", "definitions", "artifacts",
                                    "outbox")]
    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute("DROP TABLE IF EXISTS " + ", ".join(tables))
        c.execute(SCHEMA)
        exp = NOW + timedelta(hours=1)
        for s in SESSIONS + [{**SESSIONS[0], "id": "s-9", "task_id": "t-0", "created_at": "2026-10-30T12:09:00Z", "parent": "s-0"},
                             {**SESSIONS[0], "id": "s-c", "created_at": "2026-10-30T11:00:00Z", "closed": True}]:
            c.execute(f"INSERT INTO {p}_sessions (id, record, expires_at, closed) VALUES (%s, %s, %s, %s)",
                      (s["id"], Jsonb(s), s["expires_at"], s["closed"]))
        c.execute(f"INSERT INTO {p}_results (session, task_id, record, expires_at) VALUES ('s-0', 't-0', %s, %s), ('s-1', 't-1', %s, %s)",
                  (Jsonb({"session": "s-0", "final_text": "ok"}), exp, Jsonb({"session": "s-1"}), NOW - timedelta(days=1)))
        for i, (owner, group, status, created) in enumerate([("user:bob", None, "pending", "2026-10-30T12:00:00+00:00"),
                                                             ("user:x", "group:default/platform", "pending", "2026-10-30T12:10:00+00:00"),
                                                             ("user:bob", None, "answered", "2026-10-30T12:11:00+00:00")]):
            hid = f"h-{i}"
            c.execute(f"INSERT INTO {p}_human_requests (id, session, status, expires_at, record) VALUES (%s, 's-1', %s, %s, %s)",
                      (hid, status, exp, Jsonb({"id": hid, "owner": owner, "group": group, "created_at": created})))
        for eid, app, agent, at in (("ev-1", "flight-api", "coding-agent", "2026-10-30T12:01:00+00:00"),
                                    ("ev-2", "gate-api", "orchestrator", "2026-10-30T12:02:00+00:00")):
            c.execute(f"INSERT INTO {p}_events (id, received_at, source, app, routing_key, record) VALUES (%s, %s, 'glidepath', %s, %s, %s)",
                      (eid, at, app, f"{app}.x", Jsonb({"id": eid, "app": app, "received_at": at, "subject": {"id": f"{app}-s"},
                                                       "outcomes": [{"agent": agent, "result": "launched"}]})))
        c.execute(f"INSERT INTO {p}_holds (agent, until, record) VALUES ('triage-agent', %s, %s), ('orchestrator', %s, %s)",
                  (exp, Jsonb({"agent": "triage-agent", "by": "user:root"}), NOW - timedelta(minutes=1), Jsonb({"agent": "orchestrator"})))
        c.execute(f"INSERT INTO {p}_definitions (name, state, record) VALUES ('gate-triage', 'loaded', %s)", (Jsonb({"name": "gate-triage"}),))
    pool = api.Pool(DSN, size=2)
    yield api.PgReadStore(pool, clock=lambda: NOW), pool
    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute("DROP TABLE IF EXISTS " + ", ".join(tables))


def test_pg_reads_are_scoped_and_read_only(pg, defs):
    store, pool = pg
    access = api.Access(Auth(), defs, auditors=("user:auditor",), directory=Groups(), ownership=Owns())
    alice, bob, carol = (access.viewer(t, None) for t in ("alice", "bob", "carol"))
    everyone = access.viewer("audit", None)
    assert [s["id"] for s in store.sessions({}, alice, 50, None)] == ["s-9", "s-0", "s-c"]
    assert [s["id"] for s in store.sessions({}, bob, 50, None)] == ["s-1"], "bob's own and his team's agent's"
    assert len(store.sessions({}, everyone, 50, None)) == 5
    assert {s["state"] for s in store.sessions({"state": "live"}, everyone, 50, None)} == {"live"}
    assert [s["id"] for s in store.sessions({"state": "closed"}, everyone, 50, None)] == ["s-c"]
    assert store.sessions({"state": "expired"}, everyone, 50, None) == []
    assert [s["id"] for s in store.sessions({"agent": "coding-agent", "principal": "user:dave"}, everyone, 50, None)] == ["s-2"]
    assert [s["id"] for s in store.sessions({}, everyone, 2, "2026-10-30T12:02:00Z")] == ["s-1", "s-0"]
    assert store.session("s-9")["parent"] == "s-0" and store.session("nope") is None
    assert [s["id"] for s in store.task("t-0")] == ["s-c", "s-0", "s-9"]
    assert store.result("s-0", NOW)["final_text"] == "ok" and store.result("s-1", NOW) is None, "an expired result is gone"
    assert [h["id"] for h in store.human_pending(NOW, None)] == ["h-0", "h-1"]
    assert [h["id"] for h in store.human_pending(NOW, datetime(2026, 10, 30, 12, 5, tzinfo=timezone.utc))] == ["h-1"]
    assert [e["id"] for e in store.events({}, everyone, 50, None)] == ["ev-2", "ev-1"]
    assert [e["id"] for e in store.events({}, carol, 50, None)] == ["ev-1"], "an app owner sees her app's events"
    assert [e["id"] for e in store.events({}, bob, 50, None)] == ["ev-2"], "a team sees its agent's outcomes"
    assert [e["id"] for e in store.events({"agent": "coding-agent"}, everyone, 50, None)] == ["ev-1"]
    assert [e["id"] for e in store.events({"key": "gate-api.x", "subject": "gate-api-s"}, everyone, 50, None)] == ["ev-2"]
    assert [e["id"] for e in store.events({"since": "2026-10-30T12:01:30Z"}, everyone, 50, None)] == ["ev-2"]
    assert [e["id"] for e in store.events({}, everyone, 50, "2026-10-30T12:02:00+00:00")] == ["ev-1"]
    assert list(store.holds(NOW)) == ["triage-agent"], "an expired hold is not active"
    assert store.definitions() == [{"name": "gate-triage"}]
    store.ping()
    with pytest.raises(Exception, match="read-only"):
        pool.run(f"DELETE FROM {store.t['sessions']}")


def test_pg_readyz_fails_without_the_gateways_tables(pg):
    store, pool = pg
    import psycopg
    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute("DROP TABLE gateway_sessions")
    with pytest.raises(Exception):
        store.ping()
