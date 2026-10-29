"""Argument shapes for glidepath.* and airframe.* calls, and the per-app allowlist match (R022/R023).

A value that fails here is a bad argument (R008), so nothing odd ever reaches a glidepath-api URL or a
Kubernetes path; the backends' own checks are the second wall.
"""
from __future__ import annotations

import fnmatch
import posixpath
import re
from typing import Any

from clearance.scope import BadPath, normalize

# glidepath-api's own path rules (api/openapi.yaml): an argument that would not be a valid path
# segment there is a bad argument here, so nothing odd is ever put into a URL.
_GP_ARGS = {
    "app": re.compile(r"^[a-z0-9][a-z0-9-]{0,62}$"),
    "result": re.compile(r"^[0-9a-f-]{36}$"),
    "taskRun": re.compile(r"^[a-z0-9][a-z0-9.-]{0,252}$"),
    "pipelinerun": re.compile(r"^[a-z0-9][a-z0-9.-]{0,252}$"),
    "chainId": re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$"),
}


def glidepath_bad_args(args: dict[str, Any]) -> int:
    bad = 0
    for k, pat in _GP_ARGS.items():
        if k in args and not (isinstance(args[k], str) and pat.match(args[k])):
            bad += 1
    for k, hi in (("sinceHours", 744), ("limit", 1000)):
        if k in args and not (isinstance(args[k], int) and not isinstance(args[k], bool) and 1 <= args[k] <= hi):
            bad += 1
    return bad


def glidepath_app_allowed(app: str, allow: list[str]) -> bool:
    return any(fnmatch.fnmatchcase(app, g) for g in allow)


# airframe.* arguments. Shape only: every namespace and name the tools read is built from `app` (and `env`), so a
# value that is not a Kubernetes name never reaches a path; the adapter's own allowlist is the second wall.
_AF_ARGS = {
    "app": re.compile(r"^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$"),
    "env": re.compile(r"^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$"),
    "name": re.compile(r"^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$"),
    "kind": re.compile(r"^[A-Za-z][A-Za-z0-9]{0,62}$"),
    "rule": re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$"),
}
_AF_SECTIONS = frozenset({"chart", "xrds", "components", "targets", "kinds"})
_AF_DOC_NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.ya?ml$")
_AF_MAX_DOCS, _AF_MAX_BYTES = 20, 256 << 10


def _af_document_ok(d: Any) -> bool:
    if not isinstance(d, dict) or set(d) - {"path", "content"} or not isinstance(d.get("content"), str):
        return False
    try:
        path = normalize(d.get("path"))
    except BadPath:
        return False
    return bool(_AF_DOC_NAME.match(posixpath.basename(path))) and len(d["content"].encode()) <= _AF_MAX_BYTES


def airframe_bad_args(args: dict[str, Any]) -> int:
    bad = sum(1 for k, pat in _AF_ARGS.items() if k in args and not (isinstance(args[k], str) and pat.match(args[k])))
    if "section" in args and args["section"] not in _AF_SECTIONS:
        bad += 1
    if "documents" in args:
        docs = args["documents"]
        bad += not (isinstance(docs, list) and 1 <= len(docs) <= _AF_MAX_DOCS and all(_af_document_ok(d) for d in docs))
    if "mode" in args and args["mode"] not in ("values", "xr"):
        bad += 1
    if "render" in args and not isinstance(args["render"], bool):
        bad += 1
    if "spec" in args and not isinstance(args["spec"], dict):
        bad += 1
    if "baseCicd" in args and not (isinstance(args["baseCicd"], dict)
                                   or (isinstance(args["baseCicd"], str) and len(args["baseCicd"]) <= _AF_MAX_BYTES)):
        bad += 1
    return bad
