"""Tool tiers and the tool registry.

T0 read-only. T1 reversible writes (a PR a human can revert, a lower-env sync, a child
run). T2 propose-only: the tool may open a PR against an upper environment, but a human
merge is what applies it. T3 is never exposed to any identity; T3 names are registered
only as tripwires, so an attempt is denied, audited, and trips the session breaker.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from enum import IntEnum
from typing import Literal


class Tier(IntEnum):
    T0 = 0
    T1 = 1
    T2 = 2
    T3 = 3


Target = Literal["none", "lower", "upper"]


@dataclass(frozen=True)
class ToolSpec:
    name: str
    tier: Tier
    description: str
    required_args: tuple[str, ...] = ()
    github_cost: int = 0          # GitHub API calls this tool is expected to spend
    target: Target = "none"       # which environment tier the tool acts on
    propose_only: bool = False    # T2: opens a PR, never applies anything
    writes_files: bool = False    # args carry a `files` list subject to path scope
    extra: dict = field(default_factory=dict)


_TOOLS = [
    ToolSpec("catalog.read", Tier.T0, "Search the Backstage catalog", ("query",)),
    ToolSpec("metrics.query", Tier.T0, "Run a read-only PromQL query", ("promql",)),
    ToolSpec("logs.query", Tier.T0, "Run a read-only LogQL query", ("logql",)),
    ToolSpec("argo.app.get", Tier.T0, "Read an ArgoCD application", ("app",)),
    ToolSpec("app.api.get", Tier.T0, "GET a path on an allowlisted application API", ("service", "path")),
    # Glidepath (glidepath-api, glidepath docs/admin/api.md). Reads, scoped by profile.glidepath.apps.
    ToolSpec("glidepath.runs.list", Tier.T0, "List an app's CI/CD runs, newest first (sinceHours, limit optional)", ("app",)),
    ToolSpec("glidepath.run.get", Tier.T0, "Read one run: its PipelineRun and TaskRuns", ("app", "result")),
    ToolSpec("glidepath.logs.get", Tier.T0, "Read one TaskRun's archived step logs", ("app", "result", "taskRun")),
    ToolSpec("glidepath.explain", Tier.T0, "Why a run failed: a knownFailures id, its hint and the cicd.yaml field to change",
             ("app", "result")),
    ToolSpec("glidepath.releases.get", Tier.T0, "Read an app's release records, or one by chainId", ("app",)),
    # Glidepath writes (spec phase 3): through glidepath-api, which authorizes Clearance and audits the principal too.
    # These replace pipeline.rerun, which went to the cluster directly.
    ToolSpec("glidepath.run.rerun", Tier.T1, "Re-run a PipelineRun (a copy of its spec); retrying the same call starts no second run",
             ("app", "pipelinerun"), target="lower"),
    ToolSpec("glidepath.run.cancel", Tier.T1, "Cancel a running PipelineRun (its finally tasks still run)", ("app", "pipelinerun"),
             target="lower"),
    ToolSpec("glidepath.contract", Tier.T0, "Glidepath's contract: cicd.yaml fields and agent scope, gates, states, knownFailures", ()),
    # Airframe (airframe_tools.py). Reads only: the contract bundle, the validator, the planner, and XRs, Argo CD
    # Applications and Rollouts through an allowlisted GET-only adapter (airframe_api.py); scoped by profile.airframe.apps.
    ToolSpec("airframe.capabilities", Tier.T0, "What Airframe offers: chart fields, XR kinds, components and their outputs, "
             "conditions and verify checks (kind or section optional)", ()),
    ToolSpec("airframe.validate", Tier.T0, "Validate proposed env-file or values content with airframe-validate: AF-* rule ids, "
             "JSON paths and fix hints (documents: [{path, content}])", ("documents",)),
    ToolSpec("airframe.explain", Tier.T0, "What an AF-* rule id, a condition reason or a knownFailures id means, and how to fix it",
             ("rule",)),
    ToolSpec("airframe.plan", Tier.T0, "Plan an AppSpec into an ordered change set (pass baseCicd, the app's current cicd.yaml)",
             ("spec",)),
    ToolSpec("airframe.status", Tier.T0, "An app's (or one environment's) XR conditions: reason, hint, stale-status detection",
             ("app",)),
    ToolSpec("airframe.describe", Tier.T0, "One environment: its XRs, the Argo CD Application syncing it and its Rollout",
             ("app", "env")),
    ToolSpec("airframe.verify", Tier.T0, "Run an XR's sidecar verify checks from its status alone; anything needing a Secret, "
             "port-forward or exec is skipped with the reason", ("app", "kind", "name")),
    ToolSpec("chat.recv", Tier.T0, "Receive pending messages from the human in this session", ()),
    ToolSpec("chat.send", Tier.T0, "Send a message to the human in this session", ("text",)),
    ToolSpec("repo.pr.open", Tier.T1, "Open a PR on an app repo", ("repo", "branch", "title", "files"),
             github_cost=4, target="lower", writes_files=True),
    ToolSpec("xr.request", Tier.T1, "Commit an XR request for a lower environment", ("app", "env", "kind", "spec"),
             github_cost=3, target="lower"),
    ToolSpec("argo.sync.lower", Tier.T1, "Sync a lower-environment application", ("app", "project"),
             target="lower"),
    ToolSpec("artifact.put", Tier.T1, "Store an output artifact under this task (bounded, expiring)", ("name", "content")),
    ToolSpec("artifact.get", Tier.T0, "Read an artifact stored under this task tree", ("name",)),
    ToolSpec("run.spawn", Tier.T1, "Start a child agent run, narrower than this one", ("agent", "limits")),
    ToolSpec("run.close", Tier.T1, "Close one of this session's own runs", ("run_id",)),
    ToolSpec("human.request", Tier.T1, "Ask a human to decide something", ("question",)),
    ToolSpec("repo.pr.open_upper", Tier.T2, "Open a PR against an upper environment (a human merges)",
             ("repo", "branch", "title", "files"), github_cost=4, target="upper",
             propose_only=True, writes_files=True),
]

TOOLS: dict[str, ToolSpec] = {t.name: t for t in _TOOLS}

# T3: never exposed. Registered by name only so an attempt is loud, not merely unknown.
TRIPWIRES = frozenset({
    "k8s.exec", "k8s.apply", "k8s.delete", "secrets.read", "iam.grant",
    "argo.sync.upper", "argo.app.delete", "branch_protection.update",
})


def effective_tier_name(t: Tier) -> str:
    return t.name
