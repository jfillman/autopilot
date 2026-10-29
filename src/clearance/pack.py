"""Packs: the policy for a family of tools that not every deployment has.

Clearance's core knows tiers, scope, limits, sessions, the run tree and audit, and the generic tools
(tiers.CORE_TOOLS). A pack adds the policy for tools that only exist where some platform does, e.g.
clearance.packs.hangar for Glidepath and Airframe. A pack is data plus two optional functions:

  tools     ToolSpecs added to the registry (a name registered twice is an error)
  rules     CEL Rules, each with an explicit `order` among the core rules (first match wins)
  context   (tool, spec, args, session, definition) -> (ctx entries, bad-argument count): what the
            pack's rules read, under `ctx`; the count is added to ctx.missing_args (R008)
  decide    (rule, context) -> Decision | None: a matched rule's decision, for a rule that
            reports structured problems instead of its own id; None leaves the default

A pack holds policy only. What a tool does is a handler the deployment registers with the Gateway,
so the same pack works whatever backs it. Packs are passed explicitly; nothing registers on import.
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Callable

from .policy import Decision, Rule
from .session import Session
from .tiers import ToolSpec

ContextFn = Callable[[str, "ToolSpec | None", dict[str, Any], Session, Any], "tuple[dict[str, Any], int]"]
DecideFn = Callable[[Rule, dict[str, Any]], "Decision | None"]


@dataclass(frozen=True)
class Pack:
    name: str
    tools: tuple[ToolSpec, ...] = ()
    rules: tuple[Rule, ...] = ()
    context: ContextFn | None = None
    decide: DecideFn | None = None
