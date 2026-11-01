import asyncio
import json

import pytest

from clearance.server import create_server


def run(coro):
    return asyncio.run(coro)


def payload(result):
    # structured content when present, else the JSON text block
    sc = getattr(result, "structured_content", None) or getattr(result, "structuredContent", None)
    if sc:
        return sc.get("result", sc)
    return json.loads(result.content[0].text)


@pytest.fixture
def srv(gw):
    return create_server(gw, lambda: "alice")


def test_exactly_four_tools_are_exposed(srv):
    names = {t.name for t in run(srv.list_tools())}
    assert names == {"agents_list", "tools_list", "session_open", "call"}


def test_a_full_round_trip_through_mcp(srv, backends):
    opened = payload(run(srv.call_tool("session_open", {"agent": "coding-agent"})))
    assert opened["ok"], opened
    sid = opened["data"]["session"]
    r = payload(run(srv.call_tool("call", {"session": sid, "tool": "catalog.read", "args": {"query": "flight"}})))
    assert r["ok"] and backends.read.names() == ["read.catalog"]


def test_a_denial_comes_back_with_a_rule_and_a_hint_and_touches_nothing(srv, backends):
    sid = payload(run(srv.call_tool("session_open", {"agent": "coding-agent"})))["data"]["session"]
    r = payload(run(srv.call_tool("call", {"session": sid, "tool": "k8s.exec", "args": {}})))
    assert not r["ok"] and r["decision"]["rule"] == "R002" and r["decision"]["hint"]
    assert backends.read.calls == [] and backends.git.calls == []


def test_tools_list_reports_tiers(srv):
    r = payload(run(srv.call_tool("tools_list", {})))
    by = {t["name"]: t for t in r}
    assert by["catalog.read"]["tier"] == "T0" and by["repo.pr.open_upper"]["propose_only"] is True
    assert "k8s.exec" not in by, "T3 tripwires are never advertised"


def test_tools_list_for_a_session_is_its_grant_that_this_deployment_runs(gw, srv):
    sid = gw.open_session("alice", "researcher").data["session"]
    r = payload(run(srv.call_tool("tools_list", {"session": sid})))
    names = {t["name"] for t in r}
    assert names == {"catalog.read", "metrics.query", "logs.query", "argo.app.get"} & set(gw._handlers)
    assert names and "repo.pr.open" not in names


def test_tools_list_refuses_someone_elses_session(gw):
    sid = gw.open_session("bob", "researcher").data["session"]
    assert gw.session_tools("alice", sid) is None and gw.session_tools("alice", "s-nope") is None
    assert gw.session_tools("bob", sid)
