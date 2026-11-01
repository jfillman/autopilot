"""The Claude Agent SDK runtime (runtime/claude-agent-sdk) with a fake SDK: what it gives the CLI (no built-in tools,
Clearance's tools only, the model proxy and session), how a tool call reaches Clearance, and how the SDK's result
becomes the runtime's Outcome. The real SDK and CLI are exercised by the image's build check and a live run."""
import asyncio
from dataclasses import dataclass, field
from datetime import timedelta
from pathlib import Path
from typing import Any

from hangar_claude import agent

from test_runtime_loop import DENY, NOW, FakeGateway


@dataclass
class SdkMcpTool:
    name: str
    description: str
    input_schema: Any
    handler: Any


@dataclass
class TextBlock:
    text: str


@dataclass
class AssistantMessage:
    content: list
    model: str = "m"


@dataclass
class ResultMessage:
    subtype: str
    num_turns: int
    is_error: bool = False
    result: str | None = None
    stop_reason: str | None = None


@dataclass
class ClaudeAgentOptions:
    system_prompt: str = ""
    tools: Any = None
    allowed_tools: list = field(default_factory=list)
    mcp_servers: dict = field(default_factory=dict)
    strict_mcp_config: bool = False
    setting_sources: Any = None
    max_turns: int | None = None
    model: str | None = None
    env: dict = field(default_factory=dict)
    cwd: str | None = None


class FakeSdk:
    """The SDK's names. query() plays a script: ("call", tool name, args) runs that MCP tool, a message is yielded."""
    SdkMcpTool, TextBlock, AssistantMessage, ResultMessage, ClaudeAgentOptions = (
        SdkMcpTool, TextBlock, AssistantMessage, ResultMessage, ClaudeAgentOptions)

    def __init__(self, *script, hang=False):
        self.script, self.hang, self.options, self.results = list(script), hang, None, []

    @staticmethod
    def create_sdk_mcp_server(name, version, tools):
        return {"type": "sdk", "name": name, "tools": {t.name: t for t in tools}}

    async def query(self, *, prompt, options):
        self.options, self.prompt = options, prompt
        tools = options.mcp_servers["clearance"]["tools"]
        for step in self.script:
            if isinstance(step, tuple):
                self.results.append(await tools[step[1]].handler(step[2]))
            else:
                yield step
        if self.hang:
            await asyncio.sleep(3600)


def go(sdk, gw=None, **kw):
    lines = []
    out = asyncio.run(agent.run(sdk, gw or FakeGateway({"repo.pr.open": {"ok": False, "decision": DENY}}),
                                agent="researcher", task={"prompt": "find flight-api", "instructions": "Be brief."},
                                model_name="claude-sonnet-5-5", max_turns=7, env={"X": "1"}, workspace=Path("/workspace"),
                                expires_at=kw.get("expires_at", NOW + timedelta(minutes=20)), clock=lambda: NOW,
                                log=lines.append))
    return out, lines


def test_the_cli_gets_clearance_and_nothing_else():
    sdk = FakeSdk(ResultMessage("success", 1, result="ok"))
    out, lines = go(sdk)
    o = sdk.options
    assert o.tools == [] and o.strict_mcp_config is True and o.setting_sources == []
    assert set(o.allowed_tools) == {"mcp__clearance__catalog__read", "mcp__clearance__repo__pr__open",
                                    "mcp__clearance__glidepath__environments__rollbackPlan"}
    assert o.max_turns == 7 and o.model == "claude-sonnet-5-5" and o.env == {"X": "1"} and o.cwd == "/workspace"
    assert "Be brief." in o.system_prompt and "Clearance" in o.system_prompt and sdk.prompt == "find flight-api"
    assert out.status == "done" and out.final_text == "ok" and out.turns == 1


def test_a_tool_call_goes_to_clearance_and_a_refusal_comes_back_as_an_error():
    gw = FakeGateway({"repo.pr.open": {"ok": False, "decision": DENY}})
    sdk = FakeSdk(("call", "catalog__read", {"query": "flight"}), ("call", "repo__pr__open", {"repo": "x"}),
                  AssistantMessage([TextBlock("flight-api is owned by team-flight.")]),
                  ResultMessage("success", 2, result=None))
    out, lines = go(sdk, gw)
    assert gw.calls == [("catalog.read", {"query": "flight"}), ("repo.pr.open", {"repo": "x"})]
    assert sdk.results[0]["is_error"] is False and '"rows":1' in sdk.results[0]["content"][0]["text"]
    assert sdk.results[1]["is_error"] is True and "R006" in sdk.results[1]["content"][0]["text"]
    assert out.tool_calls == [{"tool": "catalog.read", "ok": True, "rule": None},
                              {"tool": "repo.pr.open", "ok": False, "rule": "R006"}]
    assert out.status == "done" and out.final_text == "flight-api is owned by team-flight." and out.turns == 2
    assert "tool repo.pr.open:deny(R006)" in lines


def test_results_map_to_the_runtime_statuses():
    assert go(FakeSdk(ResultMessage("error_max_turns", 7)))[0].status == "budget"
    out = go(FakeSdk(ResultMessage("error_during_execution", 3)))[0]
    assert out.status == "error" and "error_during_execution" in out.reason
    assert go(FakeSdk(ResultMessage("success", 1, is_error=True, result="no", stop_reason="refusal")))[0].status == "error"


def test_the_deadline_stops_a_loop_that_does_not_end(monkeypatch):
    monkeypatch.setattr(agent.loop, "EXPIRY_MARGIN", timedelta(seconds=0))
    out, _ = go(FakeSdk(hang=True), expires_at=NOW + timedelta(seconds=0.2))
    assert out.status == "expired"
    out, _ = go(FakeSdk(ResultMessage("success", 1)), expires_at=NOW)
    assert out.status == "expired"


def test_the_cli_env_points_at_the_model_proxy_with_the_session_and_one_model():
    env = agent.cli_env(model_proxy_url="http://clearance-model.autopilot-system:8081", model_token="tok",
                        session="s-1", model="claude-sonnet-5-5", max_tokens=8192, config_dir=Path("/tmp/claude"))
    assert env["ANTHROPIC_BASE_URL"].endswith(":8081") and env["ANTHROPIC_API_KEY"] == "tok"
    assert env["ANTHROPIC_CUSTOM_HEADERS"] == "x-hangar-session: s-1" and env["ANTHROPIC_AUTH_TOKEN"] == ""
    assert {env[k] for k in ("ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
                             "ANTHROPIC_DEFAULT_OPUS_MODEL")} == {"claude-sonnet-5-5"}
    assert env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] == "1" and env["HOME"] == "/tmp/claude"
