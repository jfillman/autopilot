"""serve: the deployable gateway's configuration, backend selection, expiry sweep and HTTP surface. No network."""
import json
import threading
from datetime import timedelta

import pytest
from coreenv import core_env
from starlette.testclient import TestClient

from autopilot import serve
from autopilot.airframe_tools import AirframeTools
from autopilot.backends import Backends
from autopilot.glidepath_api import GlidepathAPI
from autopilot.kube_runs import KubeAgentRuns
from autopilot.reads import KubeArgo
from autopilot.engine.auth import CachingAuthenticator, OIDCAuthenticator, RoutingAuthenticator, WorkloadAuthenticator
from autopilot.engine.dispatch import AuthError, Principal
from autopilot.engine.tiers import Tier

EPH = {"AUTOPILOT_EPHEMERAL": "1", **core_env()}
CORE_WITHOUT_BACKEND = ["catalog.read", "metrics.query", "logs.query", "app.api.get", "argo.app.get",
                        "argo.sync.lower", "artifact.put", "artifact.get", "chat.recv", "chat.send",
                        "repo.pr.open", "repo.pr.open_upper", "xr.request"]


# -- configuration ----------------------------------------------------------------------------------------------------
def test_defaults():
    c = serve.config_from_env(EPH)
    assert c.ephemeral and c.dsn is None and c.port == 8080 and c.expire_seconds == 60
    assert c.audience == "clearance" and c.oidc_issuer is None and c.issuer_url == c.resource_url
    assert c.resource_url == "http://localhost:8080/mcp"
    assert c.agents_dir == serve.REPO_AGENTS and (c.agents_dir / "coding-agent.yaml").is_file()
    assert c.glidepath_url is None and not c.glidepath_on_behalf_of
    assert str(c.glidepath_token_file) == "/var/run/secrets/kubernetes.io/serviceaccount/token"
    assert c.airframe_enabled and c.airframe_cluster == "hangar-dev" and c.airframe_upper_clusters == ("hangar-prod",)


def test_full_environment(tmp_path):
    c = serve.config_from_env({
        "GATEWAY_DSN": "postgresql://gateway@pg/gateway", "AUTOPILOT_AGENTS_DIR": str(tmp_path),
        "CLEARANCE_CORE_URL": "http://clearance-core.clearance-system:8080", "GATEWAY_OUTAGE_ALLOW_READS": "1",
        "AUTOPILOT_RESOURCE_URL": "https://clearance.hangar.io/mcp", "AUTOPILOT_OIDC_ISSUER": "https://dex.example/dex/",
        "AUTOPILOT_OIDC_AUDIENCE": "tower", "AUTOPILOT_OIDC_ALLOWED_USERS": "jfillman",
        "AUTOPILOT_AUDIENCE": "clearance-dev", "PORT": "9000",
        "AUTOPILOT_EXPIRE_SECONDS": "15", "GLIDEPATH_API_URL": "http://glidepath-api.platform-system:8080",
        "GLIDEPATH_TOKEN_FILE": "/var/run/glidepath/token", "GLIDEPATH_ON_BEHALF_OF": "1", "AIRFRAME_ENABLED": "0",
        "AIRFRAME_UPPER_CLUSTERS": "a, b,", "AIRFRAME_TENANTS_REPO": "t"})
    assert not c.ephemeral and c.dsn.startswith("postgresql://") and c.port == 9000 and c.expire_seconds == 15
    assert c.oidc_issuer == "https://dex.example/dex" and c.oidc_jwks_url == "https://dex.example/dex/keys"
    assert c.issuer_url == "https://dex.example/dex" and c.audience == "clearance-dev"
    assert c.glidepath_on_behalf_of and not c.airframe_enabled and c.airframe_upper_clusters == ("a", "b")
    assert c.core_url == "http://clearance-core.clearance-system:8080" and c.outage_allow_reads
    assert str(c.core_token_file) == serve.CORE_TOKEN_FILE


def test_the_outage_switch_is_off_by_default():
    assert serve.config_from_env(EPH).outage_allow_reads is False


