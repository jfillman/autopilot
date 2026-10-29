"""The agent runtime's loop and tool mapping (runtime/src/hangar_agent), with a fake model and a fake gateway.

The wire (the real Clearance HTTP app, the model proxy's headers, main() end to end) is tests/test_runtime_wire.py.
"""
from datetime import datetime, timedelta, timezone

import pytest

from hangar_agent import loop, tools as T
from hangar_agent.clearance import ClearanceError
from hangar_agent.model import ModelError, ModelStop

NOW = datetime(2026, 10, 29, 12, 0, 0, tzinfo=timezone.utc)
REGISTRY = [
    {"name": "catalog.read", "tier": "T0", "description": "Read the catalog.", "required_args": ["query"],
     "propose_only": False},
    {"name": "repo.pr.open", "tier": "T1", "description": "Open a pull request.",
     "required_args": ["repo", "branch", "title", "files"], "propose_only": True},
    {"name": "glidepath.environments.rollbackPlan", "tier": "T0", "description": "", "required_args": ["app", "env"],
     "propose_only": False},
]
ALLOW = {"allow": True, "rule": None, "name": None, "message": "allowed", "hint": "", "tier": 0}
DENY = {"allow": False, "rule": "R006", "name": "tool-not-permitted",
        "message": "tool is not permitted for this session", "hint": "Use a tool in your definition.", "tier": 1}


class FakeGateway:
    def __init__(self, answers=None, registry=REGISTRY):
        self.answers, self.registry, self.calls = answers or {}, registry, []

    def tools_list(self):
        return self.registry

    def call(self, tool, args):
        self.calls.append((tool, args))
        a = self.answers.get(tool, {"ok": True, "decision": ALLOW, "data": {"rows": 1}})
        if isinstance(a, Exception):
            raise a
        return {"error": None, "audit_seq": len(self.calls), "session": "s-1", **a}


def tool_use(name, args, id="tu_1"):
    return {"type": "tool_use", "id": id, "name": name, "input": args}


def reply(*blocks, stop="tool_use"):
    return {"id": "msg", "type": "message", "role": "assistant", "content": list(blocks), "stop_reason": stop}


def text(t):
    return {"type": "text", "text": t}


class FakeModel:
    """Answers from a script; an Exception in it is raised. Keeps every request."""

    def __init__(self, *script):
        self.script, self.requests = list(script), []

    def create(self, **request):
        self.requests.append(request)
        a = self.script.pop(0)
        if isinstance(a, Exception):
            raise a
        return a


def go(model, gateway, *, task=None, max_turns=30, expires_at=NOW + timedelta(minutes=20), clock=lambda: NOW,
       grant=None):
    lines = []
    out = loop.run(model, gateway, agent="researcher", task=task or {"prompt": "find flight-api"},
                   model_name="claude-sonnet-5-5", max_turns=max_turns, max_tokens=1024, expires_at=expires_at,
                   grant=grant, clock=clock, log=lines.append)
    return out, lines


# -- names and schemas ------------------------------------------------------------------------------------------------
def test_dotted_names_map_to_anthropic_names_and_back():
    ts = T.build_toolset(REGISTRY)
    names = [t["name"] for t in ts.tools]
    assert names == ["catalog__read", "repo__pr__open", "glidepath__environments__rollbackPlan"]
    assert all(T.NAME_RE.match(n) for n in names)
    assert {ts.clearance_name(n) for n in names} == {t["name"] for t in REGISTRY}
    assert ts.clearance_name("no__such") is None


def test_a_name_that_cannot_be_an_anthropic_name_is_skipped_not_mangled():
    ts = T.build_toolset([{"name": "a" * 70, "required_args": []}, {"name": "has space", "required_args": []},
                          {"name": "ok.tool", "required_args": []}])
    assert [t["name"] for t in ts.tools] == ["ok__tool"] and len(ts.skipped) == 2


def test_two_names_that_collide_keep_the_first():
    ts = T.build_toolset([{"name": "a.b", "required_args": []}, {"name": "a__b", "required_args": []}])
    assert ts.clearance_name("a__b") == "a.b" and ts.skipped == ["a__b"]


