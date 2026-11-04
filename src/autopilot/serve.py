"""The deployable Hangar gateway: `autopilot-gateway`.

Builds the Hangar Gateway (the gateway engine, Hangar's tools, Autopilot's real backends, and the Clearance core's
client) from environment variables and serves the engine's streamable-HTTP MCP app (autopilot.engine.server) with uvicorn. See
README.md, "Running the gateway", for the variables.

Only real backends are registered (human.request and human.request.get are the Gateway's own inbox, durable on
Postgres). A tool whose backend this deployment does not have (chat.* always; app.api.get without APP_API_SERVICES;
xr.request without GITHUB_PR_ENABLED or with XR_REQUEST_ENABLED=0; catalog.read without BACKSTAGE_URL; artifact.* when ephemeral; argo.sync.lower without ARGO_SYNC_ENABLED;
repo.pr.open and repo.pr.open_upper without GITHUB_PR_ENABLED; glidepath.* without GLIDEPATH_API_URL; metrics.query without
PROMETHEUS_URL; logs.query without LOKI_URL; airframe.* with AIRFRAME_ENABLED=0; argo.app.get with ARGO_ENABLED=0;
airframe.validate always, since the image carries no airframe-validate) has no handler, so a call to it is refused
as having no backend. Nothing here registers a fake.

Every gateway entry point, the MCP server's calls and the expiry sweep alike, runs under one lock
(SerializedGateway.lock): the session store is single-writer.

The Clearance core (CLEARANCE_CORE_URL) decides every call and writes every record; the gateway holds no evidence
DSN. Its own database (GATEWAY_DSN) keeps sessions, the inbox, results, artifacts, trigger history, holds, the pinned
definition set and the outbox (engine/pg.py). When the core is unreachable the outage policy applies (engine/core.py;
GATEWAY_OUTAGE_ALLOW_READS), and the expiry sweep posts what the outbox holds once it answers again.

Losing Postgres ends the process (crash-only). After a failed commit the in-memory sessions are ahead of the
database, and only a restart, which reloads them, puts the two back in step; reconnecting in place would keep
spending budget that was never saved. The kubelet restarts the container, which fails at
start (and backs off) until Postgres answers and the old session's writer lock is released. The expiry sweep pings
the database first, so a lost connection is found within one interval even when no agent is calling.
"""
from __future__ import annotations

import json
import logging
import os
import threading

import yaml
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Mapping

from .engine import profile
from .engine.auth import (JWKS, CachingAuthenticator, ForeignWorkloadAuthenticator, OIDCAuthenticator,
                          RoutingAuthenticator, WorkloadAuthenticator)
from .engine.core import DEFINITIONS_TIMEOUT, PROVIDER, Core, client_from_env
from .engine.definitions import Definitions, MemoryDefinitions
from .engine.outbox import MemoryOutbox
from .engine.backends import Handler
from .engine.dispatch import Authenticator, Gateway
from .engine.control import MemoryHolds
from .engine.events import MemoryEvents
from .engine.trigger_ledger import MemoryTriggers
from .engine.inbox import MemoryInbox
from .engine.modelproxy import ModelRoute
from .engine.session import SessionStore

from . import airframe_plan
from .airframe_api import KubeReader
from .airframe_tools import AirframeTools
from .argo_sync import ArgoSync
from .backends import Backends
from .gateway import PACKS, AgentRunBackend, hangar_handlers
from .glidepath_api import GlidepathAPI
from .kube_runs import IN_CLUSTER_API, SA_DIR, KubeAgentRuns
from .engine.results import MemoryResults
from .reads import AppAPIs, BackstageCatalog, BackstageDirectory, KubeArgo, LokiAPI, PrometheusAPI, Reads
from .github_pr import BROKER_URL, GitHubPRs
from .version import gateway_version

log = logging.getLogger("autopilot.gateway")

