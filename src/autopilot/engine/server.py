"""MCP surface for the gateway core (mcp SDK v2, `MCPServer`).

Deliberately thin: six tools, all delegating to Gateway, which owns every decision.
  tools_list    what this deployment offers (the registry its Gateway was built with: core plus
                its packs), with tiers and required arguments
  agents_list   the agent definitions: what each is for and where it runs
  session_open  open a session for an agent definition, optionally narrowing it with requested limits
  call          call one tool inside a session
  run_finish    an in-cluster run hands back its result as it exits and closes its session
  session_close the session's own caller ends it (owner-only), deleting its runs; recorded as session.end

Authentication is a token provider. In stdio development it reads CLEARANCE_TOKEN. Over HTTP
(create_http_app) the caller's bearer token is verified before any tool runs, by the gateway's own
Authenticator (auth.py), and the same token is what the gateway authenticates again per call,
so a session's owner is always the caller of the request at hand. Calls into the gateway are
serialized: the session store is single-writer.
"""
from __future__ import annotations

import functools
import json
import os
from typing import Any, Callable, Iterator

import anyio
import jwt
from mcp.server.auth.middleware.auth_context import get_access_token
from mcp.server.auth.provider import AccessToken
from mcp.server.auth.settings import AuthSettings
from mcp.server.mcpserver import MCPServer
from starlette.requests import Request
from starlette.responses import JSONResponse, Response, StreamingResponse

from . import control, inbox
from .core import OUTAGE_RULE
from .dispatch import AuthError, Authenticator, Gateway


