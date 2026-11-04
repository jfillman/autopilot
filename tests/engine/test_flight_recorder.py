"""A-4 (AP-C1, AP-C2): the flight recorder fields. Every allowed model call's outcome goes to the core with the model
that answered and its usage and charge; a run's records carry its pinned model version and its definition's Preflight
result; run.finish carries the run's cost; the session keeps its running cost. The charged tokens in the records sum
to what the proxy charged the session's budget."""
import json

import pytest
from starlette.testclient import TestClient

from autopilot.engine.modelproxy import usage_record
from autopilot.engine.server import create_model_app

from .test_run_identity import BODY, STREAM, g, launched  # noqa: F401 - g is the fixture


def outcomes(g, sid):
    return [r for r in g.core.records if r.get("session") == sid and r["tool"] == "model.call" and r.get("outcome_of")]


@pytest.fixture
def app(g, tmp_path):  # noqa: F811
    key = tmp_path / "key"
    key.write_text("sk-real\n")
    answers = []

    def post(url, k, version, body, timeout, betas=None):
        status, doc = answers.pop(0)
        return status, json.dumps(doc).encode()

    def stream(url, k, version, body, timeout, betas=None):
        return 200, iter(STREAM)
    return TestClient(create_model_app(g, key_file=str(key), post=post, stream=stream)), answers


def call(client, sid, **body):
    return client.post("/v1/messages", json={**BODY, **body}, headers={"x-api-key": "run-a", "x-hangar-session": sid})


def test_usage_record_is_every_kind_and_the_charge():
    assert usage_record({"input_tokens": 1000, "output_tokens": 200, "cache_creation_input_tokens": 400,
                         "cache_read_input_tokens": 5000}) == {
        "input_tokens": 1000, "output_tokens": 200, "cache_creation_input_tokens": 400,
        "cache_read_input_tokens": 5000, "charged_tokens": 1000 + 200 + 500 + 500}
    assert usage_record({"input_tokens": 3, "output_tokens": True, "x": 9, "cache_read_input_tokens": 1}) == {
        "input_tokens": 3, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 1,
        "charged_tokens": 4}
    assert usage_record(None)["charged_tokens"] == 0


def test_a_model_calls_outcome_carries_the_answering_model_and_what_was_charged(g, app):  # noqa: F811
    client, answers = app
    sid = launched(g)
    answers.append((200, {"id": "m1", "type": "message", "model": "claude-sonnet-5-5-20261015", "content": [],
                          "usage": {"input_tokens": 1000, "output_tokens": 50, "cache_read_input_tokens": 3000}}))
    assert call(client, sid).status_code == 200
    [out] = outcomes(g, sid)
    assert out["result"] == "ok" and out["model_version"] == "claude-sonnet-5-5-20261015"
    assert out["usage"] == {"input_tokens": 1000, "output_tokens": 50, "cache_creation_input_tokens": 0,
                            "cache_read_input_tokens": 3000, "charged_tokens": 1350}
    assert "usage" in out["claimed"] and "model_version" in out["claimed"]
    s = g.store.get(sid)
    assert s.used["model_tokens"] == 1350
    assert s.usage == {"input_tokens": 1000, "output_tokens": 50, "cache_creation_input_tokens": 0,
                       "cache_read_input_tokens": 3000, "charged_tokens": 1350, "model_calls": 1}


def test_a_stream_reports_its_model_and_usage_when_it_ends(g, app):  # noqa: F811
    client, _ = app
    sid = launched(g)
    r = call(client, sid, stream=True)
    assert r.status_code == 200 and r.content
    [out] = outcomes(g, sid)
    # message_start names no model in the fixture stream: the route's upstream model stands in.
    assert out["model_version"] == "claude-sonnet-5-5"
    assert out["usage"]["charged_tokens"] == 40 + 10 + 7 == g.store.get(sid).used["model_tokens"]


def test_a_failed_call_still_reports_an_outcome_and_charges_nothing(g, app):  # noqa: F811
    client, answers = app
    sid = launched(g)
    answers.append((529, {"type": "error", "error": {"type": "overloaded_error", "message": "busy"}}))
    assert call(client, sid).status_code == 529
    [out] = outcomes(g, sid)
    assert out["result"] == "error:Upstream529" and "usage" not in out
    assert g.store.get(sid).used["model_tokens"] == 0 and g.store.get(sid).usage["model_calls"] == 0


def test_the_records_charged_tokens_sum_to_the_sessions_charge(g, app):  # noqa: F811
    """What counts by=cost sums (each model.call outcome's usage.charged_tokens) is what the proxy charged."""
    client, answers = app
    sid = launched(g)
    for u in ({"input_tokens": 120, "output_tokens": 30}, {"input_tokens": 80, "cache_creation_input_tokens": 1000,
                                                           "output_tokens": 7}, {"cache_read_input_tokens": 9999}):
        answers.append((200, {"id": "m", "type": "message", "content": [], "usage": u}))
        assert call(client, sid).status_code == 200
    assert call(client, sid, stream=True).status_code == 200
    summed = sum(o["usage"]["charged_tokens"] for o in outcomes(g, sid))
    assert summed == g.store.get(sid).used["model_tokens"] == 150 + 1337 + 1000 + 57
    assert len(outcomes(g, sid)) == g.store.get(sid).usage["model_calls"] == 4


