"""cicd.yaml field scope (policy R021): Glidepath's GP-SCOPE rules on an agent's pull request.

Mirrors glidepath's tools/test_glidepath_validate.py `Scope` cases, but through Clearance's real
call path (Gateway.call -> policy -> backend), so an allowed change reaches the git backend and a
denied one never does. The vendored files are checked against a glidepath checkout when there is one.
"""
import copy
import os
from pathlib import Path

import pytest
import yaml

from clearance import airframe_plan as ap
from clearance import policy

ROOT = Path(__file__).resolve().parents[1]
VENDOR = ROOT / "src" / "clearance" / "vendor"
# CI clones glidepath under HANGAR_TECH (.github/workflows/test.yaml, GLIDEPATH_REF); locally it is ~/tech.
GLIDEPATH = Path(os.environ.get("HANGAR_TECH", "/Users/jerf/tech")) / "glidepath"
VENDORED = {"cicd_scope.py": "tools/cicd_scope.py", "glidepath-contract.json": "contract/glidepath-contract.json"}

# glidepath's Scope BASE, verbatim
BASE = yaml.safe_load("""
apiVersion: platform/v1
kind: PipelineConfig
build: {agent: nodejs-20, cache: {enabled: false, size: small}}
deploy:
  environments:
    - {name: dev, tier: ground}
    - {name: int, tier: ground}
    - {name: prod, tier: flight, cluster: hangar-prod, production: true}
governance: {sast: true, imageScan: true, policyCheck: true, sbom: false, githubMerge: {mode: merger, requiredApprovals: 1}}
secrets: [{name: NPM_TOKEN, key: npm-token}]
pipelines:
  ci:
    trigger: {source: git, event: push, branch: main}
    steps:
      - {stage: build}
      - {stage: deploy, env: dev}
      - {stage: test, env: dev}
      - {stage: test, env: dev, role: check-ride}
      - {stage: release, env: prod}
      - {stage: gitops-image-bump, gitopsRepo: gitops-x, manifestPath: a/b.yaml}
""")

PR = {"repo": "jfillman/flight-api", "branch": "agent/t-1", "title": "cicd change"}


# ---- drift ----------------------------------------------------------------------------------
@pytest.mark.parametrize("vendored,upstream", sorted(VENDORED.items()))
def test_vendored_file_matches_glidepath(vendored, upstream):
    src = GLIDEPATH / upstream
    if not src.exists():
        pytest.skip(f"no glidepath checkout with {upstream} at {GLIDEPATH} (CI clones it; set HANGAR_TECH)")
    assert (VENDOR / vendored).read_bytes() == src.read_bytes(), (
        f"src/clearance/vendor/{vendored} drifted from glidepath's {upstream} - copy it over byte for byte")


def test_policy_reads_the_vendored_contracts_scope_block():
    rules = policy.CICD_SCOPE["rules"]
    assert {r["agentScope"] for r in rules} == {"open", "tighten", "human"}
    assert any(r["pattern"] == "/secrets" and r["agentScope"] == "human" for r in rules)


# ---- through the gateway --------------------------------------------------------------------
def open_(gw):
    r = gw.open_session("alice", "coding-agent", None)
    assert r.ok, r.as_dict()
    return r.data["session"]


def call(gw, files):
    return gw.call("alice", open_(gw), "repo.pr.open", {**PR, "files": files})


def edit(fn):
    doc = copy.deepcopy(BASE)
    fn(doc)
    return doc


def pr_cicd(gw, fn, path="cicd.yaml"):
    return call(gw, [{"path": path, "before": yaml.safe_dump(BASE), "content": yaml.safe_dump(edit(fn))}])


def rules_of(r):
    return sorted({p["rule"] for p in r.decision.get("problems", [])})


