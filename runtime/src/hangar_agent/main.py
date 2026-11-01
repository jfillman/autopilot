"""The entrypoint, `hangar-agent`: read the pod's environment, run the loop, write result.json, exit.

result.json is written (and then handed to Clearance, run_finish) in every case the process can still write it, a bad environment included, because it is
what the run leaves behind: {status, turns, final_text, tool_calls: [{tool, ok, rule}], reason}. It is written to a
temporary file in the same directory and renamed, so a reader never sees half of one.

Exit codes: 0 when the status is done, 1 for budget, expired or error, 2 when the environment is unusable.
"""
from __future__ import annotations

import json
import os
import sys
import tempfile
from pathlib import Path
from typing import Any, Mapping

from . import loop
from .clearance import Clearance, ClearanceError
from .env import ConfigError, config_from_env
from .model import ModelProxy

RESULT = "result.json"


def write_result(output_dir: Path, result: dict[str, Any]) -> Path:
    output_dir.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=output_dir, prefix=".result-", suffix=".json")
    with os.fdopen(fd, "w") as f:
        json.dump(result, f, indent=2, sort_keys=True)
        f.write("\n")
    path = output_dir / RESULT
    os.replace(tmp, path)
    return path


def say(line: str) -> None:
    print(f"hangar-agent: {line}", flush=True)


def main(env: Mapping[str, str] | None = None) -> int:
    env = os.environ if env is None else env
    try:
        cfg = config_from_env(env)
    except ConfigError as e:
        say(f"configuration: {e}")
        out = loop.Outcome("error", reason=f"configuration: {e}")
        try:
            write_result(Path(env.get("HANGAR_OUTPUT_DIR") or "/run/output"), out.as_dict())
        except OSError:
            pass
        return 2
    say(f"run={cfg.run_id} task={cfg.task_id} agent={cfg.agent} session={cfg.session_id} "
        f"expires={cfg.expires_at.isoformat()}")
    gateway = Clearance(cfg.clearance_url, cfg.clearance_token_file, cfg.session_id)
    model = ModelProxy(cfg.model_proxy_url, cfg.model_proxy_token_file, cfg.session_id)
    grant = cfg.limits.get("tools")
    out = loop.run(model, gateway, agent=cfg.agent, task=cfg.task, model_name=cfg.model, max_turns=cfg.max_turns,
                   max_tokens=cfg.max_tokens, expires_at=cfg.expires_at,
                   ids={"task id": cfg.task_id, "run id": cfg.run_id, "session": cfg.session_id},
                   grant=[t for t in grant if isinstance(t, str)] if isinstance(grant, list) else None, log=say)
    write_result(cfg.output_dir, out.as_dict())
    # The pod's volume goes with its namespace: Clearance keeps the result for Tower. A failure here is logged and
    # changes nothing else (Clearance's sweep closes a session whose run ended without one).
    try:
        done = gateway.finish(out.as_dict())
        say("result handed to Clearance" if done.get("ok") else f"Clearance did not take the result: {done.get('rule')}")
    except ClearanceError as e:
        say(f"Clearance did not take the result: {e}")
    calls = len(out.tool_calls)
    refused = sum(1 for c in out.tool_calls if not c["ok"])
    say(f"end: status={out.status} turns={out.turns} tool_calls={calls} not_ok={refused}"
        + (f" reason={out.reason}" if out.reason else ""))
    return 0 if out.status == "done" else 1


if __name__ == "__main__":
    sys.exit(main())
