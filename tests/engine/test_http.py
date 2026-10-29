"""The streamable-HTTP surface: no tool runs without a bearer token the gateway accepts, and each call is the
caller's own (a session opened by one bearer cannot be used by another)."""
import json

import pytest
from starlette.testclient import TestClient

from clearance.dispatch import AuthError, Principal
from clearance.server import create_http_app
from clearance.tiers import Tier

HEADERS = {"Accept": "application/json, text/event-stream", "Content-Type": "application/json",
           "MCP-Protocol-Version": "2025-06-18"}


class Tokens:
    table = {"alice-token": Principal("user:alice", "user", Tier.T1),
             "bob-token": Principal("user:bob", "user", Tier.T1)}

    def authenticate(self, token):
        if token not in self.table:
            raise AuthError("no")
        return self.table[token]


@pytest.fixture
def client(gw):
    gw.auth = Tokens()
    app = create_http_app(gw, resource_url="https://clearance.example/mcp", issuer_url="https://dex.example/dex")
    with TestClient(app) as c:
        yield c


def rpc(client, method, params=None, token=None, id_=1):
    h = dict(HEADERS)
    if token:
        h["Authorization"] = f"Bearer {token}"
    body = {"jsonrpc": "2.0", "id": id_, "method": method, "params": params or {}}
    return client.post("/mcp", headers=h, content=json.dumps(body))


def tool(client, token, name, args):
    r = rpc(client, "tools/call", {"name": name, "arguments": args}, token)
    assert r.status_code == 200, r.text
    res = r.json()["result"]
    return res.get("structuredContent", {}).get("result", res.get("structuredContent")) or json.loads(res["content"][0]["text"])


def test_healthz_needs_no_token(client):
    r = client.get("/healthz")
    assert r.status_code == 200 and r.json()["ok"] is True


@pytest.mark.parametrize("token", [None, "forged"])
def test_no_or_bad_bearer_is_401_before_any_tool(client, gw, token):
    r = rpc(client, "tools/call", {"name": "session_open", "arguments": {"agent": "coding-agent"}}, token)
    assert r.status_code == 401
    assert gw.audit.records == [], "nothing reached the gateway"


def test_a_session_is_its_openers_only(client, gw, backends):
    opened = tool(client, "alice-token", "session_open", {"agent": "coding-agent"})
    sid = opened["data"]["session"]
    assert opened["ok"] and gw.store.get(sid).principal == "user:alice"
    stolen = tool(client, "bob-token", "call", {"session": sid, "tool": "catalog.read", "args": {"query": "x"}})
    assert not stolen["ok"] and stolen["decision"]["rule"] == "R001"
    mine = tool(client, "alice-token", "call", {"session": sid, "tool": "catalog.read", "args": {"query": "x"}})
    assert mine["ok"] and backends.read.names() == ["read.catalog"]
