"""reads: argo.app.get over the Application objects, metrics.query on Prometheus, logs.query on Loki. Summaries and
bounds, a rejected query as an answer, everything else a hidden failure. No network: fake openers."""
import json

import pytest

from autopilot import reads, serve
from autopilot.airframe_api import KubeReader, PathRefused
from autopilot.backends import Backends
from autopilot.kube_runs import KubeAgentRuns
from autopilot.reads import KubeArgo, LokiAPI, PrometheusAPI, ReadError, Reads
from clearance.dispatch import Denied, Principal
from clearance.tiers import Tier


class Auth:
    def authenticate(self, token):
        return Principal("user:alice", "user", Tier.T1)

APP = {"metadata": {"name": "flight-api-dev", "namespace": "argocd"},
       "spec": {"project": "flight-api", "destination": {"server": "https://kubernetes.default.svc", "namespace": "app-flight-api-dev"},
                "source": {"repoURL": "https://github.com/jfillman/gitops-flight-api", "path": "envs/dev", "targetRevision": "main",
                           "helm": {"valuesObject": {"dbPassword": "hunter2"}}}},
       "status": {"sync": {"status": "OutOfSync", "revision": "abc123"},
                  "health": {"status": "Degraded", "message": "x" * 900},
                  "operationState": {"phase": "Failed", "message": "one or more objects failed", "startedAt": "t0", "finishedAt": "t1"},
                  "conditions": [{"type": "SyncError", "message": "boom"}],
                  "resources": [{"kind": "Deployment", "namespace": "app-flight-api-dev", "name": "flight-api", "status": "Synced", "health": {"status": "Degraded", "message": "progress deadline"}},
                                {"kind": "Service", "namespace": "app-flight-api-dev", "name": "flight-api", "status": "Synced", "health": {"status": "Healthy"}},
                                {"kind": "ConfigMap", "namespace": "app-flight-api-dev", "name": "cfg", "status": "OutOfSync"}],
                  "reconciledAt": "t2"}}


class Kube:
    def __init__(self, objs):
        self.objs, self.paths = objs, []

    def __call__(self, req, timeout, ctx):
        path = req.full_url.removeprefix("https://api")
        self.paths.append(path)
        return (200, json.dumps(self.objs[path]).encode()) if path in self.objs else (404, b"{}")


@pytest.fixture
def token(tmp_path):
    f = tmp_path / "token"
    f.write_text("t")
    return f


def argo(token, objs, namespaces=("argocd", "argocd-apps")):
    k = Kube(objs)
    return KubeArgo(KubeReader("https://api", token, None, opener=k), namespaces), k


def test_get_app_summarises_and_leaves_out_the_helm_values(token):
    a, k = argo(token, {"/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/flight-api-dev": APP})
    out = a.get_app("flight-api-dev")
    assert "hunter2" not in json.dumps(out) and "helm" not in json.dumps(out)
    assert out["sources"] == [{"repoURL": "https://github.com/jfillman/gitops-flight-api", "path": "envs/dev", "targetRevision": "main"}]
    assert out["sync"]["status"] == "OutOfSync" and out["health"]["status"] == "Degraded"
    assert len(out["health"]["message"]) < 600 and out["operation"]["phase"] == "Failed"
    assert [r["name"] for r in out["unhealthyResources"]] == ["flight-api", "cfg"]
    assert k.paths == ["/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/flight-api-dev"]


def test_get_app_searches_the_namespaces_in_order(token):
    obj = {**APP, "metadata": {"name": "boarding-api", "namespace": "argocd-apps"}}
    a, k = argo(token, {"/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications/boarding-api": obj})
    assert a.get_app("boarding-api")["namespace"] == "argocd-apps"
    assert [p.split("/")[5] for p in k.paths] == ["argocd", "argocd-apps"]


def test_get_app_not_found_is_an_answer(token):
    a, _ = argo(token, {})
    assert a.get_app("nope") == {"app": "nope", "found": False, "searched": ["argocd", "argocd-apps"]}


@pytest.mark.parametrize("name", ["../secrets", "Flight", "a/b", "", "x" * 300])
def test_get_app_refuses_a_name_before_any_request(token, name):
    a, k = argo(token, {})
    with pytest.raises(Denied):
        a.get_app(name)
    assert k.paths == []


def test_argo_reads_stay_on_the_allowlist(token):
    a, k = argo(token, {}, namespaces=("argocd", "kube-system"))
    a.get_app("x")
    assert all(p.startswith("/apis/argoproj.io/v1alpha1/namespaces/") and "/applications/" in p for p in k.paths)
    with pytest.raises(PathRefused):
        a.kube.get("/api/v1/namespaces/argocd/secrets")


