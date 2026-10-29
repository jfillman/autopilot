"""The Clearance gateway client: MCP tools/call over streamable HTTP, as plain JSON-RPC with the stdlib.

The gateway serves stateless JSON (Clearance's create_http_app: stateless_http=True, json_response=True), so a single
POST per call is the whole protocol: no initialize, no session id, no event stream. That is why this is not the mcp
SDK's client, which would bring the gateway's server stack (starlette, uvicorn, pydantic) into the agent image and an
async session for a request/response exchange.

Only `tools_list` and `call` are used. The run's session already exists (HANGAR_SESSION_ID); session_open is not
this runtime's to call. The bearer token is the run's projected clearance-token, read again on every request.
"""
from __future__ import annotations

import json
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Callable

from .env import read_token

MCP_PATH = "/mcp"
PROTOCOL_VERSION = "2025-06-18"
TIMEOUT = 60.0

# post(url, headers, body, timeout) -> (HTTP status, response body). Tests replace it.
Post = Callable[[str, dict[str, str], bytes, float], tuple[int, bytes]]


class ClearanceError(RuntimeError):
    """The gateway could not be asked, or did not answer as an MCP server. Never carries the token."""


def urllib_post(url: str, headers: dict[str, str], body: bytes, timeout: float) -> tuple[int, bytes]:
    req = urllib.request.Request(url, data=body, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read() if e.fp else b""


class Clearance:
    def __init__(self, url: str, token_file: Path, session: str, *, post: Post = urllib_post,
                 timeout: float = TIMEOUT, retry_wait: float = 2.0, sleep: Callable[[float], None] = time.sleep):
        self.endpoint = url.rstrip("/") + MCP_PATH
        self.token_file, self.session = token_file, session
        self.post, self.timeout, self.retry_wait, self.sleep = post, timeout, retry_wait, sleep
        self._id = 0

    def tools_list(self) -> list[dict[str, Any]]:
        tools = self._tool("tools_list", {})
        if not isinstance(tools, list):
            raise ClearanceError("tools_list did not return a list")
        return tools

    def call(self, tool: str, args: dict[str, Any]) -> dict[str, Any]:
        """{ok, decision, data, error, audit_seq, session}: a refusal is a normal answer, not an exception."""
        r = self._tool("call", {"session": self.session, "tool": tool, "args": args})
        if not isinstance(r, dict) or "ok" not in r:
            raise ClearanceError("call did not return a result object")
        return r

    # -- the wire ---------------------------------------------------------------------------------------------------
    def _headers(self) -> dict[str, str]:
        return {"Authorization": f"Bearer {read_token(self.token_file)}", "Content-Type": "application/json",
                "Accept": "application/json, text/event-stream", "MCP-Protocol-Version": PROTOCOL_VERSION}

    def _tool(self, name: str, arguments: dict[str, Any]) -> Any:
        self._id += 1
        body = json.dumps({"jsonrpc": "2.0", "id": self._id, "method": "tools/call",
                           "params": {"name": name, "arguments": arguments}}).encode()
        status, raw = self._send(body)
        if status == 401:
            raise ClearanceError("the gateway refused the run's token (401)")
        if status != 200:
            raise ClearanceError(f"the gateway answered HTTP {status}")
        try:
            msg = json.loads(raw)
        except ValueError as e:
            raise ClearanceError("the gateway's answer is not JSON") from e
        if "error" in msg:
            err = msg["error"] if isinstance(msg["error"], dict) else {}
            raise ClearanceError(f"JSON-RPC error {err.get('code')}: {err.get('message')}")
        return unwrap(msg.get("result") or {})

    def _send(self, body: bytes) -> tuple[int, bytes]:
        """One retry, for a connection that could not be made (urllib's URLError) or a 502/503 from in front of the
        gateway: the request never reached a handler, so a tool is not run twice. A timeout or a dropped connection
        after the request was sent is not retried; the call may have happened."""
        for attempt in (1, 2):
            try:
                status, raw = self.post(self.endpoint, self._headers(), body, self.timeout)
            except urllib.error.URLError as e:
                if attempt == 2:
                    raise ClearanceError(f"cannot reach the gateway: {type(e.reason).__name__}") from e
                self.sleep(self.retry_wait)
                continue
            except (TimeoutError, ConnectionError) as e:
                raise ClearanceError(f"the gateway did not answer: {type(e).__name__}") from e
            if status in (502, 503) and attempt == 1:
                self.sleep(self.retry_wait)
                continue
            return status, raw
        raise AssertionError("unreachable")


def unwrap(result: dict[str, Any]) -> Any:
    """A tools/call result's value: structuredContent.result (the MCP SDK wraps non-object returns in "result"),
    else structuredContent, else the first text content parsed as JSON."""
    if result.get("isError"):
        text = "".join(c.get("text", "") for c in result.get("content") or [] if isinstance(c, dict))
        raise ClearanceError(f"the gateway's tool failed: {text[:300]}")
    sc = result.get("structuredContent")
    if isinstance(sc, dict):
        return sc["result"] if set(sc) == {"result"} else sc
    for c in result.get("content") or []:
        if isinstance(c, dict) and c.get("type") == "text":
            try:
                return json.loads(c["text"])
            except ValueError as e:
                raise ClearanceError("the gateway's text content is not JSON") from e
    raise ClearanceError("the gateway's result has no content")