@pytest.mark.parametrize("fn", [
    lambda d: d["build"]["cache"].update(enabled=True, size="large"),                          # open field
    lambda d: d["deploy"]["environments"].insert(2, {"name": "qa", "tier": "ground"}),          # ground env added
    lambda d: d["deploy"]["environments"].reverse(),                                            # reorder is not a change
    lambda d: d["pipelines"]["ci"]["steps"].insert(1, {"stage": "test", "env": "dev", "testName": "smoke"}),
    lambda d: d["governance"].update(sbom=True),                                                # gate turned on
    lambda d: d["governance"]["githubMerge"].update(mode="strict", requiredApprovals=2),       # tightened
], ids=["open-field", "ground-env", "env-reorder", "step-insert", "gate-on", "merge-tightened"])
def test_allowed_cicd_change_reaches_the_backend(gw, backends, fn):
    r = pr_cicd(gw, fn)
    assert r.ok, r.as_dict()
    assert backends.git.names() == ["git.open_pr"]


@pytest.mark.parametrize("fn,rule", [
    (lambda d: d["governance"].update(sast=False), "GP-SCOPE-002"),
    (lambda d: d["governance"].pop("imageScan"), "GP-SCOPE-002"),
    (lambda d: d["governance"]["githubMerge"].update(mode=""), "GP-SCOPE-002"),
    (lambda d: d["governance"]["githubMerge"].update(requiredApprovals=0), "GP-SCOPE-002"),
    (lambda d: d["secrets"].append({"name": "AWS", "key": "aws"}), "GP-SCOPE-001"),
    (lambda d: d["governance"].update(allowedCommitSigners=["agent@x"]), "GP-SCOPE-001"),
    (lambda d: d["pipelines"]["ci"]["steps"][5].update(gitopsRepo="gitops-evil"), "GP-SCOPE-001"),
    (lambda d: d["deploy"]["environments"].append({"name": "staging", "tier": "flight"}), "GP-SCOPE-003"),
    (lambda d: d["deploy"]["environments"][1].update(tier="flight"), "GP-SCOPE-003"),
    (lambda d: d["pipelines"]["ci"]["steps"].pop(3), "GP-SCOPE-002"),                          # check ride removed
    (lambda d: d["pipelines"]["ci"]["steps"][2].update(required=False), "GP-SCOPE-002"),       # test made optional
], ids=["sast-off", "imagescan-removed", "merge-mode-loosened", "approvals-lowered", "secrets", "signers",
        "gitops-repo", "flight-env-added", "tier-changed", "check-ride-removed", "test-optional"])
def test_denied_cicd_change_carries_the_gp_scope_rule_and_hint(gw, backends, fn, rule):
    r = pr_cicd(gw, fn)
    assert not r.ok
    assert r.decision["rule"] == rule and r.decision["name"] == "cicd-scope"
    assert rules_of(r) == [rule]
    assert r.decision["hint"] and r.decision["hint"] == r.decision["problems"][0]["hint"]
    assert backends.git.calls == []
    last = gw.audit.records[-1]
    assert last["decision"] == "deny" and last["rule"] == rule


def test_a_nested_cicd_yaml_is_scoped_too(gw):
    r = pr_cicd(gw, lambda d: d["governance"].update(sast=False), path="apps/x/cicd.yaml")
    assert r.decision["rule"] == "GP-SCOPE-002"
    assert r.decision["problems"][0]["message"].startswith("apps/x/cicd.yaml: ")


def test_every_problem_is_returned_not_just_the_first(gw):
    r = pr_cicd(gw, lambda d: (d["governance"].update(sast=False), d["secrets"].clear()))
    assert rules_of(r) == ["GP-SCOPE-001", "GP-SCOPE-002"]


def test_a_new_cicd_yaml_is_a_human_change(gw):
    # No `before`: a new file. Every field is added, and apiVersion/kind/secrets are human fields,
    # so leaving `before` out never lets more through than sending it.
    r = call(gw, [{"path": "cicd.yaml", "content": yaml.safe_dump(BASE)}])
    assert r.decision["rule"] == "GP-SCOPE-001"


@pytest.mark.parametrize("content", ["a: [b", "just a string", "- a list"])
def test_a_cicd_yaml_that_is_not_a_mapping_fails_closed(gw, content):
    r = call(gw, [{"path": "cicd.yaml", "before": yaml.safe_dump(BASE), "content": content}])
    assert not r.ok and r.decision["rule"] == "GP-SCOPE-001"