class HTTP:
    def __init__(self, status, body):
        self.status, self.body, self.reqs = status, body, []

    def __call__(self, req, timeout):
        self.reqs.append(req)
        return self.status, self.body if isinstance(self.body, bytes) else json.dumps(self.body).encode()


def test_promql_is_an_instant_get_and_bounded():
    series = [{"metric": {"i": str(i)}, "value": [1, "1"]} for i in range(reads.MAX_SERIES + 5)]
    op = HTTP(200, {"status": "success", "data": {"resultType": "vector", "result": series}})
    out = PrometheusAPI("http://thanos:9090/", opener=op).promql('up{job="x"}')
    req = op.reqs[0]
    assert req.get_method() == "GET" and req.full_url.startswith("http://thanos:9090/api/v1/query?")
    assert "query=up%7Bjob%3D%22x%22%7D" in req.full_url and req.get_header("Authorization") is None
    assert len(out["result"]) == reads.MAX_SERIES and out["series"] == reads.MAX_SERIES + 5 and out["truncated"]


def test_promql_scalar_passes_through():
    op = HTTP(200, {"status": "success", "data": {"resultType": "scalar", "result": [1, "2"]}})
    assert PrometheusAPI("http://p", opener=op).promql("1+1") == {"resultType": "scalar", "result": [1, "2"], "truncated": False}


@pytest.mark.parametrize("status, body", [(400, {"status": "error", "errorType": "bad_data", "error": "parse error"}),
                                          (422, {"status": "error", "errorType": "execution", "error": "too many samples"}),
                                          (503, {"status": "error", "errorType": "timeout", "error": "query timed out"})])
def test_a_rejected_query_is_an_answer(status, body):
    out = PrometheusAPI("http://p", opener=HTTP(status, body)).promql("up{")
    assert out == {"error": body["errorType"], "message": body["error"]}


@pytest.mark.parametrize("status, body", [(500, b"oops"), (503, b"down"), (401, b""), (200, b"not json")])
def test_anything_else_is_a_hidden_failure(status, body):
    with pytest.raises(ReadError):
        PrometheusAPI("http://p", opener=HTTP(status, body)).promql("up")


def test_unreachable_is_a_hidden_failure():
    def boom(req, timeout):
        raise ConnectionRefusedError()
    with pytest.raises(ReadError, match="unreachable"):
        LokiAPI("http://l", opener=boom).logql('{app="x"}')


@pytest.mark.parametrize("q", ["", "   ", "x" * (reads.MAX_QUERY + 1), None])
def test_empty_or_huge_queries_are_refused_before_a_request(q):
    op = HTTP(200, {})
    with pytest.raises(Denied):
        PrometheusAPI("http://p", opener=op).promql(q)
    assert op.reqs == []


def test_logql_is_the_last_hour_newest_first_and_bounded():
    streams = [{"stream": {"pod": f"p{i}"}, "values": [[str(t), "l" * (reads.MAX_LINE + 10)] for t in range(150)]}
               for i in range(2)]
    op = HTTP(200, {"status": "success", "data": {"resultType": "streams", "result": streams}})
    out = LokiAPI("http://loki-gateway", opener=op).logql('{namespace="app-flight-api-dev"}')
    url = op.reqs[0].full_url
    assert url.startswith("http://loki-gateway/loki/api/v1/query_range?")
    assert "since=1h" in url and f"limit={reads.MAX_LINES + 1}" in url and "direction=backward" in url
    assert out["lines"] == reads.MAX_LINES and out["truncated"]
    assert [len(s["values"]) for s in out["result"]] == [150, 50]
    assert len(out["result"][0]["values"][0][1]) == reads.MAX_LINE + 3


def test_logql_metric_query_is_a_matrix():
    op = HTTP(200, {"status": "success", "data": {"resultType": "matrix", "result": [{"metric": {}, "values": []}]}})
    assert LokiAPI("http://l", opener=op).logql('rate({app="x"}[5m])')["resultType"] == "matrix"


# -- wiring ---------------------------------------------------------------------------------------------------------
EPH = {"CLEARANCE_EPHEMERAL": "1", "AIRFRAME_ENABLED": "0"}


def test_read_tools_only_for_configured_backends():
    h = serve.build_handlers(serve.build_backends(serve.config_from_env({**EPH, "PROMETHEUS_URL": "http://p"})))
    assert {"argo.app.get", "metrics.query"} <= set(h)
    assert not {"logs.query", "catalog.read", "app.api.get", "argo.sync.lower"} & set(h)
    h = serve.build_handlers(serve.build_backends(serve.config_from_env({**EPH, "LOKI_URL": "http://l", "ARGO_ENABLED": "0"})))
    assert "logs.query" in h and not {"metrics.query", "argo.app.get", "argo.sync.lower"} & set(h)


