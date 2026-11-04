"""session_close (Gateway.close_session, the MCP tool): the session's own caller ends it now instead of at its TTL.
Owner-only; the person control (end_session, Tower's End) is unchanged. The end goes through end_session's path
(session.end through the core), and the session's runs and its children's are deleted as the expiry sweep deletes
them, each with a run.delete record."""
import asyncio
import json

import pytest

from autopilot.engine import control as ctl
from autopilot.engine.dispatch import Principal
from autopilot.engine.tiers import Tier
from autopilot.engine.server import create_server
from coreenv import verify_chain


def _team(gw):
    """alice's orchestrator session with one spawned child run."""
    sid = gw.open_session("alice", "orchestrator").data["session"]
    r = gw.call("alice", sid, "run.spawn", {"agent": "researcher", "limits": {"tool_calls": 10}})
    assert r.ok, r.as_dict()
    return sid, r.data["session"]


def test_the_owner_closes_its_session_and_its_children_runs_are_deleted(gw, backends):
    sid, child = _team(gw)
    start = len(gw.core.records)
    out = gw.close_session("alice", sid, "done with it")
    assert out["closed"] is True and out["runs_deleted"] == [child]
    assert gw.store.get(sid).closed and gw.store.get(child).closed
    assert backends.runs.live == {}, "the child's AgentRun is deleted at once, as the sweep would"
    recs = gw.core.records[start:]
    assert [r["tool"] for r in recs] == ["session.end", "run.delete"]
    end = gw.core.at(out["audit_seq"])
    assert (end["tool"], end["session"], end["target"], end["principal"], end["result"]) == (
        "session.end", sid, sid, "user:alice", "ended: done with it")
    assert end["provider"]["name"] == "autopilot-gateway"
    assert recs[1]["session"] == child and recs[1]["result"] == "deleted"
    assert not gw.call("alice", sid, "catalog.read", {"query": "x"}).ok, "a closed session refuses every call"
    assert verify_chain() == (True, None)


def test_the_reason_is_optional(gw):
    sid = gw.open_session("alice", "researcher").data["session"]
    out = gw.close_session("alice", sid)
    assert gw.core.at(out["audit_seq"])["result"] == "ended: closed by its owner (session_close)"
    assert out["runs_deleted"] == []


def test_anyone_else_is_refused_and_nothing_changes(gw):
    sid = gw.open_session("alice", "orchestrator").data["session"]
    start = len(gw.core.records)
    for who in ("bob", "holmes", "readonly"):
        with pytest.raises(ctl.ControlError) as e:
            gw.close_session(who, sid, "not mine")
        assert e.value.rule == "CT-FORBIDDEN" and e.value.status == 403
    with pytest.raises(ctl.ControlError) as e:
        gw.close_session("alice", "s-nope")
    assert e.value.rule == "CT-FORBIDDEN", "an unknown session answers the same as another's"
    assert not gw.store.get(sid).closed and len(gw.core.records) == start


def test_closing_twice_is_refused(gw):
    sid = gw.open_session("alice", "researcher").data["session"]
    gw.close_session("alice", sid)
    with pytest.raises(ctl.ControlError) as e:
        gw.close_session("alice", sid)
    assert e.value.rule == "CT-CLOSED" and e.value.status == 409


def test_an_in_cluster_run_closes_its_own_session(gw, backends):
    sid = gw.open_triggered("nightly-reviewer", "test").data["session"]
    assert gw.launch(sid).ok
    s = gw.store.get(sid)
    gw.runs.principal = lambda sess: f"agent-{sess.run}/run" if sess.run else None
    run_id = f"agent-{s.run}/run"
    gw.auth.table["run-a"] = Principal(run_id, "workload", Tier.T1)
    out = gw.close_session("run-a", sid, "finished early")
    assert out["runs_deleted"] == [sid] and backends.runs.live == {}
    assert gw.core.at(out["audit_seq"])["principal"] == run_id, "the record names the caller"


def test_the_mcp_tool(gw):
    srv = create_server(gw, lambda: "alice")

    def call(name, args):
        res = asyncio.run(srv.call_tool(name, args))
        sc = getattr(res, "structured_content", None) or getattr(res, "structuredContent", None)
        return sc.get("result", sc) if sc else json.loads(res.content[0].text)
    sid = call("session_open", {"agent": "researcher"})["data"]["session"]
    out = call("session_close", {"session": sid, "reason": "bye"})
    assert out["ok"] and out["closed"]
    again = call("session_close", {"session": sid})
    assert not again["ok"] and again["rule"] == "CT-CLOSED"
    bob = create_server(gw, lambda: "bob")
    sid2 = call("session_open", {"agent": "researcher"})["data"]["session"]
    res = asyncio.run(bob.call_tool("session_close", {"session": sid2}))
    sc = getattr(res, "structured_content", None) or getattr(res, "structuredContent", None)
    refused = sc.get("result", sc) if sc else json.loads(res.content[0].text)
    assert not refused["ok"] and refused["rule"] == "CT-FORBIDDEN"
