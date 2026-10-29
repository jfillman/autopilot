"""The agent runtime on the wire: its Clearance client against Clearance's real streamable-HTTP app, its model client
against a fake model proxy that records every request, and main() end to end over real sockets.

What these pin: the paths and headers each side is sent, that both projected tokens are read again per request,
and that neither token reaches stdout or result.json.
"""
import json
import threading
import time
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
from starlette.testclient import TestClient

from autopilot.backends import fake_backends
from autopilot.gateway import hangar_gateway
from clearance import profile
from clearance.audit import AuditLog
from clearance.dispatch import AuthError, Principal
from clearance.server import create_http_app
from clearance.session import SessionStore
from clearance.tiers import Tier
from conftest import ROOT
from hangar_agent import main as runtime_main
from hangar_agent.clearance import Clearance, ClearanceError
from hangar_agent.model import ModelProxy, ModelStop

ALICE = Principal("user:alice", "user", Tier.T1)
TOKENS = ("clr-token-first-0001", "clr-token-second-0002")
MODEL_TOKENS = ("mp-token-first-0001", "mp-token-second-0002")


class TokenTable:
    """Two tokens for one caller, as before and after the kubelet rotates the projected token; any other is refused
    the way Clearance's authenticators refuse one (AuthError, so the HTTP app answers 401)."""

    def authenticate(self, token):
        if token not in TOKENS:
            raise AuthError("unknown token")
        return ALICE


@pytest.fixture
def gateway():
    """The Hangar Gateway with fake backends and the real clock."""
    gw = hangar_gateway(profile.load_dir(ROOT / "agents"), SessionStore(), AuditLog(), fake_backends(),
                        TokenTable(), lambda: datetime.now(timezone.utc))
    gw.sid = gw.open_session(TOKENS[0], "researcher").data["session"]
    return gw


def app_of(gw):
    return create_http_app(gw, resource_url="http://clearance.test/mcp", issuer_url="http://clearance.test")


@pytest.fixture
def tokens(tmp_path):
    d = tmp_path / "hangar"
    d.mkdir()
    (d / "clearance-token").write_text(TOKENS[0] + "\n")
    (d / "model-proxy-token").write_text(MODEL_TOKENS[0] + "\n")
    return d


# -- Clearance, against the real app ------------------------------------------------------------------------------
def test_clearance_client_speaks_to_the_real_gateway_and_rereads_its_token(gateway, tokens):
    seen = []
    with TestClient(app_of(gateway)) as http:
        def post(url, headers, body, timeout):
            seen.append((url, dict(headers)))
            r = http.post(url.removeprefix("http://clearance.autopilot-system.svc:8080"), headers=headers,
                          content=body)
            return r.status_code, r.content

        c = Clearance("http://clearance.autopilot-system.svc:8080/", tokens / "clearance-token", gateway.sid, post=post)
        registry = c.tools_list()
        assert {"name", "tier", "description", "required_args", "propose_only"} <= set(registry[0])
        assert "catalog.read" in {t["name"] for t in registry}
        ok = c.call("catalog.read", {"query": "flight-api"})
        (tokens / "clearance-token").write_text(TOKENS[1])     # the kubelet rotates the projected token
        denied = c.call("repo.pr.open", {"repo": "jfillman/x", "branch": "b", "title": "t", "files": []})
    assert ok["ok"] is True and ok["session"] == gateway.sid
    assert denied["ok"] is False and denied["decision"]["rule"] == "R006" and denied["decision"]["hint"]
    assert all(u == "http://clearance.autopilot-system.svc:8080/mcp" for u, _ in seen)
    h = seen[0][1]
    assert h["Content-Type"] == "application/json" and "application/json" in h["Accept"]
    assert h["MCP-Protocol-Version"] == "2025-06-18"
    assert [s[1]["Authorization"] for s in seen] == [f"Bearer {TOKENS[0]}"] * 2 + [f"Bearer {TOKENS[1]}"]
    assert [r["tool"] for r in gateway.audit.records][-2:] == ["catalog.read", "repo.pr.open"]


def test_clearance_client_never_opens_a_session(gateway, tokens):
    bodies = []
    with TestClient(app_of(gateway)) as http:
        def post(url, headers, body, timeout):
            bodies.append(json.loads(body))
            r = http.post("/mcp", headers=headers, content=body)
            return r.status_code, r.content
        c = Clearance("http://x", tokens / "clearance-token", gateway.sid, post=post)
        c.tools_list()
        c.call("catalog.read", {"query": "q"})
    assert [b["params"]["name"] for b in bodies] == ["tools_list", "call"]
    assert all(b["method"] == "tools/call" for b in bodies)