@pytest.mark.parametrize("var", serve.CORE_ONLY)
def test_the_cores_settings_are_refused_on_the_gateway(var):
    # The evidence DSN, verifiers, provenance's logins, the GitHub App and GitHub Actions callers are the core's.
    with pytest.raises(serve.ConfigError, match=var):
        serve.config_from_env({**EPH, var: "x"})


def test_ephemeral_ignores_a_dsn():
    assert serve.config_from_env({**EPH, "GATEWAY_DSN": "postgresql://x"}).dsn is None


@pytest.mark.parametrize("env, match", [
    ({}, "GATEWAY_DSN is required"),
    ({"AUTOPILOT_EPHEMERAL": "0", "GATEWAY_DSN": ""}, "GATEWAY_DSN is required"),
    ({"AUTOPILOT_EPHEMERAL": "1"}, "CLEARANCE_CORE_URL is required"),
    ({"AUTOPILOT_EPHEMERAL": "1", "CLEARANCE_CORE_URL": "clearance-core:8080"}, "http"),
    ({**EPH, "AUTOPILOT_OIDC_ISSUER": "https://dex"}, "set together"),
    ({**EPH, "AUTOPILOT_OIDC_AUDIENCE": "tower"}, "set together"),
    ({**EPH, "AUTOPILOT_OIDC_ISSUER": "https://dex", "AUTOPILOT_OIDC_AUDIENCE": "c"}, "needs AUTOPILOT_OIDC_ALLOWED_USERS"),
    ({**EPH, "PORT": "http"}, "PORT must be a number"),
    ({**EPH, "PORT": "0"}, "PORT must be positive"),
    ({**EPH, "PORT": "70000"}, "at most 65535"),
    ({**EPH, "AUTOPILOT_EXPIRE_SECONDS": "-1"}, "AUTOPILOT_EXPIRE_SECONDS must be positive"),
    ({**EPH, "AIRFRAME_ENABLED": "maybe"}, "AIRFRAME_ENABLED must be 0 or 1"),
    ({**EPH, "AUTOPILOT_AGENTS_DIR": "/nonexistent/agents"}, "is not a directory"),
])
def test_refuses_an_environment_that_cannot_start(env, match):
    with pytest.raises(serve.ConfigError, match=match):
        serve.config_from_env(env)


# -- backends ---------------------------------------------------------------------------------------------------------
def test_default_backends_are_airframe_argo_reads_and_agentruns_only():
    b = serve.build_backends(serve.config_from_env(EPH))
    assert isinstance(b.runs, KubeAgentRuns) and isinstance(b.airframe, AirframeTools) and b.glidepath is None
    assert isinstance(b.argo, KubeArgo) and b.argo.namespaces == ("argocd",)
    assert (b.git, b.read, b.human, b.artifacts) == (None,) * 4
    assert b.airframe.kube.server == "https://kubernetes.default.svc" and b.airframe.validator is None
    h = serve.build_handlers(b)
    assert sorted(h) == sorted({"airframe.capabilities", "airframe.explain", "airframe.plan", "airframe.status",
                                "airframe.describe", "airframe.verify", "argo.app.get"})


def test_glidepath_only_when_its_url_is_set():
    cfg = serve.config_from_env({**EPH, "GLIDEPATH_API_URL": "http://gp:8080/", "AIRFRAME_ENABLED": "0",
                                 "ARGO_ENABLED": "0"})
    b = serve.build_backends(cfg)
    assert isinstance(b.glidepath, GlidepathAPI) and b.glidepath.base == "http://gp:8080" and b.airframe is None
    h = serve.build_handlers(b)
    assert h and all(t.startswith("glidepath.") for t in h) and "glidepath.explain" in h


@pytest.fixture
def opener():
    class Opener:
        def __init__(self):
            self.reqs = []

        def __call__(self, req, timeout, ctx):
            self.reqs.append((req.get_method(), req.full_url))
            return (201 if req.get_method() == "POST" else 200), b"{}"
    return Opener()


class Auth:
    def authenticate(self, token):
        return Principal("user:alice", "user", Tier.T1)


