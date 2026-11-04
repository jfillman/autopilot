"""A-5 trigger durability: the trigger ledger (engine/trigger_ledger.py), the bridge's saved cursor and catch-up
(event_bridge.py), the reconciliation sweep (reconcile.py), the scheduler's once-per-slot across restarts
(scheduler.py) and the metrics (metrics.py). The Postgres half (a real restart of a durable gateway) is at the end,
skipped without AUTOPILOT_TEST_PG. No network: glidepath-api is a fake opener."""
import io
import json
import os
import secrets
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone

import pytest
from coreenv import core_env

from autopilot import metrics as mx
from autopilot import reconcile as rc
from autopilot import scheduler as sch
from autopilot import serve
from autopilot.backends import Backends, FakeRuns
from autopilot.engine import trigger_ledger as tl
from autopilot.engine.dispatch import Principal
from autopilot.engine.tiers import Tier
from autopilot.event_bridge import EventBridge, id_time

EPH = {"CLEARANCE_EPHEMERAL": "1", "AIRFRAME_ENABLED": "0", "ARGO_ENABLED": "0", **core_env()}


class Auth:
    def authenticate(self, token):
        return Principal(token, "workload", Tier.T1)


def eid_at(t: datetime, h: str = "aaaaaaaaaaaa") -> str:
    return f"{int(t.timestamp() * 1e6):016d}-{h}"


def finished(eid, run, outcome="failure", app="gate-api"):
    return {"id": eid, "app": app, "routingKey": f"{app}.pipelinerun.finished", "receivedAt": "2026-09-26T12:00:00Z",
            "event": {"context": {"type": "dev.cdevents.pipelinerun.finished.0.3.0", "source": f"/glidepath/app-{app}-cicd/{run}"},
                      "subject": {"id": run, "type": "pipelineRun", "content": {"outcome": outcome, "pipelineName": "build"}}}}


def run_row(name, end: datetime, status="failed", app="gate-api"):
    return {"result": "uid-" + name, "name": name, "pipeline": "build", "status": status, "namespace": f"app-{app}-cicd",
            "app": app, "startTime": (end - timedelta(minutes=3)).isoformat(), "endTime": end.isoformat()}