class GatewayTokenVerifier:
    """The MCP SDK's TokenVerifier over the gateway's Authenticator: a token the authenticator refuses gets a 401
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

    @mcp.tool(description="List the agent definitions: what each is for (description), where it runs (runs_in: "
                          "\"client\" means your session is the agent, \"cluster\" means it also runs in-cluster), its kind, "
                          "tier ceiling and tools. Open a session on one with can_open_session true.")
    def agents_list() -> list[dict[str, Any]]:
        with lock:
            return gateway.agents_for(token())

    @mcp.tool(description="Open a session for an agent definition. `limits` may only narrow the definition's limits.")
    def session_open(agent: str, limits: dict[str, Any] | None = None) -> dict[str, Any]:
        with lock:
            return gateway.open_session(token(), agent, limits).as_dict()

    @mcp.tool(description="Call a tool inside a session. Returns ok, the decision (rule and hint on a denial), and data.")
    def call(session: str, tool: str, args: dict[str, Any] | None = None) -> dict[str, Any]:
        with lock:
            return gateway.call(token(), session, tool, args or {}).as_dict()

    @mcp.tool(description="For an in-cluster run only: hand back the run's result as it exits and close its session. "
                          "status is done, budget, expired or error; final_text is the answer (kept 30 days); "
                          "tool_calls is [{tool, ok, rule}].")
    def run_finish(session: str, status: str, final_text: str = "", turns: int = 0,
                   tool_calls: list[dict[str, Any]] | None = None, reason: str = "") -> dict[str, Any]:
        body = {"status": status, "final_text": final_text, "turns": turns, "tool_calls": tool_calls or [],
                "reason": reason}
        try:
            with lock:
                return {"ok": True, **gateway.finish_run(token(), session, body)}
        except control.ControlError as e:
            return {"ok": False, "rule": e.rule, "error": e.message}

    @mcp.tool(description="End one of your own sessions now instead of leaving it live until its TTL: it and its "
                          "children close, their in-cluster runs are deleted, and the end is recorded. Only the "
                          "session's own caller may; `reason` is optional and recorded.")
    def session_close(session: str, reason: str = "") -> dict[str, Any]:
        try:
            with lock:
                return {"ok": True, **gateway.close_session(token(), session, reason or None)}
        except control.ControlError as e:
            return {"ok": False, "rule": e.rule, "error": e.message}

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
        return JSONResponse({"ok": True, "tools": len(gateway.tools), "core": gateway.core.breaker_state})

    def _bearer(request: Request) -> str | None:
        auth = request.headers.get("authorization", "")
        return auth[7:].strip() if auth.lower().startswith("bearer ") and auth[7:].strip() else None

    async def _people(request: Request, op, *args, **kw) -> JSONResponse:
        """A person's human-inbox call (inbox.py): their own token, or a delegate's with X-Hangar-On-Behalf-Of."""
        token = _bearer(request)
        if token is None:
            return JSONResponse({"error": "a bearer token is required"}, status_code=401)
        who = request.headers.get("x-hangar-on-behalf-of") or None
        try:
            out = await anyio.to_thread.run_sync(_locked(gateway, functools.partial(op, on_behalf_of=who, **kw)), token, *args)
        except AuthError:
            return JSONResponse({"error": "token not accepted"}, status_code=401)
        except (inbox.InboxError, control.ControlError) as e:
            return JSONResponse({"error": e.message, "rule": e.rule, "hint": e.hint}, status_code=e.status)
        return JSONResponse(out)

    async def _body(request: Request) -> dict | JSONResponse:
        raw = await request.body()
        if len(raw) > 64 << 10:
            return JSONResponse({"error": "body too large"}, status_code=413)
        if not raw:
            return {}
        try:
            body = json.loads(raw)
        except ValueError:
            return JSONResponse({"error": "the body is not JSON"}, status_code=400)
        return body if isinstance(body, dict) else {}

    @mcp.custom_route("/v1/human/requests", methods=["GET"], include_in_schema=False)
    async def human_list(request: Request) -> JSONResponse:
        """The pending human.request questions the person may answer."""
        return await _people(request, gateway.human_requests)

    @mcp.custom_route("/v1/human/requests/{request_id}/answer", methods=["POST"], include_in_schema=False)
    async def human_answer(request: Request) -> JSONResponse:
        """Answer one: {"answer": "..."}."""
        raw = await request.body()
        if len(raw) > 64 << 10:
            return JSONResponse({"error": "body too large"}, status_code=413)
        try:
            body = json.loads(raw)
        except ValueError:
            return JSONResponse({"error": "the body is not JSON"}, status_code=400)
        answer = body.get("answer") if isinstance(body, dict) else None
        return await _people(request, gateway.human_answer, request.path_params["request_id"], answer)

    # Person controls (control.py, decided 2026-10-31: the delegate model for writes too): a person's own token, or a
    # delegate's naming them. Each is an evidence record.
    @mcp.custom_route("/v1/sessions/{session_id}/freeze", methods=["POST"], include_in_schema=False)
    async def freeze(request: Request) -> JSONResponse:
        """Trip a live session's breaker: {"reason": "..."}."""
        body = await _body(request)
        if isinstance(body, JSONResponse):
            return body
        return await _people(request, gateway.freeze_session, request.path_params["session_id"], body.get("reason"))

    @mcp.custom_route("/v1/sessions/{session_id}/end", methods=["POST"], include_in_schema=False)
    async def end(request: Request) -> JSONResponse:
        """Close a session and its children: {"reason": "..."}."""
        body = await _body(request)
        if isinstance(body, JSONResponse):
            return body
        return await _people(request, gateway.end_session, request.path_params["session_id"], body.get("reason"))

    @mcp.custom_route("/v1/agents/{agent}/hold", methods=["POST"], include_in_schema=False)
    async def hold(request: Request) -> JSONResponse:
        """Hold an agent's triggers: {"reason": "...", "hours": 24}."""
        body = await _body(request)
        if isinstance(body, JSONResponse):
            return body
        return await _people(request, gateway.hold_triggers, request.path_params["agent"], body.get("reason"), body.get("hours"))

    @mcp.custom_route("/v1/agents/{agent}/runs", methods=["POST"], include_in_schema=False)
    async def run(request: Request) -> JSONResponse:
        """Start an in-cluster run of a task agent as the person: {"input": "..."} (optional). With a `document` (a
        candidate agent definition, a PR's) and optional `budget` {ttl_minutes, tool_calls, model_tokens, children},
        a budgeted Preflight run of that candidate instead (Gateway._candidate). 201 on success."""
        body = await _body(request)
        if isinstance(body, JSONResponse):
            return body
        res = await _people(request, gateway.run_agent, request.path_params["agent"], body.get("input"),
                            document=body.get("document"), budget=body.get("budget"))
        if res.status_code == 200:
            res.status_code = 201
        return res

    @mcp.custom_route("/v1/preflight/cases", methods=["GET"], include_in_schema=False)
    async def preflight_cases(request: Request) -> JSONResponse:
        """The Preflight cases of an agent: ?agent=<name>."""
        return await _people(request, gateway.preflight_case_list, request.query_params.get("agent"))

    @mcp.custom_route("/v1/preflight/score", methods=["POST"], include_in_schema=False)
    async def preflight_score(request: Request) -> JSONResponse:
        """Score a candidate's Preflight runs: {"agent": name, "cases": [{"case": id, "sessions": [id, ...]}]}."""
        body = await _body(request)
        if isinstance(body, JSONResponse):
            return body
        return await _people(request, gateway.preflight_score, body)

    @mcp.custom_route("/v1/agents/{agent}/hold", methods=["DELETE"], include_in_schema=False)
    async def release(request: Request) -> JSONResponse:
        """Lift a hold before it expires."""
        return await _people(request, gateway.release_triggers, request.path_params["agent"])

    return mcp.streamable_http_app(streamable_http_path=path, stateless_http=True, json_response=True, host=host)


