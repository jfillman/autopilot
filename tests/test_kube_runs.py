"""kube_runs: the AgentRun store over the Kubernetes API. Two requests only; a 404 delete is success, a 409 create
is an error; anything outside the allowlist is refused before a request exists. No network: a fake opener."""
import json

import pytest

from autopilot.kube_runs import COLLECTION, KubeAgentRuns, KubeRunError, RunRefused, allowed

MANIFEST = {"apiVersion": "catalog.hangar.io/v1alpha1", "kind": "AgentRun",
            "metadata": {"name": "r-00002a", "namespace": "autopilot-runs"}, "spec": {"agent": "coding-agent"}}


class Opener:
    def __init__(self, *answers):
        self.answers, self.reqs = list(answers), []

    def __call__(self, req, timeout, ctx):
        self.reqs.append(req)
        return self.answers.pop(0)


@pytest.fixture
def token(tmp_path):
    f = tmp_path / "token"
    f.write_text("tok-1\n")
    return f


def store(token, opener):
    return KubeAgentRuns("https://api.example:6443/", token, None, opener=opener)


def test_create_posts_the_manifest_to_the_run_namespace(token):
    op = Opener((201, json.dumps({"metadata": {"name": "r-00002a"}}).encode()))
    out = store(token, op).create(MANIFEST)
    req = op.reqs[0]
    assert out["metadata"]["name"] == "r-00002a"
    assert req.get_method() == "POST"
    assert req.full_url == "https://api.example:6443/apis/catalog.hangar.io/v1alpha1/namespaces/autopilot-runs/agentruns"
    assert req.get_header("Authorization") == "Bearer tok-1"
    assert json.loads(req.data) == MANIFEST


def test_token_is_reread_every_call(token):
    op = Opener((201, b"{}"), (200, b"{}"))
    s = store(token, op)
    s.create(MANIFEST)
    token.write_text("tok-2")
    s.delete("r-00002a")
    assert [r.get_header("Authorization") for r in op.reqs] == ["Bearer tok-1", "Bearer tok-2"]


def test_create_conflict_is_an_error(token):
    with pytest.raises(KubeRunError, match="already exists"):
        store(token, Opener((409, b"{}"))).create(MANIFEST)


@pytest.mark.parametrize("status", [401, 403, 422, 500])
def test_create_failure_is_an_error(token, status):
    with pytest.raises(KubeRunError):
        store(token, Opener((status, b""))).create(MANIFEST)


def test_unreachable_is_an_error(token):
    def boom(req, timeout, ctx):
        raise ConnectionRefusedError()
    with pytest.raises(KubeRunError, match="unreachable"):
        store(token, boom).create(MANIFEST)


@pytest.mark.parametrize("status", [200, 202, 404])
def test_delete_success_including_already_gone(token, status):
    op = Opener((status, b""))
    store(token, op).delete("r-00002a")
    assert op.reqs[0].get_method() == "DELETE"
    assert op.reqs[0].full_url.endswith(f"{COLLECTION}/r-00002a")


@pytest.mark.parametrize("status", [403, 409, 500])
def test_delete_failure_is_an_error(token, status):
    with pytest.raises(KubeRunError):
        store(token, Opener((status, b""))).delete("r-00002a")


@pytest.mark.parametrize("change", [
    {"kind": "Pod"}, {"apiVersion": "v1"},
    {"metadata": {"name": "r-1", "namespace": "kube-system"}},
    {"metadata": {"name": "../secrets", "namespace": "autopilot-runs"}},
    {"metadata": {"name": "R-UPPER", "namespace": "autopilot-runs"}},
    {"metadata": {"namespace": "autopilot-runs"}},
])
def test_create_refuses_anything_but_an_agentrun_in_autopilot_runs(token, change):
    op = Opener()
    with pytest.raises(RunRefused):
        store(token, op).create({**MANIFEST, **change})
    assert op.reqs == [], "refused before any request"


@pytest.mark.parametrize("name", ["", "../../namespaces/x", "a/b", "r-1/status", "x" * 64, "R1", None])
def test_delete_refuses_a_name_that_is_not_a_dns_label(token, name):
    op = Opener()
    with pytest.raises(RunRefused):
        store(token, op).delete(name)
    assert op.reqs == []


def test_allowlist_is_exactly_two_requests():
    assert allowed("POST", COLLECTION)
    assert allowed("DELETE", COLLECTION + "/r-1")
    for method, path in [("GET", COLLECTION), ("GET", COLLECTION + "/r-1"), ("PATCH", COLLECTION + "/r-1"),
                         ("PUT", COLLECTION + "/r-1"), ("DELETE", COLLECTION), ("POST", COLLECTION + "/r-1"),
                         ("DELETE", COLLECTION + "/r-1/status"), ("POST", "/api/v1/namespaces/autopilot-runs/secrets"),
                         ("DELETE", "/apis/catalog.hangar.io/v1alpha1/namespaces/other/agentruns/r-1")]:
        assert not allowed(method, path), (method, path)
