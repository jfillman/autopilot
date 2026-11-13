"""Gateway settings: the AUTOPILOT_* names.

The gateway read most of its own settings under a CLEARANCE_ prefix, from when it was part of Clearance. R-10a renamed
them to AUTOPILOT_*, with the old names as fallbacks for one release; R-10e removed the fallbacks. Settings that address
the Clearance product (CLEARANCE_CORE_URL, CLEARANCE_CORE_TOKEN_FILE, CLEARANCE_API_URL) keep their names.

An old name that is still set is refused at start (`retired`), not ignored: running without the OIDC issuer or the
allowed-users list the operator asked for would be a silent downgrade.
"""
from __future__ import annotations

from collections.abc import Mapping

# The retired CLEARANCE_* names and what replaces each.
RETIRED: dict[str, str] = {
    "CLEARANCE_EPHEMERAL": "AUTOPILOT_EPHEMERAL",
    "CLEARANCE_OIDC_ISSUER": "AUTOPILOT_OIDC_ISSUER",
    "CLEARANCE_OIDC_AUDIENCE": "AUTOPILOT_OIDC_AUDIENCE",
    "CLEARANCE_OIDC_ALLOWED_USERS": "AUTOPILOT_OIDC_ALLOWED_USERS",
    "CLEARANCE_OIDC_JWKS_URL": "AUTOPILOT_OIDC_JWKS_URL",
    "CLEARANCE_OIDC_USER_CLAIM": "AUTOPILOT_OIDC_USER_CLAIM",
    "CLEARANCE_AGENTS_DIR": "AUTOPILOT_AGENTS_DIR",
    "CLEARANCE_AUDIENCE": "AUTOPILOT_AUDIENCE",
    "CLEARANCE_RESOURCE_URL": "AUTOPILOT_RESOURCE_URL",
    "CLEARANCE_ISSUER_URL": "AUTOPILOT_ISSUER_URL",
    "CLEARANCE_EXPIRE_SECONDS": "AUTOPILOT_EXPIRE_SECONDS",
    "CLEARANCE_DELEGATES": "AUTOPILOT_DELEGATES",
    "CLEARANCE_ADMINS": "AUTOPILOT_ADMINS",
    "CLEARANCE_GATEWAY_URL": "AUTOPILOT_GATEWAY_URL",
}


def setting(env: Mapping[str, str], name: str, *, default: str | None = None) -> str | None:
    """`name`'s value, else `default`. An empty variable is an unset one."""
    return env.get(name) or default


def retired(env: Mapping[str, str]) -> str | None:
    """The refusal message when a retired CLEARANCE_* name is set (naming each and its replacement), else None."""
    bad = sorted(k for k in RETIRED if env.get(k))
    if not bad:
        return None
    return ("retired settings (R-10e): " + "; ".join(f"{k} -> {RETIRED[k]}" for k in bad)
            + ". Rename them: the old names are no longer read")
