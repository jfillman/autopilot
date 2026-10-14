"""The planner, and the acceptance test it exists for: one sentence, four environments. A second
sentence (examples/mynewpythonapp.appspec.yaml) holds that the AppSpec reaches anything the chart allows.

    provision a new python application named parachute. give it a dev and test ground environment.
    a staging and prod flight environment. and configure it with 1 100% weight canary step, an
    URL=http://myendpoint.io env var.
"""
import copy
import json
import os
import shutil
import subprocess
import tempfile
from pathlib import Path

import jsonschema
import pytest
import yaml

from clearance import airframe_plan as ap

# Sibling checkouts. CI clones airframe under HANGAR_TECH; locally it is ~/tech.
TECH = Path(os.environ.get("HANGAR_TECH", "/Users/jerf/tech"))
XRDS = TECH / "airframe" / "xrds"
CHART = TECH / "airframe" / "charts" / "airframe-application"
CICD_SCHEMA = TECH / "glidepath" / "schemas" / "cicd.schema.json"
TENANTS = "gitops-cluster-dev-tenants"
REG = ap.Registry("kind-dev", ("kind-prod",), TENANTS)
FLEET_EXAMPLE = TECH / "airframe" / "charts" / "cluster-registry" / "tests" / "clusters.example.yaml"
FLEET_LIVE = TECH / "gitops-cluster-dev" / "clusters.yaml"

PARACHUTE = yaml.safe_load("""
apiVersion: airframe/v1
kind: AppSpec
app: parachute
stack: python
description: Parachute service
environments:
  ground: [dev, test]
  flight: [{name: staging}, {name: prod}]
config:
  rollout: {strategy: canary, steps: [{setWeight: 100}]}
  env: [{name: URL, value: "http://myendpoint.io"}]
""")

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

# what an app scaffolded before ADR-0019 (2026-10-07) still carries
LEGACY_SCAFFOLD_CICD = yaml.safe_load("""
apiVersion: platform/v1
kind: PipelineConfig
build: {agent: python-3.12, unitTest: {enabled: false}}
deploy:
  lowerEnvironments: [dev]
  upperEnvironments: []
  promotionOrder: [dev]
  strategy: rollout
""")


def spec(**over):
    s = copy.deepcopy(PARACHUTE)
    s.update(over)
    return s


def xrd_schema(name):
    d = yaml.safe_load((XRDS / f"{name}.yaml").read_text())
    return d["spec"]["versions"][0]["schema"]["openAPIV3Schema"]


# ---- the sentence ----------------------------------------------------------------------
def test_the_sentence_becomes_an_ordered_change_set():
    cs = ap.plan(PARACHUTE, REG)
    assert [s.id for s in cs.steps] == ["tenants", "repos-ready", "app-repo", "tekton-resync", "gitops", "verify"]
    assert [(s.id, s.gate, s.risk) for s in cs.steps if s.gate != "wait"] == [
        ("tenants", "human-merge", "T2"), ("app-repo", "auto-merge", "T1"), ("tekton-resync", "auto-merge", "T1"),
        ("gitops", "human-merge", "T2"), ("verify", "verify", "T0")]


def test_dependencies_form_a_dag_that_respects_the_async_gates():
    cs = ap.plan(PARACHUTE, REG)
    seen = set()
    for s in cs.steps:
        assert set(s.depends_on) <= seen, f"{s.id} depends on something not yet planned"
        seen.add(s.id)
    assert cs.step("app-repo").depends_on == ["repos-ready"], "nothing touches the app repo before it exists"


def test_one_tenants_pr_carries_the_app_and_both_flight_environments():
    files = ap.plan(PARACHUTE, REG).step("tenants").files
    assert [f.path for f in files] == [
        "tenants/parachute/xr-requests/parachute.yaml",
        "tenants/parachute/xr-requests/parachute-kind-prod-staging.yaml",
        "tenants/parachute/xr-requests/parachute-kind-prod-prod.yaml"]
    kinds = [f.content["kind"] for f in files]
    assert kinds == ["PythonApplication", "ApplicationEnvironment", "ApplicationEnvironment"]
    assert all(f.content["metadata"]["namespace"] == "app-parachute-cicd" for f in files)


def test_dev_cluster_is_the_registry_value_never_a_guess():
    x = ap.plan(PARACHUTE, ap.Registry("kind-dev", ("kind-prod",), TENANTS)).step("tenants").files[0].content
    assert x["spec"]["devCluster"] == "kind-dev"