@pytest.fixture
def built(tmp_path, opener, clock):
    token = tmp_path / "token"
    token.write_text("t")
    cfg = serve.config_from_env({**EPH, "AIRFRAME_ENABLED": "0"})
    b = Backends(None, None, KubeAgentRuns("https://api", token, None, opener=opener), None, None)
    gw, state = serve.build_gateway(cfg, backends=b, auth=Auth(), clock=clock)
    assert state is None
    return gw


def test_no_core_tool_has_a_handler(built):
    assert not set(CORE_WITHOUT_BACKEND) & set(built._handlers)
    # The Gateway's own: runs, and the human inbox (in memory here, ephemeral; PgInbox with a DSN).
    assert {"run.spawn", "run.close", "human.request", "human.request.get"} <= set(built._handlers)


PR = {"repo": "jfillman/flight-api", "branch": "agent/t-1", "title": "fix probe",
      "files": [{"path": "charts/values.yaml", "content": "x"}]}


@pytest.mark.parametrize("tool, args", [("catalog.read", {"query": "x"}), ("metrics.query", {"promql": "up"}),
                                        ("logs.query", {"logql": "{app=\"x\"}"}), ("argo.app.get", {"app": "x"}),
                                        ("repo.pr.open", PR)])
def test_an_allowed_call_without_a_backend_says_so_and_costs_nothing(built, tool, args):
    sid = built.open_session("t", "coding-agent").data["session"]
    r = built.call("t", sid, tool, args)
    assert r.decision["allow"] is True, r.as_dict()
    assert not r.ok and r.error == "no backend for this tool in this deployment"
    assert built.core.records[-1]["result"] == "error:NoHandler"
    s = built.store.get(sid)
    assert s.remaining("tool_calls") == s.limits.tool_calls


def test_spawn_and_expiry_go_through_the_kube_store(built, opener, clock):
    sid = built.open_session("t", "orchestrator").data["session"]
    r = built.call("t", sid, "run.spawn", {"agent": "researcher", "limits": {"tool_calls": 10}})
    assert r.ok, r.as_dict()
    assert opener.reqs == [("POST", "https://api/apis/catalog.hangar.io/v1alpha1/namespaces/autopilot-runs/agentruns")]
    clock.advance(days=2)
    done = serve.Expirer(built, 60).sweep()
    assert r.data["session"] in done
    assert opener.reqs[-1] == ("DELETE", "https://api/apis/catalog.hangar.io/v1alpha1/namespaces/autopilot-runs/"
                                         f"agentruns/{r.data['run']}")
    assert built.core.records[-1]["tool"] == "run.delete" and built.core.records[-1]["result"] == "deleted"


def test_expiry_waits_for_the_gateway_lock(built):
    done = threading.Event()
    with built.lock:
        t = threading.Thread(target=lambda: (built.expire(), done.set()))
        t.start()
        assert not done.wait(0.3), "expire ran while a gateway call held the lock"
    t.join(5)
    assert done.is_set()


def test_expirer_thread_sweeps_and_survives_a_failed_sweep():
    calls = []

    class GW:
        def expire(self):
            calls.append(1)
            if len(calls) == 1:
                raise RuntimeError("db down")
            return []
    e = serve.Expirer(GW(), 0.05).start()
    try:
        deadline = threading.Event()
        for _ in range(100):
            if len(calls) >= 3:
                break
            deadline.wait(0.05)
    finally:
        e.stop()
    assert len(calls) >= 3


# -- auth -------------------------------------------------------------------------------------------------------------
def test_auth_is_workload_only_without_oidc():
    a = serve.build_auth(serve.config_from_env(EPH))
    assert isinstance(a, CachingAuthenticator) and isinstance(a.inner, RoutingAuthenticator)
    assert a.inner.oidc == {} and isinstance(a.inner.workload, WorkloadAuthenticator)
    assert a.inner.workload.audience == "clearance"


