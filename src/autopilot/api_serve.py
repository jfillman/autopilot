"""`autopilot-gateway api`: the gateway's own read API (read_api.py), with Hangar's identity and catalog.

The read API runs from the gateway's image as a separate Deployment: same agent definitions, same callers' auth
(service-account tokens by TokenReview, people's OIDC, Tower's foreign workload), but its own read-only DSN and no
writer lock, no tools, no runs. On Hangar, a person's teams and apps come from Tower's catalog (BackstageDirectory,
BackstageOwnership), as the gateway's human inbox reads them. It serves the runtime's reads only; Clearance's reads
(evidence, receipts, provenance, clearances, policy) are clearance-api's (the Go `clearance api`), named in
/v1/contract as CLEARANCE_API_URL.

Environment (README "Running the read API"):
  GATEWAY_API_DSN           the read-only role's DSN on the gateway's own database (required); it reads gateway_* only
  GATEWAY_API_TABLES        retired at R-4: `gateway` (what the Deployment still sets) is accepted and ignored for one
                            release, anything else is refused; apron drops it before the next
  GATEWAY_API_POOL (5), GATEWAY_API_AUDITORS, GATEWAY_API_NOTIFIERS, CLEARANCE_DELEGATES   principal globs, comma-separated
  CLEARANCE_AGENTS_DIR      the bundled agent definitions (default: the image's agents/); the ones Agent XRs pin come
                            from the gateway's store (gateway_definitions), refreshed every GATEWAY_API_DEFINITIONS_INTERVAL s (30)
  CLEARANCE_GATEWAY_URL     where writes go, named in /v1/contract as `gateway`
  CLEARANCE_API_URL         where Clearance's reads are, named in /v1/contract as `clearance`
  PORT (8080), and the gateway's auth and catalog variables (CLEARANCE_AUDIENCE, CLEARANCE_OIDC_*, TOWER_*,
  BACKSTAGE_URL, BACKSTAGE_TOKEN_FILE)
"""
from __future__ import annotations

import logging
import os
from typing import Mapping

from .engine import profile
from .engine.definitions import Definitions

from . import read_api, serve
from .agent_xrs import DefinitionsRefresher
from .reads import BackstageCatalog, BackstageDirectory, BackstageOwnership

log = logging.getLogger("autopilot.api")


def build(env: Mapping[str, str] | None = None):
    env = os.environ if env is None else env

    def get(k: str, d: str | None = None) -> str | None:
        return env.get(k) or d

    dsn = get("GATEWAY_API_DSN")
    if not dsn:
        raise serve.ConfigError("GATEWAY_API_DSN is required (the read-only role's DSN)")
    # Retired at R-4: the read API reads only the gateway's own tables. `gateway` is accepted (the deployed value) until
    # apron drops the variable; `clearance` (clearance v0.26.1's tables on clearance-pg) is no longer served.
    if get("GATEWAY_API_TABLES", "gateway") != "gateway":
        raise serve.ConfigError(f"GATEWAY_API_TABLES is retired (the read API reads only gateway_*): unset it, "
                                f"not {env['GATEWAY_API_TABLES']!r}")
    try:
        pool_size = int(get("GATEWAY_API_POOL", "5"))
        interval = float(get("GATEWAY_API_DEFINITIONS_INTERVAL", "30"))
    except ValueError:
        raise serve.ConfigError("GATEWAY_API_POOL and GATEWAY_API_DEFINITIONS_INTERVAL are numbers") from None
    if not 1 <= pool_size <= 50:
        raise serve.ConfigError("GATEWAY_API_POOL is 1 to 50")
    # The gateway's config parser for auth, catalog and agents; the store it would open is never used here.
    # The core URL is required by the gateway's parser but never called here.
    cfg = serve.config_from_env({**env, "CLEARANCE_EPHEMERAL": "1",
                                 "CLEARANCE_CORE_URL": env.get("CLEARANCE_CORE_URL") or "http://clearance-core.invalid"})
    defs = Definitions(profile.load_dir(cfg.agents_dir))
    directory = ownership = None
    if cfg.backstage_url:
        catalog = BackstageCatalog(cfg.backstage_url, cfg.backstage_token_file)
        directory = BackstageDirectory(catalog)
        ownership = BackstageOwnership(catalog, directory)
    access = read_api.Access(serve.build_auth(cfg), defs, delegates=cfg.delegates,
                             notifiers=serve._list(get("GATEWAY_API_NOTIFIERS", "")),
                             auditors=serve._list(get("GATEWAY_API_AUDITORS", "")), directory=directory, ownership=ownership)
    store = read_api.PgReadStore(read_api.Pool(dsn, size=pool_size))
    # The cluster-pinned definitions the gateway loaded, from its store: this process fetches and decides nothing.
    DefinitionsRefresher(defs, store, interval=interval).start()
    app = read_api.create_api_app(store, access, gateway_url=get("CLEARANCE_GATEWAY_URL"),
                                  clearance_api_url=get("CLEARANCE_API_URL"), runtimes=serve.load_runtimes(),
                                  custom_images=cfg.runtime_images_extra)
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
    log.info(f"autopilot-api: port={cfg.port} agents={len(access.definitions)} auditors={list(access.auditors)} "
             f"notifiers={list(access.notifiers)} delegates={list(access.delegates)} "
             f"catalog={cfg.backstage_url or 'off'}")
    uvicorn.run(app, host="0.0.0.0", port=cfg.port, log_level=os.environ.get("LOG_LEVEL", "info").lower(),
                proxy_headers=False, server_header=False)
    return 0