def test_ground_environments_carry_rollout_config_from_the_start():
    """Airframe v0.3.91 renders no workload until an image exists, so config no longer waits for a deploy."""
    cs = ap.plan(PARACHUTE, REG)
    envs = [f for f in cs.step("app-repo").files if f.path.startswith("glidepath/envs/")]
    assert {f.path for f in envs} == {"glidepath/envs/dev.yaml", "glidepath/envs/test.yaml"}
    for f in envs:
        assert f.content["rollout"]["steps"] == [{"setWeight": 100}]
        assert f.content["env"] == [{"name": "URL", "value": "http://myendpoint.io"}]
    assert "ground-rollout" not in [s.id for s in cs.steps]


def test_flight_config_is_a_patch_on_each_bootstrapped_values_file():
    g = ap.plan(PARACHUTE, REG).step("gitops")
    assert [f.path for f in g.files] == ["kind-prod/staging/values.yaml", "kind-prod/prod/values.yaml"]
    assert all(f.op == "merge-patch" and f.content["env"][0]["name"] == "URL" for f in g.files)
    assert all(f.content["rollout"]["steps"] == [{"setWeight": 100}] for f in g.files)


def test_assumptions_and_warnings_are_surfaced_not_silent():
    cs = ap.plan(PARACHUTE, REG)
    assert any("only upper cluster is kind-prod" in a for a in cs.assumptions)
    assert any("same name as a pipeline stage" in w and "'test'" in w for w in cs.warnings)
    assert any("single 100% step" in w for w in cs.warnings)
    assert not any("no empty-image guard" in w for w in cs.warnings)


def test_verify_lists_a_check_for_every_claim():
    checks = ap.plan(PARACHUTE, REG).step("verify").checks
    joined = "\n".join(checks)
    for env in ("dev", "test", "staging", "prod"):
        assert f"{env}: rendered config contains URL=http://myendpoint.io" in joined
        assert f'{env}: rollout steps == [{{"setWeight":100}}]' in joined
    assert "PythonApplication/parachute Ready and CicdOnboarded" in joined
    assert "ApplicationEnvironment parachute-kind-prod-prod Ready" in joined


# ---- ambiguity ----------------------------------------------------------------------------
def test_two_upper_clusters_and_no_choice_is_a_question_not_a_guess():
    with pytest.raises(ap.NeedsInput) as e:
        ap.plan(PARACHUTE, ap.Registry("kind-dev", ("kind-prod", "kind-prod-2"), TENANTS))
    assert e.value.options == ["kind-prod", "kind-prod-2"] and "staging" in e.value.question


def test_an_explicit_cluster_needs_no_question():
    s = spec(environments={"ground": ["dev"], "flight": [{"name": "staging", "cluster": "kind-prod-2"}]})
    cs = ap.plan(s, ap.Registry("kind-dev", ("kind-prod", "kind-prod-2"), TENANTS))
    assert cs.step("tenants").files[1].path.endswith("parachute-kind-prod-2-staging.yaml")


@pytest.mark.parametrize("mutate,msg", [
    (lambda s: s.update(app="Bad_Name"), "app"),
    (lambda s: s.update(bogus=1), "bogus"),
    (lambda s: s["environments"].pop("flight"), "flight"),
    (lambda s: s["environments"].update(ground=[]), "ground"),
    (lambda s: s["config"].update(replicas=3), "replicas"),
    (lambda s: s["config"]["env"].append({"name": "1BAD", "value": "x"}), "name"),
])
def test_bad_specs_are_rejected_with_the_path(mutate, msg):
    s = copy.deepcopy(PARACHUTE)
    mutate(s)
    with pytest.raises(ap.SpecError) as e:
        ap.plan(s, REG)
    assert msg in str(e.value)


def test_unknown_cluster_and_duplicate_environment_names_are_rejected():
    with pytest.raises(ap.SpecError):
        ap.plan(spec(environments={"ground": ["dev"], "flight": [{"name": "staging", "cluster": "nope"}]}), REG)
    with pytest.raises(ap.SpecError):
        ap.plan(spec(environments={"ground": ["dev", "staging"], "flight": [{"name": "staging"}]}), REG)
    with pytest.raises(ap.SpecError):
        ap.plan(PARACHUTE, ap.Registry("kind-dev", (), TENANTS))