def test_auth_routes_the_oidc_issuer():
    a = serve.build_auth(serve.config_from_env({**EPH, "AUTOPILOT_OIDC_ISSUER": "https://dex/dex",
                                                "AUTOPILOT_OIDC_AUDIENCE": "tower", "AUTOPILOT_AUDIENCE": "aud2",
                                                "AUTOPILOT_OIDC_ALLOWED_USERS": "jfillman, ops-*"}))
    o = a.inner.oidc["https://dex/dex"]
    assert isinstance(o, OIDCAuthenticator) and o.audience == "tower" and o.keys.url == "https://dex/dex/keys"
    assert o.allowed == ("jfillman", "ops-*")
    assert a.inner.workload.audience == "aud2"


def test_start_line_names_no_secret():
    cfg = serve.config_from_env({"GATEWAY_DSN": "postgresql://gateway:hunter2@pg/gateway",
                                 "GLIDEPATH_API_URL": "http://gp:8080", **core_env()})
    gw, _ = serve.build_gateway(serve.config_from_env({**EPH, "GLIDEPATH_API_URL": "http://gp:8080"}), auth=Auth())
    line = serve.describe(cfg, gw)
    assert "hunter2" not in line and "postgres" in line and "glidepath=http://gp:8080" in line
    assert f"core={cfg.core_url}" in line and "outage-reads=denied" in line


# -- HTTP -------------------------------------------------------------------------------------------------------------
HEADERS = {"Accept": "application/json, text/event-stream", "Content-Type": "application/json",
           "MCP-Protocol-Version": "2025-06-18"}


@pytest.fixture
def client():
    cfg = serve.config_from_env(EPH)
    gw, _ = serve.build_gateway(cfg)
    with TestClient(serve.build_app(cfg, gw)) as c:
        c.gw = gw
        yield c


def test_ephemeral_server_answers_healthz(client):
    r = client.get("/healthz")
    assert r.status_code == 200 and r.json() == {"ok": True, "tools": len(client.gw.tools), "core": "closed"}


@pytest.mark.parametrize("bearer", [None, "not-a-jwt"])
def test_ephemeral_server_refuses_mcp_without_a_valid_bearer(client, bearer):
    h = dict(HEADERS)
    if bearer:
        h["Authorization"] = f"Bearer {bearer}"
    body = {"jsonrpc": "2.0", "id": 1, "method": "tools/call",
            "params": {"name": "session_open", "arguments": {"agent": "coding-agent"}}}
    r = client.post("/mcp", headers=h, content=json.dumps(body))
    assert r.status_code == 401
    assert client.gw.core.records == [], "nothing reached the gateway"


# -- losing the store -------------------------------------------------------------------------------------------------
class StoreLost(Exception):
    pass


def test_a_store_error_ends_the_process_and_others_do_not(built):
    lost = []
    built.store_errors, built.on_store_lost = (StoreLost,), lost.append

    def boom(*a, **k):
        raise StoreLost("server closed the connection")
    built.store.flush = boom
    with pytest.raises(StoreLost):
        built.open_session("t", "coding-agent")
    assert len(lost) == 1 and isinstance(lost[0], StoreLost)
    assert not built.lock._is_owned(), "the lock is released on the way out"

    def other(*a, **k):
        raise ValueError("not the database")
    built.store.flush = other
    with pytest.raises(ValueError):
        built.open_session("t", "coding-agent")
    assert len(lost) == 1


def test_the_sweep_pings_the_database_first(built):
    order = []

    class Conn:
        def cursor(self):
            class Cur:
                def __enter__(s):
                    return s

                def __exit__(s, *a):
                    return False

                def execute(s, q):
                    order.append(q)
                    raise StoreLost("connection is closed")
            return Cur()

    class State:
        conn = Conn()

        def commit(self):
            order.append("commit")
    lost = []
    built.state, built.store_errors, built.on_store_lost = State(), (StoreLost,), lost.append
    built.expire = lambda *a, **k: order.append("expire") or []
    with pytest.raises(StoreLost):
        serve.Expirer(built, 60).sweep()
    assert order == ["SELECT 1"] and len(lost) == 1


def test_ephemeral_ping_is_a_no_op(built):
    assert built.state is None
    built.ping()


