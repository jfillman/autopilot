"""serve: the deployable gateway's configuration, backend selection, expiry sweep and HTTP surface. No network."""
import json
import threading
from datetime import timedelta

import pytest
from starlette.testclient import TestClient

from autopilot import serve
from autopilot.airframe_tools import AirframeTools
from autopilot.backends import Backends
from autopilot.glidepath_api import GlidepathAPI
from autopilot.kube_runs import KubeAgentRuns
from autopilot.reads import KubeArgo
from clearance.auth import CachingAuthenticator, OIDCAuthenticator, RoutingAuthenticator, WorkloadAuthenticator
from clearance.dispatch import Principal
from clearance.tiers import Tier

EPH = {"CLEARANCE_EPHEMERAL": "1"}
CORE_WITHOUT_BACKEND = ["catalog.read", "metrics.query", "logs.query", "app.api.get", "argo.app.get",
                        "argo.sync.lower", "artifact.put", "artifact.get", "chat.recv", "chat.send",
                        "human.request", "repo.pr.open", "repo.pr.open_upper", "xr.request"]


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
        "CLEARANCE_DSN": "postgresql://clearance@pg/clearance", "CLEARANCE_AGENTS_DIR": str(tmp_path),
        "CLEARANCE_RESOURCE_URL": "https://clearance.hangar.io/mcp", "CLEARANCE_OIDC_ISSUER": "https://dex.example/dex/",
        "CLEARANCE_OIDC_AUDIENCE": "tower", "CLEARANCE_AUDIENCE": "clearance-dev", "PORT": "9000",
        "CLEARANCE_EXPIRE_SECONDS": "15", "GLIDEPATH_API_URL": "http://glidepath-api.platform-system:8080",
        "GLIDEPATH_TOKEN_FILE": "/var/run/glidepath/token", "GLIDEPATH_ON_BEHALF_OF": "1", "AIRFRAME_ENABLED": "0",
        "AIRFRAME_UPPER_CLUSTERS": "a, b,", "AIRFRAME_TENANTS_REPO": "t"})
    assert not c.ephemeral and c.dsn.startswith("postgresql://") and c.port == 9000 and c.expire_seconds == 15
    assert c.oidc_issuer == "https://dex.example/dex" and c.oidc_jwks_url == "https://dex.example/dex/keys"
    assert c.issuer_url == "https://dex.example/dex" and c.audience == "clearance-dev"
    assert c.glidepath_on_behalf_of and not c.airframe_enabled and c.airframe_upper_clusters == ("a", "b")


def test_ephemeral_ignores_a_dsn():
    assert serve.config_from_env({**EPH, "CLEARANCE_DSN": "postgresql://x"}).dsn is None


@pytest.mark.parametrize("env, match", [
    ({}, "CLEARANCE_DSN is required"),
    ({"CLEARANCE_EPHEMERAL": "0", "CLEARANCE_DSN": ""}, "CLEARANCE_DSN is required"),
    ({**EPH, "CLEARANCE_OIDC_ISSUER": "https://dex"}, "set together"),
    ({**EPH, "CLEARANCE_OIDC_AUDIENCE": "tower"}, "set together"),
    ({**EPH, "PORT": "http"}, "PORT must be a number"),
    ({**EPH, "PORT": "0"}, "PORT must be positive"),
    ({**EPH, "PORT": "70000"}, "at most 65535"),
    ({**EPH, "CLEARANCE_EXPIRE_SECONDS": "-1"}, "CLEARANCE_EXPIRE_SECONDS must be positive"),
    ({**EPH, "AIRFRAME_ENABLED": "maybe"}, "AIRFRAME_ENABLED must be 0 or 1"),
    ({**EPH, "CLEARANCE_AGENTS_DIR": "/nonexistent/agents"}, "is not a directory"),
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
    assert {"run.spawn", "run.close"} <= set(built._handlers)


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
    assert built.audit.records[-1]["result"] == "error:NoHandler"
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
    assert built.audit.records[-1]["tool"] == "run.delete" and built.audit.records[-1]["result"] == "deleted"


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
    a = serve.build_auth(serve.config_from_env({**EPH, "CLEARANCE_OIDC_ISSUER": "https://dex/dex",
                                                "CLEARANCE_OIDC_AUDIENCE": "tower", "CLEARANCE_AUDIENCE": "aud2"}))
    o = a.inner.oidc["https://dex/dex"]
    assert isinstance(o, OIDCAuthenticator) and o.audience == "tower" and o.keys.url == "https://dex/dex/keys"
    assert a.inner.workload.audience == "aud2"


def test_start_line_names_no_secret():
    cfg = serve.config_from_env({"CLEARANCE_DSN": "postgresql://clearance:hunter2@pg/clearance",
                                 "GLIDEPATH_API_URL": "http://gp:8080"})
    gw, _ = serve.build_gateway(serve.config_from_env({**EPH, "GLIDEPATH_API_URL": "http://gp:8080"}), auth=Auth())
    line = serve.describe(cfg, gw)
    assert "hunter2" not in line and "postgres" in line and "glidepath=http://gp:8080" in line


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
    assert r.status_code == 200 and r.json() == {"ok": True, "tools": len(client.gw.tools)}


@pytest.mark.parametrize("bearer", [None, "not-a-jwt"])
def test_ephemeral_server_refuses_mcp_without_a_valid_bearer(client, bearer):
    h = dict(HEADERS)
    if bearer:
        h["Authorization"] = f"Bearer {bearer}"
    body = {"jsonrpc": "2.0", "id": 1, "method": "tools/call",
            "params": {"name": "session_open", "arguments": {"agent": "coding-agent"}}}
    r = client.post("/mcp", headers=h, content=json.dumps(body))
    assert r.status_code == 401
    assert client.gw.audit.records == [], "nothing reached the gateway"


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


def test_github_prs_register_repo_pr_open_only():
    from autopilot.github_pr import GitHubPRs
    c = serve.config_from_env({**EPH, "GITHUB_PR_ENABLED": "1", "AIRFRAME_ENABLED": "0"})
    b = serve.build_backends(c)
    assert isinstance(b.git, GitHubPRs) and str(b.git.token_file) == "/var/run/clearance/agent-installation-token"
    h = serve.build_handlers(b)
    assert "repo.pr.open" in h and not {"repo.pr.open_upper", "xr.request"} & set(h)


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


def test_provenance_verifiers_and_agent_logins():
    c = serve.config_from_env(EPH)
    assert c.verifiers == () and c.agent_logins == (serve.AGENT_LOGIN,)
    c = serve.config_from_env({**EPH, "CLEARANCE_VERIFIERS": "app-*-cicd/pipeline-runner, gitops-*/pipeline-runner",
                               "AGENT_LOGINS": "a[bot],b[bot]"})
    assert c.verifiers == ("app-*-cicd/pipeline-runner", "gitops-*/pipeline-runner")
    assert c.agent_logins == ("a[bot]", "b[bot]")
    gw, _ = serve.build_gateway(c)
    assert gw.verifiers == c.verifiers and gw.agent_logins == c.agent_logins
