"""MCP surface for the gateway core (mcp SDK v2, `MCPServer`).

Deliberately thin: three tools, all delegating to Gateway, which owns every decision.
  tools_list    what this deployment offers (the registry its Gateway was built with: core plus
                its packs), with tiers and required arguments
  session_open  open a session for an agent definition, optionally narrowing it with requested limits
  call          call one tool inside a session

Authentication is a token provider. In stdio development it reads CLEARANCE_TOKEN. Over HTTP
(create_http_app) the caller's bearer token is verified before any tool runs, by the gateway's own
Authenticator (clearance.auth), and the same token is what the gateway authenticates again per call,
so a session's owner is always the caller of the request at hand. Calls into the gateway are
serialized: the session store and the audit chain are single-writer.
"""
from __future__ import annotations

import os
import threading
from typing import Any, Callable

import anyio
import jwt
from mcp.server.auth.middleware.auth_context import get_access_token
from mcp.server.auth.provider import AccessToken
from mcp.server.auth.settings import AuthSettings
from mcp.server.mcpserver import MCPServer
from starlette.requests import Request
from starlette.responses import JSONResponse

from .dispatch import AuthError, Authenticator, Gateway


class GatewayTokenVerifier:
    """The MCP SDK's TokenVerifier over a Clearance Authenticator: a token the authenticator refuses gets a 401
    before any tool runs. The authenticator checks the audience itself."""

    def __init__(self, auth: Authenticator):
        self.auth = auth

    async def verify_token(self, token: str) -> AccessToken | None:
        try:
            principal = await anyio.to_thread.run_sync(self.auth.authenticate, token)
        except AuthError:
            return None
        try:
            exp = jwt.decode(token, options={"verify_signature": False}).get("exp")
        except jwt.PyJWTError:
            exp = None
        return AccessToken(token=token, client_id=principal.id, scopes=[],
                           expires_at=int(exp) if isinstance(exp, (int, float)) else None, subject=principal.id)


def _request_token() -> str:
    t = get_access_token()
    if t is None:
        raise AuthError("no verified bearer token on this request")
    return t.token


def create_server(gateway: Gateway, token_provider: Callable[[], str] | None = None, *,
                  auth: AuthSettings | None = None) -> MCPServer:
    token = token_provider or (lambda: os.environ["CLEARANCE_TOKEN"])
    lock = threading.Lock()
    verifier = GatewayTokenVerifier(gateway.auth) if auth is not None else None
    mcp = MCPServer("clearance", token_verifier=verifier, auth=auth, instructions=(
        "Governed access to Hangar. Open a session, then call tools inside it. Every call is checked "
        "and audited. A denial names a rule id and a hint; do not retry a denied call unchanged."))

    @mcp.tool(description="List the tools this deployment offers, with their tier and required arguments.")
    def tools_list() -> list[dict[str, Any]]:
        return [{"name": t.name, "tier": t.tier.name, "description": t.description,
                 "required_args": list(t.required_args), "propose_only": t.propose_only}
                for t in gateway.tools.values()]

    @mcp.tool(description="Open a session for an agent definition. `limits` may only narrow the definition's limits.")
    def session_open(agent: str, limits: dict[str, Any] | None = None) -> dict[str, Any]:
        with lock:
            return gateway.open_session(token(), agent, limits).as_dict()

    @mcp.tool(description="Call a tool inside a session. Returns ok, the decision (rule and hint on a denial), and data.")
    def call(session: str, tool: str, args: dict[str, Any] | None = None) -> dict[str, Any]:
        with lock:
            return gateway.call(token(), session, tool, args or {}).as_dict()

    return mcp


def create_http_app(gateway: Gateway, *, resource_url: str, issuer_url: str, path: str = "/mcp",
                    host: str = "0.0.0.0"):
    """The streamable-HTTP app: bearer auth on every MCP request (401 without a token the gateway accepts),
    the caller's own token per call, and an unauthenticated GET /healthz for probes.

    resource_url is this server's public URL (OAuth protected-resource metadata); issuer_url is the authorization
    server people get tokens from (Dex in the lab). Workload callers bring a projected service-account token."""
    settings = AuthSettings(issuer_url=issuer_url, resource_server_url=resource_url, validate_token_resource=False)
    mcp = create_server(gateway, _request_token, auth=settings)

    @mcp.custom_route("/healthz", methods=["GET"], include_in_schema=False)
    async def healthz(_: Request) -> JSONResponse:
        return JSONResponse({"ok": True, "tools": len(gateway.tools)})

    return mcp.streamable_http_app(streamable_http_path=path, stateless_http=True, json_response=True, host=host)
