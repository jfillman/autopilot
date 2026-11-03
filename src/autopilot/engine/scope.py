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


def _glob_body(glob: str) -> str:
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
    return "".join(out)


@lru_cache(maxsize=512)
def glob_to_regex(glob: str) -> re.Pattern[str]:
    """`**` crosses directories, `*` and `?` do not; a bare name matches at any depth
    only if written with a leading `**/`."""
    return re.compile("^" + _glob_body(glob) + "$")


@lru_cache(maxsize=512)
def pointer_glob_to_regex(glob: str) -> re.Pattern[str]:
    """Like glob_to_regex, but also matches any deeper JSON pointer under the pattern - a
    deny of `/release` must also catch a change at `/release/image/tag`, not just an
    exact-length match (AF-9a)."""
    return re.compile("^" + _glob_body(glob) + "(?:/.*)?$")


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


def _ptr_escape(token: object) -> str:
    return str(token).replace("~", "~0").replace("/", "~1")


def diff_pointers(before: object, after: object, prefix: str = "") -> set[str]:
    """AF-9a: RFC-6901 JSON pointers for every subtree that differs between two parsed file
    contents (dicts/lists/scalars from yaml.safe_load or json.load). Recurses as deep as the
    two structures stay comparable (same type, same dict keys, same list length), so a single
    changed leaf yields its own precise pointer (e.g. `/components/2/spec/mode`) rather than a
    coarse one - but a structural change (a key or list length that differs) stops at the
    pointer to that container, since there's no meaningful deeper comparison to make."""
    if before == after:
        return set()
    if isinstance(before, dict) and isinstance(after, dict):
        pointers: set[str] = set()
        for k in sorted(set(before) | set(after), key=str):
            ptr = f"{prefix}/{_ptr_escape(k)}"
            if k not in before or k not in after:
                pointers.add(ptr)
            elif before[k] != after[k]:
                pointers |= diff_pointers(before[k], after[k], ptr)
        return pointers
    if isinstance(before, list) and isinstance(after, list) and len(before) == len(after):
        pointers = set()
        for i, (b, a) in enumerate(zip(before, after)):
            if b != a:
                pointers |= diff_pointers(b, a, f"{prefix}/{i}")
        return pointers
    return {prefix or "/"}


def field_violations(pointers: set[str] | list[str], deny_globs: list[str]) -> list[str]:
    """AF-9a: changed JSON pointers that match a deny glob, or fall under one as a subtree."""
    return sorted(p for p in pointers if any(pointer_glob_to_regex(g).match(p) for g in deny_globs))


def field_outside_allow(pointers: set[str] | list[str], allow_globs: list[str]) -> list[str]:
    """AF-9a: with an allowlist set, changed pointers matching none of it are violations too.
    An empty allowlist means no allow-based restriction (only denies apply)."""
    if not allow_globs:
        return []
    return sorted(p for p in pointers if not any(pointer_glob_to_regex(g).match(p) for g in allow_globs))