# -- P5: GitHub PRs, the model proxy, events ----------------------------------------------------------------------
def test_p5_backends_are_off_by_default():
    c = serve.config_from_env(EPH)
    assert not c.github_pr_enabled and c.model_key_file is None and not c.events_enabled
    h = serve.build_handlers(serve.build_backends(c))
    assert not {"repo.pr.open", "repo.pr.open_upper", "xr.request"} & set(h)
    gw, _ = serve.build_gateway(c)
    assert gw.models is None


def test_github_prs_register_repo_pr_open_open_upper_and_xr_request():
    from autopilot.github_pr import GitHubPRs
    c = serve.config_from_env({**EPH, "GITHUB_PR_ENABLED": "1", "AIRFRAME_ENABLED": "0"})
    b = serve.build_backends(c)
    assert isinstance(b.git, GitHubPRs) and str(b.git.token_file) == "/var/run/clearance/agent-installation-token"
    assert b.git.registry.tenants_repo == "gitops-hangar-dev-tenants" and b.git.registry.dev_cluster == "hangar-dev"
    h = serve.build_handlers(b)
    assert {"repo.pr.open", "repo.pr.open_upper", "xr.request"} <= set(h)


def test_xr_request_can_be_turned_off_and_needs_github_prs():
    c = serve.config_from_env({**EPH, "GITHUB_PR_ENABLED": "1", "XR_REQUEST_ENABLED": "0"})
    h = serve.build_handlers(serve.build_backends(c))
    assert "repo.pr.open" in h and "xr.request" not in h
    with pytest.raises(serve.ConfigError, match="XR_REQUEST_ENABLED needs GITHUB_PR_ENABLED"):
        serve.config_from_env({**EPH, "XR_REQUEST_ENABLED": "1"})


def test_app_api_get_has_a_backend_only_with_app_api_services():
    assert "app.api.get" not in serve.build_handlers(serve.build_backends(serve.config_from_env(EPH)))
    c = serve.config_from_env({**EPH, "APP_API_SERVICES": '{"flight-api": "http://flight-api.app-flight-api-dev.svc:8080"}'})
    b = serve.build_backends(c)
    assert "app.api.get" in serve.build_handlers(b) and b.read.apis.services == {
        "flight-api": "http://flight-api.app-flight-api-dev.svc:8080"}
    assert "app-apis=flight-api" in serve.describe(c, serve.build_gateway(c)[0])


@pytest.mark.parametrize("raw", ["[]", "{", '{"a": "ftp://x"}', '{"a": "http://u:p@x"}', '{"a": "http://x?q=1"}',
                                 '{"a": 1}', '{"": "http://x"}', '{"Flight-API": "http://x"}', '{"a/b": "http://x"}'])
def test_app_api_services_must_be_plain_base_urls(raw):
    with pytest.raises(serve.ConfigError, match="APP_API_SERVICES"):
        serve.config_from_env({**EPH, "APP_API_SERVICES": raw})


def test_argo_sync_lower_only_with_argo_sync_enabled():
    from autopilot.argo_sync import ArgoSync
    off = serve.build_backends(serve.config_from_env({**EPH, "AIRFRAME_ENABLED": "0"}))
    assert off.argo.syncer is None and "argo.sync.lower" not in serve.build_handlers(off)
    on = serve.build_backends(serve.config_from_env({**EPH, "AIRFRAME_ENABLED": "0", "ARGO_SYNC_ENABLED": "1",
                                                      "ARGO_APP_NAMESPACES": "argocd,argocd-apps"}))
    assert isinstance(on.argo.syncer, ArgoSync) and on.argo.syncer.namespaces == ("argocd", "argocd-apps")
    assert "argo.sync.lower" in serve.build_handlers(on)


def test_model_proxy_config(tmp_path):
    key = tmp_path / "k"
    c = serve.config_from_env({**EPH, "MODEL_KEY_FILE": str(key), "MODEL_ROUTES": "claude-sonnet-5-5=claude-sonnet-5-5, fast=claude-haiku-5-5"})
    assert c.model_routes == {"claude-sonnet-5-5": "claude-sonnet-5-5", "fast": "claude-haiku-5-5"} and c.model_port == 8081
    gw, _ = serve.build_gateway(c)
    assert gw.models.routes["fast"].upstream_model == "claude-haiku-5-5" and gw.models.routes["fast"].backend == "anthropic"
    assert "model-proxy=:8081" in serve.describe(c, gw) and str(key) not in serve.describe(c, gw)


