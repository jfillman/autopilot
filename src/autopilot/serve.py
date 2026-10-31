"""The deployable Hangar Clearance gateway: `autopilot-gateway`.

Builds the Hangar Gateway (core Clearance, the Hangar pack, Autopilot's real backends) from environment variables
and serves Clearance's streamable-HTTP MCP app with uvicorn. See README.md, "Running the gateway", for the variables.

Only real backends are registered (human.request and human.request.get are the Gateway's own inbox, durable on
Postgres). A tool whose backend this deployment does not have (chat.*, app.api.get and xr.request today; catalog.read without BACKSTAGE_URL; artifact.* when ephemeral; argo.sync.lower without ARGO_SYNC_ENABLED;
repo.pr.open and repo.pr.open_upper without GITHUB_PR_ENABLED; glidepath.* without GLIDEPATH_API_URL; metrics.query without
PROMETHEUS_URL; logs.query without LOKI_URL; airframe.* with AIRFRAME_ENABLED=0; argo.app.get with ARGO_ENABLED=0;
airframe.validate always, since the image carries no airframe-validate) has no handler, so a call to it is refused
as having no backend. Nothing here registers a fake.

Every gateway entry point, the MCP server's calls and the expiry sweep alike, runs under one lock
(SerializedGateway.lock): the session store and the audit chain are single-writer.

Losing Postgres ends the process (crash-only). After a failed commit the in-memory sessions are ahead of the
database, and only a restart, which reloads them and re-verifies the chain, puts the two back in step; reconnecting
in place would keep spending budget that was never recorded. The kubelet restarts the container, which fails at
start (and backs off) until Postgres answers and the old session's writer lock is released. The expiry sweep pings
the database first, so a lost connection is found within one interval even when no agent is calling.
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
from clearance.auth import (JWKS, CachingAuthenticator, ForeignWorkloadAuthenticator, OIDCAuthenticator,
                            RoutingAuthenticator, WorkloadAuthenticator)
from clearance.backends import Handler
from clearance.dispatch import Authenticator, Gateway
from clearance.control import MemoryHolds
from clearance.events import MemoryEvents
from clearance.inbox import MemoryInbox
from clearance.modelproxy import ModelRoute
from clearance.session import SessionStore

from . import airframe_plan
from .airframe_api import KubeReader
from .airframe_tools import AirframeTools
from .argo_sync import ArgoSync
from .backends import Backends
from .gateway import PACKS, AgentRunBackend, hangar_handlers
from .glidepath_api import GlidepathAPI
from .kube_runs import IN_CLUSTER_API, SA_DIR, KubeAgentRuns
from .reads import BackstageCatalog, BackstageDirectory, KubeArgo, LokiAPI, PrometheusAPI, Reads
from .github_pr import BROKER_URL, GitHubPRs

log = logging.getLogger("autopilot.gateway")

ANTHROPIC_API = "https://api.anthropic.com"
REPO_AGENTS = Path(__file__).resolve().parents[2] / "agents"   # /app/agents in the image (editable install)
# The agents' GitHub App (D4) as GitHub shows it on the PRs it opens; a PR by it that Clearance has no record of
# fails agent-identity (clearance.provenance, AI002).
AGENT_LOGIN = "hangar-autopilot-agents[bot]"


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
    argo_enabled: bool
    argo_namespaces: tuple[str, ...]
    argo_sync_enabled: bool
    prometheus_url: str | None
    loki_url: str | None
    backstage_url: str | None
    backstage_token_file: Path
    github_pr_enabled: bool
    agent_token_file: Path
    broker_url: str
    model_key_file: Path | None
    model_port: int
    model_routes: dict[str, str]
    model_audience: str
    events_enabled: bool
    verifiers: tuple[str, ...] = ()
    agent_logins: tuple[str, ...] = ()
    oidc_allowed: tuple[str, ...] = ()
    # Tower (another cluster): its backend's service-account tokens, verified against that cluster's public issuer,
    # and the delegates that may answer human requests for a person they name.
    tower_cluster: str | None = None
    tower_issuer: str | None = None
    tower_jwks_url: str | None = None
    tower_service_accounts: tuple[str, ...] = ()
    delegates: tuple[str, ...] = ()
    # Platform admins (principal globs, user:<login>) who may freeze, end, hold and release beside each agent's owner
    # group (clearance control.py).
    admins: tuple[str, ...] = ()

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


def _list(v: str) -> tuple[str, ...]:
    return tuple(x.strip() for x in v.split(",") if x.strip())


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
    allowed = _list(get("CLEARANCE_OIDC_ALLOWED_USERS", ""))
    if issuer and not allowed:
        raise ConfigError("CLEARANCE_OIDC_ISSUER needs CLEARANCE_OIDC_ALLOWED_USERS: an issuer that federates GitHub "
                          "signs in anyone with a GitHub account")
    agents = Path(get("CLEARANCE_AGENTS_DIR", str(REPO_AGENTS)))
    if not agents.is_dir():
        raise ConfigError(f"CLEARANCE_AGENTS_DIR {str(agents)!r} is not a directory")
    upper = tuple(c.strip() for c in get("AIRFRAME_UPPER_CLUSTERS", "hangar-prod").split(",") if c.strip())
    argo_ns = tuple(n.strip() for n in get("ARGO_APP_NAMESPACES", "argocd").split(",") if n.strip())
    if _flag(env, "ARGO_ENABLED", True) and not argo_ns:
        raise ConfigError("ARGO_APP_NAMESPACES names no namespace (or set ARGO_ENABLED=0)")
    routes = {}
    for pair in (get("MODEL_ROUTES", "") or "").split(","):
        if pair.strip():
            alias, sep, upstream = pair.strip().partition("=")
            if not sep or not alias.strip() or not upstream.strip():
                raise ConfigError(f"MODEL_ROUTES entries are alias=upstream-model, not {pair.strip()!r}")
            routes[alias.strip()] = upstream.strip()
    model_key = get("MODEL_KEY_FILE")
    if model_key and not routes:
        raise ConfigError("MODEL_KEY_FILE is set but MODEL_ROUTES names no model")
    model_port = _number(env, "MODEL_PORT", 8081, int)
    if model_key and model_port == port:
        raise ConfigError("MODEL_PORT must differ from PORT")
    tower_issuer = (get("TOWER_ISSUER") or "").rstrip("/") or None
    tower_cluster, tower_sas = get("TOWER_CLUSTER"), _list(get("TOWER_SERVICE_ACCOUNTS", ""))
    if tower_issuer and not (tower_cluster and tower_sas):
        raise ConfigError("TOWER_ISSUER needs TOWER_CLUSTER and TOWER_SERVICE_ACCOUNTS (namespace/name globs): the "
                          "issuer signs a token for every pod of that cluster")
    events = _flag(env, "EVENTS_ENABLED", False)
    if events and not get("GLIDEPATH_API_URL"):
        raise ConfigError("EVENTS_ENABLED needs GLIDEPATH_API_URL (the events come from glidepath-api)")
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
        argo_enabled=_flag(env, "ARGO_ENABLED", True), argo_namespaces=argo_ns,
        argo_sync_enabled=_flag(env, "ARGO_SYNC_ENABLED", False),
        prometheus_url=get("PROMETHEUS_URL"), loki_url=get("LOKI_URL"), backstage_url=get("BACKSTAGE_URL"),
        backstage_token_file=Path(get("BACKSTAGE_TOKEN_FILE", "/var/run/clearance/backstage-token")),
        github_pr_enabled=_flag(env, "GITHUB_PR_ENABLED", False),
        agent_token_file=Path(get("AGENT_TOKEN_FILE", "/var/run/clearance/agent-installation-token")),
        broker_url=get("BROKER_URL", BROKER_URL),
        model_key_file=Path(model_key) if model_key else None, model_port=model_port, model_routes=routes,
        model_audience=get("MODEL_TOKEN_AUDIENCE", "model-proxy"), events_enabled=events,
        verifiers=_list(get("CLEARANCE_VERIFIERS", "")),
        agent_logins=_list(get("AGENT_LOGINS", AGENT_LOGIN)), oidc_allowed=allowed,
        tower_cluster=tower_cluster if tower_issuer else None, tower_issuer=tower_issuer,
        tower_jwks_url=get("TOWER_JWKS_URL", f"{tower_issuer}/jwks.json" if tower_issuer else None),
        tower_service_accounts=tower_sas if tower_issuer else (), delegates=_list(get("CLEARANCE_DELEGATES", "")),
        admins=_list(get("CLEARANCE_ADMINS", "")),
    )


# -- backends -------------------------------------------------------------------------------------------------------

def build_backends(cfg: Config) -> Backends:
    """Real backends only, in-cluster: glidepath-api, Prometheus and Loki if configured, Airframe and Argo CD reads
    unless disabled (and Argo CD syncs of lower Applications with ARGO_SYNC_ENABLED), AgentRuns always. human (chat.*) is None:
    this deployment has no backend for it; human.request is the Gateway's inbox (build_gateway). artifacts is set by build_gateway, on the durable store's connection."""
    glidepath = (GlidepathAPI(cfg.glidepath_url, cfg.glidepath_token_file, on_behalf_of=cfg.glidepath_on_behalf_of)
                 if cfg.glidepath_url else None)
    airframe = argo = None
    reader = KubeReader(IN_CLUSTER_API, SA_DIR / "token", SA_DIR / "ca.crt")
    if cfg.argo_enabled:
        syncer = ArgoSync(reader, cfg.argo_namespaces) if cfg.argo_sync_enabled else None
        argo = KubeArgo(reader, cfg.argo_namespaces, syncer=syncer)
    if cfg.airframe_enabled:
        registry = airframe_plan.Registry(cfg.airframe_cluster, cfg.airframe_upper_clusters, cfg.airframe_tenants_repo)
        airframe = AirframeTools(reader, None, registry, cluster=cfg.airframe_cluster)
    read = None
    if cfg.prometheus_url or cfg.loki_url or cfg.backstage_url:
        read = Reads(PrometheusAPI(cfg.prometheus_url) if cfg.prometheus_url else None,
                     LokiAPI(cfg.loki_url) if cfg.loki_url else None,
                     BackstageCatalog(cfg.backstage_url, cfg.backstage_token_file) if cfg.backstage_url else None)
    git = GitHubPRs(cfg.agent_token_file, broker_url=cfg.broker_url) if cfg.github_pr_enabled else None
    return Backends(git, argo, KubeAgentRuns(), read, None, None, glidepath, airframe)


def build_handlers(b: Backends) -> dict[str, Handler]:
    """hangar_handlers, less every tool whose backend is absent (so it is "no backend", not "backend error")."""
    h = hangar_handlers(b)
    # No adapter yet. (argo.sync.lower and repo.pr.open_upper have a handler only when hangar_handlers wired one.)
    drop = {"app.api.get", "xr.request"}
    if getattr(b.argo, "syncer", None) is None:
        drop.add("argo.sync.lower")   # core_handlers registers it for any Argo backend; only ArgoSync can sync
    if b.read is not None:
        if getattr(b.read, "prometheus", None) is None:
            drop.add("metrics.query")
        if getattr(b.read, "loki", None) is None:
            drop.add("logs.query")
    if getattr(b.read, "backstage", None) is None:
        drop.add("catalog.read")
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
                                                  user_claim=cfg.oidc_user_claim, allowed=cfg.oidc_allowed)
    if cfg.tower_issuer:
        oidc[cfg.tower_issuer] = ForeignWorkloadAuthenticator(cfg.tower_cluster, cfg.tower_issuer, cfg.audience,
                                                              JWKS(cfg.tower_jwks_url),
                                                              allowed=cfg.tower_service_accounts)
    return CachingAuthenticator(RoutingAuthenticator(oidc, WorkloadAuthenticator(cfg.audience)))


STORE_LOST_EXIT = 70


def _exit_store_lost(e: BaseException) -> None:
    log.critical("the session store's database connection failed (%s); exiting so a restart reloads from it",
                 type(e).__name__)
    logging.shutdown()
    os._exit(STORE_LOST_EXIT)   # not sys.exit: uvicorn would catch it and keep serving


class SerializedGateway(Gateway):
    """A Gateway whose every public operation holds one lock, so the expiry sweep and MCP calls never interleave.
    (Clearance's server also serializes its own calls; that lock is always taken first, so the order is fixed.)

    `state` is the PgState (None when ephemeral); an error in `store_errors` raised by any operation means the
    database connection is gone, and `on_store_lost` ends the process (see the module docstring)."""

    def __init__(self, *a, state: Any = None, store_errors: tuple[type[BaseException], ...] = (),
                 on_store_lost: Callable[[BaseException], None] = _exit_store_lost, **k):
        super().__init__(*a, **k)
        self.lock = threading.RLock()
        self.state, self.store_errors, self.on_store_lost = state, store_errors, on_store_lost

    def _locked(self, op: Callable[..., Any], *a, **k) -> Any:
        with self.lock:
            try:
                return op(*a, **k)
            except self.store_errors as e:
                self.on_store_lost(e)
                raise

    def open_session(self, *a, **k):
        return self._locked(super().open_session, *a, **k)

    def open_triggered(self, *a, **k):
        return self._locked(super().open_triggered, *a, **k)

    def launch(self, *a, **k):
        return self._locked(super().launch, *a, **k)

    def call(self, *a, **k):
        return self._locked(super().call, *a, **k)

    def expire(self, *a, **k):
        return self._locked(super().expire, *a, **k)

    def model_authorize(self, *a, **k):
        return self._locked(super().model_authorize, *a, **k)

    def provenance(self, *a, **k):
        return self._locked(super().provenance, *a, **k)

    def model_usage(self, *a, **k):
        return self._locked(super().model_usage, *a, **k)

    def ping(self) -> None:
        """One round trip to the database, under the lock (a no-op when ephemeral)."""
        if self.state is None:
            return

        def roundtrip():
            with self.state.conn.cursor() as cur:
                cur.execute("SELECT 1")
            self.state.commit()
        self._locked(roundtrip)


def build_gateway(cfg: Config, backends: Backends | None = None, auth: Authenticator | None = None,
                  clock: Callable[[], datetime] = lambda: datetime.now(timezone.utc)) -> tuple[SerializedGateway, Any]:
    """The gateway and, for a durable store, its PgState (to close on exit; None when ephemeral)."""
    b = backends or build_backends(cfg)
    defs = profile.load_dir(cfg.agents_dir)
    state, store_errors = None, ()
    if cfg.ephemeral:
        store, audit = SessionStore(), AuditLog()
        inbox = MemoryInbox()
        events = MemoryEvents()
        holds = MemoryHolds()
    else:
        import psycopg   # only where it is used
        from clearance.pg import open_durable
        from clearance.pg import PgArtifacts, PgEvents, PgHolds, PgInbox
        state, audit, store = open_durable(cfg.dsn)
        store_errors = (psycopg.OperationalError, psycopg.InterfaceError)
        if b.artifacts is None:
            b.artifacts = PgArtifacts(state)   # on the store's connection: a put commits with its call's evidence
        inbox = PgInbox(state)                 # human.request's questions and answers, in the same transactions
        events = PgEvents(state)               # the trigger bridge's event history (clearance-api /v1/events)
        holds = PgHolds(state)                 # people's holds on agents' triggers (control.py)
    models = model_auth = None
    if cfg.model_key_file:
        models = {a: ModelRoute(a, "anthropic", ANTHROPIC_API, u) for a, u in cfg.model_routes.items()}
        model_auth = CachingAuthenticator(WorkloadAuthenticator(cfg.model_audience))
    gw = SerializedGateway(defs, store, audit, auth or build_auth(cfg), clock, packs=PACKS,
                           handlers=build_handlers(b), runs=AgentRunBackend(b.runs), state=state,
                           store_errors=store_errors, models=models, model_auth=model_auth,
                           verifiers=cfg.verifiers, agent_logins=cfg.agent_logins, inbox=inbox,
                           directory=BackstageDirectory(b.read.backstage) if getattr(b.read, "backstage", None) else None,
                           delegates=cfg.delegates, events=events, holds=holds, admins=cfg.admins)
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
        if hasattr(self.gw, "ping"):
            self.gw.ping()
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
            + f" argo={'on (' + ','.join(cfg.argo_namespaces) + ')' if cfg.argo_enabled else 'off'}"
            + f" prometheus={cfg.prometheus_url or 'off'} loki={cfg.loki_url or 'off'} catalog={cfg.backstage_url or 'off'}"
            + f" github-prs={'on (broker ' + cfg.broker_url + ')' if cfg.github_pr_enabled else 'off'}"
            + (f" model-proxy=:{cfg.model_port} routes={','.join(sorted(cfg.model_routes))}" if cfg.model_key_file
               else " model-proxy=off")
            + f" events={'on' if cfg.events_enabled else 'off'}"
            + f" agentruns=in-cluster expire-every={cfg.expire_seconds:g}s")


def main() -> int:
    import sys
    if sys.argv[1:2] == ["api"]:   # `autopilot-gateway api`: clearance-api, the read-only process (api_serve.py)
        from .api_serve import main as api_main
        return api_main()
    logging.basicConfig(level=os.environ.get("LOG_LEVEL", "INFO").upper(),
                        format="%(asctime)s %(levelname)s %(name)s %(message)s")
    try:
        cfg = config_from_env()
    except ConfigError as e:
        log.error("configuration: %s", e)
        return 2
    import asyncio

    import uvicorn
    gw, state = build_gateway(cfg)
    app = build_app(cfg, gw)
    log.info(describe(cfg, gw))
    expirer = Expirer(gw, cfg.expire_seconds).start()
    bridge = None
    if cfg.events_enabled:
        from .event_bridge import EventBridge
        bridge = EventBridge(gw, cfg.glidepath_url, cfg.glidepath_token_file).start()
    level = os.environ.get("LOG_LEVEL", "info").lower()
    servers = [uvicorn.Server(uvicorn.Config(app, host="0.0.0.0", port=cfg.port, log_level=level,
                                             proxy_headers=False, server_header=False))]
    if cfg.model_key_file:
        from clearance.server import create_model_app
        servers.append(uvicorn.Server(uvicorn.Config(create_model_app(gw, key_file=str(cfg.model_key_file)),
                                                     host="0.0.0.0", port=cfg.model_port, log_level=level,
                                                     proxy_headers=False, server_header=False)))

    async def serve_all():
        await asyncio.gather(*(s.serve() for s in servers))
    try:
        asyncio.run(serve_all())
    finally:
        if bridge is not None:
            bridge.stop()
        expirer.stop()
        if state is not None:
            state.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