# ---- purity and determinism ---------------------------------------------------------------
def test_planning_is_deterministic_and_never_mutates_its_input():
    before = json.dumps(PARACHUTE, sort_keys=True)
    a = json.dumps(ap.plan(PARACHUTE, REG), default=lambda o: o.__dict__, sort_keys=True)
    b = json.dumps(ap.plan(PARACHUTE, REG), default=lambda o: o.__dict__, sort_keys=True)
    assert a == b and json.dumps(PARACHUTE, sort_keys=True) == before


def test_per_environment_overrides_win_over_config():
    s = spec(overrides={"prod": {"rollout": {"replicas": 4}}})
    g = ap.plan(s, REG).step("gitops")
    prod = next(f for f in g.files if "/prod/" in f.path).content
    stg = next(f for f in g.files if "/staging/" in f.path).content
    assert prod["rollout"]["replicas"] == 4 and prod["rollout"]["steps"] == [{"setWeight": 100}]
    assert "replicas" not in stg["rollout"]


# ---- what Airframe should become -------------------------------------------------------------
def test_with_the_a_plus_features_the_workaround_and_the_duplication_disappear():
    cs = ap.plan(PARACHUTE, REG, ap.Features(rollout_guard=True, base_layer=True, release_split=True))
    assert [s.id for s in cs.steps] == ["tenants", "repos-ready", "app-repo", "tekton-resync", "gitops", "verify"]
    assert "glidepath/base.yaml" in [f.path for f in cs.step("app-repo").files]
    assert [f.path for f in cs.step("gitops").files] == ["base/values.yaml"], "config is written once"
    assert not any("no empty-image guard" in w for w in cs.warnings)


def test_with_only_the_chart_guard_ground_config_lands_immediately():
    cs = ap.plan(PARACHUTE, REG, ap.Features(rollout_guard=True))
    dev = next(f for f in cs.step("app-repo").files if f.path == "glidepath/envs/dev.yaml").content
    assert dev["rollout"]["steps"] == [{"setWeight": 100}] and "ground-rollout" not in [s.id for s in cs.steps]


# ---- the second sentence: anything the chart allows ------------------------------------------
# "provision a new python app with a dev, test ground envs, staging, pre-prod and prod flight envs, port 1234,
#  replicas 3, 4 canary steps: 25% weight, pause 30s, 50% weight, pause 30s. 100% weight, a pdb enabled,
#  an hpa enabled, a route called https://mynewpythonapp.hangar.io/."
EXAMPLES = Path(__file__).resolve().parents[1] / "examples"
MYNEW = yaml.safe_load((EXAMPLES / "mynewpythonapp.appspec.yaml").read_text())
FIVE = ["dev", "test", "staging", "pre-prod", "prod"]
STEPS = [{"setWeight": 25}, {"pause": {"duration": "30s"}}, {"setWeight": 50}, {"pause": {"duration": "30s"}}, {"setWeight": 100}]


def mynew(**over):
    s = copy.deepcopy(MYNEW)
    s.update(over)
    return s


def test_the_second_sentence_compiles_with_the_usual_gates():
    cs = ap.plan(MYNEW, REG)
    assert [(s.id, s.gate, s.risk) for s in cs.steps if s.gate != "wait"] == [
        ("tenants", "human-merge", "T2"), ("app-repo", "auto-merge", "T1"), ("tekton-resync", "auto-merge", "T1"),
        ("gitops", "human-merge", "T2"), ("verify", "verify", "T0")]
    assert [f.content["metadata"]["name"] for f in cs.step("tenants").files] == [
        "mynewpythonapp", "mynewpythonapp-kind-prod-staging", "mynewpythonapp-kind-prod-pre-prod", "mynewpythonapp-kind-prod-prod"]
    assert cs.step("tenants").files[0].content["spec"]["port"] == 1234
    assert "staging, pre-prod, prod" in cs.step("gitops").effects[0]


def test_every_environment_gets_every_chart_value_and_only_prod_gets_the_route():
    cs = ap.plan(MYNEW, REG)
    files = {f.path: f.content for f in cs.step("app-repo").files + cs.step("gitops").files if f.path != "cicd.yaml"}
    assert sorted(files) == sorted(["glidepath/envs/dev.yaml", "glidepath/envs/test.yaml", "kind-prod/staging/values.yaml",
                                    "kind-prod/pre-prod/values.yaml", "kind-prod/prod/values.yaml"])
    for path, v in files.items():
        assert v["rollout"]["steps"] == STEPS and v["rollout"]["replicas"] == 3, path
        assert v["rollout"]["ports"] == [{"name": "http", "containerPort": 1234}], path
        assert v["podDisruptionBudget"] == {"enabled": True} and v["autoscaling"] == {"enabled": True}, path
        assert ("httpRoute" in v) == (path == "kind-prod/prod/values.yaml"), path
    assert files["kind-prod/prod/values.yaml"]["httpRoute"]["hostnames"] == ["mynewpythonapp.hangar.io"]


