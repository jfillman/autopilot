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


class PipelineBackend(Protocol):
    def rerun(self, app: str, pipelinerun: str) -> dict[str, Any]: ...


class ArtifactBackend(Protocol):
    def put(self, task_id: str, name: str, content: str) -> dict[str, Any]: ...
    def get(self, task_id: str, name: str) -> str | None: ...


class HumanBackend(Protocol):
    def request(self, question: str, options: list[str] | None) -> dict[str, Any]: ...
    def recv(self, session_id: str) -> list[str]: ...
    def send(self, session_id: str, text: str) -> dict[str, Any]: ...


class Backends:
    def __init__(self, git, argo, runs, read, pipelines, human, artifacts=None):
        self.git, self.argo, self.runs, self.read, self.pipelines, self.human = git, argo, runs, read, pipelines, human
        self.artifacts = artifacts


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


class FakePipelines(Recorder):
    def rerun(self, app, pipelinerun):
        self._rec("pipelines.rerun", app, pipelinerun)
        return {"rerun": pipelinerun}


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


def fake_backends() -> Backends:
    return Backends(FakeGit(), FakeArgo(), FakeRuns(), FakeRead(), FakePipelines(), FakeHuman(), FakeArtifacts())
