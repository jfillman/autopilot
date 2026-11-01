"""Hangar's backends behind Clearance: the Glidepath and Airframe interfaces, the AgentRun store, and fakes.

Clearance's core tools have their own interfaces and fakes (clearance.backends); Backends here bundles
them with Hangar's, the shape gateway.hangar_handlers reads. Fakes record every call, so tests can
assert what did and did not happen.
"""
from __future__ import annotations

from typing import Any, Protocol

from clearance.backends import (ArgoBackend, ArtifactBackend, FakeArgo, FakeArtifacts, FakeGit, FakeHuman,
                                FakeRead, GitBackend, HumanBackend, ReadBackend, Recorder)

__all__ = ["AgentRunStore", "AirframeBackend", "Backends", "FakeArgo", "FakeArtifacts", "FakeGit",
           "FakeGlidepath", "FakeHuman", "FakeRead", "FakeRuns", "GlidepathBackend", "fake_backends"]


class AgentRunStore(Protocol):
    """Where AgentRun XRs go (the Kubernetes API, in the real adapter, which is not built yet)."""
    def create(self, manifest: dict[str, Any]) -> dict[str, Any]: ...
    def delete(self, name: str) -> None: ...


class GlidepathBackend(Protocol):
    """glidepath-api (glidepath_api.py is the real one). `on_behalf_of` is the session's principal,
    recorded in Glidepath's audit line next to Clearance's own identity."""
    def runs(self, app: str, since_hours: int | None, limit: int | None, on_behalf_of: str) -> Any: ...
    def run(self, app: str, result: str, on_behalf_of: str) -> Any: ...
    def log(self, app: str, result: str, task_run: str, on_behalf_of: str) -> Any: ...
    def explain(self, app: str, result: str, on_behalf_of: str) -> Any: ...
    def releases(self, app: str, chain_id: str | None, on_behalf_of: str) -> Any: ...
    def contract(self, on_behalf_of: str) -> Any: ...
    def rerun(self, app: str, pipelinerun: str, idempotency_key: str, on_behalf_of: str) -> Any: ...
    def cancel(self, app: str, pipelinerun: str, on_behalf_of: str) -> Any: ...
    def environments(self, app: str, on_behalf_of: str) -> Any: ...
    def rollback_plan(self, app: str, env: str, cluster: str, on_behalf_of: str) -> Any: ...
    def promote(self, app: str, env: str, body: dict, idempotency_key: str, dry_run: bool, on_behalf_of: str) -> Any: ...
    def rollback(self, app: str, env: str, body: dict, idempotency_key: str, dry_run: bool, on_behalf_of: str) -> Any: ...
    def validate_cicd(self, app: str, body: dict, on_behalf_of: str) -> Any: ...
    def live(self, app: str, on_behalf_of: str) -> Any: ...
    def live_log(self, app: str, task_run: str, container: str, on_behalf_of: str) -> Any: ...
    def watch(self, app: str, pipelinerun: str, timeout_seconds: int | None, on_behalf_of: str) -> Any: ...
    def release_run(self, app: str, name: str, on_behalf_of: str) -> Any: ...
    def evidence(self, app: str, image: str, on_behalf_of: str) -> Any: ...
    def tests(self, app: str, limit: int | None, on_behalf_of: str) -> Any: ...
    def test_report(self, app: str, test_id: str, on_behalf_of: str) -> Any: ...
    def events(self, app: str, type_prefix: str | None, since: str | None, limit: int | None, on_behalf_of: str) -> Any: ...
    def previews(self, app: str, on_behalf_of: str) -> Any: ...



class AirframeBackend(Protocol):
    """Airframe's contract, validator and planner, and read-only cluster state (airframe_tools.py is the real one)."""
    def capabilities(self, kind: str | None, section: str | None) -> Any: ...
    def validate(self, documents: list[dict], app: str | None, mode: str, render: bool) -> Any: ...
    def explain(self, rule: str) -> Any: ...
    def plan(self, spec: dict, base_cicd: Any) -> Any: ...
    def status(self, app: str, env: str | None) -> Any: ...
    def describe(self, app: str, env: str) -> Any: ...
    def verify(self, app: str, kind: str, name: str, env: str | None) -> Any: ...