def test_a_rejected_token_is_an_error_that_does_not_carry_it(gateway, tokens):
    (tokens / "clearance-token").write_text("not-a-known-token-xyz")
    with TestClient(app_of(gateway)) as http:
        def post(url, headers, body, timeout):
            r = http.post("/mcp", headers=headers, content=body)
            return r.status_code, r.content
        with pytest.raises(ClearanceError) as e:
            Clearance("http://x", tokens / "clearance-token", gateway.sid, post=post).tools_list()
    assert "401" in str(e.value) and "not-a-known-token-xyz" not in str(e.value)


def test_a_connection_that_cannot_be_made_is_retried_once_and_a_read_timeout_is_not(tokens):
    import urllib.error
    attempts = []

    def refused(url, headers, body, timeout):
        attempts.append(1)
        raise urllib.error.URLError(ConnectionRefusedError())

    with pytest.raises(ClearanceError):
        Clearance("http://x", tokens / "clearance-token", "s-1", post=refused, sleep=lambda s: None).tools_list()
    assert len(attempts) == 2
    attempts.clear()

    def timed_out(url, headers, body, timeout):
        attempts.append(1)
        raise TimeoutError()

    with pytest.raises(ClearanceError):
        Clearance("http://x", tokens / "clearance-token", "s-1", post=timed_out, sleep=lambda s: None).call("a", {})
    assert len(attempts) == 1


# -- the model proxy ----------------------------------------------------------------------------------------------
def message(content, stop="end_turn"):
    return {"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5", "content": content,
            "stop_reason": stop, "stop_sequence": None, "usage": {"input_tokens": 10, "output_tokens": 5}}


def error(kind, msg):
    return {"type": "error", "error": {"type": kind, "message": msg}}


class FakeProxy:
    """A model proxy on a real socket: answers from a script of (status, body) and records path, headers and body."""

    def __init__(self, *script):
        self.script, self.requests = list(script), []
        proxy = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                proxy.requests.append({"path": self.path, "headers": {k.lower(): v for k, v in self.headers.items()},
                                       "body": body})
                status, answer = proxy.script.pop(0)
                raw = json.dumps(answer).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def log_message(self, *a):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


@pytest.fixture
def proxy():
    made = []

    def make(*script):
        made.append(FakeProxy(*script))
        return made[-1]
    yield make
    for p in made:
        p.close()


def test_model_requests_carry_the_token_as_x_api_key_and_the_session_and_reread_it(proxy, tokens):
    p = proxy((200, message([{"type": "text", "text": "one"}])), (200, message([{"type": "text", "text": "two"}])))
    m = ModelProxy(p.url + "/", tokens / "model-proxy-token", "s-abc")
    first = m.create(model="claude-sonnet-5-5", max_tokens=16, messages=[{"role": "user", "content": "hi"}])
    (tokens / "model-proxy-token").write_text(MODEL_TOKENS[1])
    m.create(model="claude-sonnet-5-5", max_tokens=16, messages=[{"role": "user", "content": "hi"}])
    assert first["content"] == [{"type": "text", "text": "one"}] and first["stop_reason"] == "end_turn"
    assert [r["path"] for r in p.requests] == ["/v1/messages", "/v1/messages"]
    h = p.requests[0]["headers"]
    assert h["x-api-key"] == MODEL_TOKENS[0] and h["x-hangar-session"] == "s-abc" and "authorization" not in h
    assert p.requests[1]["headers"]["x-api-key"] == MODEL_TOKENS[1]
    assert p.requests[0]["body"]["model"] == "claude-sonnet-5-5" and "stream" not in p.requests[0]["body"]


@pytest.mark.parametrize("status,kind", [(429, "rate_limit_error"), (403, "permission_error")])
def test_a_429_or_403_stops_at_once_without_a_retry(proxy, tokens, status, kind):
    p = proxy((status, error(kind, "model budget spent for this session")))
    m = ModelProxy(p.url, tokens / "model-proxy-token", "s-abc", sleep=lambda s: None)
    with pytest.raises(ModelStop) as e:
        m.create(model="claude-sonnet-5-5", max_tokens=16, messages=[{"role": "user", "content": "hi"}])
    assert e.value.status == status and kind in str(e.value) and len(p.requests) == 1


def test_a_5xx_is_retried(proxy, tokens):
    p = proxy((529, error("overloaded_error", "busy")), (200, message([{"type": "text", "text": "ok"}])))
    m = ModelProxy(p.url, tokens / "model-proxy-token", "s-abc", sleep=lambda s: None)
    r = m.create(model="m", max_tokens=16, messages=[{"role": "user", "content": "hi"}])
    assert r["content"][0]["text"] == "ok"
    assert len(p.requests) == 2


# -- main(), end to end -------------------------------------------------------------------------------------------
@pytest.fixture
def served(gateway):
    """Clearance's real app on a real socket (uvicorn), as the pod reaches it."""
    import uvicorn
    server = uvicorn.Server(uvicorn.Config(app_of(gateway), host="127.0.0.1", port=0, log_level="warning"))
    t = threading.Thread(target=server.run, daemon=True)
    t.start()
    deadline = time.monotonic() + 10
    while not server.started:
        assert time.monotonic() < deadline, "uvicorn did not start"
        time.sleep(0.02)
    gateway.url = f"http://127.0.0.1:{server.servers[0].sockets[0].getsockname()[1]}"
    yield gateway
    server.should_exit = True
    t.join(timeout=10)