ANTHROPIC_API = "https://api.anthropic.com"
REPO_AGENTS = Path(__file__).resolve().parents[2] / "agents"   # /app/agents in the image (editable install)
# The runtime catalog (runtime/catalog.yaml): the agent images Hangar offers, handed to Clearance (runtimes.py).
REPO_RUNTIMES = REPO_AGENTS.parent / "runtime" / "catalog.yaml"


def load_runtimes(path: Path | None = None) -> tuple:
    """The catalog's entries, parsed by Clearance (a malformed catalog is a ConfigError: fail at start, not later)."""
    from .engine import runtimes as rt
    p = path or REPO_RUNTIMES
    if not p.is_file():
        return ()
    try:
        return rt.load((yaml.safe_load(p.read_text()) or {}).get("runtimes") or [])
    except ValueError as e:
        raise ConfigError(f"runtime catalog {p}: {e}") from e

# The core's token: a projected service-account token whose audience is the core's CLEARANCE_AUDIENCE.
CORE_TOKEN_FILE = "/var/run/secrets/tokens/clearance-core"

# What the gateway no longer does (R-3b): the evidence DSN, the deployment gate's verifiers, provenance, the GitHub
# App and GitHub Actions callers are the Clearance core's. A gateway configured with one fails at start.
CORE_ONLY = ("CLEARANCE_DSN", "CLEARANCE_VERIFIERS", "AGENT_LOGINS", "CLEARANCE_GITHUB_APP_ID",
             "CLEARANCE_GITHUB_APP_KEY_FILE", "CLEARANCE_GITHUB_APP_WEBHOOK_SECRET_FILE", "CLEARANCE_GITHUB_APP_COMMENTS",
             "CLEARANCE_GITHUB_OIDC_AUDIENCE", "CLEARANCE_GITHUB_OIDC_ALLOWED")


class ConfigError(ValueError):
    """The environment does not describe a gateway that can start."""


@dataclass(frozen=True)
class Config:
    agents_dir: Path
    dsn: str | None             # GATEWAY_DSN: the gateway's own database (None when ephemeral)
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
    agent_xrs_enabled: bool
    agent_xrs_interval: float
    runtime_images_extra: tuple[str, ...] = ()
    schedules_enabled: bool = False
    # The Clearance core: its URL, the gateway's token for it, and the outage switch (engine/core.py).
    core_url: str = ""
    core_token_file: Path = Path(CORE_TOKEN_FILE)
    outage_allow_reads: bool = False
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
    # xr.request (xr_request.py): a PR on the tenants repo through the agents' App; on with GitHub PRs unless said.
    xr_request_enabled: bool = False
    # app.api.get (reads.AppAPIs): {service: base URL}; empty means the tool has no backend.
    app_api_services: Mapping[str, str] = field(default_factory=dict)
    # A-5 trigger durability: the reconciliation sweep (reconcile.py; 0 = off) and its window, and the Prometheus
    # metrics port (metrics.py; 0 = off).
    reconcile_seconds: float = 300.0
    reconcile_window_hours: int = 24
    metrics_port: int = 9090

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


def _off_or_number(env: Mapping[str, str], name: str, default: float, kind: Callable[[str], Any]) -> Any:
    """A positive number, or 0 for off."""
    if (env.get(name) or "").strip() == "0":
        return kind("0")
    return _number(env, name, default, kind)


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


def _app_api_services(raw: str) -> dict[str, str]:
    """APP_API_SERVICES: a JSON object {service: base URL} (http or https, no query, no credentials in it)."""
    if not raw.strip():
        return {}
    try:
        v = json.loads(raw)
    except ValueError as e:
        raise ConfigError("APP_API_SERVICES is a JSON object {service: base URL}") from e
    if not isinstance(v, dict):
        raise ConfigError("APP_API_SERVICES is a JSON object {service: base URL}")
    from urllib.parse import urlsplit
    from .reads import _API_SERVICE
    for k, u in v.items():
        parts = urlsplit(u) if isinstance(u, str) else None
        if (not isinstance(k, str) or not _API_SERVICE.match(k) or parts is None or parts.scheme not in ("http", "https")
                or not parts.hostname or parts.username or parts.password or parts.query or parts.fragment):
            raise ConfigError(f"APP_API_SERVICES[{k!r}]: the key is a service name (a DNS label) and the value an http(s) "
                              "base URL without credentials or query")
    return dict(v)