@pytest.mark.parametrize("env, match", [
    ({"MODEL_KEY_FILE": "/k"}, "MODEL_ROUTES names no model"),
    ({"MODEL_KEY_FILE": "/k", "MODEL_ROUTES": "a"}, "alias=upstream"),
    ({"MODEL_KEY_FILE": "/k", "MODEL_ROUTES": "a=b", "MODEL_PORT": "8080"}, "must differ"),
    ({"EVENTS_ENABLED": "1"}, "needs GLIDEPATH_API_URL"),
])
def test_p5_config_refusals(env, match):
    with pytest.raises(serve.ConfigError, match=match):
        serve.config_from_env({**EPH, **env})


def test_model_calls_hold_the_gateway_lock(built):
    done = threading.Event()
    with built.lock:
        t = threading.Thread(target=lambda: (built.model_usage("s-x", 1), done.set()))
        t.start()
        assert not done.wait(0.3)
    t.join(5)
    assert done.is_set()


def test_catalog_read_only_with_backstage_url():
    off = serve.build_backends(serve.config_from_env({**EPH, "AIRFRAME_ENABLED": "0"}))
    assert "catalog.read" not in serve.build_handlers(off)
    on = serve.build_backends(serve.config_from_env({**EPH, "AIRFRAME_ENABLED": "0",
                                                      "BACKSTAGE_URL": "http://backstage.hangar-prod.local"}))
    assert on.read.backstage.base == "http://backstage.hangar-prod.local"
    assert str(on.read.backstage.token_file) == "/var/run/clearance/backstage-token"
    h = serve.build_handlers(on)
    assert "catalog.read" in h and not {"metrics.query", "logs.query"} & set(h)


def test_tower_issuer_and_delegates(tmp_path):
    from autopilot.engine.auth import ForeignWorkloadAuthenticator
    with pytest.raises(serve.ConfigError, match="TOWER_SERVICE_ACCOUNTS"):
        serve.config_from_env({**EPH, "TOWER_ISSUER": "https://hangarplatform.dev/oidc/hangar-prod"})
    c = serve.config_from_env({**EPH, "TOWER_ISSUER": "https://hangarplatform.dev/oidc/hangar-prod/",
                               "TOWER_CLUSTER": "hangar-prod", "TOWER_SERVICE_ACCOUNTS": "app-backstage-prod/backstage",
                               "AUTOPILOT_DELEGATES": "hangar-prod/app-backstage-prod/backstage"})
    assert c.tower_jwks_url == "https://hangarplatform.dev/oidc/hangar-prod/jwks.json"
    assert c.delegates == ("hangar-prod/app-backstage-prod/backstage",)
    tower = serve.build_auth(c).inner.oidc["https://hangarplatform.dev/oidc/hangar-prod"]
    assert isinstance(tower, ForeignWorkloadAuthenticator) and tower.allowed == ("app-backstage-prod/backstage",)
    assert tower.name == "hangar-prod" and tower.audience == "clearance"


