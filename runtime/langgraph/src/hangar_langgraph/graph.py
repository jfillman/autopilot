"""The graph: model -> tools -> model ... -> END, with the generic loop's stops (hangar_agent.loop) as its edges.

The state is the conversation (`messages`, Anthropic's shape) and where to go next (`route`). How the run ends is the
hangar_agent Outcome the nodes fill in, so result.json and run_finish are the generic runtime's, field for field.
"""
from __future__ import annotations

from datetime import datetime, timezone
from typing import Any, Callable, TypedDict

from langgraph.graph import END, START, StateGraph

from hangar_agent import loop
from hangar_agent import tools as T
from hangar_agent.clearance import ClearanceError
from hangar_agent.env import TokenError
from hangar_agent.model import ModelError, ModelStop


class RunState(TypedDict):
    messages: list[dict[str, Any]]
    route: str      # "tools", "model" or "end": set by the node that just ran


def build(model: loop.Model, gateway: loop.Gateway, toolset: T.Toolset, out: loop.Outcome, *, agent: str,
          task: dict[str, Any], model_name: str, max_turns: int, max_tokens: int, expires_at: datetime,
          ids: dict[str, str] | None = None, clock: Callable[[], datetime] = lambda: datetime.now(timezone.utc),
          log: Callable[[str], None] = print):
    """The compiled graph for one run. `out` is filled in as it runs."""
    system = loop.system_prompt(agent, task, ids=ids, scoped=bool(getattr(gateway, "scoped", False)))

    def model_node(state: RunState) -> RunState:
        messages = state["messages"]
        if expires_at - clock() < loop.EXPIRY_MARGIN:
            out.status, out.reason = "expired", "less than 60s of the session left"
            return {"messages": messages, "route": "end"}
        if out.turns >= max_turns:
            out.status, out.reason = "budget", f"max turns ({max_turns}) reached"
            return {"messages": messages, "route": "end"}
        try:
            resp = model.create(model=model_name, max_tokens=max_tokens, **loop.cached(system, toolset.tools, messages))
        except ModelStop as e:
            out.status = "budget" if e.status == 429 else "error"
            out.reason = f"model proxy {e.status}: {e}"
            return {"messages": messages, "route": "end"}
        except (ModelError, TokenError) as e:
            out.reason = str(e)
            return {"messages": messages, "route": "end"}
        out.turns += 1
        content = resp.get("content") or []
        stop = resp.get("stop_reason")
        out.final_text = loop.text_of(content) or out.final_text
        messages = [*messages, {"role": "assistant", "content": content}]
        if any(b.get("type") == "tool_use" for b in content):
            if out.turns >= max_turns:   # the model would never see the results: do not spend calls on them
                log(f"turn {out.turns}: stop={stop} tools not run: max turns")
                out.status, out.reason = "budget", f"max turns ({max_turns}) reached"
                return {"messages": messages, "route": "end"}
            return {"messages": messages, "route": "tools"}
        log(f"turn {out.turns}: stop={stop}{loop._usage(resp.get('usage'))}")
        if stop in ("end_turn", "stop_sequence"):
            out.status = "done"
            return {"messages": messages, "route": "end"}
        if stop == "pause_turn":
            return {"messages": messages, "route": "model"}
        out.reason = f"the model stopped with {stop}"
        return {"messages": messages, "route": "end"}

    def tools_node(state: RunState) -> RunState:
        messages = state["messages"]
        uses = [b for b in messages[-1]["content"] if b.get("type") == "tool_use"]
        results, notes = [], []
        for u in uses:
            try:
                r, note = loop._run_tool(gateway, toolset, u, out)
            except (ClearanceError, TokenError) as e:
                log(f"turn {out.turns}: tools=[{', '.join(notes)}] failed")
                out.reason = f"gateway: {e}"
                return {"messages": messages, "route": "end"}
            results.append(r)
            notes.append(note)
        log(f"turn {out.turns}: tools=[{', '.join(notes)}]")
        return {"messages": [*messages, {"role": "user", "content": results}], "route": "model"}

    g = StateGraph(RunState)
    g.add_node("model", model_node)
    g.add_node("tools", tools_node)
    g.add_edge(START, "model")
    g.add_conditional_edges("model", lambda s: s["route"], {"tools": "tools", "model": "model", "end": END})
    g.add_conditional_edges("tools", lambda s: s["route"], {"model": "model", "end": END})
    return g.compile()


def run(model: loop.Model, gateway: loop.Gateway, *, agent: str, task: dict[str, Any], model_name: str,
        max_turns: int, max_tokens: int, expires_at: datetime, grant: list[str] | None = None,
        ids: dict[str, str] | None = None, clock: Callable[[], datetime] = lambda: datetime.now(timezone.utc),
        log: Callable[[str], None] = print) -> loop.Outcome:
    """loop.run's contract, on the graph."""
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
    log(f"start: graph=langgraph model={model_name} tools={len(toolset.tools)} max_turns={max_turns}")
    graph = build(model, gateway, toolset, out, agent=agent, task=task, model_name=model_name, max_turns=max_turns,
                  max_tokens=max_tokens, expires_at=expires_at, ids=ids, clock=clock, log=log)
    # Each turn is two steps (model, tools), and pause_turn adds one: the turn limit, not this, is what stops a run.
    graph.invoke({"messages": [{"role": "user", "content": prompt}], "route": "model"},
                 config={"recursion_limit": 3 * max_turns + 5})
    return out
