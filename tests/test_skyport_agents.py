"""The Skyport AI workloads: one definition per workload shape, and the behaviours that make each
shape safe (event dedupe and rate brake, allowlisted APIs, chat only in sessions, narrower teams)."""
from datetime import timedelta
from pathlib import Path

import pytest

from clearance import profile, triggers
from clearance.audit import verify
from clearance.backends import fake_backends
from clearance.dispatch import Gateway, Principal
from clearance.audit import AuditLog
from clearance.session import SessionStore
from clearance.tiers import Tier

from conftest import T0, Clock, StaticAuth

ROOT = Path(__file__).resolve().parents[1]
SKY = profile.load_dir(ROOT / "agents" / "skyport")


@pytest.fixture
def sgw():
    clock = Clock()
    n = iter(range(1, 999))
    store = SessionStore(id_gen=lambda: f"s-{next(n):06x}")
    auth = StaticAuth({
        "gate-agent": Principal("user:gate-agent", "user", Tier.T1),
        "responder": Principal("autopilot-runs/disruption-responder", "workload", Tier.T1),
        "assistant": Principal("app-passenger-assistant-dev/passenger-assistant", "workload", Tier.T1),
    })
    b = fake_backends()
    return Gateway(SKY, store, AuditLog(), b, auth, clock), b, clock


def test_all_six_shapes_are_represented():
    assert {d.kind for d in SKY.values()} == {"task", "session", "service", "scheduled", "event", "team"}


def test_the_six_headline_workloads_and_the_team_members_exist():
    assert {"flight-briefer", "gate-copilot", "passenger-assistant", "delay-digest",
            "disruption-responder", "irregular-ops-team"} <= set(SKY)
    assert {"ops-researcher", "ops-drafter", "ops-checker"} <= set(SKY)


def test_only_the_service_agent_needs_the_hardened_sandbox_and_it_is_public_facing():
    assert SKY["passenger-assistant"].sandbox == "hardened"
    assert all(d.sandbox == "standard" for n, d in SKY.items() if n != "passenger-assistant")


def test_nothing_in_the_demo_can_apply_a_change_except_by_spawning_narrower_runs():
    for n, d in SKY.items():
        assert not ({"repo.pr.open", "repo.pr.open_upper", "xr.request", "argo.sync.lower", "pipeline.rerun"} & d.limits.tools), n
        assert d.limits.github_calls == 0 and d.limits.open_prs == 0, n


def test_business_agents_read_only_the_apis_they_need():
    assert set(SKY["passenger-assistant"].api_allow) == {"boarding-api", "flight-api"}
    assert SKY["delay-digest"].api_allow == ("flight-api",)


# ---- event: the trigger bridge -------------------------------------------------------
@pytest.mark.parametrize("pattern,key,ok", [
    ("flight.*.delayed", "flight.AC123.delayed", True),
    ("flight.*.delayed", "flight.AC123.gate_changed", False),
    ("flight.*.delayed", "flight.AC123.extra.delayed", False),
    ("flight.#", "flight.AC123.delayed", True),
    ("flight.#", "flight", True),
    ("#", "anything.at.all", True),
    ("flight.*", "flight", False),
    ("a.#.z", "a.b.c.z", True),
    ("a.#.z", "a.z", True),
])
def test_amqp_topic_matching(pattern, key, ok):
    assert triggers.amqp_topic_match(pattern, key) is ok


def test_only_the_responder_binds_delayed_events():
    assert triggers.match_event(SKY, "flights.events", "flight.AC123.delayed") == ["disruption-responder"]
    assert triggers.match_event(SKY, "flights.events", "flight.AC123.gate_changed") == []
    assert triggers.match_event(SKY, "other.exchange", "flight.AC123.delayed") == []


def test_the_same_message_maps_to_the_same_task_id_and_different_messages_do_not():
    a = triggers.event_task_id("disruption-responder", "msg-1")
    assert a == triggers.event_task_id("disruption-responder", "msg-1")
    assert a != triggers.event_task_id("disruption-responder", "msg-2")
    assert a != triggers.event_task_id("delay-digest", "msg-1")
    assert a.startswith("t-")


def test_a_redelivered_event_starts_no_second_run(sgw):
    gw, b, clock = sgw
    tid = triggers.event_task_id("disruption-responder", "msg-1")
    r1 = gw.open_triggered("disruption-responder", "amqp:flights.events", task_id=tid)
    r2 = gw.open_triggered("disruption-responder", "amqp:flights.events", task_id=tid)
    assert r1.data["session"] == r2.data["session"] and r2.data["deduped"] is True and r1.data["deduped"] is False
    r3 = gw.open_triggered("disruption-responder", "amqp:flights.events", task_id=triggers.event_task_id("disruption-responder", "msg-2"))
    assert r3.data["session"] != r1.data["session"]
    assert verify(gw.audit.records) == (True, None)


