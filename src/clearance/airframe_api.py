"""The Kubernetes read adapter behind the airframe.* tools: GET only, against a hard allowlist of paths.

Clearance reads the cluster as its own identity, a bearer token read from a file on every call (a projected
ServiceAccount token the kubelet rotates; nothing keeps it, nothing prints it), verified against a CA file that
is also read on every call. RBAC is the outer limit (gitops-hangar-dev#40: get/list/watch on catalog.hangar.io
and argoproj.io applications/rollouts, nothing else); this adapter is the inner one, so a mistake in either
alone still reads nothing else. A path outside the allowlist is refused here, before any request is built:
  /apis/catalog.hangar.io/v1alpha1[/namespaces/<ns>]/<plural>[/<name>]     any Airframe XR kind
  /apis/argoproj.io/v1alpha1[/namespaces/<ns>]/{applications,rollouts}[/<name>]
No subresource (status, exec, portforward, proxy, log), no core API group (Secrets, ConfigMaps, Pods), no
watch, and only the list parameters labelSelector, limit and continue.

profile.airframe.apps narrows what an agent may read per app before any call is made (policy R023); every
namespace and name the tools put into a path is built from that app, never taken from the agent whole.
"""
from __future__ import annotations

import json
import re
import ssl
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Callable

from .dispatch import Denied

_SEG = r"[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?"
_NS = r"[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?"
ALLOWED_PATHS = (
    re.compile(rf"^/apis/catalog\.hangar\.io/v1alpha1(/namespaces/{_NS})?/[a-z0-9]{{1,63}}(/{_SEG})?$"),
    re.compile(rf"^/apis/argoproj\.io/v1alpha1(/namespaces/{_NS})?/(applications|rollouts)(/{_SEG})?$"),
)
ALLOWED_QUERY = frozenset({"labelSelector", "limit", "continue"})
_AUTH_STATUSES = (401, 403)
MAX_PAGES = 20


class AirframeError(Exception):
    """The API server failed or could not be reached. Never shown to the agent (dispatch says "backend error")."""


class PathRefused(Denied):
    """A path outside the allowlist. Raised before any request exists, so nothing reaches the API server."""
    def __init__(self, path: str):
        super().__init__("AF-K8S-PATH", f"Clearance does not read {path!r}",
                         "The airframe.* tools read Airframe XRs and Argo CD Applications and Rollouts only; "
                         "Secrets, ConfigMaps, pods, exec, port-forward and proxy are never reachable.")


def path_allowed(path: str) -> bool:
    return any(p.match(path) for p in ALLOWED_PATHS)


Opener = Callable[[urllib.request.Request, float, ssl.SSLContext | None], tuple[int, bytes]]


def _urlopen(req: urllib.request.Request, timeout: float, ctx: ssl.SSLContext | None) -> tuple[int, bytes]:
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=ctx) as res:  # noqa: S310 - the URL is configuration
            return res.status, res.read(16 << 20)
    except urllib.error.HTTPError as e:
        return e.code, e.read(64 << 10)


class KubeReader:
    """GET-only, allowlisted reads of one API server."""

    def __init__(self, server: str, token_file: str | Path, ca_file: str | Path | None = None,
                 timeout: float = 30.0, opener: Opener = _urlopen):
        self.server = server.rstrip("/")
        self.token_file = Path(token_file)
        self.ca_file = Path(ca_file) if ca_file else None
        self.timeout = timeout
        self._open = opener

    def get(self, path: str, query: dict[str, Any] | None = None) -> Any:
        """One GET. 200 is the object, 404 is None (an answer), 401/403 a denial, anything else a hidden failure."""
        q = {k: v for k, v in (query or {}).items() if v is not None}
        if not path_allowed(path) or not set(q) <= ALLOWED_QUERY:
            raise PathRefused(path + ("?" + "&".join(sorted(q)) if q else ""))
        url = self.server + path + ("?" + urllib.parse.urlencode(q) if q else "")
        token = self.token_file.read_text().strip()
        ctx = ssl.create_default_context(cafile=str(self.ca_file)) if self.ca_file else None
        req = urllib.request.Request(url, method="GET",
                                     headers={"Authorization": f"Bearer {token}", "Accept": "application/json"})
        try:
            status, body = self._open(req, self.timeout, ctx)
        except OSError as e:
            raise AirframeError(f"API server unreachable: {type(e).__name__}") from e
        if status == 200:
            return json.loads(body)
        if status == 404:
            return None
        if status in _AUTH_STATUSES:
            raise Denied("AF-K8S-403", f"the API server refused Clearance's identity ({status})",
                         "Clearance's ServiceAccount may not read this; ask a human to check its RBAC.")
        raise AirframeError(f"API server {status}")

    def list(self, path: str, label_selector: str | None = None) -> list[dict]:
        """Every item of a list, following `continue` (bounded)."""
        items: list[dict] = []
        cont = None
        for _ in range(MAX_PAGES):
            page = self.get(path, {"labelSelector": label_selector, "limit": 500, "continue": cont})
            if page is None:
                return items
            items += page.get("items") or []
            cont = (page.get("metadata") or {}).get("continue")
            if not cont:
                return items
        raise AirframeError(f"more than {MAX_PAGES} pages")


def _seg(s: str) -> str:
    return urllib.parse.quote(s, safe="")


def xr_path(plural: str, namespace: str, name: str | None = None) -> str:
    return f"/apis/catalog.hangar.io/v1alpha1/namespaces/{_seg(namespace)}/{_seg(plural)}" + (f"/{_seg(name)}" if name else "")


def argo_path(resource: str, namespace: str, name: str | None = None) -> str:
    return f"/apis/argoproj.io/v1alpha1/namespaces/{_seg(namespace)}/{_seg(resource)}" + (f"/{_seg(name)}" if name else "")
