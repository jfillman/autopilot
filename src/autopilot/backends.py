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


def fake_backends() -> Backends:
    # Airframe's fake is the real tool logic over an in-memory cluster and a fake validator (airframe_tools.py).
    from .airframe_tools import fake_airframe
    return Backends(FakeGit(), FakeArgo(), FakeRuns(), FakeRead(), FakeHuman(), FakeArtifacts(),
                    FakeGlidepath(), fake_airframe())