def test_a_finished_run_does_not_block_a_genuine_retry_after_it_expires(sgw):
    gw, b, clock = sgw
    tid = triggers.event_task_id("disruption-responder", "msg-1")
    r1 = gw.open_triggered("disruption-responder", "x", task_id=tid)
    clock.advance(minutes=21)
    r2 = gw.open_triggered("disruption-responder", "x", task_id=tid)
    assert r2.data["session"] != r1.data["session"]


def test_the_storm_brake_allows_max_per_hour_then_recovers():
    rl = triggers.RateLimiter(3)
    assert [rl.allow(T0 + timedelta(minutes=i)) for i in range(4)] == [True, True, True, False]
    assert rl.allow(T0 + timedelta(minutes=61)) is True


def test_the_responders_own_brake_is_declared_in_its_definition():
    (t,) = SKY["disruption-responder"].triggers
    assert t["maxPerHour"] == 12 and t["bindingKey"] == "flight.*.delayed"


# ---- api allowlist and chat -----------------------------------------------------------
def test_app_api_reads_are_allowlisted_by_service(sgw):
    gw, b, _ = sgw
    sid = gw.open_session("gate-agent", "flight-briefer").data["session"]
    ok = gw.call("gate-agent", sid, "app.api.get", {"service": "flight-api", "path": "/api/flights/AC123"})
    assert ok.ok and b.read.names() == ["read.app_api"]
    bad = gw.call("gate-agent", sid, "app.api.get", {"service": "billing-api", "path": "/x"})
    assert not bad.ok and bad.decision["rule"] == "R017" and b.read.names() == ["read.app_api"]


@pytest.mark.parametrize("path", ["api/flights", "/a/../etc", "/a?x=1", "/a#frag"])
def test_app_api_paths_must_be_plain(sgw, path):
    gw, b, _ = sgw
    sid = gw.open_session("gate-agent", "flight-briefer").data["session"]
    r = gw.call("gate-agent", sid, "app.api.get", {"service": "flight-api", "path": path})
    assert not r.ok and r.decision["rule"] == "R008"


def test_chat_exists_only_for_session_agents(sgw):
    gw, b, _ = sgw
    task = gw.open_session("gate-agent", "flight-briefer").data["session"]
    assert gw.call("gate-agent", task, "chat.send", {"text": "hi"}).decision["rule"] in ("R006", "R018")
    sess = gw.open_session("gate-agent", "gate-copilot").data["session"]
    assert gw.call("gate-agent", sess, "chat.send", {"text": "Gate B12"}).ok
    assert gw.call("gate-agent", sess, "chat.recv", {}).ok
    assert b.human.names() == ["human.send", "human.recv"]


def test_the_public_service_agent_cannot_reach_write_tools_or_other_apis(sgw):
    gw, b, _ = sgw
    sid = gw.open_session("assistant", "passenger-assistant").data["session"]
    assert gw.call("assistant", sid, "run.spawn", {"agent": "ops-checker", "limits": {}}).decision["rule"] == "R006"
    assert gw.call("assistant", sid, "app.api.get", {"service": "baggage-api", "path": "/bags"}).decision["rule"] == "R017"
    assert gw.call("assistant", sid, "k8s.exec", {}).decision["rule"] == "R002"
    assert gw.call("assistant", sid, "app.api.get", {"service": "flight-api", "path": "/x"}).decision["rule"] == "R005", \
        "one tripwire attempt ends the session"


# ---- team: responder -> team -> workers -------------------------------------------------
def test_the_hand_off_chain_narrows_at_every_step(sgw):
    gw, b, _ = sgw
    root = gw.open_triggered("disruption-responder", "amqp", task_id="t-abc123abc123")
    rid = root.data["session"]
    team = gw.call("responder", rid, "run.spawn", {"agent": "irregular-ops-team", "limits": {"tool_calls": 60, "model_tokens": 200000}})
    assert team.ok, team.as_dict()
    tid = team.data["session"]
    assert gw.store.get(tid).limits.tool_calls == 60 and gw.store.get(tid).task_id == "t-abc123abc123"
    worker = gw.call("responder", tid, "run.spawn", {"agent": "ops-researcher", "limits": {"tool_calls": 20}})
    # the team session is owned by the responder principal because the parent's principal is inherited
    assert worker.ok, worker.as_dict()
    w = gw.store.get(worker.data["session"])
    assert w.depth == 2 and w.limits.tier_ceiling <= gw.store.get(tid).limits.tier_ceiling
    assert w.limits.tool_calls <= 20
    assert w.limits.tools == {"app.api.get", "metrics.query", "artifact.put"}
    assert w.limits.tools <= gw.store.get(tid).limits.tools <= SKY["disruption-responder"].limits.tools


def test_a_worker_cannot_be_told_to_exceed_its_parent(sgw):
    gw, b, _ = sgw
    root = gw.open_triggered("disruption-responder", "amqp", task_id="t-abc123abc124")
    team = gw.call("responder", root.data["session"], "run.spawn", {"agent": "irregular-ops-team", "limits": {"tool_calls": 60}})
    over = gw.call("responder", team.data["session"], "run.spawn", {"agent": "ops-researcher", "limits": {"tool_calls": 200}})
    assert not over.ok and over.decision["rule"] == "R007"
    assert len(b.runs.manifests) == 1, "only the team was created; the over-broad worker never existed"


