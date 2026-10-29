"""The real AgentRunStore: AgentRun XRs created and deleted through the Kubernetes API, as Clearance's own identity.

Two requests only, both in the run namespace, and nothing else is ever built:
  POST   /apis/catalog.hangar.io/v1alpha1/namespaces/autopilot-runs/agentruns          create (409 is an error)
  DELETE /apis/catalog.hangar.io/v1alpha1/namespaces/autopilot-runs/agentruns/<name>   delete (404 is success)

RBAC is the outer limit (Role `clearance-agentruns` in autopilot-runs); this adapter is the inner one: a manifest
that is not an AgentRun in autopilot-runs, or a name that is not a DNS label, is refused before any request exists.
The bearer token and the CA are read from their files on every call (the kubelet rotates the projected token;
nothing keeps it, nothing prints it), as airframe_api.KubeReader does.
"""
from __future__ import annotations

import json
import re
import ssl
import urllib.request
from pathlib import Path
from typing import Any

from .airframe_api import Opener, _urlopen
from .manifests import API_VERSION, RUN_NAMESPACE

SA_DIR = Path("/var/run/secrets/kubernetes.io/serviceaccount")
IN_CLUSTER_API = "https://kubernetes.default.svc"
COLLECTION = f"/apis/{API_VERSION}/namespaces/{RUN_NAMESPACE}/agentruns"
_NAME = re.compile(r"^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$")


class KubeRunError(Exception):
    """The API server refused or failed a create or delete. Never shown to the agent (dispatch says "backend error")."""


class RunRefused(KubeRunError):
    """A manifest or name outside what this adapter may send. Raised before any request is built."""


def allowed(method: str, path: str) -> bool:
    """The whole allowlist: POST to the collection, DELETE of one named AgentRun in it."""
    if method == "POST":
        return path == COLLECTION
    if method == "DELETE" and path.startswith(COLLECTION + "/"):
        return bool(_NAME.match(path[len(COLLECTION) + 1:]))
    return False


class KubeAgentRuns:
    def __init__(self, server: str = IN_CLUSTER_API, token_file: str | Path = SA_DIR / "token",
                 ca_file: str | Path | None = SA_DIR / "ca.crt", timeout: float = 30.0, opener: Opener = _urlopen):
        self.server = server.rstrip("/")
        self.token_file = Path(token_file)
        self.ca_file = Path(ca_file) if ca_file else None
        self.timeout = timeout
        self._open = opener

    def _send(self, method: str, path: str, body: dict | None = None) -> tuple[int, bytes]:
        if not allowed(method, path):
            raise RunRefused(f"Clearance does not send {method} {path!r}")
        token = self.token_file.read_text().strip()
        ctx = ssl.create_default_context(cafile=str(self.ca_file)) if self.ca_file else None
        headers = {"Authorization": f"Bearer {token}", "Accept": "application/json"}
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(self.server + path, method=method, data=data, headers=headers)
        try:
            return self._open(req, self.timeout, ctx)
        except OSError as e:
            raise KubeRunError(f"API server unreachable: {type(e).__name__}") from e

    def create(self, manifest: dict[str, Any]) -> dict[str, Any]:
        meta = manifest.get("metadata") or {}
        name = meta.get("name")
        if (manifest.get("apiVersion") != API_VERSION or manifest.get("kind") != "AgentRun"
                or meta.get("namespace") != RUN_NAMESPACE or not isinstance(name, str) or not _NAME.match(name)):
            raise RunRefused("only an AgentRun with a DNS-label name in autopilot-runs may be created")
        status, body = self._send("POST", COLLECTION, manifest)
        if status in (200, 201, 202):
            return json.loads(body) if body else {"metadata": {"name": name}}
        if status == 409:
            raise KubeRunError(f"AgentRun {name} already exists")
        raise KubeRunError(f"API server {status} creating AgentRun {name}")

    def delete(self, name: str) -> None:
        if not isinstance(name, str) or not _NAME.match(name):
            raise RunRefused("an AgentRun name is a DNS label")
        status, _ = self._send("DELETE", f"{COLLECTION}/{name}")
        if status in (200, 202, 404):   # 404: already gone, which is what a delete wants
            return
        raise KubeRunError(f"API server {status} deleting AgentRun {name}")
