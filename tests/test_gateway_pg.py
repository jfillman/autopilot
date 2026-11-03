"""The gateway's own database (engine/pg.py) and the R-4 cutover copy (tools/cutover/copy_runtime_tables.py), on a
real Postgres: AUTOPILOT_TEST_PG names a server (a superuser DSN; CI runs a postgres service), and each test makes
its own scratch databases and drops them. Skipped without it."""
import os
import secrets
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

psycopg = pytest.importorskip("psycopg")
BASE = os.environ.get("AUTOPILOT_TEST_PG")
pytestmark = pytest.mark.skipif(not BASE, reason="AUTOPILOT_TEST_PG is not set (a Postgres server for scratch databases)")

ROOT = Path(__file__).resolve().parents[1]
# The tables clearance v0.26.1's pg.py created on clearance-pg (vendored: no clearance package, R-3d).
OLD_SCHEMA = (ROOT / "tests" / "fixtures" / "clearance-v0.26.1-schema.sql").read_text()
sys.path.insert(0, str(ROOT / "tools" / "cutover"))


def _dsn(db: str) -> str:
    from psycopg.conninfo import conninfo_to_dict, make_conninfo
    return make_conninfo(**{**conninfo_to_dict(BASE), "dbname": db})


@pytest.fixture
def scratch():
    made = []

    def make() -> str:
        name = "t_" + secrets.token_hex(4)
        with psycopg.connect(BASE, autocommit=True) as c:
            c.execute(f"CREATE DATABASE {name}")
        made.append(name)
        return _dsn(name)
    yield make
    with psycopg.connect(BASE, autocommit=True) as c:
        for name in made:
            c.execute(f"DROP DATABASE IF EXISTS {name} WITH (FORCE)")


def test_the_gateway_keeps_its_state_and_outbox_and_has_one_writer(scratch):
    from autopilot.engine import pg
    from autopilot.engine.limits import ComputeClass, Limits, Network
    from autopilot.engine.session import Session
    from autopilot.engine.tiers import Tier
    dsn = scratch()
    state, store = pg.open_durable(dsn)
    with pytest.raises(pg.WriterLocked):
        pg.GatewayState(dsn)
    now = datetime.now(timezone.utc)
    lim = Limits(tier_ceiling=Tier.T1, ttl_minutes=30, tool_calls=10, github_calls=10, open_prs=1, model_tokens=10,
                 children=0, tools=frozenset({"catalog.read"}), models=frozenset(), network=Network.CLEARANCE,
                 compute=ComputeClass.SMALL)
    store.adopt({"session": "s-1", "principal": "user:a", "agent": "coding-agent", "task_id": "t-1", "parent": None,
                 "depth": 0, "limits": lim.as_dict(), "expires_at": (now + timedelta(minutes=30)).isoformat()}, now)
    box = pg.PgOutbox(state)
    assert not box.has_pending(frozenset({"run.launch"}))
    a = box.add({"op": "record", "kind": "run.launch", "body": {"session": "s-1"}}, "core-unavailable")
    b = box.add({"op": "record", "kind": "session.end", "body": {"session": "s-1"}}, "held")
    c = box.add({"op": "outcome", "decision_id": "7", "body": {"result": "ok"}}, "core-unavailable")
    store.flush()                                    # one commit: the session and the outbox rows
    state.close()
    state, store = pg.open_durable(dsn)
    assert store.get("s-1").limits.tool_calls == 10
    box = pg.PgOutbox(state)
    kinds = frozenset({"run.launch"})
    assert [oid for oid, _ in box.pending(kinds, 10)] == [a, c], "in order; the held kind is not sendable"
    box.done(a, 3)
    box.refused(c, "409")
    assert box.pending(kinds, 10) == [] and not box.has_pending(kinds)
    assert [r["state"] for r in box.all()] == ["done", "pending", "refused"] and box.all()[1]["id"] == b
    state.close()


def test_the_migration_drops_held_definition_records_and_keeps_the_rest(scratch):
    """clearance v0.28.0: the core writes definition.* itself (PUT /v1/definitions) and refuses them on POST
    /v1/records, so a definition.* row an older gateway held can never be posted. Opening the database drops those
    rows; every other held row stays for drain, and a posted or refused one is history."""
    from autopilot.engine import pg
    dsn = scratch()
    state, _ = pg.open_durable(dsn)
    box = pg.PgOutbox(state)
    load = box.add({"op": "record", "kind": "definition.load", "body": {"agent": "a"}}, "held")
    end = box.add({"op": "record", "kind": "session.end", "body": {"session": "s-1"}}, "held")
    gone = box.add({"op": "record", "kind": "definition.reject", "body": {"agent": "b"}}, "held")
    kept = box.add({"op": "record", "kind": "definition.unload", "body": {"agent": "c"}}, "refused", error="400")
    state.commit()
    state.close()
    state, _ = pg.open_durable(dsn)
    rows = pg.PgOutbox(state).all()
    assert [r["id"] for r in rows] == [end, kept]
    assert {load, gone}.isdisjoint(r["id"] for r in rows) and rows[0]["state"] == "pending"
    state.close()


