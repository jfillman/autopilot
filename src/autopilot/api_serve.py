"""`autopilot-gateway api`: clearance-api (clearance api.py) with Hangar's identity and catalog.

The read API runs from the gateway's image as a separate Deployment: same agent definitions, same callers' auth
(service-account tokens by TokenReview, people's OIDC, Tower's foreign workload), but its own read-only DSN and no
writer lock, no tools, no runs. On Hangar, a person's teams and apps come from Tower's catalog (BackstageDirectory,
BackstageOwnership), as the gateway's human inbox reads them.

Environment (README "Running the read API"):
  CLEARANCE_API_DSN         the read-only role's DSN (required)
  CLEARANCE_AGENTS_DIR      the bundled agent definitions (default: the image's agents/); the ones Agent XRs pin come
                            from the gateway's store (clearance_definitions), refreshed every CLEARANCE_API_DEFINITIONS_INTERVAL s (30)
  CLEARANCE_API_AUDITORS, CLEARANCE_API_NOTIFIERS, CLEARANCE_DELEGATES   principal globs, comma-separated
  CLEARANCE_GATEWAY_URL     where writes go, named in /v1/contract
  PORT (8080), and the gateway's auth and catalog variables (CLEARANCE_AUDIENCE, CLEARANCE_OIDC_*, TOWER_*,
  BACKSTAGE_URL, BACKSTAGE_TOKEN_FILE)
"""
from __future__ import annotations

import logging
import os
from typing import Mapping

from clearance import api, profile
from clearance.definitions import Definitions
from clearance.policy import Policy

from . import serve
from .agent_xrs import DefinitionsRefresher
from .gateway import PACKS
from .reads import BackstageCatalog, BackstageDirectory, BackstageOwnership

log = logging.getLogger("autopilot.api")


def build(env: Mapping[str, str] | None = None):
    env = os.environ if env is None else env
    get = lambda k, d=None: env.get(k) or d  # noqa: E731
    dsn = get("CLEARANCE_API_DSN")
    if not dsn:
        raise serve.ConfigError("CLEARANCE_API_DSN is required (the read-only role's DSN)")
    # The gateway's config parser for auth, catalog and agents; the store it would open is never used here.
    cfg = serve.config_from_env({**env, "CLEARANCE_EPHEMERAL": "1"})
    defs = Definitions(profile.load_dir(cfg.agents_dir))
    directory = ownership = None
    if cfg.backstage_url:
        catalog = BackstageCatalog(cfg.backstage_url, cfg.backstage_token_file)
        directory = BackstageDirectory(catalog)
        ownership = BackstageOwnership(catalog, directory)
    access = api.Access(serve.build_auth(cfg), defs, delegates=cfg.delegates,
                        notifiers=serve._list(get("CLEARANCE_API_NOTIFIERS", "")),
                        auditors=serve._list(get("CLEARANCE_API_AUDITORS", "")), directory=directory, ownership=ownership)
    store = api.PgReadStore(api.Pool(dsn, size=int(get("CLEARANCE_API_POOL", "5"))))
    # The cluster-pinned definitions the gateway loaded, from its store: this process fetches and decides nothing.
    DefinitionsRefresher(defs, store, interval=float(get("CLEARANCE_API_DEFINITIONS_INTERVAL", "30"))).start()
    # The same policy the gateway runs (core + the Hangar pack), for /v1/policy and the dry-run explainer.
    app = api.create_api_app(store, access, gateway_url=get("CLEARANCE_GATEWAY_URL"), policy=Policy(PACKS))
    return cfg, access, app


def main() -> int:
    logging.basicConfig(level=os.environ.get("LOG_LEVEL", "INFO").upper(),
                        format="%(asctime)s %(levelname)s %(name)s %(message)s")
    try:
        cfg, access, app = build()
    except serve.ConfigError as e:
        log.error("configuration: %s", e)
        return 2
    import uvicorn
    log.info(f"clearance-api: port={cfg.port} agents={len(access.definitions)} auditors={list(access.auditors)} "
             f"notifiers={list(access.notifiers)} delegates={list(access.delegates)} "
             f"catalog={cfg.backstage_url or 'off'}")
    uvicorn.run(app, host="0.0.0.0", port=cfg.port, log_level=os.environ.get("LOG_LEVEL", "info").lower(),
                proxy_headers=False, server_header=False)
    return 0
