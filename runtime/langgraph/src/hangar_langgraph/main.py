"""The entrypoint: hangar_agent's pod contract (env, result.json, run_finish) around the LangGraph graph."""
from __future__ import annotations

from typing import Callable, Mapping

from hangar_agent import loop
from hangar_agent.clearance import Clearance
from hangar_agent.env import Config
from hangar_agent.main import grant_of, run_ids, run_pod
from hangar_agent.model import ModelProxy

from . import graph


def langgraph_runner(cfg: Config, gateway: Clearance, log: Callable[[str], None]) -> loop.Outcome:
    model = ModelProxy(cfg.model_proxy_url, cfg.model_proxy_token_file, cfg.session_id)
    return graph.run(model, gateway, agent=cfg.agent, task=cfg.task, model_name=cfg.model, max_turns=cfg.max_turns,
                     max_tokens=cfg.max_tokens, expires_at=cfg.expires_at, ids=run_ids(cfg), grant=grant_of(cfg),
                     log=log)


def main(env: Mapping[str, str] | None = None) -> int:
    return run_pod(env, langgraph_runner)