def test_the_second_sentences_ambiguities_are_reported():
    cs = ap.plan(MYNEW, REG)
    assert any("rollout.ports is set to http:1234" in a for a in cs.assumptions)
    floor = [w for w in cs.warnings if "HPA owns the count" in w]
    assert len(floor) == 1 and all(e in floor[0] for e in FIVE) and "autoscaling.min 2" in floor[0]
    checks = "\n".join(cs.step("verify").checks)
    for e in FIVE:
        assert f"{e}: Rollout spec.replicas == 3" in checks and f"{e}: Service ports == http:1234" in checks
        assert f"{e}: HorizontalPodAutoscaler targets the Rollout (min 2, max 10)" in checks
        assert f"{e}: PodDisruptionBudget exists" in checks
    assert "prod: HTTPRoute for mynewpythonapp.hangar.io is Accepted by its Gateway" in checks
    assert "staging: HTTPRoute" not in checks


def test_a_hostname_shared_by_several_environments_is_a_question_not_a_guess():
    s = mynew()
    s["config"]["httpRoute"] = s.pop("overrides")["prod"]["httpRoute"]
    with pytest.raises(ap.NeedsInput) as e:
        ap.plan(s, REG)
    assert e.value.options == FIVE and "mynewpythonapp.hangar.io" in e.value.question


def test_an_override_for_an_undeclared_environment_is_rejected():
    with pytest.raises(ap.SpecError, match="overrides/qa: no environment named 'qa'"):
        ap.plan(mynew(overrides={"qa": {"rollout": {"replicas": 1}}}), REG)


def test_each_environments_merged_values_are_validated_against_the_chart():
    # each half is valid alone; merged, prod's route is enabled with no hostname
    s = mynew(environments={"ground": ["dev"], "flight": [{"name": "prod"}]},
              overrides={"prod": {"httpRoute": {"hostnames": []}}})
    s["config"]["httpRoute"] = {"enabled": True, "hostnames": ["dev.example"], "parentRefs": [{"name": "kiac"}]}
    jsonschema.validate(s, ap.SCHEMA)
    with pytest.raises(ap.SpecError, match=r"prod \(config merged with overrides.prod\): httpRoute"):
        ap.plan(s, REG)


def test_the_scaffold_port_and_the_chart_ports_must_agree():
    s = mynew()
    s["config"]["rollout"]["ports"] = [{"name": "http", "containerPort": 8080}]
    with pytest.raises(ap.SpecError, match="does not include stackOptions.port 1234"):
        ap.plan(s, REG)
    s["config"]["rollout"]["ports"] = [{"name": "web", "containerPort": 1234}]
    cs = ap.plan(s, REG)
    assert not any("rollout.ports is set" in a for a in cs.assumptions), "an explicit, agreeing ports list is left alone"


def test_a_critical_risk_value_takes_the_app_repo_step_out_of_auto_merge():
    s = mynew()
    s["config"]["extraManifests"] = [{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "x"}}]
    cs = ap.plan(s, REG)
    assert (cs.step("app-repo").gate, cs.step("app-repo").risk) == ("human-merge", "T2")
    assert any("extraManifests is critical-risk" in w for w in cs.warnings)


def test_platform_owned_chart_keys_are_not_an_appspecs_to_set():
    for key, value in (("cluster", "kind-prod"), ("envName", "prod"), ("release", {}), ("appType", "infra")):
        s = mynew()
        s["config"][key] = value
        with pytest.raises(ap.SpecError, match=key):
            ap.plan(s, REG)


def test_with_a_base_layer_flight_overrides_are_still_written():
    cs = ap.plan(MYNEW, REG, ap.Features(rollout_guard=True, base_layer=True))
    g = {f.path: f.content for f in cs.step("gitops").files}
    assert sorted(g) == ["base/values.yaml", "kind-prod/prod/values.yaml"]
    assert g["kind-prod/prod/values.yaml"] == MYNEW["overrides"]["prod"]
    assert g["base/values.yaml"]["rollout"]["ports"] == [{"name": "http", "containerPort": 1234}]


