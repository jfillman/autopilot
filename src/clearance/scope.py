"""Path and repo scope. One implementation, shared with the agent-scope CI gate.

Paths are normalised before matching so `a/../.tekton/x` cannot smuggle past a deny glob.
"""
from __future__ import annotations

import posixpath
import re
from functools import lru_cache


class BadPath(ValueError):
    pass


def normalize(path: str) -> str:
    if not isinstance(path, str) or not path:
        raise BadPath("empty path")
    if "\x00" in path or "\\" in path:
        raise BadPath(f"illegal characters in path: {path!r}")
    if path.startswith("/"):
        raise BadPath(f"absolute path not allowed: {path!r}")
    norm = posixpath.normpath(path)
    if norm == ".." or norm.startswith("../") or norm == ".":
        raise BadPath(f"path escapes the repository: {path!r}")
    return norm


@lru_cache(maxsize=512)
def glob_to_regex(glob: str) -> re.Pattern[str]:
    """`**` crosses directories, `*` and `?` do not; a bare name matches at any depth
    only if written with a leading `**/`."""
    out, i = [], 0
    while i < len(glob):
        c = glob[i]
        if glob.startswith("**/", i):
            out.append("(?:.*/)?")
            i += 3
        elif glob.startswith("**", i):
            out.append(".*")
            i += 2
        elif c == "*":
            out.append("[^/]*")
            i += 1
        elif c == "?":
            out.append("[^/]")
            i += 1
        else:
            out.append(re.escape(c))
            i += 1
    return re.compile("^" + "".join(out) + "$")


def path_violations(paths: list[str], deny_globs: list[str]) -> list[str]:
    """Return the changed paths that match a deny glob. Unsafe paths count as violations."""
    bad: list[str] = []
    for p in paths:
        try:
            n = normalize(p)
        except BadPath:
            bad.append(p)
            continue
        if any(glob_to_regex(g).match(n) for g in deny_globs):
            bad.append(p)
    return bad


def repo_allowed(repo: str, allow: list[str]) -> bool:
    """`owner/name` against globs like `owner/*`. No allowlist means nothing is allowed."""
    if repo.count("/") != 1 or repo.startswith("/") or repo.endswith("/"):
        return False
    return any(glob_to_regex(a).match(repo) for a in allow)