# -- R-10a: AUTOPILOT_* names, CLEARANCE_* aliases for one release ---------------------------------------------------------
RENAMED = {   # new -> (legacy, a value, how to read it back)
    "AUTOPILOT_EPHEMERAL": ("CLEARANCE_EPHEMERAL", "1"),
    "AUTOPILOT_OIDC_ISSUER": ("CLEARANCE_OIDC_ISSUER", "https://dex.example/dex"),
    "AUTOPILOT_OIDC_AUDIENCE": ("CLEARANCE_OIDC_AUDIENCE", "tower"),
    "AUTOPILOT_OIDC_ALLOWED_USERS": ("CLEARANCE_OIDC_ALLOWED_USERS", "jfillman"),
    "AUTOPILOT_OIDC_JWKS_URL": ("CLEARANCE_OIDC_JWKS_URL", "https://dex.example/keys2"),
    "AUTOPILOT_OIDC_USER_CLAIM": ("CLEARANCE_OIDC_USER_CLAIM", "sub"),
    "AUTOPILOT_RESOURCE_URL": ("CLEARANCE_RESOURCE_URL", "https://ap.example/mcp"),
    "AUTOPILOT_AUDIENCE": ("CLEARANCE_AUDIENCE", "aud-x"),
    "AUTOPILOT_EXPIRE_SECONDS": ("CLEARANCE_EXPIRE_SECONDS", "15"),
    "AUTOPILOT_DELEGATES": ("CLEARANCE_DELEGATES", "ns/sa"),
    "AUTOPILOT_ADMINS": ("CLEARANCE_ADMINS", "user:root"),
}
OIDC = {"AUTOPILOT_OIDC_ISSUER": "https://dex.example/dex", "AUTOPILOT_OIDC_AUDIENCE": "tower",
        "AUTOPILOT_OIDC_ALLOWED_USERS": "jfillman"}


def _readback(c):
    return {"AUTOPILOT_EPHEMERAL": c.ephemeral, "AUTOPILOT_OIDC_ISSUER": c.oidc_issuer,
            "AUTOPILOT_OIDC_AUDIENCE": c.oidc_audience, "AUTOPILOT_OIDC_ALLOWED_USERS": list(c.oidc_allowed),
            "AUTOPILOT_OIDC_JWKS_URL": c.oidc_jwks_url, "AUTOPILOT_OIDC_USER_CLAIM": c.oidc_user_claim,
            "AUTOPILOT_RESOURCE_URL": c.resource_url, "AUTOPILOT_AUDIENCE": c.audience,
            "AUTOPILOT_EXPIRE_SECONDS": c.expire_seconds, "AUTOPILOT_DELEGATES": list(c.delegates),
            "AUTOPILOT_ADMINS": list(c.admins)}


def _env_for(name, extra=None):
    env = {k: v for k, v in EPH.items() if k != "AUTOPILOT_EPHEMERAL"}
    env.update({"GATEWAY_DSN": "postgresql://x"})
    env.update({k: v for k, v in OIDC.items() if k != name} if name.startswith("AUTOPILOT_OIDC_") else {})
    return {**env, **(extra or {})}


@pytest.mark.parametrize("name", RENAMED)
def test_the_new_name_is_read_without_a_warning(name, caplog):
    legacy, value = RENAMED[name]
    env = _env_for(name, {name: value})
    with caplog.at_level("WARNING", logger="autopilot.gateway"):
        c = serve.config_from_env(env)
    got = _readback(c)[name]
    assert got not in (None, [], False, "clearance", "email", 60.0) or name == "AUTOPILOT_EPHEMERAL", (name, got)
    assert "deprecated" not in caplog.text


@pytest.mark.parametrize("name", RENAMED)
def test_the_legacy_name_still_works_and_warns_naming_the_new_one(name, caplog):
    legacy, value = RENAMED[name]
    new_env = _env_for(name, {name: value})
    old_env = _env_for(name, {legacy: value})
    with caplog.at_level("WARNING", logger="autopilot.gateway"):
        old = serve.config_from_env(old_env)
    assert _readback(old)[name] == _readback(serve.config_from_env(new_env))[name]
    warnings = [r for r in caplog.records if "deprecated" in r.getMessage()]
    assert len(warnings) == 1 and f"{legacy} -> {name}" in warnings[0].getMessage()


def test_the_new_name_wins_when_both_are_set(caplog):
    env = _env_for("AUTOPILOT_AUDIENCE", {"AUTOPILOT_AUDIENCE": "new", "CLEARANCE_AUDIENCE": "old",
                                          "AUTOPILOT_EXPIRE_SECONDS": "5", "CLEARANCE_EXPIRE_SECONDS": "99"})
    with caplog.at_level("WARNING", logger="autopilot.gateway"):
        c = serve.config_from_env(env)
    assert c.audience == "new" and c.expire_seconds == 5


