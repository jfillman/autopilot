"""One run on the Claude Agent SDK. `sdk` is the claude_agent_sdk module (tests pass a fake with the same names)."""
from __future__ import annotations

import asyncio
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable

from hangar_agent import loop
from hangar_agent import tools as T
from hangar_agent.clearance import ClearanceError
from hangar_agent.env import TokenError

SERVER = "clearance"
PREFIX = f"mcp__{SERVER}__"
# The model API's limit on a tool's name, which the CLI's prefixed MCP names must also meet.
MAX_NAME = 64

# How the SDK's result subtypes map onto the runtime's statuses (hangar_agent.loop.Outcome).
STATUS = {"success": "done", "error_max_turns": "budget", "error_max_budget_usd": "budget"}


def cli_env(*, model_proxy_url: str, model_token: str, session: str, model: str, max_tokens: int,
            config_dir: Path) -> dict[str, str]:
    """The CLI's environment: the model proxy as its API, the run's model-proxy token as its key, the session as a
    header, every model alias pinned to the run's model (so a background call never asks for one the session may
    not use), no telemetry, no updates, no other traffic. Nothing else of the pod's environment is needed by it."""
    return {
        "ANTHROPIC_BASE_URL": model_proxy_url,
        "ANTHROPIC_API_KEY": model_token,
        "ANTHROPIC_AUTH_TOKEN": "",
        "ANTHROPIC_CUSTOM_HEADERS": f"x-hangar-session: {session}",
        "ANTHROPIC_MODEL": model,
        "ANTHROPIC_DEFAULT_OPUS_MODEL": model,
        "ANTHROPIC_DEFAULT_SONNET_MODEL": model,
        "ANTHROPIC_DEFAULT_HAIKU_MODEL": model,
        "CLAUDE_CODE_MAX_OUTPUT_TOKENS": str(max_tokens),
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        "DISABLE_TELEMETRY": "1",
        "DISABLE_AUTOUPDATER": "1",
        "DISABLE_ERROR_REPORTING": "1",
        "CLAUDE_CONFIG_DIR": str(config_dir),
        "HOME": str(config_dir),
    }


def clearance_tools(sdk: Any, gateway: loop.Gateway, toolset: T.Toolset, out: loop.Outcome,
                    log: Callable[[str], None]) -> tuple[list[Any], list[str]]:
    """One in-process MCP tool per Clearance tool: (the tools, their CLI names). A call goes to Clearance in a
    thread (the client is blocking) and its answer comes back as the generic runtime would give it to the model."""
    made, names = [], []
    for t in toolset.tools:
        if len(PREFIX + t["name"]) > MAX_NAME:
            log(f"skipped {toolset.clearance_name(t['name'])}: its name is too long for an MCP tool")
            continue
        dotted = toolset.clearance_name(t["name"])

        async def handler(args: dict[str, Any], _dotted: str = dotted) -> dict[str, Any]:
            try:
                r = await asyncio.to_thread(gateway.call, _dotted, args if isinstance(args, dict) else {})
            except (ClearanceError, TokenError) as e:
                out.tool_calls.append({"tool": _dotted, "ok": False, "rule": None})
                log(f"tool {_dotted}: gateway failed")
                return {"content": [{"type": "text", "text": f"Clearance could not be asked: {e}"}], "is_error": True}
            ok, rule = bool(r.get("ok")), T.rule_of(r)
            out.tool_calls.append({"tool": _dotted, "ok": ok, "rule": rule})
            log(f"tool {_dotted}:{'ok' if ok else 'deny' if rule else 'error'}" + (f"({rule})" if rule and not ok else ""))
            res = T.tool_result("", r, _dotted)
            return {"content": [{"type": "text", "text": res["content"]}], "is_error": bool(res.get("is_error"))}

        made.append(sdk.SdkMcpTool(name=t["name"], description=t["description"], input_schema=t["input_schema"],
                                   handler=handler))
        names.append(PREFIX + t["name"])
    return made, names


async def run(sdk: Any, gateway: loop.Gateway, *, agent: str, task: dict[str, Any], model_name: str, max_turns: int,
              env: dict[str, str], workspace: Path, expires_at: datetime, grant: list[str] | None = None,
              ids: dict[str, str] | None = None, clock: Callable[[], datetime] = lambda: datetime.now(timezone.utc),
              log: Callable[[str], None] = print) -> loop.Outcome:
    """loop.run's contract on the Agent SDK: the same system prompt and first message, the same stops (turns, the
    session's deadline less loop.EXPIRY_MARGIN), the same Outcome."""
    out = loop.Outcome("error")
    prompt = loop.first_message(task)
    if prompt is None:
        out.reason = "HANGAR_INPUT has no prompt and no event: nothing to do"
        return out
    try:
        toolset = T.build_toolset(gateway.tools_list(), grant)
    except (ClearanceError, TokenError) as e:
        out.reason = f"tools_list: {e}"
        return out
    tools, names = clearance_tools(sdk, gateway, toolset, out, log)
    options = sdk.ClaudeAgentOptions(
        system_prompt=loop.system_prompt(agent, task, ids=ids, scoped=bool(getattr(gateway, "scoped", False))),
        tools=[],                       # no built-in tools at all: Clearance's are the only ones
        allowed_tools=names,
        mcp_servers={SERVER: sdk.create_sdk_mcp_server(name=SERVER, version="1.0.0", tools=tools)},
        strict_mcp_config=True,         # and no MCP server from any config file
        setting_sources=[],             # no user, project or local settings
        max_turns=max_turns, model=model_name, env=env, cwd=str(workspace))
    log(f"start: sdk=claude-agent-sdk model={model_name} tools={len(names)} max_turns={max_turns}")
    seconds = (expires_at - clock() - loop.EXPIRY_MARGIN).total_seconds()
    if seconds <= 0:
        out.status, out.reason = "expired", "less than 60s of the session left"
        return out
    try:
        await asyncio.wait_for(_consume(sdk, prompt, options, out, log), timeout=seconds)
    except asyncio.TimeoutError:
        out.status, out.reason = "expired", "less than 60s of the session left"
    except Exception as e:  # noqa: BLE001 - the CLI failing is the run's error, recorded, never a traceback
        out.status, out.reason = "error", f"claude-agent-sdk: {type(e).__name__}: {str(e)[:300]}"
    return out


async def _consume(sdk: Any, prompt: str, options: Any, out: loop.Outcome, log: Callable[[str], None]) -> None:
    async for m in sdk.query(prompt=prompt, options=options):
        if isinstance(m, sdk.AssistantMessage):
            text = "\n".join(b.text for b in m.content if isinstance(b, sdk.TextBlock)).strip()
            out.final_text = text or out.final_text
        elif isinstance(m, sdk.ResultMessage):
            out.turns = m.num_turns
            out.status = STATUS.get(m.subtype, "error")
            if m.subtype == "success" and m.is_error:
                out.status = "error"
            if m.result:
                out.final_text = m.result
            if out.status != "done":
                out.reason = f"claude-agent-sdk: {m.subtype}" + (f" ({m.stop_reason})" if getattr(m, "stop_reason", None) else "")
            log(f"end of loop: {m.subtype} turns={m.num_turns}")