def _old_rows(dsn):
    """The runtime rows as the Python gateway (clearance v0.26.1 pg.py) wrote them into clearance-pg."""
    from psycopg.types.json import Jsonb
    from autopilot.engine.limits import ComputeClass, Limits, Network
    from autopilot.engine.session import Session
    from autopilot.engine.tiers import Tier
    conn = psycopg.connect(dsn)
    conn.execute(OLD_SCHEMA)
    now = datetime.now(timezone.utc)
    exp = now + timedelta(days=1)
    lim = Limits(tier_ceiling=Tier.T1, ttl_minutes=30, tool_calls=10, github_calls=10, open_prs=1, model_tokens=10,
                 children=0, tools=frozenset({"catalog.read"}), models=frozenset(), network=Network.CLEARANCE,
                 compute=ComputeClass.SMALL)
    with conn.cursor() as cur:
        for i in range(3):
            rec = Session(f"s-{i}", "user:a", "coding-agent", lim, now, exp, task_id=f"t-{i}", closed=i == 2).to_record()
            cur.execute("INSERT INTO clearance_sessions (id, record, expires_at, closed) VALUES (%s, %s, %s, %s)",
                        (f"s-{i}", Jsonb(rec), exp, i == 2))
        cur.execute("INSERT INTO clearance_artifacts (task_id, name, content, bytes, expires_at) VALUES "
                    "('t-0', 'plan', 'é text', 7, %s)", (exp,))
        cur.execute("INSERT INTO clearance_human_requests (id, session, status, expires_at, record) VALUES "
                    "('h-0', 's-0', 'pending', %s, %s)", (exp, Jsonb({"id": "h-0"})))
        cur.execute("INSERT INTO clearance_events (id, received_at, source, app, routing_key, record) VALUES "
                    "('e-0', %s, 'glidepath', 'gate-api', NULL, %s)", (now, Jsonb({"id": "e-0"})))
        cur.execute("INSERT INTO clearance_holds (agent, until, record) VALUES ('triage-agent', %s, %s)",
                    (exp, Jsonb({"agent": "triage-agent"})))
        cur.execute("INSERT INTO clearance_results (session, task_id, record, expires_at) VALUES ('s-0', 't-0', %s, %s)",
                    (Jsonb({"final_text": "done"}), exp))
        cur.execute("INSERT INTO clearance_definitions (name, state, record) VALUES ('gate-triage', 'loaded', %s)",
                    (Jsonb({"name": "gate-triage", "state": "loaded"}),))
        cur.execute("INSERT INTO clearance_evidence (seq, hash, prev_hash, record) VALUES (0, 'h', 'p', '{}'::jsonb)")
    conn.commit()
    conn.close()


def _dump(dsn, table, order):
    with psycopg.connect(dsn) as c:
        return c.execute(f"SELECT * FROM {table} ORDER BY {order}").fetchall()


def test_the_cutover_copies_every_runtime_table_and_leaves_the_evidence(scratch):
    import copy_runtime_tables as cut
    src, dst = scratch(), scratch()
    _old_rows(src)
    lines = []
    assert cut.copy(src, dst, dry_run=True, out=lines.append)["gateway_sessions"] == 3
    assert lines[-1].startswith("dry run")
    with psycopg.connect(dst) as c:
        assert c.execute("SELECT count(*) FROM pg_tables WHERE tablename LIKE 'gateway_%'").fetchone()[0] == 0
    counts = cut.copy(src, dst, out=lines.append)
    assert counts == {"gateway_sessions": 3, "gateway_artifacts": 1, "gateway_human_requests": 1, "gateway_events": 1,
                      "gateway_holds": 1, "gateway_results": 1, "gateway_definitions": 1}
    for s_table, (t_table, cols) in cut.TABLES.items():
        assert _dump(src, s_table, cols[0]) == _dump(dst, t_table, cols[0]), t_table
    with psycopg.connect(dst) as c:
        assert c.execute("SELECT to_regclass('clearance_evidence')").fetchone()[0] is None, "no evidence copied"
    with psycopg.connect(src) as c:
        assert c.execute("SELECT count(*) FROM clearance_evidence").fetchone()[0] == 1
        assert c.execute("SELECT count(*) FROM clearance_sessions").fetchone()[0] == 3, "the source is untouched"
    # A second run refuses (the target has rows), and so does a run beside a live gateway.
    with pytest.raises(cut.CopyError, match="already has"):
        cut.copy(src, dst, out=lines.append)
    from autopilot.engine import pg
    live = pg.GatewayState(dst)
    try:
        with pytest.raises(cut.CopyError, match="writer lock"):
            cut.copy(src, dst, replace=True, out=lines.append)
    finally:
        live.close()
    # The gateway opens the copied tables.
    state, store = pg.open_durable(dst)
    assert sorted(x.id for x in store.all()) == ["s-0", "s-1", "s-2"] and store.get("s-2").closed
    assert pg.PgArtifacts(state).get("t-0", "plan") == "é text"
    assert pg.PgDefinitions(state).all() == [{"name": "gate-triage", "state": "loaded"}]
    state.close()
    assert not any("é" in line or "done" in line.split(":")[-1] for line in lines), "counts only"
