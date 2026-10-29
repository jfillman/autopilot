"""The pod's environment, as airframe's function-agentrun renders it (functions/function-agentrun, _pod_spec).

Read once at start into a frozen Config. Tokens are not part of it: they are projected service-account tokens the
kubelet rotates, so only their paths are kept and every request reads the file again (read_token).
"""
from __future__ import annotations

import json
import os
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Mapping

DEFAULT_MODEL = "claude-sonnet-5-5"
DEFAULT_MAX_TURNS = 30
DEFAULT_MAX_TOKENS = 8192
CLEARANCE_TOKEN = "clearance-token"       # audience clearance
MODEL_PROXY_TOKEN = "model-proxy-token"   # audience model-proxy


class ConfigError(ValueError):
    """The environment does not describe a run this runtime can do."""


@dataclass(frozen=True)
class Config:
    run_id: str
    task_id: str
    session_id: str
    agent: str
    expires_at: datetime
    output_dir: Path
    workspace: Path
    limits: dict[str, Any]
    clearance_url: str
    model_proxy_url: str
    token_dir: Path
    task: dict[str, Any]
    model: str
    max_turns: int
    max_tokens: int

    @property
    def clearance_token_file(self) -> Path:
        return self.token_dir / CLEARANCE_TOKEN

    @property
    def model_proxy_token_file(self) -> Path:
        return self.token_dir / MODEL_PROXY_TOKEN


def parse_ts(s: str) -> datetime:
    """RFC 3339 with a Z, as function-agentrun writes HANGAR_EXPIRES_AT."""
    dt = datetime.fromisoformat(s.replace("Z", "+00:00"))
    if dt.tzinfo is None:
        raise ValueError("no timezone")
    return dt.astimezone(timezone.utc)


def _json_object(env: Mapping[str, str], name: str) -> dict[str, Any]:
    raw = env.get(name) or "{}"
    try:
        v = json.loads(raw)
    except ValueError as e:
        raise ConfigError(f"{name} is not JSON") from e
    if not isinstance(v, dict):
        raise ConfigError(f"{name} must be a JSON object")
    return v


def _positive_int(env: Mapping[str, str], name: str, default: int) -> int:
    raw = env.get(name) or None
    try:
        n = int(raw) if raw is not None else default
    except ValueError as e:
        raise ConfigError(f"{name} must be an integer, not {raw!r}") from e
    if n <= 0:
        raise ConfigError(f"{name} must be positive")
    return n


def config_from_env(env: Mapping[str, str] | None = None) -> Config:
    env = os.environ if env is None else env
    missing = [k for k in ("HANGAR_SESSION_ID", "HANGAR_EXPIRES_AT", "CLEARANCE_URL", "MODEL_PROXY_URL",
                           "HANGAR_TOKEN_DIR") if not env.get(k)]
    if missing:
        # No MODEL_PROXY_URL means the run's network mode is not clearance+model: this runtime needs a model.
        raise ConfigError("missing " + ", ".join(missing))
    try:
        expires = parse_ts(env["HANGAR_EXPIRES_AT"])
    except ValueError as e:
        raise ConfigError(f"HANGAR_EXPIRES_AT is not an RFC 3339 time: {env['HANGAR_EXPIRES_AT']!r}") from e
    task = _json_object(env, "HANGAR_INPUT")
    model = env.get("HANGAR_MODEL") or task.get("model") or DEFAULT_MODEL
    if not isinstance(model, str):
        raise ConfigError("HANGAR_INPUT model must be a string")
    return Config(
        run_id=env.get("HANGAR_RUN_ID", ""), task_id=env.get("HANGAR_TASK_ID", ""),
        session_id=env["HANGAR_SESSION_ID"], agent=env.get("HANGAR_AGENT", ""), expires_at=expires,
        output_dir=Path(env.get("HANGAR_OUTPUT_DIR") or "/run/output"),
        workspace=Path(env.get("HANGAR_WORKSPACE") or "/workspace"),
        limits=_json_object(env, "HANGAR_LIMITS"),
        clearance_url=env["CLEARANCE_URL"].rstrip("/"), model_proxy_url=env["MODEL_PROXY_URL"].rstrip("/"),
        token_dir=Path(env["HANGAR_TOKEN_DIR"]), task=task, model=model,
        max_turns=_positive_int(env, "HANGAR_MAX_TURNS", DEFAULT_MAX_TURNS),
        max_tokens=_positive_int(env, "HANGAR_MAX_TOKENS", DEFAULT_MAX_TOKENS),
    )


class TokenError(RuntimeError):
    """A projected token is missing or empty. The message names the file, never its contents."""


def read_token(path: Path) -> str:
    """The token as it is now. Called per request: the kubelet rewrites the file before the old token expires."""
    try:
        token = path.read_text().strip()
    except OSError as e:
        raise TokenError(f"cannot read {path}: {type(e).__name__}") from e
    if not token:
        raise TokenError(f"{path} is empty")
    return token
