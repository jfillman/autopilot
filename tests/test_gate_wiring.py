"""The deployment gate on the deployed gateway: an ephemeral gateway keeps changes and classifies them, and the
catalog answers an app's owner group (BackstageAppOwners). No network."""
import json

from clearance.dispatch import Principal
from clearance.tiers import Tier

from autopilot import serve
from autopilot.backends import Backends, FakeRuns
from autopilot.reads import BackstageAppOwners, BackstageCatalog

EPH = {"CLEARANCE_EPHEMERAL": "1", "AIRFRAME_ENABLED": "0", "ARGO_ENABLED": "0",
       "CLEARANCE_VERIFIERS": "app-*-cicd/pipeline-runner"}


class Auth:
    def authenticate(self, token):
        return Principal(token, "workload", Tier.T1)


def test_the_gateway_keeps_and_classifies_changes(clock):
    g, _ = serve.build_gateway(serve.config_from_env(EPH), backends=Backends(None, None, FakeRuns(), None, None),
                               auth=Auth(), clock=clock)
    out = g.register_change("app-gate-api-cicd/pipeline-runner", {
        "app": "gate-api", "env": "prod", "cluster": "hangar-prod", "repo": "jfillman/gitops-gate-api", "pr": 3,
        "head": "a" * 40, "proposer": "user:dana", "owner": "group:default/team-gate", "treeDigest": "c" * 64,
        "objects": [{"apiVersion": "v1", "kind": "Secret", "namespace": "n", "name": "s", "op": "changed", "fields": ["/data/x"]}],
        "checks": {"ci": "pass"}})
    assert out["state"] == "holding" and out["tier"] == 3 and g.gate_policy.ttl_days == 7


def test_app_owners_from_the_catalog(tmp_path):
    tok = tmp_path / "t"; tok.write_text("x")
    answers = {"gate-api": {"spec": {"owner": "group:default/team-gate"}}, "bare": {"spec": {"owner": "skyport"}},
               "person": {"spec": {"owner": "user:default/dana"}}}
    calls = []

    def opener(req, timeout):
        name = req.full_url.rsplit("/", 1)[-1]
        calls.append(name)
        return (200, json.dumps(answers[name]).encode()) if name in answers else (404, b"{}")

    o = BackstageAppOwners(BackstageCatalog("http://bs", tok, opener=opener))
    assert o.owner_of("gate-api") == "group:default/team-gate"
    assert o.owner_of("bare") == "group:default/skyport"
    assert o.owner_of("person") is None and o.owner_of("missing") is None and o.owner_of("Bad Name!") is None
    o.owner_of("gate-api")
    assert calls.count("gate-api") == 1
