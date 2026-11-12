"""Hangar's tools: the glidepath.* and airframe.* tool specs (names, tiers, arguments) the Hangar gateway registers.

The tool registry is the gateway's (Hangar's boundary rule, 2026-11-02). The policy over these tools (R021 to R023 and
their argument checks) is Hangar's policy bundle, policy/hangar/bundle.json, which the Clearance core loads from
configuration (R-5); it names the same tools with the same tiers and arguments, and tests/test_hangar_bundle.py checks
the two agree.
"""
from __future__ import annotations

from .engine.tiers import Pack, Tier, ToolSpec

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
    # What is happening now (glidepath-api 0.8.0+): runs.list reads Tekton Results, which holds finished runs only, so a
    # run in flight, and a step's log while it runs, are read live. live.watch is the stream (GET .../live/stream) as one
    # call: it follows one PipelineRun and returns when it finishes or timeoutSeconds (1-120, default 60) passes, with
    # the changes it saw, so an agent that started a run can wait for it without polling.
    ToolSpec("glidepath.live.list", Tier.T0, "An app's PipelineRuns in the cluster now (running, and finished within the "
             "hour), each with its TaskRuns and their step containers", ("app",)),
    ToolSpec("glidepath.live.watch", Tier.T0, "Follow one live PipelineRun until it finishes or timeoutSeconds (1-120, "
             "default 60) passes; returns its state then and every task change seen", ("app", "pipelinerun")),
    ToolSpec("glidepath.live.logs.get", Tier.T0, "Read a running TaskRun's step log (container: step-<name>, from "
             "glidepath.live.list)", ("app", "taskRun", "container")),
    # A release run promote or rollback started (glidepath-api 0.8.0): its tasks in order, the PR and change request.
    ToolSpec("glidepath.releaseRun.get", Tier.T0, "Where a promote or rollback release run is: its tasks, the PR it "
             "opened and the change request (name: the pipelineRun promote or rollback returned)", ("app", "name")),
    # Supply-chain evidence and tests (glidepath-api phase 2).
    ToolSpec("glidepath.evidence.get", Tier.T0, "An app image's supply-chain evidence: its attestations (build "
             "provenance, SBOM, scans, promotion) and whether each verifies (image: ghcr.io/<owner>/<app>...)",
             ("app", "image")),
    ToolSpec("glidepath.tests.list", Tier.T0, "An app's Testkube test runs, newest first (limit 1-200 optional)", ("app",)),
    ToolSpec("glidepath.tests.report", Tier.T0, "One test run's JUnit report (id from glidepath.tests.list)", ("app", "id")),
    # Events (glidepath-api phase 5): the app's recent CDEvents from glidepath-api's buffer (best-effort, 24 h).
    ToolSpec("glidepath.events.recent", Tier.T0, "An app's recent CDEvents, oldest first (type: a CDEvent type prefix; "
             "since: an event id or RFC 3339 time; limit optional)", ("app",)),
    # Preview environments (glidepath-api 0.10.0): read only; a preview starts and ends with its PR's preview label.
    ToolSpec("glidepath.previews.list", Tier.T0, "An app's preview environments, one per PR labelled preview: the PR, "
             "image, Argo CD sync and health, and the port-forward that reaches it", ("app",)),
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
    # cicd.yaml changes (spec phase 3; glidepath-api 0.7.0). validate is a read: the verdict the cicd-validate
    # pull-request check will give a change, before any PR exists. propose is the PR: glidepath-api validates the
    # change as an agent's (schema, conventions, GP-SCOPE) and the handler refuses with that rule when it fails; a
    # clean one becomes one commit of cicd.yaml on the app repo, as the agents' App, which a human merges. It is a PR
    # tool (provenance.PR_TOOLS): its receipt carries repo, pr, commit and the path it wrote, so the agent gates
    # vouch for the PR exactly as for repo.pr.open. `repo` is the app's own (owner/<app>, or owner/gitops-infra-<app>
    # for an InfraService), named so the definition's repos.allow applies as it does to every repo tool.
    ToolSpec("glidepath.cicd.validate", Tier.T0, "What a cicd.yaml change (a JSON merge patch) would mean for an app: "
             "valid or not, each finding with its rule and fix, every field it touches with its scope, the resulting "
             "file (baseRef optional)", ("app", "patch")),
    ToolSpec("glidepath.cicd.propose", Tier.T1, "Propose a cicd.yaml change: validated by glidepath-api as an agent's "
             "change and refused with the GP rule when invalid, else opened as a pull request on the app's repo "
             "(repo: owner/<app>) that a human merges; title required, body and branch optional",
             ("app", "repo", "patch", "title"), github_cost=4, target="lower", reads_repo=True,
             receipt=("pr", "commit", "branch")),
    # Airframe (autopilot.airframe_tools). Reads only: the contract bundle, the validator, the planner, and XRs, Argo CD
    # Applications and Rollouts through an allowlisted GET-only adapter (autopilot.airframe_api); scoped by profile.airframe.apps.
    ToolSpec("airframe.capabilities", Tier.T0, "What Airframe offers: chart fields, XR kinds, components and their outputs, "
             "conditions and verify checks (kind or section optional). Call it with section `appspec` BEFORE writing an "
             "AppSpec: every field, how config (chart values for every environment) and overrides (per environment) map onto "
             "chart values, and a worked example. A large answer is clipped: narrow with section or kind", ()),
    ToolSpec("airframe.validate", Tier.T0, "Validate proposed env-file or values content with airframe-validate: AF-* rule ids, "
             "JSON paths and fix hints (documents: [{path, content}])", ("documents",)),
    ToolSpec("airframe.explain", Tier.T0, "What an AF-* rule id, a condition reason or a knownFailures id means, and how to fix it",
             ("rule",)),
    ToolSpec("airframe.plan", Tier.T0, "Plan an AppSpec into an ordered change set (pass baseCicd, the app's current cicd.yaml). "
             "The spec's shape (fields, `config` chart values for every environment, `overrides` per environment) is "
             "airframe.capabilities {section: appspec}: read it once instead of guessing",
             ("spec",)),
    ToolSpec("airframe.status", Tier.T0, "An app's (or one environment's) XR conditions: reason, hint, stale-status detection",
             ("app",)),
    ToolSpec("airframe.describe", Tier.T0, "One environment: its XRs, the Argo CD Application syncing it and its Rollout",
             ("app", "env")),
    ToolSpec("airframe.verify", Tier.T0, "Run an XR's sidecar verify checks from its status alone; anything needing a Secret, "
             "port-forward or exec is skipped with the reason", ("app", "kind", "name")),
    # Apply (autopilot.airframe_apply): the one airframe.* write. It re-plans the AppSpec itself and opens the next
    # step of the change set as a PR through the agents' App (the tenants repo, then the app's repo); one PR per call,
    # so one receipt per PR (repo, pr, commit, branch, paths). The Flight half (cicd.yaml's Flight fields, gitops-<app>)
    # is never opened and nothing is ever merged: a person merges every PR. Scoped by profile.airframe.apps (spec.app).
    ToolSpec("airframe.apply", Tier.T1, "Apply an AppSpec: opens the next step of airframe.plan's change set as a pull "
             "request on the tenants repo or the app's repo for a person to merge (one PR per call; call again with the "
             "same spec after each merge, or after an interruption: it resumes). The Flight half is left to people",
             ("spec",), github_cost=15, target="lower", receipt=("pr", "commit", "branch")),
)


HANGAR = Pack("hangar", TOOLS)
