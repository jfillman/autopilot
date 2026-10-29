"""airframe.* tools: Airframe's contract, validator and planner, and read-only cluster state (AF-8a)."""
import asyncio
import copy
import importlib.machinery
import importlib.util
import json
import os
import re
import shutil
import ssl
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

import pytest
import yaml

from autopilot import airframe_plan as ap
from autopilot import airframe_tools as at
from autopilot.airframe_api import AirframeError, KubeReader, PathRefused, path_allowed
from autopilot.backends import Backends, FakeArtifacts, FakeArgo, FakeGit, FakeHuman, FakeRead, FakeRuns
from autopilot.gateway import TOOLS, hangar_gateway
from clearance.dispatch import Denied
from clearance.packs import hangar
from clearance.server import create_server
from clearance.tiers import Tier

ROOT = Path(__file__).resolve().parents[1]
# CI clones airframe under HANGAR_TECH (.github/workflows/test.yaml); locally it is ~/tech.
AIRFRAME = Path(os.environ.get("HANGAR_TECH", "/Users/jerf/tech")) / "airframe"
REG = ap.Registry("kind-dev", ("kind-prod",), "gitops-cluster-dev-tenants")
AF = {"airframe.capabilities", "airframe.validate", "airframe.explain", "airframe.plan", "airframe.status",
      "airframe.describe", "airframe.verify"}


# ---- live-shaped objects (from hangar-dev, 2026-10-28: boarding-api) -------------------------------------------------
def cond(type_, status="True", reason=None, obs=None, message=None):
    c = {"type": type_, "status": status, "reason": reason or ("ReconcileSuccess" if type_ == "Synced" else "Available")}
    if obs is not None:
        c["observedGeneration"] = obs
    if message:
        c["message"] = message
    return c


def xr(kind, ns, name, spec=None, conditions=(), generation=3, labels=None):
    return {"apiVersion": "catalog.hangar.io/v1alpha1", "kind": kind,
            "metadata": {"name": name, "namespace": ns, "generation": generation, "labels": labels or {}},
            "spec": {"crossplane": {"resourceRefs": [{"kind": "Release", "name": name}]}, **(spec or {})},
            "status": {"conditions": list(conditions)}}


APP = xr("NodeJSApplication", "app-boarding-api-cicd", "boarding-api", {"devCluster": "hangar-dev"}, [
    cond("DevClusterReady", reason="DevClusterReady"), cond("CicdOnboarded", reason="CicdOnboarded"),
    cond("Synced", obs=7), cond("Ready", obs=7)], generation=7)
PROD = xr("ApplicationEnvironment", "app-boarding-api-cicd", "boarding-api-hangar-prod-prod",
          {"appName": "boarding-api", "cluster": "hangar-prod", "env": "prod"},
          [cond("ClusterReady", reason="ClusterEligible"), cond("AppResolved", reason="AppResolved"),
           cond("Synced", obs=6), cond("Ready", obs=6)], generation=6)
REDIS = xr("Redis", "app-boarding-api-dev", "redis", {"size": "small", "persistence": True},
           [cond("ComponentReady", reason="RedisReady"), cond("Synced", obs=3), cond("Ready", obs=3)],
           labels={"hangar.io/app": "boarding-api", "hangar.io/env": "dev"})
RABBIT = xr("RabbitMQ", "app-boarding-api-dev", "rabbitmq", {"mode": "attach"},
            [cond("ComponentReady", reason="RabbitMQAttachReady"), cond("Synced", obs=2), cond("Ready", obs=2)], generation=2)
ARGO_APP = {"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
            "metadata": {"name": "boarding-api-dev", "namespace": "argocd-apps",
                         "labels": {"hangar.io/app": "boarding-api", "hangar.io/env": "dev", "hangar.io/env-tier": "lower"}},
            "spec": {"project": "boarding-api-lower",
                     "destination": {"namespace": "app-boarding-api-dev", "server": "https://kubernetes.default.svc"},
                     "sources": [{"repoURL": "https://github.com/jfillman/airframe.git", "path": "charts/airframe-application",
                                  "targetRevision": "v0.3.167"},
                                 {"repoURL": "https://github.com/jfillman/boarding-api", "targetRevision": "main", "ref": "appsrc"}]},
            "status": {"sync": {"status": "Synced", "revisions": ["6a23383", "406f6fb"]}, "health": {"status": "Healthy"},
                       "operationState": {"phase": "Succeeded"},
                       "resources": [{"group": "argoproj.io", "kind": "Rollout", "name": "boarding-api",
                                      "namespace": "app-boarding-api-dev", "status": "Synced"},
                                     {"kind": "Secret", "name": "redis-connection", "namespace": "app-boarding-api-dev"}]}}