def config_from_env(env: Mapping[str, str] | None = None) -> Config:
    env = os.environ if env is None else env
    get = lambda k, d=None: env.get(k) or d  # noqa: E731 - an empty variable is an unset one
    stale = [k for k in CORE_ONLY if get(k)]
    if stale:
        raise ConfigError(f"{', '.join(stale)}: the Clearance core's settings, not the gateway's (the gateway has no "
                          "evidence DSN and serves no gate, provenance or GitHub App); set them on the core")
    ephemeral = _flag(env, "CLEARANCE_EPHEMERAL", False)
    dsn = get("GATEWAY_DSN")
    if not ephemeral and not dsn:
        raise ConfigError("GATEWAY_DSN is required (or CLEARANCE_EPHEMERAL=1 for an in-memory store, local runs only)")
    core_url = get("CLEARANCE_CORE_URL")
    if not core_url:
        raise ConfigError("CLEARANCE_CORE_URL is required: the Clearance core decides every call")
    if not core_url.startswith(("http://", "https://")):
        raise ConfigError("CLEARANCE_CORE_URL is an http(s) URL")
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
    if _flag(env, "EVENTS_RABBITMQ_ENABLED", False):
        # A-5 builds the flag and the design (docs/admin/trigger-durability.md, "A RabbitMQ source"), not the reader:
        # refusing to start is how nobody believes the Skyport agents' RabbitMQ triggers are live when they are not.
        raise ConfigError("EVENTS_RABBITMQ_ENABLED: the RabbitMQ event source is designed but not built in this "
                          "gateway (docs/admin/trigger-durability.md); unset it")
    reconcile_seconds = _off_or_number(env, "RECONCILE_SECONDS", 300.0, float)
    if reconcile_seconds and reconcile_seconds < 30:
        raise ConfigError("RECONCILE_SECONDS is at least 30 (or 0 to turn the sweep off)")
    window = _number(env, "RECONCILE_WINDOW_HOURS", 24, int)
    if window > 744:
        raise ConfigError("RECONCILE_WINDOW_HOURS is at most 744 (glidepath-api's sinceHours limit)")
    metrics_port = int(_off_or_number(env, "METRICS_PORT", 9090, int))
    if metrics_port > 65535:
        raise ConfigError("METRICS_PORT must be at most 65535")
    if metrics_port and (metrics_port == port or (model_key and metrics_port == model_port)):
        raise ConfigError("METRICS_PORT must differ from PORT and MODEL_PORT")
    github_prs = _flag(env, "GITHUB_PR_ENABLED", False)
    # Agent XR pins (agent_xrs.py) read definition files through the same broker token repo.pr.open uses, so they are
    # on exactly when GitHub PRs are, unless said otherwise; on without GitHub there is nothing to read with.
    agent_xrs = _flag(env, "AGENT_XRS_ENABLED", github_prs)
    if agent_xrs and not github_prs:
        raise ConfigError("AGENT_XRS_ENABLED needs GITHUB_PR_ENABLED=1 (pinned definitions are read through the agents' App)")
    xr_requests = _flag(env, "XR_REQUEST_ENABLED", github_prs)
    if xr_requests and not github_prs:
        raise ConfigError("XR_REQUEST_ENABLED needs GITHUB_PR_ENABLED=1 (an XR request is a PR opened as the agents' App)")
    app_apis = _app_api_services(get("APP_API_SERVICES", ""))
    agent_xrs_interval = _number(env, "AGENT_XRS_INTERVAL", 60.0, float)
    if agent_xrs_interval < 5:
        raise ConfigError("AGENT_XRS_INTERVAL is at least 5 seconds")
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
        github_pr_enabled=github_prs, agent_xrs_enabled=agent_xrs, agent_xrs_interval=agent_xrs_interval,
        xr_request_enabled=xr_requests, app_api_services=app_apis,
        agent_token_file=Path(get("AGENT_TOKEN_FILE", "/var/run/clearance/agent-installation-token")),
        broker_url=get("BROKER_URL", BROKER_URL),
        model_key_file=Path(model_key) if model_key else None, model_port=model_port, model_routes=routes,
        model_audience=get("MODEL_TOKEN_AUDIENCE", "model-proxy"), events_enabled=events,
        # Interval triggers (scheduler.py): scheduled agents start at the start of each interval, UTC-aligned.
        schedules_enabled=_flag(env, "SCHEDULES_ENABLED", False),
        core_url=core_url, core_token_file=Path(get("CLEARANCE_CORE_TOKEN_FILE", CORE_TOKEN_FILE)),
        outage_allow_reads=_flag(env, "GATEWAY_OUTAGE_ALLOW_READS", False), oidc_allowed=allowed,
        tower_cluster=tower_cluster if tower_issuer else None, tower_issuer=tower_issuer,
        tower_jwks_url=get("TOWER_JWKS_URL", f"{tower_issuer}/jwks.json" if tower_issuer else None),
        tower_service_accounts=tower_sas if tower_issuer else (), delegates=_list(get("CLEARANCE_DELEGATES", "")),
        admins=_list(get("CLEARANCE_ADMINS", "")),
        # Images a definition may pin beyond the runtime catalog: the registries Glidepath builds agents' own images
        # into (code agents), as globs, e.g. ghcr.io/jfillman/*@sha256:*.
        runtime_images_extra=tuple(_list(get("RUNTIME_IMAGES_EXTRA", ""))),
        reconcile_seconds=reconcile_seconds, reconcile_window_hours=window, metrics_port=metrics_port,
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
    apis = AppAPIs(dict(cfg.app_api_services)) if cfg.app_api_services else None
    if cfg.prometheus_url or cfg.loki_url or cfg.backstage_url or apis:
        read = Reads(PrometheusAPI(cfg.prometheus_url) if cfg.prometheus_url else None,
                     LokiAPI(cfg.loki_url) if cfg.loki_url else None,
                     BackstageCatalog(cfg.backstage_url, cfg.backstage_token_file) if cfg.backstage_url else None,
                     apis)
    git = None
    if cfg.github_pr_enabled:
        # xr.request's registry: this cluster, its upper clusters and the tenants repo it watches (Airframe's contract).
        from .airframe_tools import CONTRACT
        reg = airframe_plan.Registry(cfg.airframe_cluster, cfg.airframe_upper_clusters, cfg.airframe_tenants_repo)
        git = GitHubPRs(cfg.agent_token_file, broker_url=cfg.broker_url,
                        registry=reg if cfg.xr_request_enabled else None, contract=CONTRACT)
    return Backends(git, argo, KubeAgentRuns(), read, None, None, glidepath, airframe)


