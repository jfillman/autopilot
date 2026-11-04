"""The gateway's own version: the image tag it was built from (gateway-image.yaml passes it as the GATEWAY_VERSION
build argument; the Dockerfile sets AUTOPILOT_GATEWAY_VERSION from it). "dev" outside an image.

The version the evidence chain names is not this one: the core stamps every record the gateway writes with the
`version` of its autopilot-gateway entry in CLEARANCE_PROVIDERS (the core's configuration, rendered by Apron from
versions.autopilot.gateway). The two are the same value when the deployment is right; the start line (serve.describe)
prints this one so a person can compare it with the core's registration (docs/admin/provider.md).
"""
from __future__ import annotations

import os
from typing import Mapping


def gateway_version(env: Mapping[str, str] | None = None) -> str:
    v = ((os.environ if env is None else env).get("AUTOPILOT_GATEWAY_VERSION") or "").strip()
    return v[:64] if v else "dev"