ROLLOUT = {"apiVersion": "argoproj.io/v1alpha1", "kind": "Rollout",
           "metadata": {"name": "boarding-api", "namespace": "app-boarding-api-dev", "generation": 27},
           "spec": {"replicas": 1, "strategy": {"canary": {}},
                    "template": {"spec": {"containers": [{"name": "app", "image": "ghcr.io/jfillman/boarding-api:2.0.7-c5ca91d"}]}}},
           "status": {"phase": "Healthy", "replicas": 1, "readyReplicas": 1, "availableReplicas": 1, "updatedReplicas": 1,
                      "observedGeneration": "27", "stableRS": "fc9bc9958", "currentPodHash": "fc9bc9958", "currentStepIndex": 1}}
OBJECTS = [APP, PROD, REDIS, RABBIT, ARGO_APP, ROLLOUT]


@pytest.fixture
def cluster(backends):
    for o in OBJECTS:
        backends.airframe.kube.add(copy.deepcopy(o))
    return backends.airframe.kube


def open_(gw, agent="airframe-operator", token="alice"):
    r = gw.open_session(token, agent)
    assert r.ok, r.as_dict()
    return r.data["session"]


def k8s_paths(kube):
    return [p for p in kube.paths]


# ---- registration ------------------------------------------------------------------------------------------------
def test_the_seven_airframe_tools_are_reads():
    af = {n: t for n, t in TOOLS.items() if n.startswith("airframe.")}
    assert set(af) == AF
    assert all(t.tier == Tier.T0 and t.target == "none" and not t.writes_files and not t.github_cost for t in af.values())


# ---- tied to airframe main (CI clones it) ------------------------------------------------------------------------------
@pytest.mark.skipif(not (AIRFRAME / "contract" / "airframe-contract.json").exists(), reason="airframe checkout not present")
def test_vendored_contract_bundle_matches_airframes():
    assert at.CONTRACT_PATH.read_bytes() == (AIRFRAME / "contract" / "airframe-contract.json").read_bytes(), (
        "schemas/airframe-contract.json drifted from airframe's contract/airframe-contract.json - copy it over")


@pytest.mark.skipif(not (AIRFRAME / "tools" / "sidecars.py").exists(), reason="airframe checkout not present")
def test_vendored_sidecar_helpers_match_airframes():
    vendored = ROOT / "src" / "autopilot" / "vendor" / "airframe_sidecars.py"
    assert vendored.read_bytes() == (AIRFRAME / "tools" / "sidecars.py").read_bytes(), (
        "src/autopilot/vendor/airframe_sidecars.py drifted from airframe's tools/sidecars.py - copy it over")


@pytest.mark.skipif(not (AIRFRAME / "tools" / "airframe-validate").exists(), reason="airframe checkout not present")
def test_af_rules_name_exactly_the_ids_airframe_validate_can_emit():
    src = (AIRFRAME / "tools" / "airframe-validate").read_text() + (AIRFRAME / "tools" / "deadend_rules.py").read_text()
    emitted = set(re.findall(r'"(AF-[A-Z]+-\d{3})"', src))
    assert emitted and emitted == set(at.AF_RULES), (
        f"airframe-validate emits {sorted(emitted - set(at.AF_RULES))} not in AF_RULES; AF_RULES has {sorted(set(at.AF_RULES) - emitted)} it never emits")


def load_airframe_verify():
    loader = importlib.machinery.SourceFileLoader("airframe_verify", str(AIRFRAME / "tools" / "airframe-verify"))
    spec = importlib.util.spec_from_loader("airframe_verify", loader)
    mod = importlib.util.module_from_spec(spec)
    loader.exec_module(mod)
    return mod


@pytest.mark.skipif(not (AIRFRAME / "tools" / "airframe-verify").exists(), reason="airframe checkout not present")
def test_verify_runners_agree_with_airframe_verify():
    av = load_airframe_verify()
    assert at.GENERIC_FAILURES == av.GENERIC_FAILURES
    sc, _ = at.find_kind("redis")
    webhook = 'Internal error occurred: failed calling webhook "x.example": connection refused'
    cases = [REDIS, xr("Redis", "n", "r", conditions=[cond("ComponentReady", "False", "RedisReleaseFailed"), cond("Synced", obs=3)]),
             xr("Redis", "n", "r", conditions=[cond("Synced", "False", "ReconcileError", message=webhook)]),
             xr("Redis", "n", "r", conditions=[cond("ComponentReady", reason="RedisReady", obs=2)], generation=3),
             xr("Redis", "n", "r")]
    for x in cases:
        assert at.check_synced(sc, x) == av.check_synced(sc, x)
        for step in sc["verify"]:
            if step["run"]["kind"] != "condition":
                continue
            try:
                ours = ("pass", at._run_condition(step["run"], x))
            except at.StepFailed as e:
                ours = ("fail", str(e))
            try:
                theirs = ("pass", av.run_condition(step["run"], x, {}))
            except av.StepError as e:
                theirs = ("fail", str(e))
            assert ours == theirs