def build_handlers(b: Backends) -> dict[str, Handler]:
    """hangar_handlers, less every tool whose backend is absent (so it is "no backend", not "backend error")."""
    h = hangar_handlers(b)
    # (argo.sync.lower and repo.pr.open_upper have a handler only when hangar_handlers wired one.)
    drop = set()
    if getattr(b.git, "registry", None) is None:
        drop.add("xr.request")          # no tenants repo configured (XR_REQUEST_ENABLED=0), or a git backend without one
    if getattr(b.argo, "syncer", None) is None:
        drop.add("argo.sync.lower")   # core_handlers registers it for any Argo backend; only ArgoSync can sync
    if b.read is not None:
        if getattr(b.read, "prometheus", None) is None:
            drop.add("metrics.query")
        if getattr(b.read, "loki", None) is None:
            drop.add("logs.query")
        if getattr(b.read, "apis", None) is None:
            drop.add("app.api.get")
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

    def model_usage(self, *a, **k):
        return self._locked(super().model_usage, *a, **k)

    def model_outcome(self, *a, **k):
        return self._locked(super().model_outcome, *a, **k)

    def guarded(self, op: Callable[..., Any], *a, **k) -> Any:
        """Run `op` under the gateway's lock, with the same crash-only handling: the trigger ledger's and the
        cursor's calls (engine/pg.py PgTriggers, each its own committed transaction) from the bridge, the
        reconciliation sweep and the scheduler."""
        return self._locked(op, *a, **k)

    def record_event(self, *a, **k):
        """The bridge's event history (Gateway.record_event), under the lock and committed: it is written between
        gateway operations, not inside one."""
        def op():
            try:
                out = Gateway.record_event(self, *a, **k)
                if self.state is not None:
                    self.state.commit()
                return out
            except self.store_errors:
                raise
            except Exception:
                if self.state is not None:
                    self.state.conn.rollback()
                raise
        return self._locked(op)

    def ping(self) -> None:
        """One round trip to the database, under the lock (a no-op when ephemeral)."""
        if self.state is None:
            return

        def roundtrip():
            with self.state.conn.cursor() as cur:
                cur.execute("SELECT 1")
            self.state.commit()
        self._locked(roundtrip)


