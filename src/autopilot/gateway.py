"""The Hangar Gateway: core Clearance, the Hangar pack's policy, and Hangar's backends.

Clearance owns the decision (clearance.dispatch.Gateway, clearance.packs.hangar). What this module adds
is what the tools DO on Hangar: a handler per glidepath.* and airframe.* tool over the backends in
backends.py, the core tools' handlers over the same bundle, and the run tree as AgentRun XRs.

    gw = hangar_gateway(definitions, store, audit, fake_backends(), auth, clock)
"""
from __future__ import annotations

import hashlib
from datetime import datetime
from typing import Any, Callable

from clearance.audit import AuditLog
from clearance.backends import Handler, core_handlers
from clearance.dispatch import Authenticator, Gateway
from clearance.packs.hangar import HANGAR
from clearance.policy import Policy
from clearance.profile import AgentDefinition
from clearance.session import Session, SessionStore
from clearance.tiers import ToolSpec

from . import manifests
from .backends import AgentRunStore, Backends

PACKS = (HANGAR,)

# The registry a Hangar Gateway is built with (core plus the Hangar pack), for code that wants it
# without a Gateway at hand. Gateway.tools is the same thing.
TOOLS: dict[str, ToolSpec] = Policy(PACKS).tools


def rerun_key(session_id: str, app: str, pipelinerun: str) -> str:
    """glidepath-api's Idempotency-Key for a session's re-run of one run: [A-Za-z0-9._:-], at most 128."""
    return "clearance." + hashlib.sha256(f"{session_id}/{app}/{pipelinerun}".encode()).hexdigest()[:32]


def release_key(session_id: str, op: str, app: str, env: str, cluster: str, source: str) -> str:
    """The Idempotency-Key for a session's promote or rollback: the session, the app, the target environment and
    cluster, and what it promotes from (or rolls back to). Not dryRun, so a dry run names the run the real call
    would start; not a rollback's reason, so a retry that rewords it still replays."""
    return "clearance." + hashlib.sha256(f"{session_id}/{op}/{app}/{env}/{cluster}/{source}".encode()).hexdigest()[:32]


def promote_source(a: dict[str, Any]) -> str:
    if a.get("sourceEnv"):
        return f"env:{a['sourceEnv']}@{a['sourceCluster']}"
    return f"image:{a['sourceImageRepo']}:{a['sourceImageTag']}"


def pr_meta(s: Session) -> dict[str, str]:
    """What an agent PR carries about its origin (AP-B1: Clearance's provenance detector reads it)."""
    return {k: v for k, v in {"agent": s.agent, "task": s.task_id, "session": s.id, "run": s.run}.items() if v}


def _promote(b: Backends, s: Session, a: dict[str, Any]) -> Any:
    body = {"cluster": a["cluster"], **{k: a[k] for k in ("sourceEnv", "sourceCluster", "sourceImageRepo",
                                                           "sourceImageTag") if a.get(k)}}
    key = release_key(s.id, "promote", a["app"], a["env"], a["cluster"], promote_source(a))
    return b.glidepath.promote(a["app"], a["env"], body, key, bool(a.get("dryRun")), s.principal)


def _rollback(b: Backends, s: Session, a: dict[str, Any]) -> Any:
    body = {"cluster": a["cluster"], "targetReleaseId": a["targetReleaseId"], "reason": a["reason"].strip()}
    key = release_key(s.id, "rollback", a["app"], a["env"], a["cluster"], a["targetReleaseId"])
    return b.glidepath.rollback(a["app"], a["env"], body, key, bool(a.get("dryRun")), s.principal)


class AgentRunBackend:
    """Clearance's RunBackend on Hangar: a session's run is an AgentRun XR (manifests.agentrun_manifest)."""

    def __init__(self, store: AgentRunStore):
        self.store = store

    def create(self, session: Session, definition: AgentDefinition, run_input: dict[str, Any] | None) -> str:
        manifest = manifests.agentrun_manifest(session, definition, run_input=run_input)
        self.store.create(manifest)
        return manifest["metadata"]["name"]

    def delete(self, session: Session) -> None:
        self.store.delete(manifests.run_name(session))

    def principal(self, session: Session) -> str | None:
        """The identity a session's run pod authenticates as: function-agentrun runs it as the ServiceAccount `run`
        in the run's own namespace, agent-<run> (airframe functions/function-agentrun, RUN_SERVICE_ACCOUNT, ns_name).
        Clearance lets that caller act as the session, and only that session (Gateway._acting)."""
        return f"agent-{session.run}/{manifests.RUN_SERVICE_ACCOUNT}" if session.run else None