# ---- tied to the real contracts ---------------------------------------------------------------
AIRFRAME_APPSPEC = TECH / "airframe" / "contract" / "appspec.schema.json"


@pytest.mark.skipif(not AIRFRAME_APPSPEC.exists(), reason="airframe checkout (with contract/appspec.schema.json) not present")
def test_vendored_appspec_schema_matches_airframes_contract():
    # Airframe owns AppSpec (apiVersion airframe/v1); schemas/appspec.schema.json is a vendored copy.
    vendored = Path(__file__).resolve().parents[1] / "schemas" / "appspec.schema.json"
    assert vendored.read_bytes() == AIRFRAME_APPSPEC.read_bytes(), (
        "schemas/appspec.schema.json drifted from airframe's contract/appspec.schema.json - copy it over")


@pytest.mark.skipif(not (XRDS / "pythonapplication.yaml").exists(), reason="airframe checkout not present")
def test_generated_xrs_satisfy_the_real_xrd_schemas():
    files = {f.content["kind"]: f.content for f in ap.plan(PARACHUTE, REG).step("tenants").files}
    jsonschema.validate({"spec": files["PythonApplication"]["spec"]}, xrd_schema("pythonapplication"))
    jsonschema.validate({"spec": files["ApplicationEnvironment"]["spec"]}, xrd_schema("applicationenvironment"))


@pytest.mark.skipif(not CICD_SCHEMA.exists(), reason="glidepath checkout not present")
def test_the_patched_cicd_yaml_validates_against_glidepaths_schema():
    cs = ap.plan(PARACHUTE, REG, base_cicd=SCAFFOLD_CICD)
    patched = ap.patch_cicd(SCAFFOLD_CICD, PARACHUTE, [{"name": "staging", "cluster": "kind-prod"}, {"name": "prod", "cluster": "kind-prod"}])
    jsonschema.validate(patched, json.loads(CICD_SCHEMA.read_text()))
    assert patched["deploy"]["environments"] == [
        {"name": "dev", "tier": "ground"}, {"name": "test", "tier": "ground"},
        {"name": "staging", "tier": "flight", "cluster": "kind-prod"}, {"name": "prod", "tier": "flight", "cluster": "kind-prod"}]
    assert patched["deploy"]["strategy"] == "rollout", "keys the planner does not own are left alone"
    assert patched["pipelines"]["ci"]["steps"] == [
        {"stage": "build"}, {"stage": "deploy", "env": "dev"}, {"stage": "deploy", "env": "test"}, {"stage": "release", "env": "staging"}]
    assert patched["build"] == SCAFFOLD_CICD["build"], "everything the planner does not own is left alone"
    envs = cs.step("app-repo").files[0].content["deploy"]["environments"]
    assert [e["name"] for e in envs] == ["dev", "test", "staging", "prod"]


@pytest.mark.skipif(not CICD_SCHEMA.exists(), reason="glidepath checkout not present")
def test_a_pre_adr_0019_scaffold_is_brought_onto_the_current_schema():
    patched = ap.patch_cicd(LEGACY_SCAFFOLD_CICD, PARACHUTE, [{"name": "staging", "cluster": "kind-prod"}])
    assert not set(ap.LEGACY_DEPLOY_KEYS) & set(patched["deploy"]), "lowerEnvironments/upperEnvironments/promotionOrder are gone"
    jsonschema.validate(patched, json.loads(CICD_SCHEMA.read_text()))


# ---- the registry is read, never typed -----------------------------------------------------------
def test_tenants_repo_comes_from_the_registry_and_reaches_every_place_it_is_named():
    reg = ap.Registry("kind-dev", ("kind-prod",), "gitops-cluster-other-tenants", owner="someone")
    cs = ap.plan(PARACHUTE, reg)
    assert cs.step("tenants").repo == "gitops-cluster-other-tenants" and cs.step("repos-ready").repo == "gitops-cluster-other-tenants"
    ann = cs.step("tenants").files[0].content["metadata"]["annotations"]
    assert ann["terasky.backstage.io/source-file-url"].startswith("https://github.com/someone/gitops-cluster-other-tenants/blob/main/tenants/parachute/")
    assert "repo=gitops-cluster-other-tenants" in ann["terasky.backstage.io/source-info"]
    assert "gitops-cluster-dev-tenants" not in json.dumps(cs, default=lambda o: o.__dict__)