def build_core(cfg: Config, outbox) -> Core:
    """The core's client with the outage policy's defaults, the outbox, and the outage switch; and a second client
    for the definition set's publish (PUT /v1/definitions, DEFINITIONS_TIMEOUT), so its longer deadline and its
    breaker are its own."""
    return Core(client_from_env(cfg.core_url, str(cfg.core_token_file)), outbox, allow_reads=cfg.outage_allow_reads,
                definitions_client=client_from_env(cfg.core_url, str(cfg.core_token_file), timeout=DEFINITIONS_TIMEOUT))


def build_gateway(cfg: Config, backends: Backends | None = None, auth: Authenticator | None = None,
                  clock: Callable[[], datetime] = lambda: datetime.now(timezone.utc),
                  core: Core | None = None) -> tuple[SerializedGateway, Any]:
    """The gateway and, for a durable store, its GatewayState (to close on exit; None when ephemeral). `core` is a
    ready Core (tests); else one is built from the configuration, its outbox in the gateway's database."""
    b = backends or build_backends(cfg)
    # The bundled definitions (the image's agents/) plus, as the loader finds them, the ones Agent XRs pin
    # (engine/definitions.py; agent_xrs.py). A bundled name is never shadowed.
    defs = Definitions(profile.load_dir(cfg.agents_dir))
    state, store_errors = None, ()
    if cfg.ephemeral:
        store = SessionStore()
        inbox = MemoryInbox()
        events = MemoryEvents()
        holds = MemoryHolds()
        pins = MemoryDefinitions()
        results = MemoryResults()
        outbox = MemoryOutbox()
        trigger_store = MemoryTriggers()
    else:
        import psycopg   # only where it is used
        from .engine.pg import (PgArtifacts, PgDefinitions, PgEvents, PgHolds, PgInbox, PgOutbox, PgResults, PgTriggers,
                                open_durable)
        state, store = open_durable(cfg.dsn)
        store_errors = (psycopg.OperationalError, psycopg.InterfaceError)
        if b.artifacts is None:
            b.artifacts = PgArtifacts(state)   # on the store's connection: a put commits with its call's session
        inbox = PgInbox(state)                 # human.request's questions and answers, in the same transactions
        events = PgEvents(state)               # the trigger bridge's event history
        holds = PgHolds(state)                 # people's holds on agents' triggers (control.py)
        pins = PgDefinitions(state)            # the cluster-pinned definitions, for restarts and the read API
        results = PgResults(state)             # finished runs' answers, 30 days (results.py)
        outbox = PgOutbox(state)               # what the core is owed (engine/outbox.py)
        trigger_store = PgTriggers(state)      # the trigger ledger and the event cursor (engine/trigger_ledger.py)
    if core is None:
        core = build_core(cfg, outbox)
    models = model_auth = None
    if cfg.model_key_file:
        models = {a: ModelRoute(a, "anthropic", ANTHROPIC_API, u) for a, u in cfg.model_routes.items()}
        model_auth = CachingAuthenticator(WorkloadAuthenticator(cfg.model_audience))
    gw = SerializedGateway(defs, store, core, auth or build_auth(cfg), clock, packs=PACKS,
                           handlers=build_handlers(b), runs=AgentRunBackend(b.runs), state=state,
                           store_errors=store_errors, models=models, model_auth=model_auth, inbox=inbox,
                           directory=BackstageDirectory(b.read.backstage) if getattr(b.read, "backstage", None) else None,
                           delegates=cfg.delegates, events=events, holds=holds, admins=cfg.admins,
                           definitions_store=pins, runtimes=load_runtimes(), runtime_images=cfg.runtime_images_extra,
                           results=results)
    gw.triggers = trigger_store
    return gw, state