def test_merge_patch_is_applied_to_the_base_before_the_check(gw, backends):
    before = yaml.safe_dump(BASE)
    ok = call(gw, [{"path": "cicd.yaml", "op": "merge-patch", "before": before,
                    "content": {"build": {"cache": {"enabled": True}}}}])
    assert ok.ok, ok.as_dict()
    # null deletes in a merge patch: removing a tighten-only gate falls back to its loose default
    bad = call(gw, [{"path": "cicd.yaml", "op": "merge-patch", "before": before, "content": {"governance": {"sast": None}}}])
    assert bad.decision["rule"] == "GP-SCOPE-002"


def test_tekton_is_still_denied_whole_by_path(gw, backends):
    r = call(gw, [{"path": ".tekton/pr.yaml", "content": "x"},
                  {"path": "cicd.yaml", "before": yaml.safe_dump(BASE), "content": yaml.safe_dump(BASE)}])
    assert r.decision["rule"] == "R015"
    assert backends.git.calls == []


def test_an_unchanged_cicd_yaml_alongside_other_files_is_allowed(gw):
    r = call(gw, [{"path": "charts/values.yaml", "content": "x"},
                  {"path": "cicd.yaml", "before": yaml.safe_dump(BASE), "content": yaml.safe_dump(BASE)}])
    assert r.ok, r.as_dict()


def test_merge_patch_rfc7386():
    assert policy.merge_patch({"a": {"b": 1, "c": 2}, "l": [1]}, {"a": {"b": None, "d": 3}, "l": [2]}) == \
        {"a": {"c": 2, "d": 3}, "l": [2]}


# ---- the planner's app-repo step (the gap the Autopilot design recorded) ---------------------
SCAFFOLD_CICD = yaml.safe_load("""
apiVersion: platform/v1
kind: PipelineConfig
build: {agent: python-3.12, unitTest: {enabled: false}}
pipelines:
  ci:
    trigger: {source: git, event: push, branch: main}
    steps: [{stage: build}, {stage: deploy, env: dev}]
deploy:
  environments: [{name: dev, tier: ground}]
  strategy: rollout
""")
REG = ap.Registry("kind-dev", ("kind-prod",), "gitops-cluster-dev-tenants")
SPEC = {"apiVersion": "airframe/v1", "kind": "AppSpec", "app": "parachute", "stack": "python",
        "description": "Parachute service", "environments": {"ground": ["dev", "test"], "flight": []}}


def planner_cicd_file(spec, base):
    fc = next(f for f in ap.plan(spec, REG, base_cicd=base).step("app-repo").files if f.path == "cicd.yaml")
    assert fc.op == "merge-patch"
    return {"path": fc.path, "op": fc.op, "before": yaml.safe_dump(base), "content": fc.content}


def planner_call(gw, spec, base):
    return gw.call("alice", open_(gw), "repo.pr.open",
                   {"repo": "jfillman/parachute", "branch": "agent/plan", "title": "plan",
                    "files": [planner_cicd_file(spec, base)]})


def test_planner_app_repo_cicd_patch_with_ground_envs_is_allowed(gw, backends):
    r = planner_call(gw, SPEC, SCAFFOLD_CICD)
    assert r.ok, r.as_dict()
    assert backends.git.names() == ["git.open_pr"]


def test_planner_cicd_patch_that_drops_a_check_ride_is_denied(gw):
    # patch_cicd rewrites pipelines.ci.steps whole, so a check ride already in the file is lost.
    base = copy.deepcopy(SCAFFOLD_CICD)
    base["pipelines"]["ci"]["steps"].append({"stage": "test", "env": "dev", "role": "check-ride"})
    r = planner_call(gw, SPEC, base)
    assert r.decision["rule"] == "GP-SCOPE-002"


def test_planner_cicd_patch_with_flight_envs_is_denied(gw):
    spec = {**SPEC, "environments": {"ground": ["dev"], "flight": [{"name": "staging", "cluster": "kind-prod"}]}}
    r = planner_call(gw, spec, SCAFFOLD_CICD)
    assert not r.ok and "GP-SCOPE-003" in rules_of(r)
