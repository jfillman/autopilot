"""Gateway settings: the AUTOPILOT_* names, and the CLEARANCE_* names they replaced (R-10a).

The gateway read most of its own settings under a CLEARANCE_ prefix, from when it was part of Clearance. Settings the
gateway owns are AUTOPILOT_*; settings that address the Clearance product (CLEARANCE_CORE_URL, CLEARANCE_CORE_TOKEN_FILE,
CLEARANCE_API_URL) keep their names. The old names still work for one release: the new name wins when both are set, and
using an old one logs a single deprecation warning at start naming what to use instead.
"""
from __future__ import annotations

import logging
from collections.abc import Mapping

log = logging.getLogger("autopilot.gateway")


def setting(env: Mapping[str, str], name: str, *, legacy: str | None = None, default: str | None = None,
            used: dict[str, str] | None = None) -> str | None:
    """`name`'s value, else the deprecated `legacy` name's (recorded in `used` as legacy -> name), else `default`.
    An empty variable is an unset one."""
    v = env.get(name)
    if v:
        return v
    if legacy:
        v = env.get(legacy)
        if v:
            if used is not None:
                used[legacy] = name
            return v
    return default


def warn_deprecated(used: Mapping[str, str]) -> None:
    """One warning for every legacy name in use (nothing when none is)."""
    if used:
        log.warning("deprecated settings in use, renamed in this release (the old names stop working in the next): %s",
                    "; ".join(f"{old} -> {new}" for old, new in sorted(used.items())))