def build_app(cfg: Config, gw: Gateway):
    from .engine.server import create_http_app
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
    """The start line: what is enabled, and the provider contract (docs/admin/provider.md): the provider name and
    this image's version, which the core's CLEARANCE_PROVIDERS entry must carry for every record to name it. No DSN,
    no token, no file contents."""
    backed = sorted(gw._handlers)
    return (f"autopilot-gateway: provider={PROVIDER}/{gateway_version()} (runtime role; records name the version "
            f"of the core's CLEARANCE_PROVIDERS entry; model calls report as {PROVIDER}, tool model.call) "
            f"port={cfg.port} store={'memory (ephemeral)' if cfg.ephemeral else 'postgres'} "
            f"agents={len(gw.defs)} from {cfg.agents_dir} tools={len(gw.tools)} backed={len(backed)} "
            f"auth=workload(aud={cfg.audience})" + (f"+oidc({cfg.oidc_issuer})" if cfg.oidc_issuer else "")
            + f" resource={cfg.resource_url} glidepath={cfg.glidepath_url or 'off'}"
            + (f" on-behalf-of={'on' if cfg.glidepath_on_behalf_of else 'off'}" if cfg.glidepath_url else "")
            + f" airframe={'on (cluster ' + cfg.airframe_cluster + ', no validator)' if cfg.airframe_enabled else 'off'}"
            + f" argo={'on (' + ','.join(cfg.argo_namespaces) + ')' if cfg.argo_enabled else 'off'}"
            + f" prometheus={cfg.prometheus_url or 'off'} loki={cfg.loki_url or 'off'} catalog={cfg.backstage_url or 'off'}"
            + f" github-prs={'on (broker ' + cfg.broker_url + ')' if cfg.github_pr_enabled else 'off'}"
            + f" xr-requests={'on (' + cfg.airframe_tenants_repo + ')' if cfg.xr_request_enabled else 'off'}"
            + f" app-apis={','.join(sorted(cfg.app_api_services or {})) or 'off'}"
            + (f" model-proxy=:{cfg.model_port} routes={','.join(sorted(cfg.model_routes))}" if cfg.model_key_file
               else " model-proxy=off")
            + f" events={'on' if cfg.events_enabled else 'off'}"
            + (f" reconcile={'every ' + format(cfg.reconcile_seconds, 'g') + 's over ' + str(cfg.reconcile_window_hours) + 'h' if cfg.reconcile_seconds else 'off'}"
               if cfg.events_enabled else "")
            + f" metrics={':' + str(cfg.metrics_port) if cfg.metrics_port else 'off'}"
            + f" schedules={'on' if cfg.schedules_enabled else 'off'}"
            + f" agent-pins={'on (every ' + format(cfg.agent_xrs_interval, 'g') + 's)' if cfg.agent_xrs_enabled else 'off'}"
            + f" core={cfg.core_url} outage-reads={'allowed' if cfg.outage_allow_reads else 'denied'}"
            + f" agentruns=in-cluster expire-every={cfg.expire_seconds:g}s")