# ---- capabilities and explain ------------------------------------------------------------------------------------------
def test_capabilities_answers_from_the_bundle(gw):
    sid = open_(gw)
    r = gw.call("alice", sid, "airframe.capabilities", {})
    assert r.ok and {k["kind"] for k in r.data["kinds"]} == set(at.CONTRACT["xrds"])
    assert "rollout" in r.data["chart"]["fields"]
    one = gw.call("alice", sid, "airframe.capabilities", {"kind": "redis"}).data
    assert one["kind"] == "Redis" and "password" in one["outputs"] and one["xrd"]["plural"] == "redis"
    assert gw.call("alice", sid, "airframe.capabilities", {"section": "components"}).data["components"]["redis"]["kind"] == "Redis"
    assert gw.call("alice", sid, "airframe.capabilities", {"kind": "Secret"}).data["error"]["rule"] == "AF-KIND-404"
    assert gw.call("alice", sid, "airframe.capabilities", {"section": "secrets"}).decision["rule"] == "R008"


def test_explain_covers_rules_reasons_and_known_failures(gw):
    sid = open_(gw)
    r = gw.call("alice", sid, "airframe.explain", {"rule": "AF-SECRET-001"}).data
    assert r["severity"] == "error" and "references" in r["hint"]
    reason = gw.call("alice", sid, "airframe.explain", {"rule": "RedisReleaseFailed"}).data
    assert reason["matches"][0]["kind"] == "Redis" and reason["matches"][0]["condition"] == "ComponentReady"
    kf = gw.call("alice", sid, "airframe.explain", {"rule": "AdmissionWebhookUnavailable"}).data
    assert kf["matches"][0]["hint"]
    assert gw.call("alice", sid, "airframe.explain", {"rule": "AF-NOPE-999"}).data["error"]["rule"] == "AF-EXPLAIN-404"


# ---- validate ------------------------------------------------------------------------------------------------------
def test_validate_returns_rule_ids_pointers_and_hints(gw, backends):
    sid = open_(gw)
    docs = [{"path": "glidepath/envs/dev.yaml", "content": "envName: dev\nbogusKey: 1\n"},
            {"path": "glidepath/envs/dev.release.yaml", "content": "release: {image: {tag: x}}\n"}]
    r = gw.call("alice", sid, "airframe.validate", {"documents": docs, "app": "boarding-api"})
    assert r.ok and not r.data["valid"]
    bad, ok = r.data["files"]
    assert bad["path"] == "glidepath/envs/dev.yaml" and bad["problems"] == [
        {"pointer": "/bogusKey", "rule": "AF-SCHEMA-001", "message": "unknown key 'bogusKey'",
         "hint": "no such field; see values.schema.json", "severity": "error"}]
    assert ok["valid"], "a release file is content to check here, not a path an agent writes (R015 does not apply)"
    assert backends.airframe.validator.calls == [(("glidepath/envs/dev.yaml", "glidepath/envs/dev.release.yaml"), "boarding-api", "values", True)]


@pytest.mark.parametrize("docs", [
    [], [{"path": "../x.yaml", "content": ""}], [{"path": "values.json", "content": ""}],
    [{"path": "a.yaml", "content": 1}], [{"path": "a.yaml", "content": "", "before": ""}],
    [{"path": "a.yaml", "content": "x" * (256 << 10 + 1)}], [{"path": f"{i}.yaml", "content": ""} for i in range(21)],
])
def test_bad_documents_are_bad_arguments_and_run_nothing(gw, backends, docs):
    r = gw.call("alice", open_(gw), "airframe.validate", {"documents": docs})
    assert r.decision["rule"] == "R008"
    assert backends.airframe.validator.calls == []


FAKE_VALIDATE = """#!/usr/bin/env python3
import json, sys
from pathlib import Path
args = sys.argv[1:]
files = [a for a in args if a.endswith(".yaml")]
Path(sys.argv[0] + ".argv").write_text(json.dumps(args))
print(json.dumps([{"file": f, "valid": "bad" not in Path(f).read_text(), "problems": [] if "bad" not in Path(f).read_text()
                   else [{"loc": "/x", "rule": "AF-SCHEMA-002", "message": "m", "hint": "h"}], "warnings": []} for f in files]))
sys.exit(1 if any("bad" in Path(f).read_text() for f in files) else 0)
"""


def test_the_validator_command_runs_without_a_shell_and_parses_json(tmp_path):
    script = tmp_path / "airframe-validate"
    script.write_text(FAKE_VALIDATE)
    script.chmod(0o755)
    v = at.ValidatorCommand([sys.executable, str(script)])
    out = v.run([{"path": "kind-prod/prod/values.yaml", "content": "bad: 1"}, {"path": "x/release.yaml", "content": "ok"}],
                "flight-api", "values", False)
    assert [o["valid"] for o in out] == [False, True]
    argv = json.loads((tmp_path / "airframe-validate.argv").read_text())
    assert argv[:5] == ["--format", "json", "--app", "flight-api", "--no-render"]
    assert [Path(a).name for a in argv[5:]] == ["values.yaml", "release.yaml"], "basenames are kept (release-file detection)"
    xr_mode = at.ValidatorCommand([sys.executable, str(script)]).run([{"path": "a.yaml", "content": "ok"}], "x", "xr", True)
    assert xr_mode[0]["valid"] and json.loads((tmp_path / "airframe-validate.argv").read_text())[:3] == ["--format", "json", "--xr"]
    with pytest.raises(AirframeError):
        at.ValidatorCommand([sys.executable, "-c", "import sys; sys.exit(2)"]).run([{"path": "a.yaml", "content": ""}], None, "values", True)


