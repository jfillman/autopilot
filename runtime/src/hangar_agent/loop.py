"""The agent loop: ask the model, run the tool calls it asks for through Clearance, hand back the answers, repeat.

It stops, in this order of checks, when:
  - the session has less than EXPIRY_MARGIN left (expired): Clearance would refuse every call after expiry and the
    sweep deletes the run, so the runtime stops while it can still write its result;
  - the model has been asked max_turns times (budget);
  - the model proxy answers 429 (budget: the session's model tokens are spent) or 403 (error: access denied);
  - the model ends its turn (done);
  - the model or the gateway fails in a way a retry did not fix (error).

What is logged is one line per turn: the turn, the stop reason, and each tool with Clearance's decision. Never a
token, never arguments, never tool data: the pod's stdout is collected outside the run's audit trail.
"""
from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any, Callable, Protocol

from . import tools as T
from .clearance import ClearanceError
from .env import TokenError
from .model import ModelError, ModelStop

EXPIRY_MARGIN = timedelta(seconds=60)

SYSTEM = """\
You are a Hangar agent ({agent}), running as one Autopilot run inside a bounded, audited session.

Every action you take is a tool call, and every tool call goes through Clearance, Hangar's policy gateway. Clearance \
checks each call against this session's agent definition, tier ceiling, scope and limits, and records it. A call can \
be refused. A refusal comes back as an error result naming the rule that refused it and a hint.

When a call is refused, read the rule and the hint and change your approach: use another tool, narrow the arguments, \
or explain in your final answer what you could not do and why. Do not retry the same call with the same arguments; \
it will be refused again, and every call counts against the session's limits. {tools_note}

Work toward the task with as few calls as it needs. When you are done, answer with a short summary of what you did, \
what you found, and anything you could not do. That final answer is the run's result."""


class Model(Protocol):
    def create(self, **request: Any) -> dict[str, Any]: ...


class Gateway(Protocol):
    def tools_list(self) -> list[dict[str, Any]]: ...
    def call(self, tool: str, args: dict[str, Any]) -> dict[str, Any]: ...


@dataclass
class Outcome:
    status: str                     # done | budget | expired | error
    turns: int = 0
    final_text: str = ""
    tool_calls: list[dict[str, Any]] = field(default_factory=list)
    reason: str = ""

    def as_dict(self) -> dict[str, Any]:
        return {"status": self.status, "turns": self.turns, "final_text": self.final_text,
                "tool_calls": self.tool_calls, "reason": self.reason}


SCOPED = "The tools you have are the ones this session may call; arguments are still checked on every call."
UNSCOPED = ("The tool list is the deployment's whole registry, so some tools are outside this session's grant and will "
            "be refused.")


def system_prompt(agent: str, task: dict[str, Any], *, ids: dict[str, str] | None = None, scoped: bool = False) -> str:
    s = SYSTEM.format(agent=agent or "unnamed agent", tools_note=SCOPED if scoped else UNSCOPED)
    facts = {k: v for k, v in (ids or {}).items() if v}
    if facts:
        # The run's own ids, so instructions that name them ("a branch named agent/<task id>") can be followed.
        s += "\n\nThis run: " + ", ".join(f"{k} {v}" for k, v in facts.items()) + "."
    if isinstance(task.get("instructions"), str) and task["instructions"].strip():
        s += "\n\nInstructions for this agent:\n" + task["instructions"].strip()
    return s


def first_message(task: dict[str, Any]) -> str | None:
    """The user turn: the prompt, then the triggering event as JSON. None when the input has neither."""
    parts = []
    if isinstance(task.get("prompt"), str) and task["prompt"].strip():
        parts.append(task["prompt"].strip())
    if task.get("event") is not None:
        parts.append("The event that started this run:\n" + json.dumps(task["event"], indent=2, sort_keys=True))
    return "\n\n".join(parts) or None


def text_of(content: list[dict[str, Any]]) -> str:
    return "\n".join(b.get("text", "") for b in content if b.get("type") == "text").strip()


