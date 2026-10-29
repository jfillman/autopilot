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

import json
import os
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
    lock = gateway.lock
    verifier = GatewayTokenVerifier(gateway.auth) if auth is not None else None
    mcp = MCPServer("clearance", token_verifier=verifier, auth=auth, instructions=(
        "Governed access to Hangar. Open a session, then call tools inside it. Every call is checked "
        "and audited. A denial names a rule id and a hint; do not retry a denied call unchanged."))

    @mcp.tool(description="List the tools this deployment offers, with their tier and required arguments. With "
                          "`session` (one of your own), only the tools that session may call and this deployment runs.")
    def tools_list(session: str | None = None) -> list[dict[str, Any]]:
        names = None
        if session:
            with lock:
                names = gateway.session_tools(token(), session)
            if names is None:
                raise ValueError("no such session of yours")
        return [{"name": t.name, "tier": t.tier.name, "description": t.description,
                 "required_args": list(t.required_args), "propose_only": t.propose_only}
                for t in gateway.tools.values() if names is None or t.name in names]

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


# -- the model proxy -----------------------------------------------------------------------------------------------

ANTHROPIC_VERSION = "2023-06-01"
MAX_BODY = 4 << 20


def _anthropic_post(url: str, key: str, version: str, body: bytes, timeout: float) -> tuple[int, bytes]:
    import urllib.error
    import urllib.request
    req = urllib.request.Request(url, method="POST", data=body, headers={
        "x-api-key": key, "anthropic-version": version, "content-type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:  # noqa: S310 - the URL is configuration
            return r.status, r.read(MAX_BODY)
    except urllib.error.HTTPError as e:
        return e.code, e.read(64 << 10)


def _estimate(payload: dict[str, Any], raw: bytes) -> int:
    """Tokens a call may use before it is made: its max_tokens plus its input at ~4 bytes a token, so the budget
    check refuses a call that could overrun it. The real usage is charged after the call."""
    return int(payload.get("max_tokens") or 0) + len(raw) // 4


def _error(status: int, kind: str, message: str, rule: str | None = None) -> JSONResponse:
    body: dict[str, Any] = {"type": "error", "error": {"type": kind, "message": message}}
    if rule:
        body["error"]["rule"] = rule
    return JSONResponse(body, status_code=status)


def _locked(gateway: Gateway, fn: Callable[..., Any]) -> Callable[..., Any]:
    def run(*a):
        with gateway.lock:
            return fn(*a)
    return run


def create_model_app(gateway: Gateway, *, key_file: str, timeout: float = 300.0,
                     post: Callable[[str, str, str, bytes, float], tuple[int, bytes]] = _anthropic_post):
    """The model proxy over HTTP: Anthropic's Messages API (`POST /v1/messages`, non-streaming) for runs, which hold
    no model key. A run sends its model-proxy service-account token as `x-api-key` (what the Anthropic SDKs send
    for `api_key`) and its session in `x-hangar-session`. Each call is decided by Gateway.model_authorize (the
    session's model allowlist, network mode, breaker and token budget, recorded in the evidence chain), forwarded
    to the route's upstream with the provider key read from `key_file` on every call (never logged, never
    returned), and its actual usage charged back with Gateway.model_usage. Only the request body is forwarded: no
    client header reaches the provider, and `model` is rewritten to the route's upstream model.

    Refusals are Anthropic-shaped errors with the Clearance rule: 401 for a bad token, 403 for a policy denial, 429
    for the token budget (M007), 400 for a request the proxy does not take (streaming, no model)."""
    from pathlib import Path

    from starlette.applications import Starlette
    from starlette.routing import Route

    key_path = Path(key_file)

    async def messages(request: Request) -> JSONResponse:
        token = request.headers.get("x-api-key") or ""
        if not token and request.headers.get("authorization", "").lower().startswith("bearer "):
            token = request.headers["authorization"][7:]
        session_id = request.headers.get("x-hangar-session") or ""
        raw = await request.body()
        if len(raw) > MAX_BODY:
            return _error(413, "request_too_large", "request body too large")
        try:
            payload = json.loads(raw)
        except ValueError:
            return _error(400, "invalid_request_error", "body is not JSON")
        if not isinstance(payload, dict) or not isinstance(payload.get("model"), str):
            return _error(400, "invalid_request_error", "a Messages API body with a model is required")
        if payload.get("stream"):
            return _error(400, "invalid_request_error", "streaming is not supported by this proxy")
        if not token or not session_id:
            return _error(401, "authentication_error", "x-api-key (the run's model-proxy token) and x-hangar-session are required")
        try:
            d = await anyio.to_thread.run_sync(_locked(gateway, gateway.model_authorize), token, session_id,
                                               payload["model"], _estimate(payload, raw))
        except AuthError:
            return _error(401, "authentication_error", "token refused")
        if not d.allow:
            return _error(429 if d.rule == "M007" else 403,
                          "rate_limit_error" if d.rule == "M007" else "permission_error", d.message, d.rule)
        if d.route.backend != "anthropic":
            return _error(501, "api_error", f"backend {d.route.backend!r} is not forwarded by this proxy")
        body = json.dumps({**payload, "model": d.route.upstream_model}).encode()
        version = request.headers.get("anthropic-version") or ANTHROPIC_VERSION
        try:
            status, out = await anyio.to_thread.run_sync(
                post, d.route.upstream.rstrip("/") + "/v1/messages", key_path.read_text().strip(), version, body, timeout)
        except OSError:
            return _error(502, "api_error", "the model provider is unreachable")
        try:
            doc = json.loads(out)
        except ValueError:
            return _error(502, "api_error", f"the model provider answered {status} with no JSON")
        usage = doc.get("usage") if isinstance(doc, dict) else None
        if isinstance(usage, dict):
            used = sum(int(usage.get(k) or 0) for k in ("input_tokens", "output_tokens", "cache_creation_input_tokens",
                                                        "cache_read_input_tokens"))
            await anyio.to_thread.run_sync(_locked(gateway, gateway.model_usage), session_id, used)
        return JSONResponse(doc, status_code=status)

    async def healthz(_: Request) -> JSONResponse:
        return JSONResponse({"ok": True, "routes": sorted(gateway.models.routes) if gateway.models else []})

    return Starlette(routes=[Route("/v1/messages", messages, methods=["POST"]),
                             Route("/healthz", healthz, methods=["GET"])])
