"""The LangGraph runtime (runtime/langgraph): the generic loop's behaviour, run as a graph. Each scenario runs through
both loop.run and hangar_langgraph.graph.run with the same fakes and must end the same way."""
from datetime import timedelta

import pytest

pytest.importorskip("langgraph")

from hangar_agent import loop  # noqa: E402
from hangar_agent.model import ModelStop  # noqa: E402
from hangar_langgraph import graph  # noqa: E402

from test_runtime_loop import DENY, NOW, FakeGateway, FakeModel, reply, text, tool_use  # noqa: E402


def both(script, gateway_answers=None, **kw):
    outs = []
    for runner in (loop.run, graph.run):
        model, gw, lines = FakeModel(*script), FakeGateway(gateway_answers), []
        out = runner(model, gw, agent="researcher", task=kw.get("task", {"prompt": "find flight-api"}),
                     model_name="claude-sonnet-5-5", max_turns=kw.get("max_turns", 30), max_tokens=1024,
                     expires_at=kw.get("expires_at", NOW + timedelta(minutes=20)), clock=lambda: NOW, log=lines.append)
        outs.append((out, model, gw, lines))
    (a, ma, ga, _), (b, mb, gb, lines) = outs
    assert a.as_dict() == b.as_dict(), "the graph ends exactly as the loop does"
    assert ga.calls == gb.calls and len(ma.requests) == len(mb.requests)
    return b, mb, gb, lines


def test_a_run_to_done_with_a_tool_call_and_a_refusal():
    out, model, gw, lines = both(
        [reply(tool_use("catalog__read", {"query": "flight"}), tool_use("repo__pr__open", {"repo": "x"}, id="tu_2")),
         reply(text("flight-api is in the catalog; I may not open PRs."), stop="end_turn")],
        {"repo.pr.open": {"ok": False, "decision": DENY}})
    assert out.status == "done" and out.turns == 2 and out.final_text.startswith("flight-api")
    assert [c["tool"] for c in out.tool_calls] == ["catalog.read", "repo.pr.open"] and out.tool_calls[1]["rule"] == "R006"
    sent = model.requests[1]["messages"][-1]["content"]
    assert sent[1]["is_error"] is True
    assert lines[0].startswith("start: graph=langgraph")


def test_the_turn_limit_and_the_budget_stop_it():
    out, *_ = both([reply(tool_use("catalog__read", {"query": "a"}))] * 3, max_turns=2)
    assert out.status == "budget" and out.turns == 2
    out, *_ = both([ModelStop(429, "spent")])
    assert out.status == "budget" and "429" in out.reason


def test_near_the_deadline_it_stops_before_asking():
    out, model, *_ = both([reply(text("x"), stop="end_turn")], expires_at=NOW + timedelta(seconds=30))
    assert out.status == "expired" and model.requests == []


def test_pause_turn_continues_and_nothing_to_do_is_an_error():
    out, *_ = both([reply(text("thinking"), stop="pause_turn"), reply(text("done"), stop="end_turn")])
    assert out.status == "done" and out.turns == 2
    out, *_ = both([], task={})
    assert out.status == "error" and "nothing to do" in out.reason
