"""The model-proxy client: the Anthropic Messages API, non-streaming, through Hangar's model proxy.

The proxy speaks Anthropic's API at {MODEL_PROXY_URL}/v1/messages, so this is the official anthropic SDK with
base_url set to it. The run's model-proxy token goes in `x-api-key`, where the proxy reads it, and nowhere else;
`x-hangar-session` names the session the tokens are charged to. A new client is built per request because the token
is a projected file the kubelet rotates: a client holding the key it started with would fail an hour in.

The SDK's own retries are off (max_retries=0): it would retry a 429, and from the proxy a 429 means the session's
model budget is spent, which no wait fixes. Overload, 5xx and connection failures are retried here, a few times.
"""
from __future__ import annotations

import time
from pathlib import Path
from typing import Any, Callable

from .env import read_token

API_KEY_HEADER = "x-api-key"
SESSION_HEADER = "x-hangar-session"
TIMEOUT = 300.0
RETRIES = 2


class ModelStop(RuntimeError):
    """The proxy said stop: 429 (the session's model budget is spent) or 403 (model access denied)."""

    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status


class ModelError(RuntimeError):
    """The model could not be reached or failed in a way retrying did not fix."""


def anthropic_client(api_key: str, base_url: str, session: str, timeout: float):
    """The SDK client for one request. The token goes in `x-api-key` explicitly and `Authorization` is omitted:
    the SDK's own placement of api_key changed between majors (1.x sends X-Api-Key, 2.x sends Authorization: Bearer),
    and the proxy reads x-api-key. Passing api_key as well keeps the SDK from looking for credentials elsewhere."""
    import anthropic   # imported where used: env, clearance, tools and loop need only the stdlib
    return anthropic.Anthropic(api_key=api_key, base_url=base_url, max_retries=0, timeout=timeout,
                               default_headers={API_KEY_HEADER: api_key, "Authorization": anthropic.Omit(),
                                                SESSION_HEADER: session})


class ModelProxy:
    def __init__(self, base_url: str, token_file: Path, session: str, *,
                 client_factory: Callable[..., Any] = anthropic_client, timeout: float = TIMEOUT,
                 retries: int = RETRIES, sleep: Callable[[float], None] = time.sleep):
        self.base_url, self.token_file, self.session = base_url.rstrip("/"), token_file, session
        self.client_factory, self.timeout, self.retries, self.sleep = client_factory, timeout, retries, sleep

    def create(self, **request: Any) -> dict[str, Any]:
        """messages.create, as a plain dict (the SDK model's JSON form)."""
        import anthropic
        for attempt in range(self.retries + 1):
            client = self.client_factory(read_token(self.token_file), self.base_url, self.session, self.timeout)
            try:
                msg = client.messages.create(**request)
            except anthropic.APIStatusError as e:
                client.close()
                if e.status_code in (403, 429):
                    raise ModelStop(e.status_code, _error_message(e)) from e
                if (e.status_code >= 500 or e.status_code == 408) and attempt < self.retries:
                    self.sleep(2.0 * (attempt + 1))
                    continue
                raise ModelError(f"the model proxy answered HTTP {e.status_code}: {_error_message(e)}") from e
            except anthropic.APIConnectionError as e:   # includes APITimeoutError
                client.close()
                if attempt < self.retries:
                    self.sleep(2.0 * (attempt + 1))
                    continue
                raise ModelError(f"cannot reach the model proxy: {type(e).__name__}") from e
            client.close()
            return msg.model_dump(mode="json", exclude_none=True)
        raise AssertionError("unreachable")


def _error_message(e: Any) -> str:
    """The Anthropic-style error's type and message ({"type":"error","error":{"type","message"}}), short."""
    body = getattr(e, "body", None)
    err = body.get("error") if isinstance(body, dict) else None
    if isinstance(err, dict):
        return f"{err.get('type', 'error')}: {str(err.get('message', ''))[:300]}"
    return type(e).__name__