def main() -> int:
    import sys
    if sys.argv[1:2] == ["api"]:   # `autopilot-gateway api`: clearance-api, the read-only process (api_serve.py)
        from .api_serve import main as api_main
        return api_main()
    if sys.argv[1:2] == ["conformance-control"]:   # test only, off by default (conformance.py)
        from .conformance import main as conformance_main
        return conformance_main(sys.argv[2:])
    logging.basicConfig(level=os.environ.get("LOG_LEVEL", "INFO").upper(),
                        format="%(asctime)s %(levelname)s %(name)s %(message)s")
    from . import otlp_logs
    try:
        cfg = config_from_env()
        # A copy of every log line to the OpenTelemetry collector, so it reaches Loki (otlp_logs.py).
        otlp = otlp_logs.install()
    except (ConfigError, ValueError) as e:
        log.error("configuration: %s", e)
        return 2
    import asyncio

    import uvicorn
    gw, state = build_gateway(cfg)
    app = build_app(cfg, gw)
    log.info(describe(cfg, gw))
    expirer = Expirer(gw, cfg.expire_seconds).start()
    bridge = pins = reconciler = metrics_srv = None
    sched = None
    from . import metrics as mx
    trigger_metrics = mx.TriggerMetrics()
    if cfg.metrics_port:
        metrics_srv = mx.serve(trigger_metrics.registry, cfg.metrics_port)
    if cfg.schedules_enabled:
        from .scheduler import Scheduler
        sched = Scheduler(gw, metrics=trigger_metrics).start()
    if cfg.events_enabled:
        from .event_bridge import EventBridge
        bridge = EventBridge(gw, cfg.glidepath_url, cfg.glidepath_token_file, metrics=trigger_metrics).start()
        if cfg.reconcile_seconds:
            from .reconcile import Reconciler
            reconciler = Reconciler(bridge, interval=cfg.reconcile_seconds,
                                    window_hours=cfg.reconcile_window_hours).start()
    if cfg.agent_xrs_enabled:
        from .agent_xrs import AgentXRs
        pins = AgentXRs(gw, KubeReader(IN_CLUSTER_API, SA_DIR / "token", SA_DIR / "ca.crt"),
                        GitHubPRs(cfg.agent_token_file, broker_url=cfg.broker_url), interval=cfg.agent_xrs_interval).start()
    level = os.environ.get("LOG_LEVEL", "info").lower()
    servers = [uvicorn.Server(uvicorn.Config(app, host="0.0.0.0", port=cfg.port, log_level=level,
                                             proxy_headers=False, server_header=False))]
    if cfg.model_key_file:
        from .engine.server import create_model_app
        servers.append(uvicorn.Server(uvicorn.Config(create_model_app(gw, key_file=str(cfg.model_key_file)),
                                                     host="0.0.0.0", port=cfg.model_port, log_level=level,
                                                     proxy_headers=False, server_header=False)))

    async def serve_all():
        await asyncio.gather(*(s.serve() for s in servers))
    try:
        asyncio.run(serve_all())
    finally:
        if sched is not None:
            sched.stop()
        if pins is not None:
            pins.stop()
        if reconciler is not None:
            reconciler.stop()
        if bridge is not None:
            bridge.stop()
        if metrics_srv is not None:
            metrics_srv.shutdown()
        expirer.stop()
        if state is not None:
            state.close()
        if otlp is not None:
            otlp.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
