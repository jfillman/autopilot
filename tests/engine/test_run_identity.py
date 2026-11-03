"""A run acts as its session: when the RunBackend names the identity a run's workload authenticates as, that caller
owns the session (and only that session); records still name the caller. The model proxy's decisions go through the
same binding, on the Gateway's store and chain."""
import json

import pytest
from starlette.testclient import TestClient

from clearance.audit import AuditLog
from clearance.backends import FakeRunBackend, core_handlers
from clearance.dispatch import AuthError, Gateway, Principal
from clearance.modelproxy import ModelRoute
from clearance.packs.hangar import HANGAR
from clearance.server import create_model_app
from clearance.session import SessionStore
from clearance.tiers import Tier



class StaticAuth:
    def __init__(self, table):
        self.table = table

    def authenticate(self, token):
        if token not in self.table:
            raise AuthError("unknown token")
        return self.table[token]


class Runs(FakeRunBackend):
    def principal(self, session):
        return f"agent-{session.run}/run" if session.run else None


ROUTES = {"claude-sonnet-5": ModelRoute("claude-sonnet-5", "anthropic", "https://api.example", "claude-sonnet-5-5")}


@pytest.fixture
def g(defs, backends, clock):
    counter = iter(range(1, 1000))
    auth = StaticAuth({
        "sched": Principal("autopilot-runs/nightly-reviewer", "workload", Tier.T1),
        "run-a": Principal("agent-r-000001/run", "workload", Tier.T1),
        "run-b": Principal("agent-r-000002/run", "workload", Tier.T1),
        "alice": Principal("user:alice", "user", Tier.T1),
    })
    return Gateway(defs, SessionStore(id_gen=lambda: f"s-{next(counter):06x}"), AuditLog(), auth, clock,
                   packs=(HANGAR,), handlers=core_handlers(argo=backends.argo, read=backends.read), runs=Runs(),
                   models=ROUTES, model_auth=auth)


def launched(g):
    sid = g.open_triggered("nightly-reviewer", "test").data["session"]
    assert g.launch(sid).ok
    return sid


def test_the_run_acts_as_its_session_and_the_record_names_the_run(g):
    sid = launched(g)
    r = g.call("run-a", sid, "argo.app.get", {"app": "x"})
    assert r.ok, r.as_dict()
    assert g.audit.records[-1]["principal"] == "agent-r-000001/run"


def test_another_run_or_person_does_not(g):
    sid = launched(g)
    launched(g)  # r-000002 exists, for another session
    for who in ("run-b", "alice"):
        r = g.call(who, sid, "argo.app.get", {"app": "x"})
        assert not r.ok and r.decision["rule"] == "R001", (who, r.as_dict())


def test_without_a_run_the_run_identity_owns_nothing(g):
    sid = g.open_triggered("nightly-reviewer", "test").data["session"]   # not launched
    assert g.call("run-a", sid, "argo.app.get", {"app": "x"}).decision["rule"] == "R001"


def test_a_backend_without_principal_keeps_the_old_rule(defs, clock):
    auth = StaticAuth({"run-a": Principal("agent-r-000001/run", "workload", Tier.T1)})
    gw = Gateway(defs, SessionStore(id_gen=lambda: "s-000001"), AuditLog(), auth, clock, packs=(HANGAR,),
                 runs=FakeRunBackend())
    sid = gw.open_triggered("nightly-reviewer", "t").data["session"]
    gw.launch(sid)
    assert gw.call("run-a", sid, "argo.app.get", {"app": "x"}).decision["rule"] == "R001"


def test_model_calls_bind_the_same_way_and_are_charged(g):
    sid = launched(g)
    d = g.model_authorize("run-a", sid, "claude-sonnet-5", 100)
    assert d.allow and d.route.upstream_model == "claude-sonnet-5-5"
    assert g.audit.records[-1]["tool"] == "model.call" and g.audit.records[-1]["principal"] == "agent-r-000001/run"
    assert g.model_authorize("run-b", sid, "claude-sonnet-5", 100).rule == "M002"
    g.model_usage(sid, 1234)
    assert g.store.get(sid).used["model_tokens"] == 1234


def test_no_routes_no_model_calls(gw):
    sid = gw.open_session("alice", "coding-agent").data["session"]
    assert gw.model_authorize("alice", sid, "claude-sonnet-5", 1).rule == "M006"