def test_a_container_validator_mounts_the_directory(tmp_path):
    script = tmp_path / "run"
    # stands in for `podman run -v {dir}:/work`: maps /work back to the host directory it was given
    script.write_text("#!/usr/bin/env python3\nimport os,sys\nhost=sys.argv[1].split(':')[0]\n"
                      "files=[a.replace('/work', host) for a in sys.argv[2:] if a.endswith('.yaml')]\n"
                      "import json\nprint(json.dumps([{'file': f, 'valid': os.path.exists(f), 'problems': [], 'warnings': []} for f in files]))\n")
    script.chmod(0o755)
    out = at.ValidatorCommand([sys.executable, str(script), "{dir}:/work:ro"], mount="/work").run(
        [{"path": "a.yaml", "content": "x"}], None, "values", True)
    assert out[0]["valid"] and out[0]["file"].endswith("/0/a.yaml")


REAL_VALIDATE = AIRFRAME / "tools" / "airframe-validate"


@pytest.mark.skipif(not (REAL_VALIDATE.exists() and shutil.which("helm")), reason="airframe checkout or helm not present")
def test_the_real_airframe_validate_through_the_tool():
    tools = at.AirframeTools(at.MemoryKube(), at.ValidatorCommand([sys.executable, str(REAL_VALIDATE)]))
    r = tools.validate([{"path": "glidepath/envs/dev.yaml", "content": "envName: dev\nrolout: {}\n"},
                        {"path": "glidepath/envs/staging.yaml", "content": "envName: staging\nenv: [{name: T, value: ghp_" + "a" * 36 + "}]\n"},
                        {"path": "glidepath/envs/qa.yaml", "content": "envName: qa\n"}], "parachute")
    typo, secret, fine = r["files"]
    assert typo["problems"][0]["rule"] == "AF-SCHEMA-001" and typo["problems"][0]["pointer"] == "/rolout" and "rollout" in typo["problems"][0]["hint"]
    assert [p["rule"] for p in secret["problems"]] == ["AF-SECRET-001"] and secret["problems"][0]["pointer"] == "/env/0/value"
    assert fine["valid"] and not r["valid"]