def test_a_runs_records_carry_the_pinned_model_preflight_and_cost(g, app, clock):  # noqa: F811
    client, answers = app
    sid = launched(g)
    [launch] = [r for r in g.core.records if r["tool"] == "run.launch" and r["session"] == sid]
    assert launch["model_version"] == "claude-sonnet-5-5"     # nightly-reviewer's claude-sonnet-5 through its route
    assert launch["preflight"] == {"result": "not-run", "definition": "nightly-reviewer"}
    answers.append((200, {"id": "m", "type": "message", "content": [], "usage": {"input_tokens": 100, "output_tokens": 20}}))
    call(client, sid)
    assert g.call("run-a", sid, "argo.app.get", {"app": "x"}).ok
    clock.advance(seconds=95)
    g.finish_run("run-a", sid, {"status": "done", "final_text": "ok", "turns": 1})
    [fin] = [r for r in g.core.records if r["tool"] == "run.finish" and r["session"] == sid]
    assert fin["model_version"] == "claude-sonnet-5-5" and fin["preflight"]["result"] == "not-run"
    assert fin["cost"] == {"charged_tokens": 120, "input_tokens": 100, "output_tokens": 20,
                           "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "model_calls": 1,
                           "tool_calls": 1, "wall_seconds": 95, "compute": "small"}
    assert {"cost", "preflight", "model_version"} <= set(fin["claimed"])
    # The session keeps its running cost for the read API.
    assert g.store.get(sid).to_record()["usage"]["charged_tokens"] == 120


def test_a_preflight_result_reaches_the_record(g):  # noqa: F811
    g.preflight = lambda d: {"result": "pass", "cases": 6} if d.name == "nightly-reviewer" else None
    sid = launched(g)
    [launch] = [r for r in g.core.records if r["tool"] == "run.launch" and r["session"] == sid]
    assert launch["preflight"] == {"result": "pass", "definition": "nightly-reviewer", "cases": 6}
    g.preflight = lambda d: {"result": "maybe"}
    assert g.preflight_of(g.defs["nightly-reviewer"], "nightly-reviewer") == {"result": "not-run",
                                                                              "definition": "nightly-reviewer"}


def test_the_session_usage_survives_a_reload():
    from datetime import datetime, timezone

    from autopilot.engine.limits import Limits
    from autopilot.engine.session import Session

    lim = Limits.from_dict({"tier_ceiling": "T1", "tools": [], "repos": [], "ttl_minutes": 5, "tool_calls": 1,
                            "github_calls": 0, "open_prs": 0, "model_tokens": 10, "children": 0, "models": [],
                            "network": "none", "compute": "small"})
    now = datetime(2026, 11, 3, tzinfo=timezone.utc)
    s = Session("s-1", "p", "a", lim, now, now)
    s.add_usage(usage_record({"input_tokens": 5}))
    back = Session.from_record(json.loads(json.dumps(s.to_record())))
    assert back.usage == s.usage and back.usage["model_calls"] == 1
    old = s.to_record()
    del old["usage"]                                   # a session saved before A-4
    assert Session.from_record(old).usage["charged_tokens"] == 0


def test_a_stream_that_breaks_is_not_an_ok_call(g, tmp_path):  # noqa: F811
    """A provider that drops the connection mid-stream: the outcome says so, and what was metered is charged."""
    key = tmp_path / "key"
    key.write_text("sk-real\n")

    def chunks():
        yield STREAM[0] + STREAM[1]
        raise ConnectionResetError("provider went away")
    app = create_model_app(g, key_file=str(key), post=lambda *a, **k: (500, b""), stream=lambda *a, **k: (200, chunks()))
    sid = launched(g)
    try:
        TestClient(app).post("/v1/messages", json={**BODY, "stream": True},
                             headers={"x-api-key": "run-a", "x-hangar-session": sid})
    except ConnectionResetError:
        pass
    [out] = outcomes(g, sid)
    assert out["result"] == "error:StreamInterrupted"
    assert out["usage"]["charged_tokens"] == g.store.get(sid).used["model_tokens"] > 0


def test_the_cost_names_the_sessions_granted_compute_class(g):  # noqa: F811
    from dataclasses import replace

    from autopilot.engine.limits import ComputeClass
    sid = launched(g)
    s = g.store.get(sid)
    s.limits = replace(s.limits, compute=ComputeClass.parse("small"))
    d = g.defs["nightly-reviewer"]
    g.defs["nightly-reviewer"] = replace(d, limits=replace(d.limits, compute=ComputeClass.parse("large")))
    assert g.run_cost(s, g.defs["nightly-reviewer"])["compute"] == "small"


def test_an_overrun_trips_the_breaker_and_the_record_keeps_the_whole_charge(g, app):  # noqa: F811
    """The budget counts a charge up to its limit (remaining never goes negative); the outcome and the session's
    running cost keep what the call cost, so the cost counts never under-report a run that overran."""
    client, answers = app
    sid = launched(g)
    s = g.store.get(sid)
    room = s.remaining("model_tokens")
    answers.append((200, {"id": "m", "type": "message", "content": [], "usage": {"input_tokens": room + 5000}}))
    assert call(client, sid, max_tokens=1).status_code == 200
    [out] = outcomes(g, sid)
    assert out["usage"]["charged_tokens"] == room + 5000 == s.usage["charged_tokens"]
    assert s.used["model_tokens"] == room and s.remaining("model_tokens") == 0
    assert s.tripped and s.trip_reason == "model-token overrun"
