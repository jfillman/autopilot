"""The deployable Hangar Clearance gateway: `autopilot-gateway`.

Builds the Hangar Gateway (core Clearance, the Hangar pack, Autopilot's real backends) from environment variables
and serves Clearance's streamable-HTTP MCP app with uvicorn. See README.md, "Running the gateway", for the variables.

Only real backends are registered. A tool whose backend this deployment does not have (git, argo, read, human,
artifacts today; glidepath.* without GLIDEPATH_API_URL; airframe.* with AIRFRAME_ENABLED=0; airframe.validate
always, since the image carries no airframe-validate) has no handler, so a call to it is refused as having no
backend. Nothing here registers a fake.

Every gateway entry point, the MCP server's calls and the expiry sweep alike, runs under one lock
(SerializedGateway.lock): the session store and the audit chain are single-writer.
"""
from __future__ import annotations

import logging
import os
import threading
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Mapping

from clearance import profile
from clearance.audit import AuditLog
from clearance.auth import JWKS, CachingAuthenticator, OIDCAuthenticator, RoutingAuthenticator, WorkloadAuthenticator
from clearance.backends import Handler
from clearance.dispatch import Authenticator, Gateway
from clearance.session import SessionStore

from . import airframe_plan
from .airframe_api import KubeReader
from .airframe_tools import AirframeTools
from .backends import Backends
from .gateway import PACKS, AgentRunBackend, hangar_handlers
from .glidepath_api import GlidepathAPI
from .kube_runs import IN_CLUSTER_API, SA_DIR, KubeAgentRuns

log = logging.getLogger("autopilot.gateway")

REPO_AGENTS = Path(__file__).resolve().parents[2] / "agents"   # /app/agents in the image (editable install)


class ConfigError(ValueError):
    """The environment does not describe a gateway that can start."""


@dataclass(frozen=True)
class Config:
    agents_dir: Path
    dsn: str | None
    ephemeral: bool
    resource_url: str
    oidc_issuer: str | None
    oidc_audience: str | None
    oidc_jwks_url: str | None
    oidc_user_claim: str
    audience: str
    port: int
    expire_seconds: float
    glidepath_url: str | None
    glidepath_token_file: Path
    glidepath_on_behalf_of: bool
    airframe_enabled: bool
    airframe_cluster: str
    airframe_upper_clusters: tuple[str, ...]
    airframe_tenants_repo: str

    @property
    def issuer_url(self) -> str:
        """The authorization server named in the protected-resource metadata: the OIDC issuer, else this server."""
        return self.oidc_issuer or self.resource_url


def _flag(env: Mapping[str, str], name: str, default: bool) -> bool:
    v = env.get(name)
    if v is None or v == "":
        return default
    if v.lower() in ("1", "true", "yes", "on"):
        return True
    if v.lower() in ("0", "false", "no", "off"):
        return False
    raise ConfigError(f"{name} must be 0 or 1, not {v!r}")


def _number(env: Mapping[str, str], name: str, default: float, kind: Callable[[str], Any]) -> Any:
    v = env.get(name) or None
    try:
        n = kind(v) if v is not None else default
    except ValueError as e:
        raise ConfigError(f"{name} must be a number, not {v!r}") from e
    if n <= 0:
        raise ConfigError(f"{name} must be positive")
    return n


