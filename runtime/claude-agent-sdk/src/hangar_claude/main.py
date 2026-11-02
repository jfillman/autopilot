"""The entrypoint: hangar_agent's pod contract (env, result.json, run_finish) around the Agent SDK's loop."""
from __future__ import annotations

import asyncio
from typing import Callable, Mapping

from hangar_agent import loop
from hangar_agent.clearance import Clearance
from hangar_agent.env import Config, TokenError, read_token
from hangar_agent.main import grant_of, run_ids, run_pod

from . import agent

# Where the CLI keeps its state: the root filesystem is read-only and /tmp is the pod's writable scratch.
CONFIG_DIR = "/tmp/claude"


def claude_runner(cfg: Config, gateway: Clearance, log: Callable[[str], None]) -> loop.Outcome:
    import claude_agent_sdk as sdk   # only in this image; 140 MB with its bundled CLI
    from pathlib import Path
    try:
        token = read_token(cfg.model_proxy_token_file)
    except TokenError as e:
        return loop.Outcome("error", reason=str(e))
    # The token is read once: the CLI holds it for the run. It is valid for an hour from the pod's start (the
    # AgentRun's projected token), longer than any run's TTL.
    env = agent.cli_env(model_proxy_url=cfg.model_proxy_url, model_token=token, session=cfg.session_id,
                        model=cfg.model, max_tokens=cfg.max_tokens, config_dir=Path(CONFIG_DIR))
    return asyncio.run(agent.run(sdk, gateway, agent=cfg.agent, task=cfg.task, model_name=cfg.model,
                                 max_turns=cfg.max_turns, env=env, workspace=cfg.workspace,
                                 expires_at=cfg.expires_at, grant=grant_of(cfg), ids=run_ids(cfg), log=log))


def main(env: Mapping[str, str] | None = None) -> int:
    return run_pod(env, claude_runner)
