"""Clearance's tools as Anthropic tools, and Clearance's answers as tool results.

tools_list is the deployment's whole registry, not what this session may call: the runtime cannot see the agent
definition, and Clearance is the one that decides. So every tool is offered (unless the run is told its grant), and a
refusal (rule and hint) goes back to the model as an is_error tool_result it can learn from.

Names: Clearance's are dotted (glidepath.run.get); Anthropic's must match ^[a-zA-Z0-9_-]{1,64}$. A dot becomes `__`
and the way back is a lookup table built from tools_list, not a string replace, so a name that already holds `__` or
`-` still maps back exactly. A name that cannot be expressed is left out and logged.

Schemas: tools_list gives only the names of the required arguments, so each becomes a property (a string unless the
type is known below) and additionalProperties stays true for the optional ones the description mentions.
"""
from __future__ import annotations

import json
import re
from dataclasses import dataclass
from typing import Any

NAME_RE = re.compile(r"^[a-zA-Z0-9_-]{1,64}$")
MAX_RESULT_CHARS = 60_000   # one tool result's share of the context; the rest is cut and the cut is said

# Required arguments whose type is not a string, from Clearance's tool specs (clearance.packs, the core registry).
KNOWN_TYPES: dict[str, dict[str, Any]] = {
    "files": {"type": "array", "items": {"type": "object"}, "description": "[{path, content}]"},
    "documents": {"type": "array", "items": {"type": "object"}},
    "spec": {"type": "object"},
    "limits": {"type": "object"},
}


def to_model_name(dotted: str) -> str:
    return dotted.replace(".", "__")


@dataclass(frozen=True)
class Toolset:
    tools: list[dict[str, Any]]     # Anthropic tool definitions
    names: dict[str, str]           # model name -> Clearance name
    skipped: list[str]              # Clearance names that cannot be model names

    def clearance_name(self, model_name: str) -> str | None:
        return self.names.get(model_name)


def schema_for(required: list[str]) -> dict[str, Any]:
    props = {a: dict(KNOWN_TYPES.get(a, {"type": "string"})) for a in required}
    return {"type": "object", "properties": props, "required": list(required), "additionalProperties": True}


def describe(t: dict[str, Any]) -> str:
    notes = [f"tier {t['tier']}"] if t.get("tier") else []
    if t.get("propose_only"):
        notes.append("propose-only: it proposes a change for a human or pipeline to apply")
    text = (t.get("description") or t["name"]).strip()
    return f"{text} ({'; '.join(notes)})" if notes else text


def build_toolset(registry: list[dict[str, Any]], grant: list[str] | None = None) -> Toolset:
    """`grant`, when the run is told its session's tools (HANGAR_LIMITS "tools"; AgentRun limits do not carry it
    today), narrows the offer to them. Without it, the whole registry is offered and Clearance refuses the rest."""
    tools, names, skipped = [], {}, []
    for t in registry:
        dotted = t.get("name")
        if not isinstance(dotted, str) or (grant is not None and dotted not in grant):
            continue
        name = to_model_name(dotted)
        if not NAME_RE.match(name) or name in names:
            skipped.append(dotted)
            continue
        names[name] = dotted
        tools.append({"name": name, "description": describe(t),
                      "input_schema": schema_for([a for a in t.get("required_args") or [] if isinstance(a, str)])})
    return Toolset(tools, names, skipped)


# -- results --------------------------------------------------------------------------------------------------------
def rule_of(result: dict[str, Any]) -> str | None:
    d = result.get("decision")
    return d.get("rule") if isinstance(d, dict) else None


def tool_result(tool_use_id: str, result: dict[str, Any]) -> dict[str, Any]:
    """An Anthropic tool_result for Clearance's answer. ok: the data. Not ok: a refusal (the rule, its message and
    hint) or a backend error, with is_error so the model treats it as one."""
    if result.get("ok"):
        return {"type": "tool_result", "tool_use_id": tool_use_id, "content": _text(result.get("data"))}
    d = result.get("decision") if isinstance(result.get("decision"), dict) else {}
    denied = d.get("allow") is False
    body: dict[str, Any] = {"refused": denied}
    for k in ("rule", "name", "message", "hint", "problems"):
        if d.get(k):
            body[k] = d[k]
    if result.get("error"):
        body["error"] = result["error"]
    body["advice"] = ("Clearance refused this call. Read the hint and change the approach; do not repeat the same "
                      "call." if denied else "The call was allowed but did not succeed.")
    return {"type": "tool_result", "tool_use_id": tool_use_id, "content": _text(body), "is_error": True}


def error_result(tool_use_id: str, message: str) -> dict[str, Any]:
    return {"type": "tool_result", "tool_use_id": tool_use_id, "content": message, "is_error": True}


def _text(v: Any) -> str:
    s = v if isinstance(v, str) else json.dumps(v, separators=(",", ":"), default=str)
    if len(s) > MAX_RESULT_CHARS:
        s = s[:MAX_RESULT_CHARS] + f"... [cut: {len(s) - MAX_RESULT_CHARS} more characters]"
    return s