def test_argo_needs_a_namespace():
    with pytest.raises(serve.ConfigError, match="ARGO_APP_NAMESPACES"):
        serve.config_from_env({**EPH, "ARGO_APP_NAMESPACES": " , "})
    c = serve.config_from_env({**EPH, "ARGO_APP_NAMESPACES": "argocd, argocd-apps"})
    assert c.argo_namespaces == ("argocd", "argocd-apps")


def test_read_calls_through_the_gateway_are_charged_and_audited(tmp_path, clock):
    token = tmp_path / "token"
    token.write_text("t")
    k = Kube({"/apis/argoproj.io/v1alpha1/namespaces/argocd/applications/flight-api-dev": APP})
    prom = HTTP(200, {"status": "success", "data": {"resultType": "vector", "result": []}})
    b = Backends(None, KubeArgo(KubeReader("https://api", token, None, opener=k)), KubeAgentRuns("https://api", token, None),
                 Reads(PrometheusAPI("http://p", opener=prom)), None)
    gw, _ = serve.build_gateway(serve.config_from_env(EPH), backends=b, auth=Auth(), clock=clock)
    sid = gw.open_session("t", "researcher").data["session"]
    r = gw.call("t", sid, "argo.app.get", {"app": "flight-api-dev"})
    assert r.ok and r.data["health"]["status"] == "Degraded"
    assert gw.call("t", sid, "metrics.query", {"promql": "up"}).ok
    r = gw.call("t", sid, "logs.query", {"logql": '{app="x"}'})
    assert not r.ok and r.error == "no backend for this tool in this deployment"
    assert [x["result"] for x in gw.audit.records[-3:]] == ["ok", "ok", "error:NoHandler"]
    assert gw.store.get(sid).used["tool_calls"] == 2


# -- catalog.read ---------------------------------------------------------------------------------------------------

def _catalog(tmp_path, status=200, doc=None):
    from autopilot.reads import BackstageCatalog
    sent = []

    def opener(req, timeout):
        sent.append(req)
        return status, json.dumps(doc if doc is not None else {"items": [], "totalItems": 0}).encode()
    tok = tmp_path / "backstage-token"
    tok.write_text("projected\n")
    return BackstageCatalog("http://backstage.test/", tok, opener=opener), sent


def test_catalog_searches_by_text_or_filter_with_the_projected_token(tmp_path):
    import urllib.parse
    entity = {"kind": "Component", "metadata": {"namespace": "default", "name": "gate-api", "title": "Gate API",
                                                 "description": "gates", "annotations": {"secret-ish": "x"}},
              "spec": {"type": "service", "owner": "group:default/skyport", "lifecycle": "production", "values": {}}}
    c, sent = _catalog(tmp_path, doc={"items": [entity], "totalItems": 1, "pageInfo": {}})
    r = c.catalog("gate")
    assert r == {"entities": [{"ref": "component:default/gate-api", "kind": "Component", "title": "Gate API",
                               "description": "gates", "type": "service", "owner": "group:default/skyport",
                               "lifecycle": "production"}], "totalItems": 1, "truncated": False}
    url = urllib.parse.urlsplit(sent[0].full_url)
    q = urllib.parse.parse_qs(url.query)
    assert url.path == "/api/catalog/entities/by-query" and q["fullTextFilterTerm"] == ["gate"] and "filter" not in q
    assert "metadata.annotations" not in q["fields"] and q["limit"] == ["25"]
    assert sent[0].headers["Authorization"] == "Bearer projected" and sent[0].get_method() == "GET"
    c.catalog("kind=component,spec.owner=group:default/skyport")
    q = urllib.parse.parse_qs(urllib.parse.urlsplit(sent[1].full_url).query)
    assert q["filter"] == ["kind=component,spec.owner=group:default/skyport"] and "fullTextFilterTerm" not in q


def test_catalog_refusals(tmp_path):
    from autopilot.reads import ReadError
    c, sent = _catalog(tmp_path)
    for bad in ("", "  ", "x" * 501):
        with pytest.raises(Denied):
            c.catalog(bad)
    assert sent == []
    with pytest.raises(Denied) as e:
        _catalog(tmp_path, status=403)[0].catalog("gate")
    assert e.value.rule == "READ-CATALOG-403"
    assert _catalog(tmp_path, status=400)[0].catalog("kind=")["error"] == "bad-query"
    with pytest.raises(ReadError):
        _catalog(tmp_path, status=500)[0].catalog("gate")
