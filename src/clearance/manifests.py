"""Build the AgentRun claim for a session. A pure function, so it can be checked against the
XRD's own schema in tests (see tests/test_manifests.py)."""
from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

from .profile import AgentDefinition
from .session import Session

RUN_NAMESPACE = "autopilot-runs"
API_VERSION = "catalog.idp.io/v1alpha1"


def iso(dt: datetime) -> str:
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def run_name(session: Session) -> str:
    return "r-" + session.id.removeprefix("s-")


def agentrun_manifest(session: Session, definition: AgentDefinition, *, image: str | None = None,
                      run_input: dict[str, Any] | None = None) -> dict[str, Any]:
    img = image or definition.image
    if not img:
        raise ValueError(f"agent {definition.name!r} has no image; a run needs one")
    lim = session.limits
    spec: dict[str, Any] = {
        "agent": definition.name,
        "image": img,
        "framework": definition.framework,
        "sandbox": definition.sandbox,
        "compute": lim.compute.name.lower(),
        "network": {"mode": lim.network.label()},
        "expiresAt": iso(session.expires_at),
        "taskId": session.task_id,
        "sessionId": session.id,
        "limits": {"toolCalls": lim.tool_calls, "githubCalls": lim.github_calls,
                   "modelTokens": lim.model_tokens},
    }
    if session.parent_id:
        spec["parentSessionId"] = session.parent_id
    if definition.sidecars:
        spec["sidecars"] = [dict(s) for s in definition.sidecars]
    if run_input:
        spec["input"] = run_input
    return {
        "apiVersion": API_VERSION, "kind": "AgentRun",
        "metadata": {
            "name": run_name(session), "namespace": RUN_NAMESPACE,
            "labels": {"hangar.io/component": "autopilot", "hangar.io/agent": definition.name,
                       "hangar.io/task": session.task_id, "hangar.io/session": session.id},
        },
        "spec": spec,
    }