def test_schema_is_the_required_args_strings_unless_known_and_open_to_more():
    ts = T.build_toolset(REGISTRY)
    s = ts.tools[1]["input_schema"]
    assert s["type"] == "object" and s["additionalProperties"] is True
    assert s["required"] == ["repo", "branch", "title", "files"]
    assert s["properties"]["repo"] == {"type": "string"} and s["properties"]["files"]["type"] == "array"
    assert "tier T1" in ts.tools[1]["description"] and "propose-only" in ts.tools[1]["description"]
    assert ts.tools[2]["description"].startswith("glidepath.environments.rollbackPlan")


def test_a_grant_narrows_the_offer():
    ts = T.build_toolset(REGISTRY, ["catalog.read"])
    assert [t["name"] for t in ts.tools] == ["catalog__read"]


# -- the loop ---------------------------------------------------------------------------------------------------------
def test_end_turn_is_done_with_the_final_text():
    model = FakeModel(reply(tool_use("catalog__read", {"query": "flight-api"})),
                      reply(text("flight-api is owned by team-a."), stop="end_turn"))
    gw = FakeGateway()
    out, lines = go(model, gw)
    assert out.status == "done" and out.turns == 2 and out.final_text == "flight-api is owned by team-a."
    assert gw.calls == [("catalog.read", {"query": "flight-api"})]
    assert out.tool_calls == [{"tool": "catalog.read", "ok": True, "rule": None}]
    first = model.requests[0]
    assert first["model"] == "claude-sonnet-5-5" and first["max_tokens"] == 1024
    assert [t["name"] for t in first["tools"]] == ["catalog__read", "repo__pr__open",
                                                   "glidepath__environments__rollbackPlan"]
    assert "Clearance" in first["system"] and "Do not retry the same call" in first["system"]
    result = model.requests[1]["messages"][-1]["content"][0]
    assert result == {"type": "tool_result", "tool_use_id": "tu_1", "content": '{"rows":1}'}


def test_a_refusal_goes_back_to_the_model_as_an_error_with_rule_and_hint():
    model = FakeModel(reply(tool_use("repo__pr__open", {"repo": "jfillman/x", "branch": "b", "title": "t",
                                                        "files": []})),
                      reply(text("I may not open pull requests."), stop="end_turn"))
    gw = FakeGateway({"repo.pr.open": {"ok": False, "decision": DENY, "data": None,
                                       "error": "tool is not permitted for this session"}})
    out, lines = go(model, gw)
    result = model.requests[1]["messages"][-1]["content"][0]
    assert result["is_error"] is True and result["tool_use_id"] == "tu_1"
    assert '"rule":"R006"' in result["content"] and "Use a tool in your definition." in result["content"]
    assert '"refused":true' in result["content"] and "do not repeat the same call" in result["content"]
    assert out.status == "done" and out.tool_calls == [{"tool": "repo.pr.open", "ok": False, "rule": "R006"}]
    assert "repo.pr.open:deny(R006)" in lines[1]


def test_an_unknown_tool_name_is_an_error_result_and_reaches_no_gateway():
    model = FakeModel(reply(tool_use("secrets__read", {})), reply(text("ok"), stop="end_turn"))
    gw = FakeGateway()
    out, _ = go(model, gw)
    assert gw.calls == [] and model.requests[1]["messages"][-1]["content"][0]["is_error"] is True
    assert out.tool_calls == [{"tool": "secrets__read", "ok": False, "rule": None}]


def test_every_tool_use_in_a_turn_gets_its_result_in_order():
    model = FakeModel(reply(tool_use("catalog__read", {"query": "a"}, "t1"),
                            tool_use("catalog__read", {"query": "b"}, "t2")),
                      reply(text("done"), stop="end_turn"))
    out, _ = go(model, FakeGateway())
    assert [r["tool_use_id"] for r in model.requests[1]["messages"][-1]["content"]] == ["t1", "t2"]
    assert len(out.tool_calls) == 2


