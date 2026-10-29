"""The Hangar pack: the policy for Glidepath and Airframe tools.

Policy only. Which glidepath.* and airframe.* tools exist, their tiers and arguments, and the rules
about them are Clearance's (D16): R021 Glidepath's cicd.yaml scope on any file-writing tool, R022 and
R023 the per-agent app allowlists (profile.glidepath.apps, profile.airframe.apps). What the tools do
is Autopilot's: its handlers call glidepath-api and Airframe (autopilot.gateway).

    Gateway(..., packs=(HANGAR,), handlers=...)
"""
from __future__ import annotations

from typing import Any

from clearance.pack import Pack
from clearance.policy import Decision, Rule
from clearance.session import Session
from clearance.tiers import Tier, ToolSpec

from .args import airframe_bad_args, glidepath_app_allowed, glidepath_bad_args, glidepath_promote_source_ok
from .cicd import CICD_SCOPE, cicd_scope_problems, merge_patch

__all__ = ["HANGAR", "TOOLS", "RULES", "CICD_SCOPE", "cicd_scope_problems", "merge_patch",
           "glidepath_bad_args", "airframe_bad_args", "glidepath_app_allowed"]

TOOLS: tuple[ToolSpec, ...] = (
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
    # Glidepath releases (spec phase 4). Two reads, then two propose-only writes: each starts the app's release
    # Pipeline, whose open-release-pr step opens the gitops PR with the platform's GitHub App; a human merges it.
    # Neither Clearance nor glidepath-api holds a GitHub identity that can merge, push or approve.
    ToolSpec("glidepath.environments.list", Tier.T0, "An app's release plan: its environments in promotion order, "
             "Ground (lower) and Flight (upper, with cluster)", ("app",)),
    ToolSpec("glidepath.environments.rollbackPlan", Tier.T0, "A Flight environment's current release and the earlier "
             "healthy releases it may roll back to (newest first)", ("app", "env", "cluster")),
    ToolSpec("glidepath.promote", Tier.T2, "Propose promoting to the next Flight environment: starts the release run "
             "that opens the gitops PR a human merges (sourceEnv+sourceCluster, or sourceImageRepo+sourceImageTag for "
             "a first deploy; dryRun checks everything and starts nothing); retrying the same call starts no second run",
             ("app", "env", "cluster"), target="upper", propose_only=True),
    ToolSpec("glidepath.rollback", Tier.T2, "Propose rolling a Flight environment back to one of its rollback plan's "
             "targets: starts the rollback release run whose PR a human merges (dryRun starts nothing); retrying the "
             "same call starts no second run", ("app", "env", "cluster", "targetReleaseId", "reason"),
             target="upper", propose_only=True),
    # Airframe (autopilot.airframe_tools). Reads only: the contract bundle, the validator, the planner, and XRs, Argo CD
    # Applications and Rollouts through an allowlisted GET-only adapter (autopilot.airframe_api); scoped by profile.airframe.apps.
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
)

# Orders place these exactly where they ran before the split: R022/R023 after R017 (170), R021 last, after R020 (220).
RULES: tuple[Rule, ...] = (
    Rule("R022", "glidepath-app-not-allowed", "ctx.has_glidepath_app && !ctx.glidepath_app_ok",
         "that app is not on this agent's Glidepath allowlist", "Read only the apps your definition names under profile.glidepath.apps.",
         order=180),
    Rule("R023", "airframe-app-not-allowed", "ctx.has_airframe_app && !ctx.airframe_app_ok",
         "that app is not on this agent's Airframe allowlist", "Read only the apps your definition names under profile.airframe.apps.",
         order=190),
    # Glidepath's cicd.yaml scope. The decision carries the GP-SCOPE rule id and hint of the first
    # problem (and every problem under `problems`), not R021, so the agent sees the same rule id the
    # pull request's own `glidepath validate --agent` check would report.
    Rule("R021", "cicd-scope", "ctx.cicd_scope_problems > 0",
         "the cicd.yaml change is outside what an agent may change",
         "Change only the fields Glidepath's contract marks open; see the GP-SCOPE rule in the decision.",
         order=230),
)

def _apps(definition: Any, section: str) -> list[str]:
    """profile.<section>.apps from the definition's raw profile. No definition, or none listed, means no apps."""
    raw = getattr(definition, "raw_profile", None) or {}
    return list((raw.get(section) or {}).get("apps", []))


def context(tool: str, spec: ToolSpec | None, args: dict[str, Any], session: Session,
            definition: Any) -> tuple[dict[str, Any], int]:
    bad = 0
    gp_app = args.get("app") if tool.startswith("glidepath.") and isinstance(args.get("app"), str) else None
    if tool.startswith("glidepath.") and spec:
        bad += glidepath_bad_args(args)
        if tool == "glidepath.promote" and not glidepath_promote_source_ok(args):
            bad += 1
    # Every airframe.* call that names an app is scoped by it; the ones that read no app state (capabilities,
    # explain, plan, and validate without an app) are not, so an agent with no apps listed reads no cluster state.
    af_app = args.get("app") if tool.startswith("airframe.") and isinstance(args.get("app"), str) else None
    if tool.startswith("airframe.") and spec:
        bad += airframe_bad_args(args)
    files = args.get("files") if isinstance(args.get("files"), list) else []
    cicd = cicd_scope_problems(files) if (files and spec and spec.writes_files) else []
    return {
        "cicd_scope": cicd,
        "cicd_scope_problems": len(cicd),
        "has_glidepath_app": gp_app is not None,
        "glidepath_app_ok": glidepath_app_allowed(gp_app, _apps(definition, "glidepath")) if gp_app else True,
        "has_airframe_app": af_app is not None,
        "airframe_app_ok": glidepath_app_allowed(af_app, _apps(definition, "airframe")) if af_app else True,
    }, bad


def decide(rule: Rule, ctx: dict[str, Any]) -> Decision | None:
    if rule.id != "R021":
        return None
    probs = tuple(ctx["ctx"]["cicd_scope"])
    first = probs[0]
    return Decision(False, first["rule"], rule.name, "; ".join(p["message"] for p in probs), first["hint"],
                    ctx["tool"]["tier"], probs)


HANGAR = Pack("hangar", tools=TOOLS, rules=RULES, context=context, decide=decide)