# -- the model proxy -----------------------------------------------------------------------------------------------

ANTHROPIC_VERSION = "2023-06-01"
MAX_BODY = 4 << 20
STREAM_CHUNK = 16 << 10


def _upstream_headers(key: str, version: str, betas: str | None) -> dict[str, str]:
    h = {"x-api-key": key, "anthropic-version": version, "content-type": "application/json"}
    if betas:
        h["anthropic-beta"] = betas
    return h


def _anthropic_post(url: str, key: str, version: str, body: bytes, timeout: float,
                    betas: str | None = None) -> tuple[int, bytes]:
    import urllib.error
    import urllib.request
    req = urllib.request.Request(url, method="POST", data=body, headers=_upstream_headers(key, version, betas))
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:  # noqa: S310 - the URL is configuration
            return r.status, r.read(MAX_BODY)
    except urllib.error.HTTPError as e:
        return e.code, e.read(64 << 10)


def _anthropic_stream(url: str, key: str, version: str, body: bytes, timeout: float,
                      betas: str | None = None) -> tuple[int, Iterator[bytes]]:
    """A streaming Messages call: (status, the response body's chunks as they arrive). An error answer is one
    chunk of JSON. The connection closes when the chunks are exhausted or the iterator is closed."""
    import urllib.error
    import urllib.request
    req = urllib.request.Request(url, method="POST", data=body, headers={
        **_upstream_headers(key, version, betas), "accept": "text/event-stream"})
    try:
        r = urllib.request.urlopen(req, timeout=timeout)  # noqa: S310 - the URL is configuration
    except urllib.error.HTTPError as e:
        return e.code, iter([e.read(64 << 10)])

    def chunks() -> Iterator[bytes]:
        with r:
            while True:
                b = r.read1(STREAM_CHUNK)
                if not b:
                    return
                yield b
    return r.status, chunks()


class StreamUsage:
    """The usage of a streaming Messages answer, read from its server-sent events as they pass: message_start
    carries the input side (input, cache write, cache read), message_delta the output so far (cumulative, and on
    newer APIs the input side again). Only `data:` lines are parsed; a line split across chunks is joined first."""

    def __init__(self):
        self.usage: dict[str, Any] = {}
        self.model: str | None = None   # message_start's message.model: the model that answered (AP-C1)
        self._buf = b""

    def feed(self, chunk: bytes) -> None:
        self._buf += chunk
        *lines, self._buf = self._buf.split(b"\n")
        for line in lines:
            if not line.startswith(b"data:"):
                continue
            try:
                ev = json.loads(line[5:])
            except ValueError:
                continue
            if not isinstance(ev, dict):
                continue
            if ev.get("type") == "message_start" and isinstance((ev.get("message") or {}).get("model"), str):
                self.model = ev["message"]["model"]
            u = (ev.get("message") or {}).get("usage") if ev.get("type") == "message_start" else ev.get("usage")
            if ev.get("type") in ("message_start", "message_delta") and isinstance(u, dict):
                self.usage.update({k: v for k, v in u.items() if isinstance(v, (int, float))})