def test_max_turns_stops_and_the_last_turns_tools_are_not_run():
    model = FakeModel(*[reply(tool_use("catalog__read", {"query": str(i)}, f"t{i}")) for i in range(3)])
    gw = FakeGateway()
    out, lines = go(model, gw, max_turns=3)
    assert out.status == "budget" and out.turns == 3 and "max turns" in out.reason
    assert len(gw.calls) == 2 and "not-run" in lines[-1]


def test_stops_when_the_session_has_less_than_a_minute_left():
    t = [NOW]

    def clock():
        return t[0]

    class Advancing(FakeModel):
        def create(self, **request):
            t[0] += timedelta(minutes=5)
            return super().create(**request)

    model = Advancing(*[reply(tool_use("catalog__read", {"query": "x"})) for _ in range(5)])
    out, _ = go(model, FakeGateway(), expires_at=NOW + timedelta(minutes=10, seconds=30), clock=clock)
    assert out.status == "expired" and out.turns == 2


def test_already_inside_the_margin_never_asks_the_model():
    model = FakeModel()
    out, _ = go(model, FakeGateway(), expires_at=NOW + timedelta(seconds=59))
    assert out.status == "expired" and out.turns == 0 and model.requests == []


def test_a_429_from_the_proxy_is_budget_and_a_403_is_error_both_clean():
    out, _ = go(FakeModel(ModelStop(429, "rate_limit_error: model budget spent")), FakeGateway())
    assert out.status == "budget" and "429" in out.reason
    out, _ = go(FakeModel(reply(tool_use("catalog__read", {"query": "x"})), ModelStop(403, "permission_error: no")),
                FakeGateway())
    assert out.status == "error" and out.turns == 1 and "403" in out.reason


def test_a_model_error_or_a_gateway_failure_is_error():
    out, _ = go(FakeModel(ModelError("cannot reach the model proxy")), FakeGateway())
    assert out.status == "error" and "cannot reach" in out.reason
    gw = FakeGateway({"catalog.read": ClearanceError("the gateway answered HTTP 500")})
    out, lines = go(FakeModel(reply(tool_use("catalog__read", {"query": "x"}))), gw)
    assert out.status == "error" and "HTTP 500" in out.reason and "catalog.read:failed" in lines[-1]


def test_a_stop_other_than_end_turn_without_tools_is_error_and_pause_turn_continues():
    out, _ = go(FakeModel(reply(text("cut"), stop="max_tokens")), FakeGateway())
    assert out.status == "error" and "max_tokens" in out.reason
    out, _ = go(FakeModel(reply(stop="pause_turn"), reply(text("fine"), stop="end_turn")), FakeGateway())
    assert out.status == "done" and out.turns == 2


def test_input_without_prompt_or_event_does_nothing():
    model = FakeModel()
    out, _ = go(model, FakeGateway(), task={"model": "x"})
    assert out.status == "error" and model.requests == []


def test_instructions_join_the_system_prompt_and_the_event_the_user_turn():
    model = FakeModel(reply(text("ok"), stop="end_turn"))
    go(model, FakeGateway(), task={"instructions": "Only read.", "event": {"kind": "alert", "app": "flight-api"}})
    req = model.requests[0]
    assert req["system"].endswith("Only read.")
    assert '"app": "flight-api"' in req["messages"][0]["content"]


def test_the_turn_log_names_tools_and_decisions_never_arguments():
    secret_arg = "query-value-that-must-not-be-logged"
    model = FakeModel(reply(tool_use("catalog__read", {"query": secret_arg})), reply(text("ok"), stop="end_turn"))
    _, lines = go(model, FakeGateway())
    assert lines[1] == "turn 1: stop=tool_use tools=[catalog.read:ok]"
    assert all(secret_arg not in line for line in lines)


@pytest.mark.parametrize("status", ["done", "budget", "expired", "error"])
def test_outcome_shape(status):
    d = loop.Outcome(status).as_dict()
    assert set(d) == {"status", "turns", "final_text", "tool_calls", "reason"} and d["status"] == status