def pod_env(gw, tokens, out, model_url, **extra):
    expires = (datetime.now(timezone.utc) + timedelta(minutes=20)).strftime("%Y-%m-%dT%H:%M:%SZ")
    return {"HANGAR_RUN_ID": "r-1", "HANGAR_TASK_ID": "t-1", "HANGAR_SESSION_ID": gw.sid, "HANGAR_AGENT": "researcher",
            "HANGAR_EXPIRES_AT": expires, "HANGAR_OUTPUT_DIR": str(out), "HANGAR_WORKSPACE": str(out.parent),
            "HANGAR_LIMITS": '{"githubCalls":0,"modelTokens":300000,"toolCalls":100}',
            "CLEARANCE_URL": gw.url, "MODEL_PROXY_URL": model_url, "HANGAR_TOKEN_DIR": str(tokens),
            "HANGAR_INPUT": json.dumps({"prompt": "Who owns flight-api?"}), **extra}


def test_main_runs_a_session_to_done_and_leaves_no_token_behind(served, proxy, tokens, tmp_path, capsys):
    p = proxy((200, message([{"type": "tool_use", "id": "tu_1", "name": "catalog__read",
                              "input": {"query": "flight-api"}},
                             {"type": "tool_use", "id": "tu_2", "name": "repo__pr__open",
                              "input": {"repo": "jfillman/x", "branch": "b", "title": "t", "files": []}}],
                            stop="tool_use")),
              (200, message([{"type": "text", "text": "flight-api is in the catalog; I may not open PRs."}])))
    out = tmp_path / "output"
    code = runtime_main.main(pod_env(served, tokens, out, p.url))
    stdout = capsys.readouterr().out
    result = json.loads((out / "result.json").read_text())
    assert code == 0
    assert result == {"status": "done", "turns": 2, "final_text": "flight-api is in the catalog; I may not open PRs.",
                      "tool_calls": [{"tool": "catalog.read", "ok": True, "rule": None},
                                     {"tool": "repo.pr.open", "ok": False, "rule": "R006"}], "reason": ""}
    sent = p.requests[1]["body"]["messages"][-1]["content"]
    assert "is_error" not in sent[0] and "flight-api" in sent[0]["content"]
    assert sent[1]["is_error"] is True and "R006" in sent[1]["content"]
    assert "turn 1: stop=tool_use tools=[catalog.read:ok, repo.pr.open:deny(R006)]" in stdout
    everything = stdout + (out / "result.json").read_text()
    assert not any(t in everything for t in TOKENS + MODEL_TOKENS)
    assert "flight-api\"" not in stdout and "jfillman/x" not in stdout, "no tool arguments in the log"


def test_main_stops_cleanly_on_a_spent_budget(served, proxy, tokens, tmp_path, capsys):
    p = proxy((429, error("rate_limit_error", "the session's model budget is spent")))
    out = tmp_path / "output"
    code = runtime_main.main(pod_env(served, tokens, out, p.url))
    result = json.loads((out / "result.json").read_text())
    assert code == 1 and result["status"] == "budget" and result["turns"] == 0
    assert not any(t in capsys.readouterr().out for t in TOKENS + MODEL_TOKENS)


def test_main_with_an_unusable_environment_still_writes_a_result(tmp_path, capsys):
    out = tmp_path / "output"
    code = runtime_main.main({"HANGAR_OUTPUT_DIR": str(out), "HANGAR_SESSION_ID": "s-1"})
    result = json.loads((out / "result.json").read_text())
    assert code == 2 and result["status"] == "error" and "MODEL_PROXY_URL" in result["reason"]


def test_model_comes_from_hangar_model_then_input_then_the_default(tokens, tmp_path):
    from hangar_agent.env import DEFAULT_MODEL, config_from_env
    base = {"HANGAR_SESSION_ID": "s-1", "HANGAR_EXPIRES_AT": "2026-10-29T12:00:00Z", "CLEARANCE_URL": "http://c",
            "MODEL_PROXY_URL": "http://m", "HANGAR_TOKEN_DIR": str(tokens)}
    assert config_from_env(base).model == DEFAULT_MODEL == "claude-sonnet-5-5"
    assert config_from_env({**base, "HANGAR_INPUT": '{"model": "claude-opus-5"}'}).model == "claude-opus-5"
    assert config_from_env({**base, "HANGAR_INPUT": '{"model": "claude-opus-5"}', "HANGAR_MODEL": "m2"}).model == "m2"
    c = config_from_env(base)
    assert c.max_turns == 30 and c.expires_at == datetime(2026, 10, 29, 12, tzinfo=timezone.utc)
    assert c.clearance_token_file == tokens / "clearance-token"