# -- the HTTP forwarder ---------------------------------------------------------------------------------------------
@pytest.fixture
def proxy(g, tmp_path):
    key = tmp_path / "key"
    key.write_text("sk-real\n")
    sent = []

    def post(url, k, version, body, timeout, betas=None):
        sent.append({"url": url, "key": k, "version": version, "body": json.loads(body), "betas": betas})
        return 200, json.dumps({"id": "msg_1", "type": "message", "content": [{"type": "text", "text": "hi"}],
                                "usage": {"input_tokens": 40, "output_tokens": 2}}).encode()

    def stream(url, k, version, body, timeout, betas=None):
        sent.append({"url": url, "key": k, "version": version, "body": json.loads(body), "betas": betas, "stream": True})
        return 200, iter(STREAM)
    return TestClient(create_model_app(g, key_file=str(key), post=post, stream=stream)), sent


# A streaming answer as the provider sends it, cut mid-line across chunks the way a socket may.
_EVENTS = (
    'event: message_start\ndata: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":40,'
    '"cache_read_input_tokens":100,"output_tokens":1}}}\n\n'
    'event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}\n\n'
    'event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}\n\n'
    'event: message_stop\ndata: {"type":"message_stop"}\n\n').encode()
STREAM = [_EVENTS[:57], _EVENTS[57:200], _EVENTS[200:]]


BODY = {"model": "claude-sonnet-5", "max_tokens": 64, "messages": [{"role": "user", "content": "hi"}]}


def test_forwarded_with_the_real_key_and_upstream_model_and_charged(g, proxy):
    client, sent = proxy
    sid = launched(g)
    r = client.post("/v1/messages", json=BODY, headers={"x-api-key": "run-a", "x-hangar-session": sid,
                                                         "anthropic-beta": "x", "cookie": "c"})
    assert r.status_code == 200 and r.json()["content"][0]["text"] == "hi"
    assert sent[0]["url"] == "https://api.example/v1/messages" and sent[0]["key"] == "sk-real"
    assert sent[0]["body"]["model"] == "claude-sonnet-5-5" and sent[0]["version"] == "2023-06-01"
    assert sent[0]["betas"] == "x", "anthropic-beta is the one client header forwarded"
    assert g.store.get(sid).used["model_tokens"] == 42
    assert "sk-real" not in json.dumps(g.audit.records)


@pytest.mark.parametrize("headers, body, status, rule", [
    ({}, BODY, 401, None),
    ({"x-api-key": "nobody"}, BODY, 401, None),
    ({"x-api-key": "run-b"}, BODY, 403, "M002"),
    ({"x-api-key": "run-a"}, {**BODY, "model": "gpt-9"}, 403, "M005"),
    ({"x-api-key": "run-a"}, {**BODY, "max_tokens": 10**9}, 429, "M007"),
    ({"x-api-key": "run-a"}, {**BODY, "stream": True, "max_tokens": 10**9}, 429, "M007"),
])
def test_refusals_never_reach_the_provider(g, proxy, headers, body, status, rule):
    client, sent = proxy
    sid = launched(g)
    if headers:
        headers = {**headers, "x-hangar-session": sid}
    r = client.post("/v1/messages", json=body, headers=headers)
    assert r.status_code == status, r.text
    assert r.json()["type"] == "error" and r.json()["error"].get("rule") == rule
    assert sent == []


def test_the_run_sees_its_sessions_tools_and_another_run_does_not(g):
    sid = launched(g)
    assert g.session_tools("run-a", sid) == ["argo.app.get", "catalog.read", "metrics.query"]
    assert g.session_tools("run-b", sid) is None


def test_a_stream_is_relayed_as_it_comes_and_its_usage_charged(g, proxy):
    client, sent = proxy
    sid = launched(g)
    r = client.post("/v1/messages", json={**BODY, "stream": True},
                    headers={"x-api-key": "run-a", "x-hangar-session": sid, "anthropic-beta": "oauth-2025-04-20, fine-1"})
    assert r.status_code == 200 and r.headers["content-type"].startswith("text/event-stream")
    assert r.content == _EVENTS, "the provider's bytes, unchanged"
    assert sent[0]["stream"] and sent[0]["body"]["model"] == "claude-sonnet-5-5" and sent[0]["betas"] == "fine-1"
    assert g.store.get(sid).used["model_tokens"] == 40 + 10 + 7     # input + a tenth of the cache read + output


def test_a_streaming_error_from_the_provider_comes_back_as_json(g, tmp_path):
    key = tmp_path / "key"
    key.write_text("sk-real\n")
    err = json.dumps({"type": "error", "error": {"type": "overloaded_error", "message": "busy"}}).encode()
    app = create_model_app(g, key_file=str(key), post=lambda *a, **k: (500, b""), stream=lambda *a, **k: (529, iter([err])))
    sid = launched(g)
    r = TestClient(app).post("/v1/messages", json={**BODY, "stream": True}, headers={"x-api-key": "run-a", "x-hangar-session": sid})
    assert r.status_code == 529 and r.json()["error"]["type"] == "overloaded_error"
    assert g.store.get(sid).used["model_tokens"] == 0