def config_from_env(env: Mapping[str, str] | None = None) -> Config:
    env = os.environ if env is None else env
    get = lambda k, d=None: env.get(k) or d  # noqa: E731 - an empty variable is an unset one
    ephemeral = _flag(env, "CLEARANCE_EPHEMERAL", False)
    dsn = get("CLEARANCE_DSN")
    if not ephemeral and not dsn:
        raise ConfigError("CLEARANCE_DSN is required (or CLEARANCE_EPHEMERAL=1 for an in-memory store, local runs only)")
    port = _number(env, "PORT", 8080, int)
    if port > 65535:
        raise ConfigError("PORT must be at most 65535")
    issuer, audience = get("CLEARANCE_OIDC_ISSUER"), get("CLEARANCE_OIDC_AUDIENCE")
    if bool(issuer) != bool(audience):
        raise ConfigError("CLEARANCE_OIDC_ISSUER and CLEARANCE_OIDC_AUDIENCE are set together or not at all")
    issuer = issuer.rstrip("/") if issuer else None
    agents = Path(get("CLEARANCE_AGENTS_DIR", str(REPO_AGENTS)))
    if not agents.is_dir():
        raise ConfigError(f"CLEARANCE_AGENTS_DIR {str(agents)!r} is not a directory")
    upper = tuple(c.strip() for c in get("AIRFRAME_UPPER_CLUSTERS", "hangar-prod").split(",") if c.strip())
    return Config(
        agents_dir=agents, dsn=None if ephemeral else dsn, ephemeral=ephemeral,
        resource_url=get("CLEARANCE_RESOURCE_URL", f"http://localhost:{port}/mcp"),
        oidc_issuer=issuer, oidc_audience=audience,
        oidc_jwks_url=get("CLEARANCE_OIDC_JWKS_URL", f"{issuer}/keys" if issuer else None),
        oidc_user_claim=get("CLEARANCE_OIDC_USER_CLAIM", "email"),
        audience=get("CLEARANCE_AUDIENCE", "clearance"), port=port,
        expire_seconds=_number(env, "CLEARANCE_EXPIRE_SECONDS", 60.0, float),
        glidepath_url=get("GLIDEPATH_API_URL"),
        glidepath_token_file=Path(get("GLIDEPATH_TOKEN_FILE", str(SA_DIR / "token"))),
        glidepath_on_behalf_of=_flag(env, "GLIDEPATH_ON_BEHALF_OF", False),
        airframe_enabled=_flag(env, "AIRFRAME_ENABLED", True),
        airframe_cluster=get("AIRFRAME_CLUSTER", "hangar-dev"),
        airframe_upper_clusters=upper,
        airframe_tenants_repo=get("AIRFRAME_TENANTS_REPO", "gitops-hangar-dev-tenants"),
    )


# -- backends -------------------------------------------------------------------------------------------------------

def build_backends(cfg: Config) -> Backends:
    """Real backends only, in-cluster: glidepath-api if configured, Airframe reads unless disabled, AgentRuns always.
    git, argo, read, human and artifacts are None: this deployment has no backend for them."""
    glidepath = (GlidepathAPI(cfg.glidepath_url, cfg.glidepath_token_file, on_behalf_of=cfg.glidepath_on_behalf_of)
                 if cfg.glidepath_url else None)
    airframe = None
    if cfg.airframe_enabled:
        reader = KubeReader(IN_CLUSTER_API, SA_DIR / "token", SA_DIR / "ca.crt")
        registry = airframe_plan.Registry(cfg.airframe_cluster, cfg.airframe_upper_clusters, cfg.airframe_tenants_repo)
        airframe = AirframeTools(reader, None, registry, cluster=cfg.airframe_cluster)
    return Backends(None, None, KubeAgentRuns(), None, None, None, glidepath, airframe)


def build_handlers(b: Backends) -> dict[str, Handler]:
    """hangar_handlers, less every tool whose backend is absent (so it is "no backend", not "backend error")."""
    h = hangar_handlers(b)
    drop = set()
    if b.glidepath is None:
        drop |= {t for t in h if t.startswith("glidepath.")}
    if b.airframe is None:
        drop |= {t for t in h if t.startswith("airframe.")}
    elif getattr(b.airframe, "validator", None) is None:
        drop.add("airframe.validate")
    return {t: f for t, f in h.items() if t not in drop}


def build_auth(cfg: Config) -> Authenticator:
    oidc: dict[str, Authenticator] = {}
    if cfg.oidc_issuer:
        oidc[cfg.oidc_issuer] = OIDCAuthenticator(cfg.oidc_issuer, cfg.oidc_audience, JWKS(cfg.oidc_jwks_url),
                                                  user_claim=cfg.oidc_user_claim)
    return CachingAuthenticator(RoutingAuthenticator(oidc, WorkloadAuthenticator(cfg.audience)))