class Backends:
    def __init__(self, git: GitBackend, argo: ArgoBackend, runs: AgentRunStore, read: ReadBackend,
                 human: HumanBackend, artifacts: ArtifactBackend | None = None,
                 glidepath: GlidepathBackend | None = None, airframe: AirframeBackend | None = None):
        self.git, self.argo, self.runs, self.read, self.human = git, argo, runs, read, human
        self.artifacts, self.glidepath, self.airframe = artifacts, glidepath, airframe


class FakeRuns(Recorder):
    def __init__(self):
        super().__init__()
        self.manifests: dict[str, dict] = {}

    def create(self, manifest):
        self._rec("runs.create", manifest["metadata"]["name"])
        self.manifests[manifest["metadata"]["name"]] = manifest
        return {"name": manifest["metadata"]["name"]}

    def delete(self, name):
        self._rec("runs.delete", name)
        self.manifests.pop(name, None)


class FakeGlidepath(Recorder):
    def runs(self, app, since_hours, limit, on_behalf_of):
        self._rec("glidepath.runs", app, since_hours, limit, on_behalf_of=on_behalf_of)
        return {"runs": [{"result": "0b9d1a2e-1111-4c4c-9999-aaaaaaaaaaaa", "name": f"{app}-push-abc", "status": "failed"}]}

    def run(self, app, result, on_behalf_of):
        self._rec("glidepath.run", app, result, on_behalf_of=on_behalf_of)
        return {"result": result, "pipelineRun": {}, "taskRuns": []}

    def log(self, app, result, task_run, on_behalf_of):
        self._rec("glidepath.log", app, result, task_run, on_behalf_of=on_behalf_of)
        return {"log": "[build] error: no space left on device\n"}

    def explain(self, app, result, on_behalf_of):
        self._rec("glidepath.explain", app, result, on_behalf_of=on_behalf_of)
        return {"result": result, "status": "failed", "failures": [
            {"taskRun": f"{app}-push-abc-build", "step": "build", "log": "archived",
             "knownFailure": {"id": "SourceVolumeFull", "hint": "Raise build.sourceVolume.size one step.",
                              "field": "/build/sourceVolume/size"},
             "evidence": "error: no space left on device"}]}

    def releases(self, app, chain_id, on_behalf_of):
        self._rec("glidepath.releases", app, chain_id, on_behalf_of=on_behalf_of)
        return {"releases": []}

    def contract(self, on_behalf_of):
        self._rec("glidepath.contract", on_behalf_of=on_behalf_of)
        return {"contractVersion": 1}

    def rerun(self, app, pipelinerun, idempotency_key, on_behalf_of):
        self._rec("glidepath.rerun", app, pipelinerun, idempotency_key, on_behalf_of=on_behalf_of)
        replayed = sum(1 for c in self.calls if c[0] == "glidepath.rerun" and c[1][2] == idempotency_key) > 1
        return {"name": f"{pipelinerun}-rerun-abcde", "replayed": replayed}

    def cancel(self, app, pipelinerun, on_behalf_of):
        self._rec("glidepath.cancel", app, pipelinerun, on_behalf_of=on_behalf_of)
        return {"name": pipelinerun, "status": "cancelled"}

    def live(self, app, on_behalf_of):
        self._rec("glidepath.live", app, on_behalf_of=on_behalf_of)
        return {"pipelineRuns": [{"name": f"{app}-ci-0-build-abcde", "pipeline": "build", "status": "running", "tasks": [
            {"name": "build", "taskRun": f"{app}-ci-0-build-abcde-build", "status": "running",
             "steps": [{"name": "build", "container": "step-build", "state": "running"}]}]}]}

    def live_log(self, app, task_run, container, on_behalf_of):
        self._rec("glidepath.live_log", app, task_run, container, on_behalf_of=on_behalf_of)
        return {"log": "2026-11-01T00:00:00Z [build] compiling\n"}

    def watch(self, app, pipelinerun, timeout_seconds, on_behalf_of):
        self._rec("glidepath.watch", app, pipelinerun, timeout_seconds, on_behalf_of=on_behalf_of)
        return {"finished": True, "waitedSeconds": 1.0, "via": "stream", "changes": [
            {"task": "build", "taskRun": f"{pipelinerun}-build", "status": "succeeded"}],
            "pipelineRun": {"name": pipelinerun, "status": "succeeded", "tasks": []}}

    def release_run(self, app, name, on_behalf_of):
        self._rec("glidepath.release_run", app, name, on_behalf_of=on_behalf_of)
        return {"name": name, "status": "running", "phase": "running", "tasks": [{"name": "preflight", "status": "succeeded"}]}

    def evidence(self, app, image, on_behalf_of):
        self._rec("glidepath.evidence", app, image, on_behalf_of=on_behalf_of)
        return {"image": image, "digest": "sha256:" + "0" * 64, "attestations": []}

    def tests(self, app, limit, on_behalf_of):
        self._rec("glidepath.tests", app, limit, on_behalf_of=on_behalf_of)
        return {"runs": []}

    def test_report(self, app, test_id, on_behalf_of):
        self._rec("glidepath.test_report", app, test_id, on_behalf_of=on_behalf_of)
        return {"report": "<testsuites/>"}

    def events(self, app, type_prefix, since, limit, on_behalf_of):
        self._rec("glidepath.events", app, type_prefix, since, limit, on_behalf_of=on_behalf_of)
        return {"events": []}

    def previews(self, app, on_behalf_of):
        self._rec("glidepath.previews", app, on_behalf_of=on_behalf_of)
        return {"previews": [{"pr": 55, "application": f"{app}-pr-55", "namespace": f"app-{app}-pr-55",
                              "sync": "Synced", "health": "Healthy", "createdAt": "2026-10-26T21:38:47Z"}]}

    def environments(self, app, on_behalf_of):
        self._rec("glidepath.environments", app, on_behalf_of=on_behalf_of)
        return {"order": ["dev", "staging", "prod"], "orderSource": "promotionOrder", "lower": ["dev"],
                "upper": [{"name": "staging", "cluster": "hangar-prod"},
                          {"name": "prod", "cluster": "hangar-prod", "production": True}]}

    def rollback_plan(self, app, env, cluster, on_behalf_of):
        self._rec("glidepath.rollback_plan", app, env, cluster, on_behalf_of=on_behalf_of)
        return {"current": {"releaseId": "rel-3", "image": f"ghcr.io/o/{app}:1.2.0-3333333", "state": "healthy"},
                "targets": [{"releaseId": "rel-2", "image": f"ghcr.io/o/{app}:1.1.0-2222222", "gitRevision": "2222222",
                             "gitUrl": f"https://github.com/o/{app}", "healthyAt": "2026-10-28T00:00:00Z",
                             "state": "superseded"}]}

    # glidepath-api's POST /v1/apps/{app}/cicd/validations, as its validator answers it for a small cicd.yaml: the
    # patch applied to CICD_BASE, every changed field listed with its scope, and (agent) GP-SCOPE-002 when a gate is
    # turned off. Enough for the propose handler's three paths: clean, refused, nothing to change.
    CICD_BASE = {"apiVersion": "platform/v1", "kind": "PipelineConfig",
                 "build": {"agent": "nodejs-22", "cache": {"enabled": False, "size": "small"}},
                 "governance": {"sast": True, "imageScan": True},
                 "pipelines": {"ci": {"trigger": {"source": "git", "event": "push", "branch": "main"}, "steps": [{"stage": "build"}]}}}

    def validate_cicd(self, app, body, on_behalf_of):
        self._rec("glidepath.validate_cicd", app, body, on_behalf_of=on_behalf_of)
        from clearance.packs.hangar.cicd import merge_patch
        from clearance.packs.hangar.vendor.cicd_scope import MISSING, changes
        import yaml
        base_info = {"repo": f"jfillman/{app}", "ref": body.get("baseRef") or "HEAD", "sha": "a" * 40, "present": True}
        if app == "unknown-app":   # not onboarded: glidepath-api knows no such app
            return {"error": {"rule": "GP-API-404", "message": f"no app named {app}", "hint": ""}}
        if "content" in body:      # repo.pr.open's whole file
            try:
                head = yaml.safe_load(body["content"])
            except yaml.YAMLError:
                head = None
            if not isinstance(head, dict):
                return {"valid": False, "changes": [], "summary": "1 error(s): GP-YAML-001; no change", "base": base_info,
                        "resulting": body["content"], "contractVersion": 1, "problems": [
                            {"pointer": "/", "rule": "GP-YAML-001", "severity": "error", "message": "cicd.yaml is not a mapping",
                             "hint": "a cicd.yaml is a YAML mapping starting with apiVersion: platform/v1"}]}
        else:
            head = merge_patch(self.CICD_BASE, body.get("patch") or {})
        out_changes = []
        for ptr, b, h in changes(self.CICD_BASE, head):
            c = {"pointer": ptr, "change": "removed" if h is MISSING else "added" if b is MISSING else "changed",
                 "agentScope": "tighten" if ptr.startswith("/governance/") else "open"}
            if b is not MISSING:
                c["before"] = b
            if h is not MISSING:
                c["after"] = h
            out_changes.append(c)
        problems = []
        # The schema, as the validator reads it: an agent the contract does not list.
        if head.get("build", {}).get("agent") not in (None, "nodejs-22", "nodejs-20", "python-3.12", "go-1.23"):
            problems.append({"pointer": "/build/agent", "rule": "GP-SCHEMA-002", "severity": "error",
                             "message": f"/build/agent: {head['build']['agent']!r} is not one of the build agents",
                             "hint": "see docs/user/cicd-yaml-reference.md"})
        if body.get("agent") and head.get("governance", {}).get("sast") is False:
            problems.append({"pointer": "/governance/sast", "rule": "GP-SCOPE-002", "severity": "error",
                             "message": "/governance/sast loosened (true -> false)",
                             "hint": "An agent may turn a gate on or make it stricter, never the other way."})
        summary = ("valid" if not problems else f"{len(problems)} error(s): " + ", ".join(sorted({p['rule'] for p in problems}))) + \
            ("; no change" if not out_changes else f"; {len(out_changes)} change(s)")
        return {"valid": not problems, "problems": problems, "changes": out_changes, "summary": summary, "base": base_info,
                "resulting": body["content"] if "content" in body else yaml.safe_dump(head, sort_keys=True), "contractVersion": 1}

    def _replayed(self, name, key, dry_run):
        return not dry_run and sum(1 for c in self.calls if c[0] == name and c[1][3] == key and not c[1][4]) > 1

    def promote(self, app, env, body, idempotency_key, dry_run, on_behalf_of):
        self._rec("glidepath.promote", app, env, body, idempotency_key, dry_run, on_behalf_of=on_behalf_of)
        out = {"mode": "pipeline", "pipelineRun": f"promote-{env}-3333333a-{idempotency_key[-6:]}"}
        if self._replayed("glidepath.promote", idempotency_key, dry_run):
            out["replayed"] = True
        if dry_run:
            out.update(dryRun=True, object={"kind": "PipelineRun"})
        return out

    def rollback(self, app, env, body, idempotency_key, dry_run, on_behalf_of):
        self._rec("glidepath.rollback", app, env, body, idempotency_key, dry_run, on_behalf_of=on_behalf_of)
        out = {"alreadyOpen": False, "pipelineRun": f"rollback-{env}-2222222b-{idempotency_key[-6:]}",
               "target": {"releaseId": body["targetReleaseId"]}, "replaces": "rel-3"}
        if self._replayed("glidepath.rollback", idempotency_key, dry_run):
            out["replayed"] = True
        if dry_run:
            out.update(dryRun=True, object={"kind": "PipelineRun"})
        return out


def fake_backends() -> Backends:
    # Airframe's fake is the real tool logic over an in-memory cluster and a fake validator (airframe_tools.py).
    from .airframe_tools import fake_airframe
    return Backends(FakeGit(), FakeArgo(), FakeRuns(), FakeRead(), FakeHuman(), FakeArtifacts(),
                    FakeGlidepath(), fake_airframe())
