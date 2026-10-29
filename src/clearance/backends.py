"""Backend interfaces Clearance delegates to, plus in-memory fakes for tests.

None of the real adapters (GitHub via the token-review-interceptor, the ArgoCD API, the
Kubernetes API for AgentRun XRs, Backstage MCP) are implemented yet. The interfaces are
deliberately small so they can be built and verified one at a time.
"""
from __future__ import annotations

from typing import Any, Protocol


class GitBackend(Protocol):
    def open_pr(self, repo: str, branch: str, title: str, files: list[dict], base: str = "main") -> dict[str, Any]: ...
    def commit_xr(self, app: str, env: str, kind: str, spec: dict[str, Any]) -> dict[str, Any]: ...


class ArgoBackend(Protocol):
    def get_app(self, app: str) -> dict[str, Any]: ...
    def sync(self, app: str, project: str) -> dict[str, Any]: ...


class RunBackend(Protocol):
    def create(self, manifest: dict[str, Any]) -> dict[str, Any]: ...
    def delete(self, name: str) -> None: ...


class ReadBackend(Protocol):
    def catalog(self, query: str) -> Any: ...
    def promql(self, q: str) -> Any: ...
    def logql(self, q: str) -> Any: ...
    def app_api(self, service: str, path: str) -> Any: ...


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


class AirframeBackend(Protocol):
    """Airframe's contract, validator and planner, and read-only cluster state (airframe_tools.py is the real one)."""
    def capabilities(self, kind: str | None, section: str | None) -> Any: ...
    def validate(self, documents: list[dict], app: str | None, mode: str, render: bool) -> Any: ...
    def explain(self, rule: str) -> Any: ...
    def plan(self, spec: dict, base_cicd: Any) -> Any: ...
    def status(self, app: str, env: str | None) -> Any: ...
    def describe(self, app: str, env: str) -> Any: ...
    def verify(self, app: str, kind: str, name: str, env: str | None) -> Any: ...


class ArtifactBackend(Protocol):
    def put(self, task_id: str, name: str, content: str) -> dict[str, Any]: ...
    def get(self, task_id: str, name: str) -> str | None: ...


class HumanBackend(Protocol):
    def request(self, question: str, options: list[str] | None) -> dict[str, Any]: ...
    def recv(self, session_id: str) -> list[str]: ...
    def send(self, session_id: str, text: str) -> dict[str, Any]: ...


class Backends:
    def __init__(self, git, argo, runs, read, human, artifacts=None, glidepath=None, airframe=None):
        self.git, self.argo, self.runs, self.read, self.human = git, argo, runs, read, human
        self.artifacts, self.glidepath, self.airframe = artifacts, glidepath, airframe


class Recorder:
    """Fake that records every call, so tests can assert what did and did not happen."""
    def __init__(self):
        self.calls: list[tuple[str, tuple, dict]] = []

    def _rec(self, name, *a, **k):
        self.calls.append((name, a, k))

    def names(self) -> list[str]:
        return [c[0] for c in self.calls]


class FakeGit(Recorder):
    def open_pr(self, repo, branch, title, files, base="main"):
        self._rec("git.open_pr", repo, branch, title, files, base=base)
        return {"pr": len([c for c in self.calls if c[0] == "git.open_pr"]), "url": f"https://github.com/{repo}/pull/1"}

    def commit_xr(self, app, env, kind, spec):
        self._rec("git.commit_xr", app, env, kind, spec)
        return {"commit": "abc123"}


class FakeArgo(Recorder):
    def get_app(self, app):
        self._rec("argo.get_app", app)
        return {"app": app, "sync": "Synced", "health": "Healthy"}

    def sync(self, app, project):
        self._rec("argo.sync", app, project)
        return {"app": app, "operation": "started"}


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


class FakeRead(Recorder):
    def catalog(self, query):
        self._rec("read.catalog", query)
        return [{"name": "flight-api"}]

    def promql(self, q):
        self._rec("read.promql", q)
        return {"result": []}

    def logql(self, q):
        self._rec("read.logql", q)
        return {"result": []}

    def app_api(self, service, path):
        self._rec("read.app_api", service, path)
        return {"service": service, "path": path, "body": {}}


class FakeHuman(Recorder):
    def request(self, question, options=None):
        self._rec("human.request", question)
        return {"ticket": "h-1"}

    def recv(self, session_id):
        self._rec("human.recv", session_id)
        return []

    def send(self, session_id, text):
        self._rec("human.send", session_id, text)
        return {"sent": True}


class FakeArtifacts(Recorder):
    def __init__(self):
        super().__init__()
        self.store: dict[tuple[str, str], str] = {}

    def put(self, task_id, name, content):
        self._rec("artifacts.put", task_id, name)
        self.store[(task_id, name)] = content
        return {"stored": name, "bytes": len(content)}

    def get(self, task_id, name):
        self._rec("artifacts.get", task_id, name)
        return self.store.get((task_id, name))


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
    # Airframe's fake is the real tool logic over an in-memory cluster and a fake validator (airframe_tools.py),
    # imported here so the clearance core never imports it at module load.
    from .airframe_tools import fake_airframe
    return Backends(FakeGit(), FakeArgo(), FakeRuns(), FakeRead(), FakeHuman(), FakeArtifacts(),
                    FakeGlidepath(), fake_airframe())
