import pytest

from clearance.limits import Claim, Network
from clearance.modelproxy import ModelProxy, ModelRoute

ROUTES = {"claude-sonnet-5": ModelRoute("claude-sonnet-5", "anthropic", "https://api.anthropic.com", "claude-sonnet-5"),
          "qwen": ModelRoute("qwen", "modelplane", "http://gw.inference.svc", "qwen/qwen3-8b")}


@pytest.fixture
def mp(gw, clock):
    return ModelProxy(ROUTES, gw.store, gw.audit, clock)


def sess(gw, claim=None, agent="coding-agent"):
    return gw.open_session("alice", agent, claim).data["session"]


def test_allowed_call_names_the_route_and_carries_the_session_as_caller(gw, mp):
    sid = sess(gw)
    d = mp.authorize(sid, "user:alice", "claude-sonnet-5", 1000)
    assert d.allow and d.route.backend == "anthropic"
    assert d.headers["x-modelplane-caller"] == sid and d.headers["x-hangar-task"].startswith("t-")


@pytest.mark.parametrize("model,est,rule", [
    ("gpt-9", 10, "M005"), ("claude-sonnet-5", 10**9, "M007"), ("claude-sonnet-5", -1, "M007"),
])
def test_denials(gw, mp, model, est, rule):
    sid = sess(gw)
    assert mp.authorize(sid, "user:alice", model, est).rule == rule


def test_wrong_principal_unknown_session_and_tripped(gw, mp):
    sid = sess(gw)
    assert mp.authorize(sid, "user:bob", "claude-sonnet-5", 1).rule == "M002"
    assert mp.authorize("s-none", "user:alice", "claude-sonnet-5", 1).rule == "M001"
    gw.store.trip(sid, "x")
    assert mp.authorize(sid, "user:alice", "claude-sonnet-5", 1).rule == "M003"


def test_network_mode_without_the_model_proxy_is_denied(gw, mp):
    sid = sess(gw, {"network": "clearance"})
    assert mp.authorize(sid, "user:alice", "claude-sonnet-5", 1).rule == "M004"


def test_model_not_routed_is_denied(gw, mp):
    sid = sess(gw)
    gw.store.get(sid).limits = gw.store.get(sid).limits.__class__(**{**gw.store.get(sid).limits.__dict__,
                                                                    "models": frozenset({"ghost"})})
    assert mp.authorize(sid, "user:alice", "ghost", 1).rule == "M006"


def test_usage_is_charged_and_overrun_trips_the_breaker(gw, mp):
    sid = sess(gw, {"model_tokens": 1000})
    s = gw.store.get(sid)
    mp.record_usage(sid, 400)
    assert s.remaining("model_tokens") == 600
    mp.record_usage(sid, 900)          # actual use exceeded what was left
    assert s.remaining("model_tokens") == 0 and s.tripped


def test_every_model_call_is_audited(gw, mp):
    sid = sess(gw)
    n = len(gw.audit.records)
    mp.authorize(sid, "user:alice", "claude-sonnet-5", 5)
    mp.authorize(sid, "user:alice", "gpt-9", 5)
    recs = gw.audit.records[n:]
    assert [r["decision"] for r in recs] == ["allow", "deny"] and all(r["tool"] == "model.call" for r in recs)
