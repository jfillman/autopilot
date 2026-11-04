"""xr.request: one new XR request file in tenants/<app>/xr-requests/ of the dev cluster's tenants repo, opened as a PR
by the agents' App. The file is built here (kind, namespace, annotations, path), never taken from the agent; only
requestable kinds and lower environments; nothing overwritten. No network: test_github_pr's fake opener."""
import json

import pytest
import yaml

from autopilot import airframe_plan, xr_request
from autopilot.airframe_tools import CONTRACT
from autopilot.engine.dispatch import Denied
from autopilot.github_pr import GitHubError, GitHubPRs
from test_github_pr import GH, META

REG = airframe_plan.Registry("hangar-dev", ("hangar-prod",), "gitops-hangar-dev-tenants")
TENANTS = "repos/jfillman/gitops-hangar-dev-tenants"
APP_DIR = ("GET", f"{TENANTS}/contents/tenants/flight-api?ref=main")
SPEC = {"lockSeconds": 60}


def file_key(name):
    return ("GET", f"{TENANTS}/contents/tenants/flight-api/xr-requests/{name}.yaml?ref=main")


@pytest.fixture
def token(tmp_path):
    f = tmp_path / "t"
    f.write_text("sa-1\n")
    return f


def gh(over=None):
    base = {APP_DIR: (200, b"[]"), file_key("orders"): (404, b"{}"),
            file_key("flight-api-dev-messagequeue"): (404, b"{}"),
            file_key("flight-api-hangar-dev-dev"): (404, b"{}"),
            ("POST", f"{TENANTS}/pulls"): (201, b'{"number": 3, "html_url": "https://github.com/x/pull/3"}')}
    return GH({**base, **(over or {})})


def prs(token, g, registry=REG):
    return GitHubPRs(token, broker_url="http://broker", api="https://api.github.test", opener=g, registry=registry,
                     contract=CONTRACT)


def test_requestable_kinds_are_dependencies_observability_and_environments_only():
    kinds = xr_request.requestable_kinds(CONTRACT)
    assert {"MessageQueue", "ObjectStorage", "ManagedPostgreSQL", "SLO", "RolloutWatch", "ApplicationEnvironment"} <= set(kinds)
    assert not {"Agent", "AgentRun", "SecretStore", "TektonCICD", "NodeJSApplication", "AgentRepository"} & set(kinds)


def test_a_dependency_request_is_one_new_file_opened_as_a_pr_on_the_tenants_repo(token):
    g = gh()
    out = prs(token, g).commit_xr("flight-api", "dev", "MessageQueue", SPEC, "orders", meta=META)
    assert out["path"] == "tenants/flight-api/xr-requests/orders.yaml" and out["pr"] == 3
    assert out["namespace"] == "app-flight-api-cicd" and out["kind"] == "MessageQueue"
    broker = g.reqs[0]
    # one token, for the tenants repo, asked for the app the request is for
    assert broker[3] == {"owner": "jfillman", "repo": "gitops-hangar-dev-tenants", "app": "flight-api"}
    assert sum(1 for r in g.reqs if r[1].endswith("/agent-installation-token")) == 1
    tree = next(r[3] for r in g.reqs if r[1].endswith("/git/trees"))
    [entry] = tree["tree"]
    assert entry["path"] == "tenants/flight-api/xr-requests/orders.yaml"
    xr = yaml.safe_load(entry["content"])
    assert xr["apiVersion"] == "catalog.hangar.io/v1alpha1" and xr["kind"] == "MessageQueue"
    assert xr["metadata"]["name"] == "orders" and xr["metadata"]["namespace"] == "app-flight-api-cicd"
    assert xr["metadata"]["annotations"]["terasky.backstage.io/source-file-url"].endswith(
        "/gitops-hangar-dev-tenants/blob/main/tenants/flight-api/xr-requests/orders.yaml")
    assert xr["spec"] == SPEC
    ref = next(r[3] for r in g.reqs if r[1].endswith("/git/refs"))
    assert ref["ref"] == "refs/heads/agent/t-abc123-xr-orders"
    commit = next(r[3] for r in g.reqs if r[1].endswith("/git/commits") and r[0] == "POST")
    assert "Hangar-Task: t-abc123" in commit["message"] and "Hangar-Agent: triage-agent" in commit["message"]


def test_a_default_name_and_an_application_environment_named_as_airframes_planner_names_it(token):
    g = gh()
    assert prs(token, g).commit_xr("flight-api", "dev", "MessageQueue", SPEC)["name"] == "flight-api-dev-messagequeue"
    g = gh()
    out = prs(token, g).commit_xr("flight-api", "dev", "ApplicationEnvironment", {"configMapGenerator": False})
    assert out["name"] == "flight-api-hangar-dev-dev"
    xr = yaml.safe_load(next(r[3] for r in g.reqs if r[1].endswith("/git/trees"))["tree"][0]["content"])
    assert xr["spec"] == {"appName": "flight-api", "cluster": "hangar-dev", "env": "dev", "configMapGenerator": False}