def test_the_responder_can_start_only_one_team(sgw):
    gw, b, _ = sgw
    root = gw.open_triggered("disruption-responder", "amqp", task_id="t-abc123abc125")
    assert gw.call("responder", root.data["session"], "run.spawn", {"agent": "irregular-ops-team", "limits": {"tool_calls": 40}}).ok
    second = gw.call("responder", root.data["session"], "run.spawn", {"agent": "irregular-ops-team", "limits": {"tool_calls": 40}})
    assert not second.ok and second.decision["rule"] == "R016"


def test_scheduled_and_event_agents_can_be_triggered_but_session_and_task_agents_cannot(sgw):
    gw, b, _ = sgw
    assert gw.open_triggered("delay-digest", "cron").ok
    assert gw.open_triggered("disruption-responder", "amqp").ok
    for n in ("gate-copilot", "flight-briefer", "irregular-ops-team"):
        assert not gw.open_triggered(n, "cron").ok, n


def test_the_service_agent_can_be_started_by_its_own_workload_identity(sgw):
    gw, b, _ = sgw
    assert gw.open_triggered("passenger-assistant", "deploy").ok      # kind: service is allowed
    assert gw.open_session("assistant", "passenger-assistant").ok


def test_the_digest_is_due_daily_and_only_the_digest():
    assert triggers.due(SKY, {}, T0) == ["delay-digest"]
    assert triggers.due(SKY, {"delay-digest": T0 - timedelta(hours=23)}, T0) == []


# ---- outputs leave the sandbox through Clearance, and triggered runs get an AgentRun XR ------------------
def test_a_run_stores_its_output_under_its_own_task_and_its_team_can_read_it(sgw):
    gw, b, _ = sgw
    root = gw.open_triggered("disruption-responder", "amqp", task_id="t-aaaaaaaaaaaa")
    rid = root.data["session"]
    assert gw.call("responder", rid, "artifact.put", {"name": "draft.md", "content": "hello"}).ok
    team = gw.call("responder", rid, "run.spawn", {"agent": "irregular-ops-team", "limits": {"tool_calls": 40}})
    got = gw.call("responder", team.data["session"], "artifact.get", {"name": "draft.md"})
    assert got.ok and got.data == "hello", "one task id spans the tree, so the team reads the parent's artifact"
    assert gw.call("responder", rid, "artifact.get", {"name": "nope"}).data is None


def test_an_oversized_artifact_is_denied_and_never_stored(sgw):
    gw, b, _ = sgw
    sid = gw.open_triggered("delay-digest", "cron").data["session"]
    from clearance.dispatch import Principal
    gw.auth.table["delay"] = Principal("autopilot-runs/delay-digest", "workload", Tier.T1)
    big = gw.call("delay", sid, "artifact.put", {"name": "x", "content": "a" * (1024 * 1024 + 1)})
    assert not big.ok and big.decision["rule"] == "R019" and b.artifacts.store == {}


def test_a_triggered_top_level_run_gets_an_agentrun_xr_that_satisfies_the_xrd(sgw):
    import jsonschema, yaml
    gw, b, _ = sgw
    sid = gw.open_triggered("delay-digest", "cron").data["session"]
    r = gw.launch(sid, run_input={"day": "2026-09-25"})
    assert r.ok and list(b.runs.manifests) == [r.data["run"]]
    m = b.runs.manifests[r.data["run"]]
    xrd = yaml.safe_load((ROOT / "airframe-drafts" / "xrds" / "agentrun.yaml").read_text())
    jsonschema.validate({"spec": m["spec"]}, xrd["spec"]["versions"][0]["schema"]["openAPIV3Schema"])
    assert m["spec"]["input"] == {"day": "2026-09-25"} and m["spec"]["taskId"].startswith("t-")


def test_service_agents_are_never_launched_as_runs(sgw):
    gw, b, _ = sgw
    sid = gw.open_triggered("passenger-assistant", "deploy").data["session"]
    r = gw.launch(sid)
    assert not r.ok and "deployed as applications" in r.error and b.runs.manifests == {}


def test_a_parent_must_hold_every_tool_it_delegates(sgw):
    """A child's tools are its own list intersected with its parent's, so delegating cannot smuggle a tool in."""
    gw, b, _ = sgw
    root = gw.open_triggered("disruption-responder", "amqp", task_id="t-bbbbbbbbbbbb")
    team = gw.call("responder", root.data["session"], "run.spawn", {"agent": "irregular-ops-team", "limits": {"tool_calls": 40, "tools": ["app.api.get", "run.spawn"]}})
    assert team.ok
    kid = gw.call("responder", team.data["session"], "run.spawn", {"agent": "ops-researcher", "limits": {"tool_calls": 10}})
    assert gw.store.get(kid.data["session"]).limits.tools == {"app.api.get"}, "the team narrowed itself, so its worker lost metrics and artifacts too"
