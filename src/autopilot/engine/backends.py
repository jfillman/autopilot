"""Backend interfaces for the core tools, the handlers that call them, and in-memory fakes.

Clearance decides; a backend acts. The Gateway takes a handler per tool (tool name -> callable(session,
args)); core_handlers() builds the core tools' handlers over these interfaces, and a deployment adds
its packs' handlers next to them (autopilot.gateway does, for glidepath.* and airframe.*).

RunBackend is the one interface the Gateway calls itself: run.spawn and run.close need the session
store, so their handlers are core, but what a run IS (an AgentRun XR on Hangar) is the deployment's.

None of the real adapters (GitHub via the token-review-interceptor, the ArgoCD API, Backstage MCP)
are implemented yet. The interfaces are deliberately small so they can be built and verified one at a time.
"""
from __future__ import annotations

from typing import TYPE_CHECKING, Any, Callable, Protocol

if TYPE_CHECKING:
    from .profile import AgentDefinition
    from .session import Session

Handler = Callable[["Session", dict[str, Any]], Any]


class GitBackend(Protocol):
    """read_file is optional: a backend without it gives repo.file.get no handler."""
    def open_pr(self, repo: str, branch: str, title: str, files: list[dict], base: str = "main") -> dict[str, Any]: ...
    def commit_xr(self, app: str, env: str, kind: str, spec: dict[str, Any]) -> dict[str, Any]: ...


class ArgoBackend(Protocol):
    def get_app(self, app: str) -> dict[str, Any]: ...
    def sync(self, app: str, project: str) -> dict[str, Any]: ...


class RunBackend(Protocol):
    """Starts and stops the workload behind a session. create returns the run's name.

    A backend may also define `principal(session) -> str | None`: the identity the run's own workload
    authenticates as (on Hangar, the run namespace's ServiceAccount). The Gateway then treats that caller as the
    session's owner for calls into that session only, so a run acts as its session without holding its owner's
    credential. Without it, only the session's principal owns the session."""
    def create(self, session: "Session", definition: "AgentDefinition", run_input: dict[str, Any] | None) -> str: ...
    def delete(self, session: "Session") -> None: ...


class ReadBackend(Protocol):
    def catalog(self, query: str) -> Any: ...
    def promql(self, q: str) -> Any: ...
    def logql(self, q: str) -> Any: ...
    def app_api(self, service: str, path: str) -> Any: ...


class ArtifactBackend(Protocol):
    def put(self, task_id: str, name: str, content: str) -> dict[str, Any]: ...
    def get(self, task_id: str, name: str) -> str | None: ...


class HumanBackend(Protocol):
    def request(self, question: str, options: list[str] | None) -> dict[str, Any]: ...
    def recv(self, session_id: str) -> list[str]: ...
    def send(self, session_id: str, text: str) -> dict[str, Any]: ...


def core_handlers(*, git: GitBackend | None = None, argo: ArgoBackend | None = None,
                  read: ReadBackend | None = None, human: HumanBackend | None = None,
                  artifacts: ArtifactBackend | None = None) -> dict[str, Handler]:
    """Handlers for the core tools whose backend is given; a tool whose backend is left out has none,
    so a call to it is refused as having no backend in this deployment."""
    h: dict[str, Handler] = {}
    if read is not None:
        h["catalog.read"] = lambda s, a: read.catalog(a["query"])
        h["metrics.query"] = lambda s, a: read.promql(a["promql"])
        h["logs.query"] = lambda s, a: read.logql(a["logql"])
        h["app.api.get"] = lambda s, a: read.app_api(a["service"], a["path"])
    if argo is not None:
        h["argo.app.get"] = lambda s, a: argo.get_app(a["app"])
        h["argo.sync.lower"] = lambda s, a: argo.sync(a["app"], a["project"])
    if artifacts is not None:
        h["artifact.put"] = lambda s, a: artifacts.put(s.task_id, a["name"], a["content"])
        h["artifact.get"] = lambda s, a: artifacts.get(s.task_id, a["name"])
    if human is not None:
        h["chat.recv"] = lambda s, a: human.recv(s.id)
        h["chat.send"] = lambda s, a: human.send(s.id, a["text"])
        h["human.request"] = lambda s, a: human.request(a["question"], a.get("options"))
    if git is not None and hasattr(git, "read_file"):
        h["repo.file.get"] = lambda s, a: git.read_file(a["repo"], a["path"], a.get("ref", "main"))
    if git is not None:
        h["repo.pr.open"] = lambda s, a: git.open_pr(a["repo"], a["branch"], a["title"], a["files"], a.get("base", "main"))
        h["repo.pr.open_upper"] = lambda s, a: git.open_pr(a["repo"], a["branch"], a["title"], a["files"], a.get("base", "main"))
        h["xr.request"] = lambda s, a: git.commit_xr(a["app"], a["env"], a["kind"], a["spec"])
    return h


class Recorder:
    """Fake that records every call, so tests can assert what did and did not happen."""
    def __init__(self):
        self.calls: list[tuple[str, tuple, dict]] = []

    def _rec(self, name, *a, **k):
        self.calls.append((name, a, k))

    def names(self) -> list[str]:
        return [c[0] for c in self.calls]


class FakeGit(Recorder):
    def read_file(self, repo, path, ref="main"):
        self._rec("git.read_file", repo, path, ref)
        return {"path": path, "ref": ref, "content": "x"}

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


class FakeRunBackend(Recorder):
    """A RunBackend that starts nothing: it names the run after the session and records the calls."""
    def __init__(self):
        super().__init__()
        self.live: dict[str, str] = {}

    def create(self, session, definition, run_input):
        name = "r-" + session.id.removeprefix("s-")
        self._rec("runs.create", name)
        self.live[session.id] = name
        return name

    def delete(self, session):
        self._rec("runs.delete", self.live.pop(session.id, None))


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
