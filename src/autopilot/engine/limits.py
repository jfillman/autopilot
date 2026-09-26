"""Limits, and the narrow-only rule.

A claim (a run's request for itself) or a child run may only *narrow* the limits it
inherits. It can never widen them. This is the ephemeral-plane invariant: the durable
plane (git) sets the ceiling, and everything at runtime can only stay under it.
"""
from __future__ import annotations

from dataclasses import dataclass, replace
from enum import IntEnum
from typing import Any

from .tiers import Tier


class Network(IntEnum):
    """Ordered by reach. Narrowing means going down."""
    NONE = 0
    CLEARANCE = 1            # only the gateway
    CLEARANCE_MODEL = 2      # gateway + model proxy
    ALLOWLIST = 3            # plus an explicit host allowlist

    @classmethod
    def parse(cls, s: str) -> "Network":
        try:
            return cls[s.upper().replace("+", "_").replace("-", "_")]
        except KeyError as e:
            raise ValueError(f"unknown network mode {s!r}") from e

    def label(self) -> str:
        return self.name.lower().replace("_", "+")


class ComputeClass(IntEnum):
    SMALL = 0
    MEDIUM = 1
    LARGE = 2
    GPU = 3

    @classmethod
    def parse(cls, s: str) -> "ComputeClass":
        try:
            return cls[s.upper()]
        except KeyError as e:
            raise ValueError(f"unknown compute class {s!r}") from e


BUDGET_FIELDS = ("tool_calls", "github_calls", "open_prs", "model_tokens", "children")


@dataclass(frozen=True)
class Limits:
    tier_ceiling: Tier
    ttl_minutes: int
    tool_calls: int
    github_calls: int
    open_prs: int
    model_tokens: int
    children: int
    tools: frozenset[str]
    models: frozenset[str]
    network: Network
    compute: ComputeClass

    def as_dict(self) -> dict[str, Any]:
        return {
            "tier_ceiling": self.tier_ceiling.name, "ttl_minutes": self.ttl_minutes,
            "tool_calls": self.tool_calls, "github_calls": self.github_calls,
            "open_prs": self.open_prs, "model_tokens": self.model_tokens,
            "children": self.children, "tools": sorted(self.tools), "models": sorted(self.models),
            "network": self.network.label(), "compute": self.compute.name.lower(),
        }


class WideningError(ValueError):
    def __init__(self, fields: list[str]):
        self.fields = fields
        super().__init__("claim widens: " + ", ".join(fields))


@dataclass(frozen=True)
class Claim:
    """What a run asks for. Every field is optional; missing means inherit."""
    tier_ceiling: Tier | None = None
    ttl_minutes: int | None = None
    tool_calls: int | None = None
    github_calls: int | None = None
    open_prs: int | None = None
    model_tokens: int | None = None
    children: int | None = None
    tools: frozenset[str] | None = None
    models: frozenset[str] | None = None
    network: Network | None = None
    compute: ComputeClass | None = None

    @classmethod
    def from_dict(cls, d: dict[str, Any] | None) -> "Claim":
        d = d or {}
        known = {"tier_ceiling", "ttl_minutes", "tool_calls", "github_calls", "open_prs",
                 "model_tokens", "children", "tools", "models", "network", "compute"}
        unknown = set(d) - known
        if unknown:
            raise ValueError(f"unknown claim fields: {sorted(unknown)}")
        return cls(
            tier_ceiling=Tier[d["tier_ceiling"]] if d.get("tier_ceiling") else None,
            ttl_minutes=d.get("ttl_minutes"), tool_calls=d.get("tool_calls"),
            github_calls=d.get("github_calls"), open_prs=d.get("open_prs"),
            model_tokens=d.get("model_tokens"), children=d.get("children"),
            tools=frozenset(d["tools"]) if d.get("tools") is not None else None,
            models=frozenset(d["models"]) if d.get("models") is not None else None,
            network=Network.parse(d["network"]) if d.get("network") else None,
            compute=ComputeClass.parse(d["compute"]) if d.get("compute") else None,
        )


def narrow(parent: Limits, claim: Claim) -> Limits:
    """Apply a claim to inherited limits. Raises WideningError listing every field that
    tried to go above what was inherited."""
    widened: list[str] = []
    out = parent

    def cap(name: str, requested, inherited, better_is_lower=False):
        nonlocal out
        if requested is None:
            return
        if requested > inherited:
            widened.append(name)
        else:
            out = replace(out, **{name: requested})

    if claim.tier_ceiling is not None:
        if claim.tier_ceiling > parent.tier_ceiling:
            widened.append("tier_ceiling")
        else:
            out = replace(out, tier_ceiling=claim.tier_ceiling)
    cap("ttl_minutes", claim.ttl_minutes, parent.ttl_minutes)
    for f in BUDGET_FIELDS:
        cap(f, getattr(claim, f), getattr(parent, f))
    if claim.tools is not None:
        extra = claim.tools - parent.tools
        if extra:
            widened.append("tools:" + ",".join(sorted(extra)))
        else:
            out = replace(out, tools=claim.tools)
    if claim.models is not None:
        extra = claim.models - parent.models
        if extra:
            widened.append("models:" + ",".join(sorted(extra)))
        else:
            out = replace(out, models=claim.models)
    if claim.network is not None:
        if claim.network > parent.network:
            widened.append("network")
        else:
            out = replace(out, network=claim.network)
    if claim.compute is not None:
        if claim.compute > parent.compute:
            widened.append("compute")
        else:
            out = replace(out, compute=claim.compute)
    if widened:
        raise WideningError(widened)
    return out