def test_several_legacy_names_are_one_warning(caplog):
    env = _env_for("x", {"CLEARANCE_AUDIENCE": "a", "CLEARANCE_ADMINS": "user:r", "CLEARANCE_DELEGATES": "ns/sa"})
    with caplog.at_level("WARNING", logger="autopilot.gateway"):
        serve.config_from_env(env)
    msgs = [r.getMessage() for r in caplog.records if "deprecated" in r.getMessage()]
    assert len(msgs) == 1
    for old in ("AUDIENCE", "ADMINS", "DELEGATES"):
        assert f"CLEARANCE_{old} -> AUTOPILOT_{old}" in msgs[0]


def test_an_empty_new_name_falls_back_to_the_legacy_one():
    assert serve.config_from_env({**EPH, "AUTOPILOT_AUDIENCE": "", "CLEARANCE_AUDIENCE": "old"}).audience == "old"


def test_the_clearance_client_names_are_unchanged():
    c = serve.config_from_env({**EPH, "CLEARANCE_CORE_TOKEN_FILE": "/tmp/tok"})
    assert c.core_url == EPH["CLEARANCE_CORE_URL"] and str(c.core_token_file) == "/tmp/tok"
    with pytest.raises(serve.ConfigError, match="CLEARANCE_CORE_URL is required"):
        serve.config_from_env({"AUTOPILOT_EPHEMERAL": "1"})


def test_aliases_do_not_touch_the_core_only_refuse_list():
    # Every core-only name is still refused, with the aliasing in play, and none of them is an alias or a new name.
    assert not set(serve.CORE_ONLY) & (set(RENAMED) | {v[0] for v in RENAMED.values()})
    for var in serve.CORE_ONLY:
        with pytest.raises(serve.ConfigError, match=var):
            serve.config_from_env({**EPH, "CLEARANCE_AUDIENCE": "old", "AUTOPILOT_AUDIENCE": "new", var: "x"})


# -- run-pod token audiences (R-10b) ----------------------------------------------------------------------------------
def _review(audiences_returned, calls):
    def opener(req, timeout):
        body = json.loads(req.data)
        calls.append(body["spec"]["audiences"])
        return {"status": {"authenticated": True, "audiences": audiences_returned,
                           "user": {"username": "system:serviceaccount:autopilot-runs:run"}}}
    return opener


def test_run_audiences_default_to_autopilot_and_the_core_audience():
    assert serve.config_from_env(EPH).run_audiences == ("autopilot", "clearance")
    assert serve.config_from_env({**EPH, "CLEARANCE_AUDIENCE": "aud2"}).run_audiences == ("autopilot", "aud2")
    c = serve.config_from_env({**EPH, "AUTOPILOT_RUN_AUDIENCES": " autopilot , clearance,x"})
    assert c.run_audiences == ("autopilot", "clearance", "x")
    with pytest.raises(serve.ConfigError):
        serve.config_from_env({**EPH, "AUTOPILOT_RUN_AUDIENCES": " , "})


def test_workload_auth_accepts_either_run_audience_and_asks_token_review_for_both(tmp_path):
    f = tmp_path / "t"
    f.write_text("sa")
    a = serve.build_auth(serve.config_from_env(EPH)).inner.workload
    assert a.audiences == ("clearance", "autopilot")
    for returned in (["autopilot"], ["clearance"]):
        calls = []
        w = WorkloadAuthenticator("clearance", token_file=f, opener=_review(returned, calls), audiences=("autopilot",))
        p = w.authenticate("run-token")
        assert p.id == "autopilot-runs/run" and calls == [["clearance", "autopilot"]]


def test_workload_auth_refuses_a_token_for_another_audience(tmp_path):
    f = tmp_path / "t"
    f.write_text("sa")
    w = WorkloadAuthenticator("clearance", token_file=f, opener=_review(["model-proxy"], []), audiences=("autopilot",))
    with pytest.raises(AuthError):
        w.authenticate("run-token")
    # the single-audience form is unchanged
    only = WorkloadAuthenticator("clearance", token_file=f, opener=_review(["autopilot"], []))
    with pytest.raises(AuthError):
        only.authenticate("run-token")