def run(model: Model, gateway: Gateway, *, agent: str, task: dict[str, Any], model_name: str, max_turns: int,
        max_tokens: int, expires_at: datetime, grant: list[str] | None = None, ids: dict[str, str] | None = None,
        clock: Callable[[], datetime] = lambda: datetime.now(timezone.utc),
        log: Callable[[str], None] = print) -> Outcome:
    out = Outcome("error")
    prompt = first_message(task)
    if prompt is None:
        out.reason = "HANGAR_INPUT has no prompt and no event: nothing to do"
        return out
    try:
        toolset = T.build_toolset(gateway.tools_list(), grant)
    except (ClearanceError, TokenError) as e:
        out.reason = f"tools_list: {e}"
        return out
    log(f"start: model={model_name} tools={len(toolset.tools)} max_turns={max_turns}"
        + (f" skipped={','.join(toolset.skipped)}" if toolset.skipped else ""))
    messages: list[dict[str, Any]] = [{"role": "user", "content": prompt}]
    system = system_prompt(agent, task, ids=ids, scoped=bool(getattr(gateway, "scoped", False)))
    while True:
        if expires_at - clock() < EXPIRY_MARGIN:
            out.status, out.reason = "expired", "less than 60s of the session left"
            return out
        if out.turns >= max_turns:
            out.status, out.reason = "budget", f"max turns ({max_turns}) reached"
            return out
        try:
            resp = model.create(model=model_name, max_tokens=max_tokens, **cached(system, toolset.tools, messages))
        except ModelStop as e:
            out.status = "budget" if e.status == 429 else "error"
            out.reason = f"model proxy {e.status}: {e}"
            return out
        except (ModelError, TokenError) as e:
            out.reason = str(e)
            return out
        out.turns += 1
        use_note = _usage(resp.get("usage"))
        content = resp.get("content") or []
        stop = resp.get("stop_reason")
        out.final_text = text_of(content) or out.final_text
        uses = [b for b in content if b.get("type") == "tool_use"]
        messages.append({"role": "assistant", "content": content})
        if not uses:
            log(f"turn {out.turns}: stop={stop}{use_note}")
            if stop in ("end_turn", "stop_sequence"):
                out.status = "done"
                return out
            if stop == "pause_turn":
                continue
            out.reason = f"the model stopped with {stop}"
            return out
        if out.turns >= max_turns:   # the model would never see these results: do not spend calls on them
            log(f"turn {out.turns}: stop={stop} tools=[{_notes(toolset, uses, ':not-run')}]")
            out.status, out.reason = "budget", f"max turns ({max_turns}) reached"
            return out
        results, notes = [], []
        for u in uses:
            try:
                r, note = _run_tool(gateway, toolset, u, out)
            except (ClearanceError, TokenError) as e:
                log(f"turn {out.turns}: stop={stop} tools=[{', '.join(notes + [_notes(toolset, [u], ':failed')])}]")
                out.reason = f"gateway: {e}"
                return out
            results.append(r)
            notes.append(note)
        log(f"turn {out.turns}: stop={stop} tools=[{', '.join(notes)}]{use_note}")
        messages.append({"role": "user", "content": results})


def _usage(u: Any) -> str:
    """The turn's token use for the log: input, cache read, cache write, output. Empty when the answer has none."""
    if not isinstance(u, dict):
        return ""
    keys = ("input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "output_tokens")
    return " usage=" + "/".join(str(int(u.get(k) or 0)) for k in keys)


CACHE = {"type": "ephemeral"}


def cached(system: str, tools: list[dict[str, Any]], messages: list[dict[str, Any]]) -> dict[str, Any]:
    """The request's system, tools and messages with prompt-cache breakpoints, on copies (the conversation keeps
    none, so there are never more than three): after the tools and the system prompt, which never change, and
    after the newest message, so each turn reads everything before it from the cache and writes only what is new.
    Every turn resends the whole conversation; cached, it costs the session a tenth (clearance USAGE_WEIGHTS)."""
    tools_c = [dict(t) for t in tools]
    if tools_c:
        tools_c[-1]["cache_control"] = CACHE
    msgs = [dict(m) for m in messages]
    last = msgs[-1]
    content = last["content"]
    blocks = [{"type": "text", "text": content}] if isinstance(content, str) else [dict(b) for b in content]
    if blocks:
        blocks[-1]["cache_control"] = CACHE
    last["content"] = blocks
    return {"system": [{"type": "text", "text": system, "cache_control": CACHE}], "tools": tools_c, "messages": msgs}


def _notes(toolset: T.Toolset, uses: list[dict[str, Any]], suffix: str) -> str:
    return ", ".join((toolset.clearance_name(u["name"]) or u["name"]) + suffix for u in uses)


def _run_tool(gateway: Gateway, toolset: T.Toolset, use: dict[str, Any], out: Outcome) -> tuple[dict, str]:
    """One tool_use through Clearance: (its tool_result, its log note)."""
    dotted = toolset.clearance_name(use["name"])
    if dotted is None:
        out.tool_calls.append({"tool": use["name"], "ok": False, "rule": None})
        return T.error_result(use["id"], f"There is no tool named {use['name']}."), f"{use['name']}:unknown"
    args = use.get("input") if isinstance(use.get("input"), dict) else {}
    r = gateway.call(dotted, args)
    ok, rule = bool(r.get("ok")), T.rule_of(r)
    out.tool_calls.append({"tool": dotted, "ok": ok, "rule": rule})
    d = r.get("decision") if isinstance(r.get("decision"), dict) else {}
    verdict = "ok" if ok else ("deny" if d.get("allow") is False else "error")
    return T.tool_result(use["id"], r, dotted), f"{dotted}:{verdict}" + (f"({rule})" if rule and not ok else "")
