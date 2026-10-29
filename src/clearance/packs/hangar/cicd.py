"""Glidepath's cicd.yaml scope (GP-SCOPE-001/002/003) on every changed cicd.yaml: the check behind R021.

An agent may change cicd.yaml in a pull request, but only field by field, under Glidepath's own rules.
The rules and the code that reads them are Glidepath's, vendored (vendor/), so the decision here is the
one the pull request's own `glidepath validate --agent` check would give.
"""
from __future__ import annotations

import copy
import json
import posixpath
from pathlib import Path
from typing import Any

import yaml

from clearance.scope import BadPath, normalize

from .vendor.cicd_scope import scope_problems

# Glidepath's contract, vendored byte for byte from glidepath (contract/glidepath-contract.json) with
# the code that reads it (vendor/cicd_scope.py); tests/test_cicd_scope.py fails if either drifts.
CICD_SCOPE = json.loads((Path(__file__).resolve().parent / "vendor" / "glidepath-contract.json").read_text())["scope"]


def merge_patch(target: Any, patch: Any) -> Any:
    """RFC 7386 JSON merge patch: dicts merge recursively, null deletes a key, anything else replaces."""
    if not isinstance(patch, dict):
        return copy.deepcopy(patch)
    out = copy.deepcopy(target) if isinstance(target, dict) else {}
    for k, v in patch.items():
        if v is None:
            out.pop(k, None)
        else:
            out[k] = merge_patch(out.get(k), v)
    return out


def _is_cicd_yaml(path: Any) -> bool:
    try:
        return posixpath.basename(normalize(path)) == "cicd.yaml"
    except BadPath:
        return False            # path_violations (R015) already denies an unsafe path


def _whole_file(path: str, why: str) -> dict[str, str]:
    return {"pointer": "", "rule": "GP-SCOPE-001", "message": f"{path}: {why}",
            "hint": "Send cicd.yaml as YAML (a mapping), with `before` set to its current content, "
                    "so Clearance can tell which fields change."}


def cicd_scope_problems(files: list[dict[str, Any]], scope: dict = CICD_SCOPE) -> list[dict[str, str]]:
    """Glidepath's GP-SCOPE rules on every changed cicd.yaml, at any depth.

    base: the file's `before` (its current content, the same field AF-9a diffs). No `before` means
    the file is new, so every field counts as added, and a new cicd.yaml with apiVersion/kind is a
    human's change (GP-SCOPE-001). Leaving `before` out therefore never widens what passes.
    head: `content`, parsed as YAML; with `op: merge-patch` (the planner's FileChange) it is a merge
    patch (a dict, or YAML text of one) applied to the base first. Anything that doesn't parse to a
    mapping fails closed as a whole-file change."""
    problems: list[dict[str, str]] = []
    for f in files:
        if not isinstance(f, dict) or not _is_cicd_yaml(f.get("path")):
            continue
        path = f["path"]
        before, content = f.get("before"), f.get("content")
        try:
            base = yaml.safe_load(before) if isinstance(before, str) else before
            head = yaml.safe_load(content) if isinstance(content, str) else content
        except yaml.YAMLError:
            problems.append(_whole_file(path, "does not parse as YAML"))
            continue
        if base is not None and not isinstance(base, dict):
            problems.append(_whole_file(path, "the current content (`before`) is not a YAML mapping"))
            continue
        if f.get("op") == "merge-patch":
            if not isinstance(head, dict):
                problems.append(_whole_file(path, "a merge-patch must be a mapping"))
                continue
            head = merge_patch(base or {}, head)
        if not isinstance(head, dict):
            problems.append(_whole_file(path, "the new content is not a YAML mapping"))
            continue
        try:
            found = scope_problems(base, head, scope)
        except Exception as e:  # noqa: BLE001 - a rule that can't be evaluated denies, never allows
            problems.append(_whole_file(path, f"the scope check could not be evaluated ({type(e).__name__})"))
            continue
        for p in found:
            problems.append({**p, "message": f"{path}: {p['message']}"})
    return problems