class Res(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


class FakeGlidepath:
    """glidepath-api for the bridge: /v1/events (one batch per connection), /v1/events/recent and /v1/runs."""

    def __init__(self):
        self.stream: list[dict] = []
        self.recent: list[dict] = []
        self.runs: list[dict] = []
        self.calls: list[tuple[str, dict, str | None]] = []

    def __call__(self, req: urllib.request.Request, timeout):
        u = urllib.parse.urlsplit(req.full_url)
        q = dict(urllib.parse.parse_qsl(u.query))
        self.calls.append((u.path, q, req.get_header("Last-event-id")))
        if u.path == "/v1/events":
            body = "".join(f"id: {e['id']}\ndata: {json.dumps(e)}\n\n" for e in self.stream)
            return Res(body.encode())
        if u.path == "/v1/events/recent":
            return Res(json.dumps({"events": [e for e in self.recent if e["id"] > q.get("since", "")]}).encode())
        if u.path == "/v1/runs":
            return Res(json.dumps({"runs": [r for r in self.runs if not q.get("app") or r["app"] == q["app"]]}).encode())
        raise AssertionError(u.path)


def build(clock, ledger=None):
    runs = FakeRuns()
    b = Backends(None, None, runs, None, None)
    g, _ = serve.build_gateway(serve.config_from_env(EPH), backends=b, auth=Auth(), clock=clock)
    if ledger is not None:
        g.triggers = ledger
    return g, runs


@pytest.fixture
def tok(tmp_path):
    p = tmp_path / "token"
    p.write_text("sa")
    return p


# -- keys ---------------------------------------------------------------------------------------------------------
def test_a_pipelinerun_event_is_keyed_by_its_run_and_anything_else_by_its_id():
    assert tl.trigger_key(finished("1-a", "gate-api-push-x")) == "gate-api.pipelinerun.finished/gate-api-push-x"
    other = {"id": "2-b", "app": "gate-api", "routingKey": "gate-api.service.deployed",
             "event": {"subject": {"id": "gate-api"}}}
    assert tl.trigger_key(other) == "event:2-b"
    spoof = {**finished("3-c", "r"), "routingKey": "boarding-api.pipelinerun.finished"}   # not the sender's app
    assert tl.trigger_key(spoof) == "event:3-c"
    assert tl.trigger_key({"routingKey": "x"}) is None
    assert tl.slot_key("24h", 20759) == "slot:24h:20759"


def test_the_run_record_rebuilds_the_events_key_and_what_triggers_match():
    end = datetime(2026, 9, 26, 11, tzinfo=timezone.utc)
    env = rc.envelope_from_run(run_row("gate-api-push-x", end))
    assert tl.trigger_key(env) == tl.trigger_key(finished("1-a", "gate-api-push-x"))
    assert env["event"]["subject"]["content"] == {"outcome": "failure", "pipelineName": "build"}
    assert env["id"] == "reconcile:gate-api/gate-api-push-x"
    assert rc.envelope_from_run({**run_row("y", end), "status": "cancelled"})["event"]["subject"]["content"]["outcome"] == "cancel"
    assert rc.envelope_from_run({**run_row("y", end), "status": "unknown"})["event"]["subject"]["content"]["outcome"] == "error"
    assert rc.envelope_from_run({**run_row("y", end), "app": None}) is None
    assert rc._time("2026-10-29T12:00:12.123456789Z") == datetime(2026, 10, 29, 12, 0, 12, 123456, tzinfo=timezone.utc)
    assert rc._time("yesterday") is None and rc._time(None) is None


def test_the_sweep_reads_only_the_apps_a_trigger_can_match(defs):
    assert rc.watched_apps(defs) == (True, {"gate-api"})     # agents/triage-agent.yaml

    class D:
        def __init__(self, *trigs):
            self.triggers = trigs
    assert rc.watched_apps({"a": D({"type": "event", "source": "glidepath", "bindingKey": "*.pipelinerun.finished"})}) == (True, set())
    assert rc.watched_apps({"a": D({"type": "event", "source": "glidepath", "bindingKey": "gate-api.release.#"})}) == (False, set())
    assert rc.watched_apps({"a": D({"type": "interval", "every": "24h"})}) == (False, set())


def test_the_ledger_claims_once_and_lets_a_stale_claim_go():
    led, t = tl.MemoryTriggers(), datetime(2026, 9, 26, tzinfo=timezone.utc)
    assert led.claim("a", "k", "stream", "1", t) and not led.claim("a", "k", "reconcile", None, t)
    assert led.claim("b", "k", "stream", "1", t), "per agent"
    assert led.claim("a", "k", "reconcile", None, t + tl.STALE), "a claim nobody decided, after STALE"
    led.decide("a", "k", "launched", "s-1", "r-1")
    assert not led.claim("a", "k", "reconcile", None, t + timedelta(days=1))
    assert led.get("a", "k")["state"] == "launched"
    with pytest.raises(ValueError):
        led.claim("a", "k2", "carrier-pigeon", None, t)
    with pytest.raises(ValueError):
        led.decide("a", "k", "maybe")
    c = led.cursor("glidepath", t)
    led.advance("glidepath", "0002-b")
    led.advance("glidepath", "0001-a")
    assert led.cursor("glidepath", t + timedelta(days=1)) == tl.Cursor("glidepath", "0002-b", t), "forward only; floor kept"
    assert c.floor == t and led.prune(t + timedelta(days=31)) == 2


# -- the bridge across a restart ------------------------------------------------------------------------------------
def test_a_restarted_gateway_launches_nothing_twice_and_catches_up_from_its_cursor(clock, tok):
    led, gp = tl.MemoryTriggers(), FakeGlidepath()
    e1, e2, e3 = (finished(eid_at(clock() + timedelta(seconds=i)), f"gate-api-push-{i}") for i in (1, 2, 3))
    g1, runs1 = build(clock, led)
    gp.stream = [e1, e2]
    EventBridge(g1, "http://gp", tok, clock=clock, opener=gp).run_once()
    assert len(runs1.manifests) == 2 and led.cursor("glidepath", clock()).last_id == e2["id"]

    # The gateway restarts (a new process: no sessions in memory, the same database). While it was down e3 came.
    g2, runs2 = build(clock, led)
    gp.calls.clear()
    gp.recent, gp.stream = [e1, e2, e3], [e2, e3]      # the stream's resume replays a little before the cursor too
    m = mx.TriggerMetrics()
    b2 = EventBridge(g2, "http://gp", tok, clock=clock, opener=gp, metrics=m)
    b2.run_once()
    assert gp.calls[0] == ("/v1/events/recent", {"since": e2["id"], "limit": "10000"}, None)
    assert gp.calls[1] == ("/v1/events", {}, e3["id"]), "then the stream, from what the catch-up reached"
    assert [m["spec"]["taskId"] for m in runs2.manifests.values()] and len(runs2.manifests) == 1, "only e3, once"
    assert led.get("triage-agent", tl.trigger_key(e3))["via"] == "recent"
    assert m.resumes.value(result="caught-up") == 1 and m.events_received.value(via="recent") == 1
    assert m.outcomes.value(agent="triage-agent", via="recent", result="launched") == 1
    assert m.missed.value(agent="triage-agent", result="launched") == 0
    assert m.cursor_time.value() == pytest.approx(id_time(e3["id"]).timestamp())


def test_two_events_for_one_pipelinerun_launch_once(clock, tok):
    g, runs = build(clock)
    b = EventBridge(g, "http://gp", tok, clock=clock)
    assert [d["result"] for d in b.handle(finished("1-a", "gate-api-push-x"))] == ["launched"]
    (d,) = b.handle(finished("2-b", "gate-api-push-x"))
    assert d["result"] == "deduped" and not d["new"] and len(runs.manifests) == 1


def test_a_cursor_older_than_the_buffer_says_events_may_be_gone(clock, tok):
    led, gp = tl.MemoryTriggers(), FakeGlidepath()
    led.cursor("glidepath", clock())
    led.advance("glidepath", eid_at(clock() - timedelta(hours=30)))
    m = mx.TriggerMetrics()
    g, _ = build(clock, led)
    assert EventBridge(g, "http://gp", tok, clock=clock, opener=gp, metrics=m).resume() == "stale-cursor"
    assert m.resumes.value(result="stale-cursor") == 1


def test_a_failed_catch_up_still_resumes_the_stream_with_last_event_id(clock, tok):
    led, gp = tl.MemoryTriggers(), FakeGlidepath()
    led.cursor("glidepath", clock())
    led.advance("glidepath", "1793291758385806-aaaaaaaaaaaa")

    def opener(req, timeout):
        if "/recent" in req.full_url:
            raise OSError("connection refused")
        return gp(req, timeout)
    g, _ = build(clock, led)
    m = mx.TriggerMetrics()
    EventBridge(g, "http://gp", tok, clock=clock, opener=opener, metrics=m).run_once()
    assert gp.calls[-1] == ("/v1/events", {}, "1793291758385806-aaaaaaaaaaaa")
    assert m.resumes.value(result="failed") == 1


def test_the_first_start_has_no_cursor_and_starts_live(clock, tok):
    gp = FakeGlidepath()
    g, _ = build(clock)
    EventBridge(g, "http://gp", tok, clock=clock, opener=gp).run_once()
    assert gp.calls == [("/v1/events", {}, None)], "no catch-up and no backlog on the first start"


# -- the reconciliation sweep ---------------------------------------------------------------------------------------
def test_a_seeded_gap_is_reconciled_once_and_a_late_event_is_a_dedupe(clock, tok):
    gp, m = FakeGlidepath(), mx.TriggerMetrics()
    g, runs = build(clock)
    bridge = EventBridge(g, "http://gp", tok, clock=clock, opener=gp, metrics=m)
    bridge.resume()                                  # the floor: now
    clock.advance(minutes=10)
    end = clock() - timedelta(minutes=5)
    gp.runs = [run_row("gate-api-push-lost", end),                      # the gap: its event never came
               run_row("gate-api-push-ok", end, status="succeeded"),    # no trigger matches a success
               run_row("gate-api-push-old", clock() - timedelta(hours=1)),       # before the floor
               run_row("gate-api-push-new", clock() - timedelta(seconds=30))]    # inside SETTLE: its event's turn
    r = rc.Reconciler(bridge, clock=clock)
    (d,) = r.sweep()
    assert (d["agent"], d["result"], d["via"], d["event"]) == ("triage-agent", "launched", "reconcile",
                                                               "reconcile:gate-api/gate-api-push-lost")
    assert gp.calls[-1] == ("/v1/runs", {"app": "gate-api", "sinceHours": "24", "limit": "1000"}, None)
    (manifest,) = runs.manifests.values()
    assert manifest["spec"]["input"]["event"]["subject"]["id"] == "gate-api-push-lost"
    assert m.missed.value(agent="triage-agent", result="launched") == 1 and m.sweeps.value(result="ok") == 1
    hist = {e.id: e for e in g.events.recent()}
    assert hist["reconcile:gate-api/gate-api-push-lost"].outcomes[0]["via"] == "reconcile"
    assert set(hist) == {"reconcile:gate-api/gate-api-push-lost"}, "a run already handled is not history again"

    assert r.sweep() == [] and len(runs.manifests) == 1, "the next sweep: nothing new"
    late = finished(eid_at(clock()), "gate-api-push-lost")
    (d,) = bridge.handle(late)
    assert d["result"] == "deduped" and not d["new"] and len(runs.manifests) == 1, "the event arrives after all"
    assert m.missed.value(agent="triage-agent", result="launched") == 1


def test_a_run_whose_event_came_is_not_a_gap(clock, tok):
    gp, m = FakeGlidepath(), mx.TriggerMetrics()
    g, runs = build(clock)
    bridge = EventBridge(g, "http://gp", tok, clock=clock, opener=gp, metrics=m)
    bridge.resume()
    clock.advance(minutes=1)
    bridge.handle(finished(eid_at(clock()), "gate-api-push-x"))
    clock.advance(minutes=10)
    gp.runs = [run_row("gate-api-push-x", clock() - timedelta(minutes=9))]
    bridge._seen.clear()                              # as after a restart
    assert rc.Reconciler(bridge, clock=clock).sweep() == [] and len(runs.manifests) == 1
    assert m.missed.value(agent="triage-agent", result="launched") == 0


def test_a_braked_or_held_event_stays_decided(clock, tok):
    from autopilot.engine.control import Hold
    gp = FakeGlidepath()
    g, runs = build(clock)
    bridge = EventBridge(g, "http://gp", tok, clock=clock, opener=gp)
    bridge.resume()
    clock.advance(minutes=1)
    g.holds.put(Hold(agent="triage-agent", by="user:root", via=None, reason="noisy", since=clock(),
                     until=clock() + timedelta(minutes=5)))
    assert bridge.handle(finished(eid_at(clock()), "gate-api-push-h"))[0]["result"] == "held"
    clock.advance(minutes=30)                         # the hold is over
    gp.runs = [run_row("gate-api-push-h", clock() - timedelta(minutes=29))]
    bridge._seen.clear()
    assert rc.Reconciler(bridge, clock=clock).sweep() == [] and runs.manifests == {}, "the event was history, not a run"


def test_a_claim_the_gateway_died_holding_is_retried_by_the_sweep(clock, tok):
    led, gp = tl.MemoryTriggers(), FakeGlidepath()
    g, runs = build(clock, led)
    bridge = EventBridge(g, "http://gp", tok, clock=clock, opener=gp)
    bridge.resume()
    clock.advance(minutes=1)
    key = "gate-api.pipelinerun.finished/gate-api-push-c"
    assert led.claim("triage-agent", key, "stream", eid_at(clock()), clock())   # ... and the process died here
    clock.advance(minutes=10)
    gp.runs = [run_row("gate-api-push-c", clock() - timedelta(minutes=9))]
    (d,) = rc.Reconciler(bridge, clock=clock).sweep()
    assert d["result"] == "launched" and led.get("triage-agent", key)["state"] == "launched"


def test_a_failed_sweep_is_counted_and_raises(clock, tok):
    def opener(req, timeout):
        raise OSError("glidepath-api is down")
    m = mx.TriggerMetrics()
    g, _ = build(clock)
    bridge = EventBridge(g, "http://gp", tok, clock=clock, opener=opener, metrics=m)
    with pytest.raises(OSError):
        rc.Reconciler(bridge, clock=clock).sweep()
    assert m.sweeps.value(result="failed") == 1 and m.sweep_ok.value() == 0


# -- the scheduler across a restart ---------------------------------------------------------------------------------
class Result:
    def __init__(self, ok=True, data=None, decision=None, session="s-1"):
        self.ok, self.data, self.decision, self.session, self.error = ok, data or {}, decision or {}, session, None


class Def:
    kind, identity_type, instructions = "scheduled", "workload", "review"
    triggers = ({"type": "interval", "every": "24h"},)


class GW:
    def __init__(self):
        self.defs, self.opened = {"nightly-reviewer": Def()}, []

    def open_triggered(self, agent, trigger, task_id=None):
        self.opened.append(task_id)
        return Result(True, {"deduped": False}, session=f"s-{len(self.opened)}")

    def launch(self, session, run_input):
        return Result(True, {"run": "r-" + session})


def at(day, h, m=0):
    return datetime(2026, 11, day, h, m, tzinfo=timezone.utc)


def test_a_restart_inside_the_window_after_the_slot_fired_does_not_fire_it_again():
    led, m = tl.MemoryTriggers(), mx.TriggerMetrics()
    led.cursor(f"{sch.SOURCE}:nightly-reviewer", at(1, 12))                       # A-5 has been on since yesterday
    gw = GW()
    assert [d["result"] for d in sch.Scheduler(gw, clock=lambda: at(2, 0, 1), ledger=led, metrics=m).tick()] == ["launched"]
    assert m.slot_lag.value(agent="nightly-reviewer") == 60
    # The run finished at 00:03; the gateway restarted at 00:04, inside the grace window.
    assert sch.Scheduler(gw, clock=lambda: at(2, 0, 4), ledger=led, metrics=m).tick() == [] and len(gw.opened) == 1
    assert m.slot_launches.value(agent="nightly-reviewer", result="launched") == 1 and m.slot_missed.value(agent="nightly-reviewer") == 0


def test_a_slot_the_gateway_was_down_for_is_missed_once_and_not_run_late():
    led, m = tl.MemoryTriggers(), mx.TriggerMetrics()
    led.cursor(f"{sch.SOURCE}:nightly-reviewer", at(1, 12))
    gw = GW()
    s = sch.Scheduler(gw, clock=lambda: at(2, 0, 15), ledger=led, metrics=m)      # back at 00:15: the window is gone
    assert [d["result"] for d in s.tick()] == ["missed"] and gw.opened == []
    assert s.tick() == [] and sch.Scheduler(gw, clock=lambda: at(2, 1), ledger=led, metrics=m).tick() == []
    assert m.slot_missed.value(agent="nightly-reviewer") == 1
    slot = sch.slot_of(at(2, 0), timedelta(hours=24))[0]
    assert led.get("nightly-reviewer", tl.slot_key("24h", slot))["state"] == "missed"


def test_nothing_before_the_ledger_existed_is_missed():
    gw, m = GW(), mx.TriggerMetrics()
    assert sch.Scheduler(gw, clock=lambda: at(2, 9), metrics=m).tick() == []
    assert m.slot_missed.value(agent="nightly-reviewer") == 0


# -- metrics and configuration --------------------------------------------------------------------------------------
def test_metrics_render_and_serve():
    m = mx.TriggerMetrics()
    m.missed.inc(agent='tri"age', result="launched")
    m.stream_connected.set(1)
    text = m.registry.render()
    assert '# TYPE autopilot_trigger_missed_total counter' in text
    assert 'autopilot_trigger_missed_total{agent="tri\\"age",result="launched"} 1' in text
    assert "autopilot_events_stream_connected 1" in text
    assert "\nautopilot_reconcile_last_success_timestamp_seconds" not in text, "a gauge never set has no sample"
    with pytest.raises(ValueError):
        m.missed.inc(app="x")
    srv = mx.serve(m.registry, 0, host="127.0.0.1")
    try:
        port = srv.server_address[1]
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/metrics", timeout=5) as res:   # noqa: S310 - local
            assert res.headers["Content-Type"].startswith("text/plain; version=0.0.4") and b"autopilot_" in res.read()
        with pytest.raises(urllib.error.HTTPError) as e:
            urllib.request.urlopen(f"http://127.0.0.1:{port}/mcp", timeout=5)   # noqa: S310 - local
        assert e.value.code == 404
    finally:
        srv.shutdown()


def test_trigger_durability_configuration():
    c = serve.config_from_env(EPH)
    assert (c.reconcile_seconds, c.reconcile_window_hours, c.metrics_port) == (300.0, 24, 9090)
    c = serve.config_from_env({**EPH, "RECONCILE_SECONDS": "0", "METRICS_PORT": "0", "RECONCILE_WINDOW_HOURS": "48"})
    assert (c.reconcile_seconds, c.reconcile_window_hours, c.metrics_port) == (0, 48, 0)
    for env, match in [({"EVENTS_RABBITMQ_ENABLED": "1"}, "designed but not built"),
                       ({"RECONCILE_SECONDS": "5"}, "at least 30"),
                       ({"RECONCILE_WINDOW_HOURS": "800"}, "at most 744"),
                       ({"METRICS_PORT": "8080"}, "must differ")]:
        with pytest.raises(serve.ConfigError, match=match):
            serve.config_from_env({**EPH, **env})


# -- Postgres: a real restart -----------------------------------------------------------------------------------------
BASE = os.environ.get("AUTOPILOT_TEST_PG")
pg_only = pytest.mark.skipif(not BASE, reason="AUTOPILOT_TEST_PG is not set (a Postgres server for scratch databases)")


@pytest.fixture
def scratch_dsn():
    psycopg = pytest.importorskip("psycopg")
    from psycopg.conninfo import conninfo_to_dict, make_conninfo
    name = "t_" + secrets.token_hex(4)
    with psycopg.connect(BASE, autocommit=True) as c:
        c.execute(f"CREATE DATABASE {name}")
    yield make_conninfo(**{**conninfo_to_dict(BASE), "dbname": name})
    with psycopg.connect(BASE, autocommit=True) as c:
        c.execute(f"DROP DATABASE IF EXISTS {name} WITH (FORCE)")


def durable(dsn, clock):
    env = {k: v for k, v in EPH.items() if k != "CLEARANCE_EPHEMERAL"}
    runs = FakeRuns()
    g, state = serve.build_gateway(serve.config_from_env({**env, "GATEWAY_DSN": dsn}),
                                   backends=Backends(None, None, runs, None, None), auth=Auth(), clock=clock)
    return g, state, runs


@pg_only
def test_postgres_restart_during_a_build_loses_no_trigger_and_launches_none_twice(scratch_dsn, clock, tok):
    from autopilot.engine import pg
    gp = FakeGlidepath()
    e1, e2 = (finished(eid_at(clock() + timedelta(seconds=i)), f"gate-api-push-{i}") for i in (1, 2))
    g1, state1, runs1 = durable(scratch_dsn, clock)
    assert isinstance(g1.triggers, pg.PgTriggers)
    gp.stream = [e1]
    EventBridge(g1, "http://gp", tok, clock=clock, opener=gp).run_once()
    assert len(runs1.manifests) == 1
    state1.close()                                     # the gateway restarts; a build finishes meanwhile (e2)

    g2, state2, runs2 = durable(scratch_dsn, clock)
    gp.calls.clear()
    gp.recent = gp.stream = [e1, e2]
    EventBridge(g2, "http://gp", tok, clock=clock, opener=gp).run_once()
    assert gp.calls[0][1]["since"] == e1["id"] and gp.calls[1][2] == e2["id"]
    assert len(runs2.manifests) == 1, "e2 launched once; e1 not again"
    with state2.conn.cursor() as cur:
        cur.execute("SELECT key, via, state FROM gateway_trigger_launches ORDER BY key")
        rows = cur.fetchall()
        cur.execute("SELECT last_id FROM gateway_event_cursors WHERE source = 'glidepath'")
        assert cur.fetchone()[0] == e2["id"]
        cur.execute("SELECT count(*) FROM gateway_events")
        assert cur.fetchone()[0] == 2, "history committed, a replay not written twice"
    state2.commit()
    assert rows == [(tl.trigger_key(e1), "stream", "launched"), (tl.trigger_key(e2), "recent", "launched")]

    # A seeded gap: a failed run whose event no replica kept. The sweep launches it, once, after a restart too.
    clock.advance(minutes=10)
    gp.runs = [run_row("gate-api-push-gap", clock() - timedelta(minutes=5))]
    b2 = EventBridge(g2, "http://gp", tok, clock=clock, opener=gp)
    b2.resume()
    (d,) = rc.Reconciler(b2, clock=clock).sweep()
    assert d["result"] == "launched" and len(runs2.manifests) == 2
    state2.close()
    g3, state3, runs3 = durable(scratch_dsn, clock)
    b3 = EventBridge(g3, "http://gp", tok, clock=clock, opener=gp)
    b3.resume()
    assert rc.Reconciler(b3, clock=clock).sweep() == [] and runs3.manifests == {}
    state3.close()


@pg_only
def test_postgres_ledger_claims_are_unique_and_the_cursor_moves_forward_bytewise(scratch_dsn):
    from autopilot.engine import pg
    t = datetime(2026, 11, 3, tzinfo=timezone.utc)
    state = pg.GatewayState(scratch_dsn)
    led = pg.PgTriggers(state)
    assert led.claim("a", "k", "stream", "1", t) and not led.claim("a", "k", "reconcile", None, t)
    assert led.claim("a", "k", "reconcile", None, t + tl.STALE)
    led.decide("a", "k", "launched", "s-1", "r-1")
    assert not led.claim("a", "k", "reconcile", None, t + timedelta(hours=1))
    assert led.get("a", "k")["state"] == "launched" and led.get("a", "nope") is None
    with pytest.raises(ValueError):
        led.decide("a", "k", "maybe")
    assert led.cursor("glidepath", t) == tl.Cursor("glidepath", None, t)
    led.advance("glidepath", "1793291758385806-ffffffffffff")
    led.advance("glidepath", "1793291758385806-0aaaaaaaaaaa")     # older (bytewise): kept out
    led.advance("glidepath", "1793291758385807-000000000000")
    assert led.cursor("glidepath", t + timedelta(days=2)) == tl.Cursor("glidepath", "1793291758385807-000000000000", t)
    assert led.prune(t + timedelta(days=31)) == 1
    state.close()


def test_a_claim_from_a_process_that_died_is_taken_by_the_next_one_at_once(clock, tok):
    """One writer holds the database, so a `claimed` row from before this process started is an orphan: the
    replayed event launches it now, not STALE later (any event, not only the pipelinerun ones the sweep rebuilds)."""
    led = tl.MemoryTriggers()
    ev = {"id": eid_at(clock()), "app": "gate-api", "routingKey": "gate-api.pipelinerun.finished",
          "event": {"subject": {"id": "gate-api-push-o", "content": {"outcome": "failure"}}}}
    assert led.claim("triage-agent", tl.trigger_key(ev), "stream", ev["id"], clock())     # ... and died
    clock.advance(seconds=20)
    g, runs = build(clock, led)
    b = EventBridge(g, "http://gp", tok, clock=clock)
    clock.advance(seconds=1)
    assert [d["result"] for d in b.handle(ev, "recent")] == ["launched"] and len(runs.manifests) == 1
    assert not led.claim("triage-agent", tl.trigger_key(ev), "stream", ev["id"], clock(),
                         tl.stale_before(b.started, clock())), "this process's own decided claim stays"


def test_the_monitoring_manifest_alerts_on_series_the_gateway_exports():
    """docs/admin/monitoring/autopilot-triggers.yaml is for apron: every series an alert names is one metrics.py
    exports, the rule carries the label Prometheus's rule selector matches, and Prometheus may reach the port."""
    import re
    from pathlib import Path
    import yaml
    path = Path(__file__).resolve().parents[1] / "docs" / "admin" / "monitoring" / "autopilot-triggers.yaml"
    docs = {d["kind"]: d for d in yaml.safe_load_all(path.read_text()) if d}
    names = {m.name for m in mx.TriggerMetrics().registry.metrics}
    rule = docs["PrometheusRule"]
    assert rule["metadata"]["labels"]["release"] == "kube-prometheus-stack"
    exprs = [r["expr"] for g in rule["spec"]["groups"] for r in g["rules"]]
    used = {n for e in exprs for n in re.findall(r"autopilot_[a-z_]+", e)}
    assert used and used <= names, used - names
    assert "autopilot_trigger_missed_total" in used
    assert docs["PodMonitor"]["spec"]["podMetricsEndpoints"][0]["port"] == "metrics"
    (ing,) = docs["NetworkPolicy"]["spec"]["ingress"]
    assert ing["ports"] == [{"protocol": "TCP", "port": serve.config_from_env(EPH).metrics_port}]


def test_a_gap_the_sweep_finds_while_the_agent_is_held_is_counted_as_held_not_launched(clock, tok):
    from autopilot.engine.control import Hold
    gp, m = FakeGlidepath(), mx.TriggerMetrics()
    g, runs = build(clock)
    bridge = EventBridge(g, "http://gp", tok, clock=clock, opener=gp, metrics=m)
    bridge.resume()
    clock.advance(minutes=10)
    g.holds.put(Hold(agent="triage-agent", by="user:root", via=None, reason="noisy", since=clock(),
                     until=clock() + timedelta(hours=1)))
    gp.runs = [run_row("gate-api-push-g", clock() - timedelta(minutes=5))]
    (d,) = rc.Reconciler(bridge, clock=clock).sweep()
    assert d["result"] == "held" and runs.manifests == {}
    assert m.missed.value(agent="triage-agent", result="held") == 1
    assert m.missed.value(agent="triage-agent", result="launched") == 0


def test_a_slot_a_dead_process_was_firing_is_not_called_missed():
    led, m = tl.MemoryTriggers(), mx.TriggerMetrics()
    led.cursor(f"{sch.SOURCE}:nightly-reviewer", at(1, 12))
    slot = sch.slot_of(at(2, 0), timedelta(hours=24))[0]
    assert led.claim("nightly-reviewer", tl.slot_key("24h", slot), "schedule", None, at(2, 0, 1))   # ... and died
    gw = GW()
    assert sch.Scheduler(gw, clock=lambda: at(2, 0, 15), ledger=led, metrics=m).tick() == [] and gw.opened == []
    assert m.slot_missed.value(agent="nightly-reviewer") == 0
    assert led.get("nightly-reviewer", tl.slot_key("24h", slot))["state"] == "claimed"


def test_a_gateway_down_for_three_nights_records_three_missed_slots():
    led, m = tl.MemoryTriggers(), mx.TriggerMetrics()
    led.cursor(f"{sch.SOURCE}:nightly-reviewer", at(1, 12))
    gw = GW()
    out = sch.Scheduler(gw, clock=lambda: at(4, 9), ledger=led, metrics=m).tick()       # down 2nd, 3rd, 4th 00:00
    assert [d["result"] for d in out] == ["missed"] * 3 and gw.opened == []
    assert m.slot_missed.value(agent="nightly-reviewer") == 3
    assert sch.Scheduler(gw, clock=lambda: at(4, 10), ledger=led, metrics=m).tick() == [], "each recorded once"
    assert m.slot_missed.value(agent="nightly-reviewer") == 3


def test_an_agent_added_later_has_no_earlier_slots_called_missed():
    led, m = tl.MemoryTriggers(), mx.TriggerMetrics()
    led.cursor(f"{sch.SOURCE}:other-reviewer", at(1, 12))      # another agent has been scheduled since the 1st
    gw = GW()                                                   # nightly-reviewer first appears on the 4th at 09:00
    assert sch.Scheduler(gw, clock=lambda: at(4, 9), ledger=led, metrics=m).tick() == []
    assert m.slot_missed.value(agent="nightly-reviewer") == 0
    assert [d["result"] for d in sch.Scheduler(gw, clock=lambda: at(5, 0, 1), ledger=led, metrics=m).tick()] == ["launched"]