def hangar_handlers(b: Backends) -> dict[str, Handler]:
    """Every handler a Hangar Gateway registers (run.spawn and run.close are the Gateway's own).
    The glidepath and airframe backends are read at call time, so a deployment without one fails that
    tool's calls as a backend error rather than refusing to start."""
    h = core_handlers(git=b.git, argo=b.argo, read=b.read, human=b.human, artifacts=b.artifacts)
    if b.git is not None and getattr(b.git, "takes_meta", False):
        # The real adapter (github_pr.GitHubPRs) records who opened the PR in its commit trailer and body.
        h["repo.pr.open"] = lambda s, a: b.git.open_pr(a["repo"], a["branch"], a["title"], a["files"],
                                                       a.get("base", "main"), meta=pr_meta(s))
        h["repo.pr.open_upper"] = lambda s, a: b.git.open_pr(a["repo"], a["branch"], a["title"], a["files"],
                                                             a.get("base", "main"), meta=pr_meta(s))
    if b.argo is not None and getattr(b.argo, "syncer", None) is not None:
        # The operation names the agent, task, session and principal, so Argo CD's history shows who asked.
        h["argo.sync.lower"] = lambda s, a: b.argo.sync(a["app"], a["project"],
                                                        who={**pr_meta(s), "principal": s.principal})
    h.update({
        "glidepath.runs.list": lambda s, a: b.glidepath.runs(a["app"], a.get("sinceHours"), a.get("limit"), s.principal),
        "glidepath.run.get": lambda s, a: b.glidepath.run(a["app"], a["result"], s.principal),
        "glidepath.logs.get": lambda s, a: b.glidepath.log(a["app"], a["result"], a["taskRun"], s.principal),
        "glidepath.explain": lambda s, a: b.glidepath.explain(a["app"], a["result"], s.principal),
        "glidepath.releases.get": lambda s, a: b.glidepath.releases(a["app"], a.get("chainId"), s.principal),
        "glidepath.contract": lambda s, a: b.glidepath.contract(s.principal),
        # The key is the session and the run, so an agent retrying the same re-run starts no second one.
        "glidepath.run.rerun": lambda s, a: b.glidepath.rerun(a["app"], a["pipelinerun"], rerun_key(s.id, a["app"], a["pipelinerun"]), s.principal),
        "glidepath.run.cancel": lambda s, a: b.glidepath.cancel(a["app"], a["pipelinerun"], s.principal),
        "glidepath.environments.list": lambda s, a: b.glidepath.environments(a["app"], s.principal),
        "glidepath.environments.rollbackPlan": lambda s, a: b.glidepath.rollback_plan(a["app"], a["env"], a["cluster"], s.principal),
        # T2, propose only: each starts a release run whose PR a human merges. The key is the session and the
        # promotion (or rollback), so an agent retrying it replays the first answer instead of starting another.
        "glidepath.promote": lambda s, a: _promote(b, s, a),
        "glidepath.rollback": lambda s, a: _rollback(b, s, a),
        "airframe.capabilities": lambda s, a: b.airframe.capabilities(a.get("kind"), a.get("section")),
        "airframe.validate": lambda s, a: b.airframe.validate(a["documents"], a.get("app"), a.get("mode", "values"),
                                                              a.get("render", True)),
        "airframe.explain": lambda s, a: b.airframe.explain(a["rule"]),
        "airframe.plan": lambda s, a: b.airframe.plan(a["spec"], a.get("baseCicd")),
        "airframe.status": lambda s, a: b.airframe.status(a["app"], a.get("env")),
        "airframe.describe": lambda s, a: b.airframe.describe(a["app"], a["env"]),
        "airframe.verify": lambda s, a: b.airframe.verify(a["app"], a["kind"], a["name"], a.get("env")),
    })
    return h


def hangar_gateway(definitions: dict[str, AgentDefinition], store: SessionStore, audit: AuditLog,
                   backends: Backends, auth: Authenticator, clock: Callable[[], datetime]) -> Gateway:
    return Gateway(definitions, store, audit, auth, clock, packs=PACKS,
                   handlers=hangar_handlers(backends), runs=AgentRunBackend(backends.runs))