@pytest.mark.skipif(not FLEET_EXAMPLE.exists(), reason="airframe checkout not present")
def test_the_registry_is_derived_from_the_fleet_file_the_cluster_registry_chart_reads():
    reg = ap.Registry.from_fleet(yaml.safe_load(FLEET_EXAMPLE.read_text()))
    assert reg == ap.Registry("kind-dev", ("kind-prod",), "gitops-cluster-dev-tenants"), "aliases (kiac-dev) are not clusters"


@pytest.mark.skipif(not FLEET_LIVE.exists(), reason="gitops-cluster-dev checkout not present")
def test_the_live_fleet_file_yields_the_registry_the_tests_assume():
    assert ap.Registry.from_fleet(yaml.safe_load(FLEET_LIVE.read_text())) == REG


def test_a_fleet_without_exactly_one_control_plane_cluster_or_without_a_tenants_repo_is_rejected():
    with pytest.raises(ap.SpecError):
        ap.Registry.from_fleet({"clusters": [{"name": "a", "zone": "upper", "roles": ["workloads"]}]})
    with pytest.raises(ap.SpecError):
        ap.Registry.from_fleet({"clusters": [{"name": "d", "zone": "lower", "roles": ["control-plane"]}]})


helm_ok = shutil.which("helm") and CHART.exists()


def render(values: dict) -> tuple[int, str]:
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as f:
        yaml.safe_dump(values, f)
    r = subprocess.run(["helm", "template", "parachute", str(CHART), "-f", f.name], capture_output=True, text=True)
    return r.returncode, r.stdout


BOOT = {"appName": "parachute", "appType": "app", "cluster": "kind-prod", "envName": "staging"}


@pytest.mark.skipif(not helm_ok, reason="helm or the airframe chart is not available")
def test_the_flight_patch_renders_the_step_and_the_env_var_on_the_real_chart():
    g = ap.plan(PARACHUTE, REG).step("gitops")
    patch = g.files[0].content
    values = ap.merge(BOOT, ap.merge({"rollout": {"image": {"repository": "ghcr.io/x/parachute", "tag": "1.0.0"}}}, patch))
    code, out = render(values)
    assert code == 0 and "- setWeight: 100" in out and "name: URL" in out and "value: http://myendpoint.io" in out


@pytest.mark.skipif(not helm_ok, reason="helm or the airframe chart is not available")
def test_the_second_sentence_renders_on_the_real_chart():
    cs = ap.plan(MYNEW, REG)
    files = {f.path: f.content for f in cs.step("app-repo").files + cs.step("gitops").files if f.path != "cicd.yaml"}
    image = {"rollout": {"image": {"repository": "ghcr.io/x/mynewpythonapp", "tag": "1.0.0"}}}
    for path, v in files.items():
        env = path.split("/")[-2] if path.startswith("kind-prod/") else Path(path).stem
        cluster = "kind-prod" if path.startswith("kind-prod/") else "kind-dev"
        boot = {"appName": "mynewpythonapp", "appType": "app", "cluster": cluster, "envName": env}
        code, out = render(ap.merge(boot, ap.merge(image, v)))
        assert code == 0, path
        docs = {d["kind"]: d for d in yaml.safe_load_all(out) if d}
        ro = docs["Rollout"]["spec"]
        assert ro["replicas"] == 3 and ro["strategy"]["canary"]["steps"] == STEPS, path
        assert {"name": "http", "containerPort": 1234} in ro["template"]["spec"]["containers"][0]["ports"], path
        assert docs["Service"]["spec"]["ports"][0]["port"] == 1234, path
        assert docs["HorizontalPodAutoscaler"]["spec"]["scaleTargetRef"]["kind"] == "Rollout", path
        assert "PodDisruptionBudget" in docs, path
        assert ("HTTPRoute" in docs) == (env == "prod"), path
        if env == "prod":
            assert docs["HTTPRoute"]["spec"]["hostnames"] == ["mynewpythonapp.hangar.io"]
            assert docs["HTTPRoute"]["spec"]["parentRefs"][0] == {"name": "kiac", "namespace": "kiac-gateway"}


@pytest.mark.skipif(not helm_ok, reason="helm or the airframe chart is not available")
def test_the_planners_ground_workaround_renders_no_rollout_before_the_first_image():
    dev = next(f for f in ap.plan(PARACHUTE, REG).step("app-repo").files if f.path == "glidepath/envs/dev.yaml").content
    code, out = render({**BOOT, "cluster": "kind-dev", "envName": "dev", **dev})
    assert code == 0 and "kind: Rollout\n" not in out and "image: ':'" not in out


