"""Tool tiers and the tool registry.

The registry here is the core one: tools any Clearance deployment can offer. A pack (pack.py)
contributes more, e.g. clearance.packs.hangar's glidepath.* and airframe.* tools.

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
    reads_repo: bool = False      # args name a `repo` the definition's repos.allow must admit
    extra: dict = field(default_factory=dict)


_TOOLS = [
    ToolSpec("catalog.read", Tier.T0, "Search the Backstage catalog", ("query",)),
    ToolSpec("metrics.query", Tier.T0, "Run a read-only PromQL query", ("promql",)),
    ToolSpec("logs.query", Tier.T0, "Run a read-only LogQL query", ("logql",)),
    ToolSpec("argo.app.get", Tier.T0, "Read an ArgoCD application", ("app",)),
    ToolSpec("app.api.get", Tier.T0, "GET a path on an allowlisted application API", ("service", "path")),
    ToolSpec("chat.recv", Tier.T0, "Receive pending messages from the human in this session", ()),
    ToolSpec("chat.send", Tier.T0, "Send a message to the human in this session", ("text",)),
    ToolSpec("repo.file.get", Tier.T0, "Read a file, or list a directory (path empty: the repo root), in an allowed repo "
             "at a ref (default main)", ("repo",), github_cost=2, reads_repo=True),
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


def registry(*groups: "tuple[ToolSpec, ...] | list[ToolSpec]") -> dict[str, ToolSpec]:
    """One registry from several groups of specs (core, then each pack's). A name registered
    twice, or a tripwire name registered as a tool, is a configuration error, never an override."""
    out: dict[str, ToolSpec] = {}
    for group in groups:
        for t in group:
            if t.name in out:
                raise ValueError(f"tool {t.name!r} is registered twice")
            if t.name in TRIPWIRES:
                raise ValueError(f"tool {t.name!r} is a T3 tripwire and can never be registered")
            out[t.name] = t
    return out


# T3: never exposed. Registered by name only so an attempt is loud, not merely unknown.
TRIPWIRES = frozenset({
    "k8s.exec", "k8s.apply", "k8s.delete", "secrets.read", "iam.grant",
    "argo.sync.upper", "argo.app.delete", "branch_protection.update",
})


# The core registry: tools whose policy is Clearance's own and that every deployment can offer.
# A pack (pack.py) adds its own; Gateway.tools is the registry one deployment was built with.
CORE_TOOLS: tuple[ToolSpec, ...] = tuple(_TOOLS)
TOOLS: dict[str, ToolSpec] = registry(CORE_TOOLS)


def effective_tier_name(t: Tier) -> str:
    return t.name