def _betas(header: str | None) -> str | None:
    """The client's anthropic-beta values the provider is sent: every one but OAuth's, since the proxy calls with
    the deployment's API key, never a person's OAuth token. The one client header that is forwarded: Claude Code
    and the Agent SDK send betas their request bodies depend on."""
    keep = [b.strip() for b in (header or "").split(",") if b.strip() and not b.strip().startswith("oauth-")]
    return ",".join(keep) or None


def _estimate(payload: dict[str, Any], raw: bytes) -> int:
    """Tokens a call may use before it is made: its max_tokens plus its input at ~4 bytes a token, so the budget
    check refuses a call that could overrun it. The real usage is charged after the call."""
    return int(payload.get("max_tokens") or 0) + len(raw) // 4


def _error(status: int, kind: str, message: str, rule: str | None = None) -> JSONResponse:
    body: dict[str, Any] = {"type": "error", "error": {"type": kind, "message": message}}
    if rule:
        body["error"]["rule"] = rule
    return JSONResponse(body, status_code=status)


def _result(status: int) -> str:
    """A model call's outcome result line from the upstream's status."""
    return "ok" if 200 <= status < 300 else f"error:Upstream{status}"


def _locked(gateway: Gateway, fn: Callable[..., Any]) -> Callable[..., Any]:
    def run(*a, **kw):
        with gateway.lock:
            return fn(*a, **kw)
    return run