# ---- plan: the planner's own output passes R021 --------------------------------------------------------------------------
EXAMPLES = ROOT / "examples"
SCAFFOLD = yaml.safe_load("""
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
GATED = copy.deepcopy(SCAFFOLD)
GATED["pipelines"]["ci"]["steps"] += [{"stage": "test", "env": "dev"}, {"stage": "test", "env": "dev", "role": "check-ride"}]


def as_file(f):
    return {"path": f["path"], "op": f["op"], "before": yaml.safe_dump(f["before"]), "content": f["content"]}


@pytest.mark.parametrize("example", ["parachute", "mynewpythonapp"])
@pytest.mark.parametrize("base", [SCAFFOLD, GATED], ids=["scaffold", "with-check-ride"])
def test_the_planners_cicd_patch_passes_r021_and_the_rest_is_a_human_step(gw, backends, example, base):
    spec = yaml.safe_load((EXAMPLES / f"{example}.appspec.yaml").read_text())
    cs = gw.call("alice", open_(gw), "airframe.plan", {"spec": spec, "baseCicd": yaml.safe_dump(base)}).data
    steps = {s["id"]: s for s in cs["steps"]}
    agent = next(f for f in steps["app-repo"]["files"] if f["path"] == "cicd.yaml")
    assert hangar.cicd_scope_problems([as_file(agent)]) == []
    # and through the gateway, as the coding agent would open it
    r = gw.call("alice", gw.open_session("alice", "coding-agent").data["session"], "repo.pr.open",
                {"repo": f"jfillman/{example}", "branch": "agent/plan", "title": "plan", "files": [as_file(agent)]})
    assert r.ok, r.as_dict()
    head = hangar.merge_patch(base, agent["content"])
    assert [s for s in base["pipelines"]["ci"]["steps"] if s not in head["pipelines"]["ci"]["steps"]] == [], "no step is dropped"
    assert all(e["tier"] == "ground" for e in head["deploy"]["environments"])
    human = steps["cicd-flight"]
    assert human["gate"] == "human-merge" and human["risk"] == "T2"
    rules = {p["rule"] for p in hangar.cicd_scope_problems([as_file(human["files"][0])])}
    assert "GP-SCOPE-003" in rules, "Flight environments are exactly what an agent may not write"
    final = hangar.merge_patch(head, human["files"][0]["content"])
    assert [e["name"] for e in final["deploy"]["environments"] if e["tier"] == "flight"] == \
        [e["name"] for e in spec["environments"]["flight"]]
    assert backends.git.names() == ["git.open_pr"]


def test_a_pre_adr_scaffold_puts_the_whole_cicd_change_in_the_human_step(gw):
    legacy = yaml.safe_load("apiVersion: platform/v1\nkind: PipelineConfig\ndeploy: {lowerEnvironments: [dev], promotionOrder: [dev]}\n")
    cs = gw.call("alice", open_(gw), "airframe.plan", {"spec": yaml.safe_load((EXAMPLES / "parachute.appspec.yaml").read_text()),
                                                      "baseCicd": legacy}).data
    steps = {s["id"]: s for s in cs["steps"]}
    assert "cicd.yaml" not in [f["path"] for f in steps["app-repo"]["files"]]
    patch = steps["cicd-flight"]["files"][0]["content"]
    assert patch["deploy"]["lowerEnvironments"] is None and "lowerEnvironments" not in hangar.merge_patch(legacy, patch)["deploy"]
    assert any("pre-ADR-0019" in w for w in cs["warnings"])


def test_plan_questions_and_errors_are_answers(gw, backends):
    sid = open_(gw)
    backends.airframe.registry = ap.Registry("kind-dev", ("kind-prod", "kind-prod-2"), "t")
    spec = yaml.safe_load((EXAMPLES / "parachute.appspec.yaml").read_text())
    assert gw.call("alice", sid, "airframe.plan", {"spec": spec}).data["needsInput"]["options"] == ["kind-prod", "kind-prod-2"]
    assert gw.call("alice", sid, "airframe.plan", {"spec": {**spec, "bogus": 1}}).data["error"]["rule"] == "AF-SPEC-001"
    assert gw.call("alice", sid, "airframe.plan", {"spec": spec, "baseCicd": "- a list"}).data["error"]["rule"] == "AF-SPEC-002"
    assert gw.call("alice", sid, "airframe.plan", {"spec": "app: x"}).decision["rule"] == "R008"


# ---- status ----------------------------------------------------------------------------------------------------------
def test_status_normalises_conditions_with_meaning_and_freshness(gw, cluster):
    r = gw.call("alice", open_(gw), "airframe.status", {"app": "boarding-api", "env": "dev"})
    assert r.ok
    by = {(x["kind"], x["name"]): x for x in r.data["xrs"]}
    assert set(by) == {("NodeJSApplication", "boarding-api"), ("Redis", "redis"), ("RabbitMQ", "rabbitmq")}, \
        "the prod ApplicationEnvironment is another environment's"
    redis = {c["type"]: c for c in by[("Redis", "redis")]["conditions"]}
    assert redis["ComponentReady"]["reason"] == "RedisReady" and "deployed" in redis["ComponentReady"]["meaning"]
    assert redis["ComponentReady"]["observedGeneration"] == 3 and redis["ComponentReady"]["observedGenerationFrom"] == "Synced"
    assert redis["Ready"]["stale"] is False and "hint" not in redis["Ready"]
    assert r.data["summary"] == {"xrs": 3, "ready": 3, "notReady": [], "stale": []}
    assert r.data["namespaces"] == {"cicd": "app-boarding-api-cicd", "env": "app-boarding-api-dev"}


def test_status_detects_stale_status_and_names_known_failures(gw, cluster):
    stale = xr("Redis", "app-boarding-api-dev", "cache", conditions=[
        cond("ComponentReady", "False", "RedisReleaseFailed"), cond("Synced", obs=4), cond("Ready", "False", "Creating", obs=4)], generation=5)
    hook = xr("Redis", "app-boarding-api-dev", "hooked", conditions=[
        cond("Synced", "False", "ReconcileError", obs=2, message='failed calling webhook "x": refused')], generation=2)
    cluster.add(stale)
    cluster.add(hook)
    xs = {x["name"]: x for x in gw.call("alice", open_(gw), "airframe.status", {"app": "boarding-api", "env": "dev"}).data["xrs"]}
    c = {k["type"]: k for k in xs["cache"]["conditions"]}
    assert xs["cache"]["stale"] and set(xs["cache"]["staleConditions"]) == {"ComponentReady", "Synced", "Ready"}
    assert c["ComponentReady"]["hint"].startswith("The Helm release failed")
    assert c["Ready"]["hint"] and xs["cache"]["notTrue"] == ["ComponentReady", "Ready"]
    h = {k["type"]: k for k in xs["hooked"]["conditions"]}["Synced"]
    assert h["knownFailure"] == "AdmissionWebhookUnavailable" and h["stale"] is False


def test_status_of_an_unknown_app_is_an_answer(gw, cluster):
    r = gw.call("alice", open_(gw), "airframe.status", {"app": "nope"})
    assert r.ok and r.data["error"]["rule"] == "AF-APP-404"


# ---- describe ----------------------------------------------------------------------------------------------------------
def test_describe_joins_the_xrs_the_application_and_its_rollout(gw, cluster):
    d = gw.call("alice", open_(gw), "airframe.describe", {"app": "boarding-api", "env": "dev"}).data
    assert d["tier"] == "ground" and "notes" not in d
    [app] = d["applications"]
    assert app["name"] == "boarding-api-dev" and app["sync"]["status"] == "Synced" and app["health"]["status"] == "Healthy"
    assert app["sync"]["revisions"][0] == {"repoURL": "https://github.com/jfillman/airframe.git", "path": "charts/airframe-application",
                                           "targetRevision": "v0.3.167", "revision": "6a23383"}
    [ro] = d["rollouts"]
    assert ro == {**ro, "phase": "Healthy", "readyReplicas": 1, "images": ["ghcr.io/jfillman/boarding-api:2.0.7-c5ca91d"],
                  "observedGeneration": 27, "generation": 27, "stale": False}
    redis = next(x for x in d["xrs"] if x["kind"] == "Redis")
    assert redis["spec"] == {"size": "small", "persistence": True} and redis["composedResources"] == 1
    assert not any("/api/v1/" in p for p in cluster.paths), "the Secret in status.resources is never fetched"


def test_describe_a_flight_environment_says_where_it_runs(gw, cluster):
    d = gw.call("alice", open_(gw), "airframe.describe", {"app": "boarding-api", "env": "prod"}).data
    assert d["tier"] == "flight" and d["applications"] == [] and d["rollouts"] == []
    assert [x["name"] for x in d["xrs"] if x["kind"] == "ApplicationEnvironment"] == ["boarding-api-hangar-prod-prod"]
    assert "hangar-prod" in d["notes"][0]


# ---- verify ----------------------------------------------------------------------------------------------------------
def test_verify_answers_from_the_xr_and_skips_what_needs_a_secret_or_a_port_forward(gw, cluster):
    v = gw.call("alice", open_(gw), "airframe.verify", {"app": "boarding-api", "env": "dev", "kind": "Redis", "name": "redis"}).data
    steps = {s["id"]: s for s in v["steps"]}
    assert steps["xr-synced"]["status"] == "pass" and steps["xr-ready"]["status"] == "pass"
    assert steps["secret-exists"]["status"] == "skipped" and "never reads Secrets" in steps["secret-exists"]["reason"]
    assert steps["secret-exists"]["names"] == {"secret": "redis-connection", "namespace": "app-boarding-api-dev", "keys": ["password"]}
    assert steps["reachable"]["status"] == "skipped" and "port-forward" in steps["reachable"]["reason"]
    assert v["passed"] and v["counts"] == {"pass": 2, "fail": 0, "skipped": 2, "manual": 0}
    assert all(path_allowed(p) for p in cluster.paths) and not any(p.startswith("/api/") for p in cluster.paths)


def test_verify_when_and_resource_steps(gw, cluster):
    sid = open_(gw)
    rb = {s["id"]: s for s in gw.call("alice", sid, "airframe.verify",
                                       {"app": "boarding-api", "env": "dev", "kind": "rabbitmqs", "name": "rabbitmq"}).data["steps"]}
    assert rb["broker-reachable"]["status"] == "skipped" and "does not match" in rb["broker-reachable"]["reason"]
    assert "ConfigMaps are outside" in rb["configmap-exists"]["reason"]
    py = xr("PythonApplication", "app-parachute-cicd", "parachute", conditions=[
        cond("DevClusterReady", reason="DevClusterReady"), cond("CicdOnboarded", "False", "CicdOnboardingPending"), cond("Synced", obs=3)])
    cluster.add(py)
    p = gw.call("alice", sid, "airframe.verify", {"app": "parachute", "kind": "PythonApplication", "name": "parachute"}).data
    s = {x["id"]: x for x in p["steps"]}
    assert s["cicd-onboarded"]["status"] == "fail" and "CicdOnboardingPending" in s["cicd-onboarded"]["detail"]
    assert s["source-repo"]["status"] == "skipped" and "outside Clearance's read allowlist" in s["source-repo"]["reason"]
    assert not p["passed"]


def test_verify_misses_are_answers(gw, cluster):
    sid = open_(gw)
    assert gw.call("alice", sid, "airframe.verify", {"app": "boarding-api", "kind": "Secret", "name": "x"}).data["error"]["rule"] == "AF-KIND-404"
    assert gw.call("alice", sid, "airframe.verify", {"app": "boarding-api", "kind": "Redis", "name": "x", "env": "dev"}).data["error"]["rule"] == "AF-XR-404"


# ---- scope and arguments ------------------------------------------------------------------------------------------------
def test_an_app_off_the_allowlist_is_refused_before_any_read(gw, cluster, defs):
    object.__setattr__(defs["airframe-operator"], "raw_profile", {**defs["airframe-operator"].raw_profile, "airframe": {"apps": ["gate-*"]}})
    sid = open_(gw)
    r = gw.call("alice", sid, "airframe.status", {"app": "boarding-api"})
    assert r.decision["rule"] == "R023" and r.decision["hint"]
    assert cluster.paths == []


def test_no_apps_listed_means_none_but_contract_reads_still_work(gw, cluster, defs):
    object.__setattr__(defs["airframe-operator"], "raw_profile", {**defs["airframe-operator"].raw_profile, "airframe": {"apps": []}})
    sid = open_(gw)
    for tool, args in (("airframe.status", {"app": "boarding-api"}), ("airframe.describe", {"app": "boarding-api", "env": "dev"}),
                       ("airframe.verify", {"app": "boarding-api", "kind": "Redis", "name": "redis"}),
                       ("airframe.validate", {"app": "boarding-api", "documents": [{"path": "a.yaml", "content": ""}]})):
        assert gw.call("alice", sid, tool, args).decision["rule"] == "R023", tool
    assert cluster.paths == []
    assert gw.call("alice", sid, "airframe.capabilities", {}).ok and gw.call("alice", sid, "airframe.explain", {"rule": "AF-ENV-001"}).ok


def test_other_agents_cannot_use_airframe_tools(gw):
    assert gw.call("alice", open_(gw, agent="cicd-explainer"), "airframe.status", {"app": "boarding-api"}).decision["rule"] == "R006"


@pytest.mark.parametrize("tool,args", [
    ("airframe.status", {"app": "../etc"}), ("airframe.status", {"app": "Boarding"}),
    ("airframe.describe", {"app": "boarding-api", "env": "dev/../x"}),
    ("airframe.verify", {"app": "boarding-api", "kind": "secrets/x", "name": "y"}),
    ("airframe.verify", {"app": "boarding-api", "kind": "Redis", "name": "a/b"}),
    ("airframe.explain", {"rule": "../../x"}),
])
def test_arguments_that_are_not_kubernetes_names_are_bad_arguments(gw, cluster, tool, args):
    assert gw.call("alice", open_(gw), tool, args).decision["rule"] == "R008"
    assert cluster.paths == []


# ---- the Kubernetes adapter --------------------------------------------------------------------------------------------
class Opener:
    def __init__(self, *responses):
        self.responses = list(responses) or [(200, b'{"items": []}')]
        self.reqs = []

    def __call__(self, req, timeout, ctx):
        self.reqs.append((req, ctx))
        return self.responses.pop(0) if len(self.responses) > 1 else self.responses[0]


@pytest.fixture
def token(tmp_path):
    p = tmp_path / "token"
    p.write_text("tok-1\n")
    return p


@pytest.mark.parametrize("path", [
    "/api/v1/namespaces/app-x-dev/secrets/redis-connection",
    "/api/v1/namespaces/app-x-dev/secrets",
    "/api/v1/namespaces/app-x-dev/configmaps/c",
    "/api/v1/namespaces/app-x-dev/pods/p/exec",
    "/api/v1/namespaces/app-x-dev/pods/p/portforward",
    "/api/v1/namespaces/app-x-dev/services/s:80/proxy/",
    "/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applicationsets",
    "/apis/argoproj.io/v1alpha1/namespaces/n/rollouts/r/status",
    "/apis/catalog.hangar.io/v1alpha1/namespaces/n/redis/r/status",
    "/apis/catalog.hangar.io/v1alpha1/namespaces/../redis",
    "/apis/catalog.hangar.io/v1alpha1/namespaces/n/redis/a%2Fb",
    "/apis/repo.github.m.upbound.io/v1alpha1/namespaces/n/repositories/r",
    "/apis/external-secrets.io/v1/clustersecretstores/x",
])
def test_the_adapter_refuses_every_other_path_without_a_request(token, path):
    o = Opener()
    api = KubeReader("https://apiserver:6443", token, opener=o)
    with pytest.raises(PathRefused) as e:
        api.get(path)
    assert isinstance(e.value, Denied) and e.value.rule == "AF-K8S-PATH"
    assert o.reqs == [], "nothing was sent"


def test_the_adapter_refuses_watch_and_unknown_query_parameters(token):
    o = Opener()
    api = KubeReader("https://apiserver:6443", token, opener=o)
    with pytest.raises(PathRefused):
        api.get("/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications", {"watch": "true"})
    assert o.reqs == []


def test_the_adapter_gets_with_a_token_reread_every_call_and_the_ca(tmp_path, token):
    ca = tmp_path / "ca.crt"
    ca.write_text("not read until a context is built\n")
    o = Opener((200, b'{"kind": "Redis"}'))
    built = []
    api = KubeReader("https://apiserver:6443/", token, ca_file=ca, opener=o)
    orig = ssl.create_default_context
    ssl.create_default_context = lambda cafile=None, **k: built.append(cafile) or orig()
    try:
        api.get("/apis/catalog.hangar.io/v1alpha1/namespaces/app-x-dev/redis/r")
        token.write_text("tok-2")
        api.get("/apis/catalog.hangar.io/v1alpha1/namespaces/app-x-dev/redis/r")
    finally:
        ssl.create_default_context = orig
    (first, c1), (second, _) = o.reqs
    assert first.get_method() == "GET" and first.data is None
    assert first.full_url == "https://apiserver:6443/apis/catalog.hangar.io/v1alpha1/namespaces/app-x-dev/redis/r"
    assert first.get_header("Authorization") == "Bearer tok-1" and second.get_header("Authorization") == "Bearer tok-2"
    assert built == [str(ca), str(ca)] and c1 is not None, "the CA is read on every call too"
    assert "tok-1" not in repr(api.__dict__) and "tok-2" not in repr(api.__dict__), "the adapter holds no token"


def test_the_adapter_maps_statuses_and_follows_continue(token):
    path = "/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications"
    assert KubeReader("https://a", token, opener=Opener((404, b"{}"))).get(path + "/x") is None
    with pytest.raises(Denied) as e:
        KubeReader("https://a", token, opener=Opener((403, b"{}"))).get(path)
    assert e.value.rule == "AF-K8S-403"
    with pytest.raises(AirframeError):
        KubeReader("https://a", token, opener=Opener((500, b"{}"))).get(path)
    pages = Opener((200, b'{"items": [{"a": 1}], "metadata": {"continue": "c1"}}'), (200, b'{"items": [{"a": 2}], "metadata": {}}'))
    assert KubeReader("https://a", token, opener=pages).list(path, "hangar.io/app=x") == [{"a": 1}, {"a": 2}]
    assert "continue=c1" in pages.reqs[1][0].full_url and "labelSelector=hangar.io%2Fapp%3Dx" in pages.reqs[0][0].full_url


# ---- an agent starting cold, through MCP, against an HTTP API server ---------------------------------------------------
class StubAPIServer(BaseHTTPRequestHandler):
    seen: list = []
    objects: dict = {}

    def do_GET(self):  # noqa: N802
        path, _, query = self.path.partition("?")
        StubAPIServer.seen.append((path, self.headers.get("Authorization")))
        items = [o for p, o in StubAPIServer.objects.items() if p.rsplit("/", 1)[0] == path]
        sel = dict(kv.split("=", 1) for kv in re.findall(r"labelSelector=([^&]*)", query)[0].replace("%2F", "/").replace("%3D", "=")
                   .replace("%2C", ",").split(",")) if "labelSelector=" in query else {}
        items = [o for o in items if all((o["metadata"].get("labels") or {}).get(k) == v for k, v in sel.items())]
        body = StubAPIServer.objects.get(path) or ({"items": items, "metadata": {}} if path.count("/") in (6, 4) else None)
        if body is None:
            self.send_response(404)
            self.end_headers()
            self.wfile.write(b'{"kind":"Status","code":404}')
            return
        raw = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *a):
        pass


def test_an_agent_reads_an_environment_using_only_mcp(defs, clock, token):
    kube = at.MemoryKube(OBJECTS)
    StubAPIServer.objects = dict(kube.objects)
    StubAPIServer.seen = []
    srv = HTTPServer(("127.0.0.1", 0), StubAPIServer)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        from conftest import StaticAuth
        from clearance.audit import AuditLog
        from clearance.dispatch import Principal
        from clearance.session import SessionStore
        reader = KubeReader(f"http://127.0.0.1:{srv.server_address[1]}", token)
        tools = at.AirframeTools(reader, at.FakeValidator(), REG, cluster="hangar-dev")
        b = Backends(FakeGit(), FakeArgo(), FakeRuns(), FakeRead(), FakeHuman(), FakeArtifacts(), None, tools)
        gw = hangar_gateway(defs, SessionStore(), AuditLog(), b, StaticAuth({"alice": Principal("user:alice", "user", Tier.T1)}), clock)
        mcp = create_server(gw, lambda: "alice")

        def call(name, args):
            r = asyncio.run(mcp.call_tool(name, args))
            sc = getattr(r, "structured_content", None) or getattr(r, "structuredContent", None)
            return sc.get("result", sc) if sc else json.loads(r.content[0].text)

        sid = call("session_open", {"agent": "airframe-operator"})["data"]["session"]
        st = call("call", {"session": sid, "tool": "airframe.status", "args": {"app": "boarding-api", "env": "dev"}})
        assert st["ok"] and st["data"]["summary"]["ready"] == 3
        d = call("call", {"session": sid, "tool": "airframe.describe", "args": {"app": "boarding-api", "env": "dev"}})
        assert d["data"]["rollouts"][0]["images"] == ["ghcr.io/jfillman/boarding-api:2.0.7-c5ca91d"]
        v = call("call", {"session": sid, "tool": "airframe.verify", "args": {"app": "boarding-api", "env": "dev", "kind": "Redis", "name": "redis"}})
        assert v["data"]["passed"] and {s["status"] for s in v["data"]["steps"]} == {"pass", "skipped"}
        assert StubAPIServer.seen and all(auth == "Bearer tok-1" for _, auth in StubAPIServer.seen)
        assert all(path_allowed(p) for p, _ in StubAPIServer.seen) and not any(p.startswith("/api/") for p, _ in StubAPIServer.seen)
        with pytest.raises(PathRefused):
            reader.get("/api/v1/namespaces/app-boarding-api-dev/secrets/redis-connection")
        assert not any(p.startswith("/api/") or p.endswith("/secrets/redis-connection") for p, _ in StubAPIServer.seen), "the Secret read never reached the API server"
    finally:
        srv.shutdown()