class SerializedGateway(Gateway):
    """A Gateway whose every public operation holds one lock, so the expiry sweep and MCP calls never interleave.
    (Clearance's server also serializes its own calls; that lock is always taken first, so the order is fixed.)"""

    def __init__(self, *a, **k):
        super().__init__(*a, **k)
        self.lock = threading.RLock()

    def open_session(self, *a, **k):
        with self.lock:
            return super().open_session(*a, **k)

    def open_triggered(self, *a, **k):
        with self.lock:
            return super().open_triggered(*a, **k)

    def launch(self, *a, **k):
        with self.lock:
            return super().launch(*a, **k)

    def call(self, *a, **k):
        with self.lock:
            return super().call(*a, **k)

    def expire(self, *a, **k):
        with self.lock:
            return super().expire(*a, **k)


def build_gateway(cfg: Config, backends: Backends | None = None, auth: Authenticator | None = None,
                  clock: Callable[[], datetime] = lambda: datetime.now(timezone.utc)) -> tuple[SerializedGateway, Any]:
    """The gateway and, for a durable store, its PgState (to close on exit; None when ephemeral)."""
    b = backends or build_backends(cfg)
    defs = profile.load_dir(cfg.agents_dir)
    state = None
    if cfg.ephemeral:
        store, audit = SessionStore(), AuditLog()
    else:
        from clearance.pg import open_durable   # psycopg only where it is used
        state, audit, store = open_durable(cfg.dsn)
    gw = SerializedGateway(defs, store, audit, auth or build_auth(cfg), clock, packs=PACKS,
                           handlers=build_handlers(b), runs=AgentRunBackend(b.runs))
    return gw, state


def build_app(cfg: Config, gw: Gateway):
    from clearance.server import create_http_app
    return create_http_app(gw, resource_url=cfg.resource_url, issuer_url=cfg.issuer_url)


class Expirer:
    """Calls gateway.expire() every `interval` seconds on a daemon thread. A failed sweep is logged and the next one
    retries (the gateway keeps a run whose deletion failed)."""

    def __init__(self, gw: Gateway, interval: float):
        self.gw, self.interval = gw, interval
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._loop, name="clearance-expire", daemon=True)

    def start(self) -> "Expirer":
        self._thread.start()
        return self

    def stop(self) -> None:
        self._stop.set()
        self._thread.join(timeout=self.interval + 5)

    def sweep(self) -> list[str]:
        done = self.gw.expire()
        if done:
            log.info("expired %d session(s) and deleted their runs", len(done))
        return done

    def _loop(self) -> None:
        while not self._stop.wait(self.interval):
            try:
                self.sweep()
            except Exception as e:  # noqa: BLE001 - one bad sweep must not end the thread
                log.error("expiry sweep failed: %s", type(e).__name__)


def describe(cfg: Config, gw: Gateway) -> str:
    """The start line: what is enabled. No DSN, no token, no file contents."""
    backed = sorted(gw._handlers)
    return (f"autopilot-gateway: port={cfg.port} store={'memory (ephemeral)' if cfg.ephemeral else 'postgres'} "
            f"agents={len(gw.defs)} from {cfg.agents_dir} tools={len(gw.tools)} backed={len(backed)} "
            f"auth=workload(aud={cfg.audience})" + (f"+oidc({cfg.oidc_issuer})" if cfg.oidc_issuer else "")
            + f" resource={cfg.resource_url} glidepath={cfg.glidepath_url or 'off'}"
            + (f" on-behalf-of={'on' if cfg.glidepath_on_behalf_of else 'off'}" if cfg.glidepath_url else "")
            + f" airframe={'on (cluster ' + cfg.airframe_cluster + ', no validator)' if cfg.airframe_enabled else 'off'}"
            + f" agentruns=in-cluster expire-every={cfg.expire_seconds:g}s")


def main() -> int:
    logging.basicConfig(level=os.environ.get("LOG_LEVEL", "INFO").upper(),
                        format="%(asctime)s %(levelname)s %(name)s %(message)s")
    try:
        cfg = config_from_env()
    except ConfigError as e:
        log.error("configuration: %s", e)
        return 2
    import uvicorn
    gw, state = build_gateway(cfg)
    app = build_app(cfg, gw)
    log.info(describe(cfg, gw))
    expirer = Expirer(gw, cfg.expire_seconds).start()
    try:
        uvicorn.run(app, host="0.0.0.0", port=cfg.port, log_level=os.environ.get("LOG_LEVEL", "info").lower(),
                    proxy_headers=False, server_header=False)
    finally:
        expirer.stop()
        if state is not None:
            state.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
