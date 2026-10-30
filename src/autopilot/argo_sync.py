"""argo.sync.lower's adapter: start a sync of one lower-environment Argo CD Application, as Clearance's own identity.

Argo CD starts a sync when an Application's `operation` field is set (what the argocd CLI and UI do through the Argo
CD API), so no Argo CD account or token is needed: one merge PATCH through the Kubernetes API, and nothing else is
ever sent:
  PATCH /apis/argoproj.io/v1alpha1/namespaces/<ns>/applications/<name>   application/merge-patch+json

Clearance decided the call before this runs: the tool's tier, the definition's tools, and R012, which reads "lower"
from the `project` argument (a project named <app>-lower). The agent names that project, so this adapter checks the
Application it would patch: it must be in one of the configured namespaces, its spec.project must be exactly the
argument and end in -lower, and no operation may be in flight. The patch carries the resourceVersion that was read,
so an Application changed in between (moved to another project) is a 409, not a sync. The body sets `operation` and
the resourceVersion only: a sync of the Application's own target revision, with its own sync options.

Three limits, outer to inner:
  RBAC        Role `autopilot-clearance-argo-sync` in argocd-apps: get and patch applications, nothing else
  admission   ValidatingAdmissionPolicy `autopilot-clearance-argo-sync`: for Clearance's identity, an Application
              update must be in a -lower project, change nothing but `operation`, start a sync and never override
              its source (apron 55-autopilot/clearance/argo-sync-policy.yaml)
  here        the checks above, before any request is built

The operation names who asked (initiatedBy.username `clearance:<agent>`, info: task, session, principal), so Argo
CD's own history shows it. The result says the sync was requested; argo.app.get shows how it went.
"""
from __future__ import annotations

import json
import re
import ssl
import urllib.request
from pathlib import Path
from typing import Any

from clearance.dispatch import Denied

from .airframe_api import KubeReader, Opener, _urlopen, argo_path
from .kube_runs import IN_CLUSTER_API, SA_DIR

LOWER_SUFFIX = "-lower"
APP_NAME = re.compile(r"^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$")


class ArgoSyncError(Exception):
    """The API server refused or failed the patch. Never shown to the agent (dispatch says "backend error")."""


class ArgoSync:
    def __init__(self, reader: KubeReader, namespaces: tuple[str, ...] = ("argocd-apps",), server: str = IN_CLUSTER_API,
                 token_file: str | Path = SA_DIR / "token", ca_file: str | Path | None = SA_DIR / "ca.crt",
                 timeout: float = 30.0, opener: Opener = _urlopen):
        self.kube, self.namespaces = reader, tuple(namespaces)
        self.server, self.token_file = server.rstrip("/"), Path(token_file)
        self.ca_file = Path(ca_file) if ca_file else None
        self.timeout, self._open = timeout, opener

    def _find(self, app: str) -> tuple[str, dict[str, Any]] | None:
        for ns in self.namespaces:
            obj = self.kube.get(argo_path("applications", ns, app))
            if obj is not None:
                return ns, obj
        return None

    def sync(self, app: str, project: str, who: dict[str, str]) -> dict[str, Any]:
        if not isinstance(project, str) or not project.endswith(LOWER_SUFFIX):
            raise Denied("ARGO-PROJECT", f"{project!r} is not a lower-environment project",
                         "argo.sync.lower syncs Applications in an <app>-lower project only.")
        if not isinstance(app, str) or not APP_NAME.match(app):
            raise Denied("READ-ARGO-NAME", f"{app!r} is not an Application name",
                         "Pass the Argo CD Application's name, e.g. flight-api-dev.")
        found = self._find(app)
        if found is None:
            return {"app": app, "found": False, "searched": list(self.namespaces)}
        ns, obj = found
        actual = (obj.get("spec") or {}).get("project")
        if actual != project:
            raise Denied("ARGO-PROJECT", f"{app} is in project {actual!r}, not {project!r}",
                         "Pass the Application's own project (argo.app.get shows it); only -lower projects sync.")
        if obj.get("operation") or ((obj.get("status") or {}).get("operationState") or {}).get("phase") == "Running":
            raise Denied("ARGO-BUSY", f"{app} already has an operation in progress",
                         "Wait for it to finish (argo.app.get), then sync again if it is still needed.")
        rv = (obj.get("metadata") or {}).get("resourceVersion")
        operation = {
            "initiatedBy": {"username": f"clearance:{who.get('agent', 'agent')}"},
            "info": [{"name": k, "value": v} for k, v in sorted(who.items()) if k != "agent" and v],
            "sync": {},
        }
        status, body = self._patch(argo_path("applications", ns, app),
                                   {"metadata": {"resourceVersion": rv}, "operation": operation})
        if status == 200:
            return {"app": app, "namespace": ns, "project": project, "requested": True,
                    "hint": "Argo CD is syncing it; argo.app.get shows the operation's phase and the app's health."}
        if status == 409:
            raise Denied("ARGO-CHANGED", f"{app} changed while the sync was being requested",
                         "Read it again with argo.app.get and retry.")
        if status in (401, 403):
            raise Denied("ARGO-K8S-403", f"the API server refused to sync {app} ({status})",
                         "Clearance may sync lower-environment Applications only; ask a human to check its grant.")
        raise ArgoSyncError(f"API server {status} patching {app}: {body[:200]!r}")

    def _patch(self, path: str, body: dict[str, Any]) -> tuple[int, bytes]:
        token = self.token_file.read_text().strip()
        ctx = ssl.create_default_context(cafile=str(self.ca_file)) if self.ca_file else None
        req = urllib.request.Request(self.server + path, method="PATCH", data=json.dumps(body).encode(),
                                     headers={"Authorization": f"Bearer {token}", "Accept": "application/json",
                                              "Content-Type": "application/merge-patch+json"})
        try:
            return self._open(req, self.timeout, ctx)
        except OSError as e:
            raise ArgoSyncError(f"API server unreachable: {type(e).__name__}") from e