def create_model_app(gateway: Gateway, *, key_file: str, timeout: float = 300.0,
                     post: Callable[..., tuple[int, bytes]] = _anthropic_post,
                     stream: Callable[..., tuple[int, Iterator[bytes]]] = _anthropic_stream):
    """The model proxy over HTTP: Anthropic's Messages API (`POST /v1/messages`, plain or streaming) for runs, which hold
    no model key. A run sends its model-proxy service-account token as `x-api-key` (what the Anthropic SDKs send
    for `api_key`) and its session in `x-hangar-session`. Each call is decided by Gateway.model_authorize (the
    session's model allowlist, network mode, breaker and token budget, recorded in the evidence chain), forwarded
    to the route's upstream with the provider key read from `key_file` on every call (never logged, never
    returned), and its actual usage charged back with Gateway.model_usage, weighted by modelproxy.USAGE_WEIGHTS (a
    prompt-cache read costs a tenth of an input token). Only the request body is forwarded, with `model` rewritten
    to the route's upstream model, and of the client's headers only anthropic-beta (less OAuth's, _betas).

    Streaming (v0.26.0, `"stream": true`, which Claude Code and the Agent SDK always send): decided the same way,
    the provider's server-sent events relayed as they arrive, and the usage they carry charged when the stream ends,
    however it ends (StreamUsage).

    Every allowed call gets exactly one outcome (Gateway.model_outcome, POST /v1/decisions/{id}/outcome), written as
    the gateway's provider: "ok" for a 2xx answer (a stream that ran to its end), else "error:<Kind>", with the
    receipt `stream` (true for a streamed call) and `upstream_status`. A streamed call is provable from the chain.
    The same outcome carries the flight recorder's fields (A-4): `model_version` (the model that answered) and,
    for a call that used tokens, `usage` (the four kinds and `charged_tokens`, the call's weighted charge;
    the session's budget counts it up to its limit, and an overrun trips the breaker while the record keeps it whole).

    Refusals are Anthropic-shaped errors with the Clearance rule: 401 for a bad token, 403 for a policy denial, 429
    for the token budget (M007), 400 for a request the proxy does not take (no model)."""
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
        if not token or not session_id:
            return _error(401, "authentication_error", "x-api-key (the run's model-proxy token) and x-hangar-session are required")
        try:
            d = await anyio.to_thread.run_sync(_locked(gateway, gateway.model_authorize), token, session_id,
                                               payload["model"], _estimate(payload, raw))
        except AuthError:
            return _error(401, "authentication_error", "token refused")
        if not d.allow and d.rule == OUTAGE_RULE:
            return _error(503, "api_error", d.message, d.rule)
        if not d.allow:
            return _error(429 if d.rule == "M007" else 403,
                          "rate_limit_error" if d.rule == "M007" else "permission_error", d.message, d.rule)
        streamed = bool(payload.get("stream"))

        async def report(result: str, upstream_status: int | None = None, usage: dict | None = None,
                         answered: Any = None) -> None:
            """The call's one outcome. A call that used tokens is charged first (Gateway.model_usage: the budget, the
            session's running cost) and its usage and charge go on this outcome with the model that answered (the
            answer's own `model`, else the route's upstream model): A-4's flight recorder fields."""
            version = answered if isinstance(answered, str) and answered else d.route.upstream_model
            with anyio.CancelScope(shield=True):
                rec = None
                if isinstance(usage, dict) and usage:
                    rec = await anyio.to_thread.run_sync(_locked(gateway, gateway.model_usage), session_id, usage)
                await anyio.to_thread.run_sync(functools.partial(
                    _locked(gateway, functools.partial(gateway.model_outcome, stream=streamed,
                                                       upstream_status=upstream_status, usage=rec,
                                                       model_version=version)), d.decision_id, result))

        if d.route.backend != "anthropic":
            await report("error:UnsupportedBackend")
            return _error(501, "api_error", f"backend {d.route.backend!r} is not forwarded by this proxy")
        body = json.dumps({**payload, "model": d.route.upstream_model}).encode()
        version = request.headers.get("anthropic-version") or ANTHROPIC_VERSION
        betas = _betas(request.headers.get("anthropic-beta"))
        extra = {"betas": betas} if betas else {}
        url, key = d.route.upstream.rstrip("/") + "/v1/messages", key_path.read_text().strip()
        if streamed:
            return await _relay(session_id, url, key, version, body, extra, report)
        try:
            status, out = await anyio.to_thread.run_sync(
                functools.partial(post, url, key, version, body, timeout, **extra))
        except OSError:
            await report("error:ModelUnreachable")
            return _error(502, "api_error", "the model provider is unreachable")
        try:
            doc = json.loads(out)
        except ValueError:
            await report("error:BadUpstreamAnswer", status)
            return _error(502, "api_error", f"the model provider answered {status} with no JSON")
        usage = doc.get("usage") if isinstance(doc, dict) else None
        await report(_result(status), status, usage if isinstance(usage, dict) else None,
                     doc.get("model") if isinstance(doc, dict) else None)
        return JSONResponse(doc, status_code=status)

    async def _relay(session_id: str, url: str, key: str, version: str, body: bytes, extra: dict, report) -> Response:
        try:
            status, chunks = await anyio.to_thread.run_sync(
                functools.partial(stream, url, key, version, body, timeout, **extra))
        except OSError:
            await report("error:ModelUnreachable")
            return _error(502, "api_error", "the model provider is unreachable")
        if status != 200:
            out = b"".join(chunks)
            await report(_result(status), status)
            return Response(out, status_code=status, media_type="application/json")
        meter = StreamUsage()

        async def events():
            ended = False
            try:
                while True:
                    chunk = await anyio.to_thread.run_sync(next, chunks, None)
                    if chunk is None:
                        ended = True
                        return
                    meter.feed(chunk)
                    yield chunk
            finally:
                close = getattr(chunks, "close", None)
                if close:
                    close()
                # A stream that did not run to its end (the run went away, the provider dropped it) is not "ok";
                # what was metered before it broke is still charged and recorded.
                await report("ok" if ended else "error:StreamInterrupted", status, meter.usage or None, meter.model)

        return StreamingResponse(events(), media_type="text/event-stream", headers={"cache-control": "no-cache"})

    async def healthz(_: Request) -> JSONResponse:
        return JSONResponse({"ok": True, "routes": sorted(gateway.models.routes) if gateway.models else []})

    return Starlette(routes=[Route("/v1/messages", messages, methods=["POST"]),
                             Route("/healthz", healthz, methods=["GET"])])