@pytest.mark.parametrize("app,env,kind,spec,name,rule", [
    ("flight-api", "prod", "MessageQueue", SPEC, None, "XR-ENV"),
    ("flight-api", "staging", "MessageQueue", SPEC, None, "XR-ENV"),
    ("Flight", "dev", "MessageQueue", SPEC, None, "XR-APP"),
    ("../x", "dev", "MessageQueue", SPEC, None, "XR-APP"),
    ("flight-api", "dev", "Agent", SPEC, None, "XR-KIND"),
    ("flight-api", "dev", "AgentRun", SPEC, None, "XR-KIND"),
    ("flight-api", "dev", "SecretStore", SPEC, None, "XR-KIND"),
    ("flight-api", "dev", "NodeJSApplication", SPEC, None, "XR-KIND"),
    ("flight-api", "dev", "Secret", SPEC, None, "XR-KIND"),
    ("flight-api", "dev", "MessageQueue", "lockSeconds: 1", None, "XR-SPEC"),
    ("flight-api", "dev", "MessageQueue", {"metadata": {"namespace": "kube-system"}}, None, "XR-SPEC"),
    ("flight-api", "dev", "MessageQueue", {"x": "y" * (xr_request.MAX_SPEC_BYTES + 1)}, None, "XR-SPEC"),
    ("flight-api", "dev", "MessageQueue", SPEC, "../../etc", "XR-NAME"),
    ("flight-api", "dev", "MessageQueue", SPEC, "Orders", "XR-NAME"),
    ("flight-api", "dev", "ApplicationEnvironment", {"cluster": "hangar-prod"}, None, "XR-SPEC"),
    ("flight-api", "dev", "ApplicationEnvironment", {}, "other", "XR-NAME"),
])
def test_bad_requests_are_refused_before_any_request(token, app, env, kind, spec, name, rule):
    g = gh()
    with pytest.raises(Denied) as e:
        prs(token, g).commit_xr(app, env, kind, spec, name, meta=META)
    assert e.value.rule == rule and e.value.hint
    assert g.reqs == []


def test_an_existing_request_is_never_overwritten(token):
    g = gh({file_key("orders"): (200, json.dumps({"type": "file", "sha": "x"}).encode())})
    with pytest.raises(Denied) as e:
        prs(token, g).commit_xr("flight-api", "dev", "MessageQueue", SPEC, "orders", meta=META)
    assert e.value.rule == "XR-EXISTS"
    assert not any(r[0] == "POST" and "/git/" in r[1] for r in g.reqs)


def test_an_app_that_is_not_onboarded_is_refused(token):
    g = gh({APP_DIR: (404, b"{}")})
    with pytest.raises(Denied) as e:
        prs(token, g).commit_xr("flight-api", "dev", "MessageQueue", SPEC, "orders")
    assert e.value.rule == "XR-APP-UNKNOWN"


def test_a_broker_that_refuses_the_tenants_repo_is_said_plainly(token):
    g = gh({("POST", "agent-installation-token"): (403, b'{"error": "not an app repo"}')})
    with pytest.raises(Denied) as e:
        prs(token, g).commit_xr("flight-api", "dev", "MessageQueue", SPEC, "orders")
    assert e.value.rule == "XR-TENANTS-TOKEN" and "agentToken" in e.value.hint


def test_github_failing_is_a_hidden_backend_error(token):
    g = gh({APP_DIR: (500, b"{}")})
    with pytest.raises(GitHubError):
        prs(token, g).commit_xr("flight-api", "dev", "MessageQueue", SPEC, "orders")


def test_without_a_registry_there_is_no_xr_request(token):
    with pytest.raises(NotImplementedError):
        prs(token, gh(), registry=None).commit_xr("flight-api", "dev", "MessageQueue", SPEC)


def test_xr_request_through_the_gateway_reaches_the_backend_for_a_lower_env_only(gw, backends):
    """The bundled coding-agent holds xr.request (T1, lower): the core allows dev and refuses prod before any backend."""
    sid = gw.open_session("alice", "coding-agent").data["session"]
    r = gw.call("alice", sid, "xr.request", {"app": "flight-api", "env": "dev", "kind": "MessageQueue", "spec": SPEC})
    assert r.ok, r.as_dict()
    assert backends.git.names() == ["git.commit_xr"]
    r = gw.call("alice", sid, "xr.request", {"app": "flight-api", "env": "prod", "kind": "MessageQueue", "spec": SPEC})
    assert not r.ok and backends.git.names() == ["git.commit_xr"]
