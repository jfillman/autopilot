"""MCP surface for the gateway core (mcp SDK v2, `MCPServer`).

Deliberately thin: three tools, all delegating to Gateway, which owns every decision.
  tools_list    what this deployment offers, with tiers and required arguments
  session_open  open a session for an agent definition, optionally narrowing it with requested limits
  call          call one tool inside a session

Authentication is a token provider, not something this module knows about. In stdio
development it reads CLEARANCE_TOKEN. The HTTP deployment (a per-request bearer from the
Authorization header, verified by Tower auth or TokenReview) is NOT built yet.
"""
from __future__ import annotations

import os
from typing import Any, Callable

from mcp.server.mcpserver import MCPServer

from .dispatch import Gateway
from .tiers import TOOLS


def create_server(gateway: Gateway, token_provider: Callable[[], str] | None = None) -> MCPServer:
    token = token_provider or (lambda: os.environ["CLEARANCE_TOKEN"])
    mcp = MCPServer("clearance", instructions=(
        "Governed access to Hangar. Open a session, then call tools inside it. Every call is checked "
        "and audited. A denial names a rule id and a hint; do not retry a denied call unchanged."))

    @mcp.tool(description="List the tools this deployment offers, with their tier and required arguments.")
    def tools_list() -> list[dict[str, Any]]:
        return [{"name": t.name, "tier": t.tier.name, "description": t.description,
                 "required_args": list(t.required_args), "propose_only": t.propose_only}
                for t in TOOLS.values()]

    @mcp.tool(description="Open a session for an agent definition. `limits` may only narrow the definition's limits.")
    def session_open(agent: str, limits: dict[str, Any] | None = None) -> dict[str, Any]:
        return gateway.open_session(token(), agent, limits).as_dict()

    @mcp.tool(description="Call a tool inside a session. Returns ok, the decision (rule and hint on a denial), and data.")
    def call(session: str, tool: str, args: dict[str, Any] | None = None) -> dict[str, Any]:
        return gateway.call(token(), session, tool, args or {}).as_dict()

    return mcp
