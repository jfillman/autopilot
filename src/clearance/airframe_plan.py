"""AppSpec -> ChangeSet: the planner.

One statement of intent ("a Python app called parachute with dev and test ground environments and
staging and prod flight environments, one 100% canary step, and a URL env var") becomes an ordered
set of repo changes, each with its gate, its dependencies and what it will do. A pure function: no
network, no clock, same input gives the same output, so it is tested without a cluster and safe to
re-run.

An AppSpec's `config` and `overrides.<env>` are the airframe-application chart's values (every
human-owned key, generated into the schema by airframe's tools/gen_appspec_schema.py), so the planner
passes them through whole. What it adds on top: each environment's merged values are validated
against the chart again (two valid halves can make an invalid whole), a hostname may serve only one
environment, the scaffold's port and the chart's rollout.ports agree, and a critical-risk key takes
the app-repo step out of auto-merge.

It encodes what Airframe requires TODAY (Features all False) and what it should require once the
A+ program lands (Features True), so the same acceptance test tracks the migration.
"""
from __future__ import annotations

import copy
import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import jsonschema

SCHEMA = json.loads((Path(__file__).resolve().parents[2] / "schemas" / "appspec.schema.json").read_text())
CHART_VALUES = jsonschema.Draft202012Validator({"$defs": SCHEMA["$defs"], "$ref": "#/$defs/chartValues"})
CHART_KEYS = SCHEMA["$defs"]["chartValues"]["properties"]
CHART_DEFAULT_PORT = 8080                 # airframe-application's rollout.ports default and every stack XRD's port default
STACK_KIND = {"nodejs": "NodeJSApplication", "springboot": "SpringBootApplication",
              "go": "GoApplication", "python": "PythonApplication"}
PIPELINE_STAGES = {"build", "test", "deploy", "release"}
ENVS_DIR = "glidepath/envs"               # Glidepath ADR-0018/0019: one file per Ground environment in the app repo
BASE_FILE = "glidepath/base.yaml"         # optional values shared by every Ground environment (chart contract)
LEGACY_DEPLOY_KEYS = ("lowerEnvironments", "upperEnvironments", "promotionOrder")   # removed from the schema 2026-10-07


class NeedsInput(Exception):
    """The intent is ambiguous in a way a default should not paper over."""
    def __init__(self, question: str, options: list[str]):
        self.question, self.options = question, options
        super().__init__(question)


class SpecError(ValueError):
    pass


@dataclass(frozen=True)
class Registry:
    """What the planner knows about the fleet. Built from the platform's cluster registry (the fleet
    clusters.yaml that airframe's cluster-registry chart renders into the per-cluster ConfigMaps), never
    typed in: the dev cluster's canonical name, the upper clusters a flight environment may land on, and
    the tenants repo the dev cluster watches for XR requests."""
    dev_cluster: str                      # the value to put in devCluster (kind-dev, even on kiac-dev)
    upper_clusters: tuple[str, ...]
    tenants_repo: str                     # the dev cluster's tenantsRepo: where the XR requests are committed
    owner: str = "jfillman"

    @classmethod
    def from_fleet(cls, fleet: dict, owner: str = "jfillman") -> "Registry":
        """From the fleet record shape (`clusters:` list; name, zone, roles, tenantsRepo, aliases). The dev
        cluster is the one that runs the control plane; every other cluster in the upper zone takes flight
        environments. Aliases (kiac-dev for kind-dev) are not clusters, so they never appear here."""
        clusters = fleet.get("clusters") or []
        dev = [c for c in clusters if "control-plane" in (c.get("roles") or [])]
        if len(dev) != 1:
            raise SpecError(f"the cluster registry must name exactly one control-plane cluster, found {len(dev)}")
        if not dev[0].get("tenantsRepo"):
            raise SpecError(f"cluster {dev[0].get('name')!r} runs the control plane but declares no tenantsRepo")
        upper = tuple(c["name"] for c in clusters if c is not dev[0] and (c.get("zone") or "upper") == "upper")
        return cls(dev[0]["name"], upper, dev[0]["tenantsRepo"], owner)


@dataclass(frozen=True)
class Features:
    """What the current Airframe can do. False = not yet. True = shipped (rollout_guard: Airframe v0.3.91)."""
    rollout_guard: bool = True            # chart renders no Rollout until an image exists (v0.3.91)
    base_layer: bool = False              # a shared base values file under the per-env files
    release_split: bool = False           # machine-owned values live in their own file


@dataclass
class FileChange:
    path: str
    op: str                               # create | merge-patch
    content: Any                          # text for create, dict for merge-patch
    before: Any = None                    # merge-patch: the content it applies to (what Clearance's R021 diffs against)


@dataclass
class Step:
    id: str
    title: str
    repo: str
    gate: str                             # auto-merge | human-merge | wait | verify
    risk: str                             # T0 | T1 | T2
    depends_on: list[str] = field(default_factory=list)
    files: list[FileChange] = field(default_factory=list)
    wait_for: str | None = None
    effects: list[str] = field(default_factory=list)
    checks: list[str] = field(default_factory=list)


@dataclass
class ChangeSet:
    app: str
    steps: list[Step]
    assumptions: list[str]
    warnings: list[str]

    def step(self, sid: str) -> Step:
        return next(s for s in self.steps if s.id == sid)


def merge(a: dict, b: dict) -> dict:
    """Deep merge, b wins; lists are replaced, not concatenated."""
    out = copy.deepcopy(a)
    for k, v in b.items():
        out[k] = merge(out[k], v) if isinstance(v, dict) and isinstance(out.get(k), dict) else copy.deepcopy(v)
    return out


def env_config(spec: dict, env: str) -> dict:
    return merge(spec.get("config", {}), spec.get("overrides", {}).get(env, {}))


def risk(key: str) -> str:
    return CHART_KEYS.get(key, {}).get("x-hangar-risk", "unknown")


def route_hosts(c: dict) -> list[str]:
    """Hostnames an environment's values would serve: an enabled httpRoute's hostnames, an enabled ingress's host."""
    hosts = []
    if (c.get("httpRoute") or {}).get("enabled"):
        hosts += c["httpRoute"].get("hostnames") or []
    if (c.get("ingress") or {}).get("enabled") and c["ingress"].get("host"):
        hosts.append(c["ingress"]["host"])
    return hosts


def env_checks(env: str, c: dict) -> list[str]:
    """What the verify step looks for in one environment, for each claim its values make."""
    out = []
    if c.get("env"):
        out.append(f"{env}: rendered config contains " + ", ".join(
            f"{e['name']}={e['value']}" if "value" in e else e["name"] for e in c["env"]))
    r = c.get("rollout") or {}
    if r.get("steps"):
        out.append(f"{env}: rollout steps == {json.dumps(r['steps'], separators=(',', ':'))}")
    if "replicas" in r:
        out.append(f"{env}: Rollout spec.replicas == {r['replicas']}")
    if r.get("ports"):
        out.append(f"{env}: Service ports == " + ", ".join(f"{p['name']}:{p['containerPort']}" for p in r["ports"]))
    a = c.get("autoscaling") or {}
    if a.get("enabled"):
        out.append(f"{env}: HorizontalPodAutoscaler targets the Rollout (min {a.get('min', 2)}, max {a.get('max', 10)})")
    if (c.get("podDisruptionBudget") or {}).get("enabled"):
        out.append(f"{env}: PodDisruptionBudget exists for the Rollout's pods")
    for h in route_hosts(c):
        out.append(f"{env}: HTTPRoute for {h} is Accepted by its Gateway")
    return out


def xr_annotations(app: str, filename: str, reg: Registry) -> dict:
    owner = reg.owner
    src = {"pushToGit": True, "gitBranch": "main", "gitRepo": f"github.com?owner={owner}&repo={reg.tenants_repo}",
           "gitLayout": "custom", "basePath": ""}
    return {
        "terasky.backstage.io/source-info": json.dumps(src, separators=(",", ":")),
        "terasky.backstage.io/add-to-catalog": "true",
        "terasky.backstage.io/owner": f"group:default/{owner}",
        "terasky.backstage.io/system": f"app-{app}-cicd",
        "terasky.backstage.io/source-file-url": f"https://github.com/{owner}/{reg.tenants_repo}/blob/main/tenants/{app}/xr-requests/{filename}",
    }


def app_xr(spec: dict, reg: Registry) -> tuple[str, dict]:
    app, kind = spec["app"], STACK_KIND[spec["stack"]]
    opts = spec.get("stackOptions", {})
    body: dict[str, Any] = {"devCluster": reg.dev_cluster, "visibility": opts.get("visibility", "private"),
                            "description": spec.get("description", f"{app}")}
    if spec["stack"] == "python":
        body.update(pythonVersion=opts.get("pythonVersion", "3.12"), packageManager=opts.get("packageManager", "pip"),
                    port=opts.get("port", 8080))
    else:
        body["port"] = opts.get("port", 8080)
    fn = f"{app}.yaml"
    return fn, {"apiVersion": "catalog.hangar.io/v1alpha1", "kind": kind,
                "metadata": {"annotations": xr_annotations(app, fn, reg), "name": app, "namespace": f"app-{app}-cicd"},
                "spec": body}


def env_xr(app: str, cluster: str, env: str, reg: Registry) -> tuple[str, dict]:
    name = f"{app}-{cluster}-{env}"
    fn = f"{name}.yaml"
    return fn, {"apiVersion": "catalog.hangar.io/v1alpha1", "kind": "ApplicationEnvironment",
                "metadata": {"annotations": xr_annotations(app, fn, reg), "name": name, "namespace": f"app-{app}-cicd"},
                "spec": {"appName": app, "cluster": cluster, "configMapGenerator": False, "env": env}}


def resolve_flight(spec: dict, reg: Registry) -> tuple[list[dict], list[str]]:
    assumptions, out = [], []
    for e in spec["environments"]["flight"]:
        cluster = e.get("cluster")
        if cluster is None:
            if len(reg.upper_clusters) == 1:
                cluster = reg.upper_clusters[0]
                assumptions.append(f"flight environment '{e['name']}' has no cluster; the only upper cluster is {cluster}, so it goes there")
            elif not reg.upper_clusters:
                raise SpecError("no upper cluster is registered; a flight environment cannot be created")
            else:
                raise NeedsInput(f"Which upper cluster should flight environment '{e['name']}' use?", list(reg.upper_clusters))
        if cluster not in reg.upper_clusters:
            raise SpecError(f"{cluster!r} is not a registered upper cluster ({', '.join(reg.upper_clusters)})")
        out.append({"name": e["name"], "cluster": cluster})
    return out, assumptions


def _after(steps: list[dict], pred) -> int:
    """The index just past the last step matching pred (0 when none does)."""
    return max((i + 1 for i, st in enumerate(steps) if pred(st)), default=0)


def ground_cicd(base: dict, spec: dict) -> dict:
    """The part of the environment declaration an agent may write (Glidepath's GP-SCOPE rules, vendor/cicd_scope.py):
    Ground environments and their deploy steps, added to what the file already has.

    Nothing is removed or rewritten: every existing environment and step stays as it is, in place, so a check-ride
    step or a required test step the file carries survives (GP-SCOPE-002), and a Flight environment is never
    touched (GP-SCOPE-003). A missing Ground environment goes after the last Ground one (promotion order: Ground
    before Flight), and its deploy step after the last deploy step."""
    out = copy.deepcopy(base)
    dep = out.setdefault("deploy", {})
    envs = dep.setdefault("environments", [{"name": "dev", "tier": "ground"}])   # the schema's documented default, made explicit
    names = {e.get("name") for e in envs if isinstance(e, dict)}
    for g in spec["environments"]["ground"]:
        if g not in names:
            envs.insert(_after(envs, lambda e: isinstance(e, dict) and e.get("tier") == "ground"), {"name": g, "tier": "ground"})
            names.add(g)
    ci = out.setdefault("pipelines", {}).setdefault("ci", {"trigger": {"source": "git", "event": "push", "branch": "main"}})
    steps = ci.setdefault("steps", [])
    if not any(isinstance(st, dict) and st.get("stage") == "build" for st in steps):
        steps.insert(0, {"stage": "build"})
    for g in spec["environments"]["ground"]:
        if not any(isinstance(st, dict) and st.get("stage") == "deploy" and st.get("env") == g for st in steps):
            at = _after(steps, lambda st: isinstance(st, dict) and st.get("stage") in ("build", "deploy"))
            steps.insert(at, {"stage": "deploy", "env": g})
    return out


def flight_cicd(doc: dict, flight: list[dict]) -> dict:
    """What only a human may write: each Flight environment (GP-SCOPE-003; its `cluster` is a human field too), the
    release step into the first one, and dropping the pre-ADR-0019 deploy keys (no scope rule covers them, so
    removing one is a human change, GP-SCOPE-001)."""
    out = copy.deepcopy(doc)
    dep = out.setdefault("deploy", {})
    for k in LEGACY_DEPLOY_KEYS:
        dep.pop(k, None)
    envs = dep.setdefault("environments", [])
    have = {e.get("name") for e in envs if isinstance(e, dict)}
    for e in flight:
        if e["name"] not in have:
            envs.append({"name": e["name"], "tier": "flight", "cluster": e["cluster"]})
    if flight:
        steps = out.setdefault("pipelines", {}).setdefault("ci", {}).setdefault("steps", [])
        if not any(isinstance(st, dict) and st.get("stage") == "release" for st in steps):
            steps.append({"stage": "release", "env": flight[0]["name"]})
    return out


def patch_cicd(base: dict, spec: dict, flight: list[dict]) -> dict:
    """The whole environment declaration applied to a scaffolded cicd.yaml (ground_cicd, then flight_cicd). The rest
    of the file is left alone.

    Glidepath ADR-0019: one `deploy.environments` list, in promotion order, each entry a Ground environment
    (deployed on every push) or a Flight environment (deployed through a release). A flight entry names its cluster
    because a flight environment never runs on the app's own dev cluster. The pre-ADR keys are dropped if the
    scaffold still carries them: the schema rejects them."""
    return flight_cicd(ground_cicd(base, spec), flight)


def _cicd_patch(doc: dict, legacy_from: dict | None = None) -> dict:
    """A merge patch carrying deploy and pipelines whole (lists replace in RFC 7386, so they are sent entire);
    nulls remove the legacy keys `legacy_from` still has."""
    dep = dict(doc.get("deploy") or {})
    for k in LEGACY_DEPLOY_KEYS:
        if k in ((legacy_from or {}).get("deploy") or {}):
            dep[k] = None
    return {"deploy": dep, "pipelines": doc.get("pipelines") or {}}


def plan(spec_in: dict, reg: Registry, features: Features = Features(), base_cicd: dict | None = None) -> ChangeSet:
    try:
        jsonschema.validate(spec_in, SCHEMA)
    except jsonschema.ValidationError as e:
        path = "/".join(str(p) for p in e.absolute_path) or "<root>"
        raise SpecError(f"{path}: {e.message}") from e
    spec = copy.deepcopy(spec_in)
    app, owner = spec["app"], reg.owner
    ground = spec["environments"]["ground"]
    flight, assumptions = resolve_flight(spec, reg)
    warnings: list[str] = []
    all_envs = ground + [e["name"] for e in flight]
    if len(set(all_envs)) != len(all_envs):
        raise SpecError("an environment name is used twice across ground and flight")
    for k in spec.get("overrides") or {}:
        if k not in all_envs:
            raise SpecError(f"overrides/{k}: no environment named {k!r} ({', '.join(all_envs)})")

    port = (spec.get("stackOptions") or {}).get("port")
    if port is not None:
        rollout = spec.setdefault("config", {}).get("rollout") or {}
        if "ports" not in rollout:
            spec["config"]["rollout"] = {**rollout, "ports": [{"name": "http", "containerPort": port}]}
            if port != CHART_DEFAULT_PORT:
                assumptions.append(f"stackOptions.port {port}: the chart's rollout.ports is set to http:{port} "
                                   f"(its default is {CHART_DEFAULT_PORT}), so the Service reaches the scaffolded server")

    hosts: dict[str, list[str]] = {}
    for e in all_envs:
        c = env_config(spec, e)
        errs = sorted(CHART_VALUES.iter_errors(c), key=lambda x: list(x.absolute_path))
        if errs:
            path = "/".join(str(p) for p in errs[0].absolute_path) or "<root>"
            raise SpecError(f"{e} (config merged with overrides.{e}): {path}: {errs[0].message}")
        if port is not None and port not in [p["containerPort"] for p in (c.get("rollout") or {}).get("ports") or []]:
            raise SpecError(f"{e}: rollout.ports does not include stackOptions.port {port}; the Service would miss the scaffolded server")
        for h in route_hosts(c):
            hosts.setdefault(h, []).append(e)
    for h, envs in hosts.items():
        if len(envs) > 1:
            raise NeedsInput(f"Hostname {h} would be served by {', '.join(envs)}; a hostname belongs to one environment. "
                             f"Which one is it for? (Put it under overrides.<env>.)", envs)

    for e in all_envs:
        if e in PIPELINE_STAGES:
            warnings.append(f"environment '{e}' has the same name as a pipeline stage; cicd.yaml will read `stage: {e}, env: {e}`. Legal, but easy to misread.")
    cfg = spec.get("config", {})
    steps_cfg = (cfg.get("rollout") or {}).get("steps")
    if steps_cfg and len(steps_cfg) == 1 and steps_cfg[0].get("setWeight") == 100:
        warnings.append("a single 100% step promotes immediately: it is a valid canary but has no gradual ramp")
    floors: dict[tuple, list[str]] = {}
    for e in all_envs:
        c = env_config(spec, e)
        replicas, a = (c.get("rollout") or {}).get("replicas"), c.get("autoscaling") or {}
        if a.get("enabled") and replicas is not None and a.get("min", 2) < replicas:
            floors.setdefault((replicas, a.get("min", 2)), []).append(e)
    for (replicas, floor), envs in floors.items():
        warnings.append(f"{', '.join(envs)}: autoscaling is on and rollout.replicas is {replicas}, but the HPA owns the count and "
                        f"may scale down to autoscaling.min {floor}; set autoscaling.min: {replicas} if {replicas} is a floor")
    critical = sorted({k for e in all_envs for k in env_config(spec, e) if risk(k) == "critical"})
    if critical:
        warnings.append(f"{', '.join(critical)} is critical-risk: the app-repo step needs a human merge")

    cs: list[Step] = []
    # 1. tenants repo: the app XR and every flight environment, in one PR
    files = []
    fn, xr = app_xr(spec, reg)
    files.append(FileChange(f"tenants/{app}/xr-requests/{fn}", "create", xr))
    for e in flight:
        fn2, xr2 = env_xr(app, e["cluster"], e["name"], reg)
        files.append(FileChange(f"tenants/{app}/xr-requests/{fn2}", "create", xr2))
    cs.append(Step("tenants", f"Create {app} and its flight environments", reg.tenants_repo, "human-merge", "T2", files=files,
                   effects=[f"creates the source repo {owner}/{app} and the deploy repo {owner}/gitops-{app}",
                            "onboards the CI/CD pipeline", "commits per-app SecretStore XRs (the compositions do this themselves)"]
                   + [f"creates flight environment {e['name']} on {e['cluster']}" for e in flight]))
    cs.append(Step("repos-ready", "Wait for the repos and onboarding", reg.tenants_repo, "wait", "T0", depends_on=["tenants"],
                   wait_for=f"{STACK_KIND[spec['stack']]}/{app} CicdOnboarded=True and repos {app}, gitops-{app} exist"))

    # 2. app repo: cicd.yaml + ground environments
    def ground_file(env: str) -> dict:
        c = env_config(spec, env)
        d: dict[str, Any] = {"envName": env}
        if features.base_layer:
            d.update({k: v for k, v in (spec.get("overrides", {}).get(env) or {}).items()})
        else:
            d.update(c)
        return d

    # cicd.yaml in two halves: what an agent may change (Ground environments, their deploy steps) rides the
    # auto-merge app-repo step, and passes Clearance's R021 / `glidepath validate --agent`; the rest (Flight
    # environments, the release step, retiring pre-ADR keys) is its own step that a human merges.
    base = base_cicd or {}
    if base_cicd is None:
        assumptions.append("no current cicd.yaml was given: the cicd.yaml patches assume the scaffold's (dev only); "
                           "read the app's cicd.yaml and plan again before opening the pull request")
    legacy = [k for k in LEGACY_DEPLOY_KEYS if k in (base.get("deploy") or {})]
    ground_doc = base if legacy else ground_cicd(base, spec)
    final_doc = patch_cicd(base, spec, flight)
    if legacy:
        warnings.append(f"cicd.yaml still carries {', '.join(legacy)} (pre-ADR-0019): removing them is a human change, "
                        "so the whole cicd.yaml change is in the human-merge cicd-flight step")
    app_files = []
    if ground_doc != base:
        app_files.append(FileChange("cicd.yaml", "merge-patch", _cicd_patch(ground_doc), before=copy.deepcopy(base)))
    if features.base_layer:
        app_files.append(FileChange(BASE_FILE, "create", {k: v for k, v in cfg.items()}))
    for g in ground:
        app_files.append(FileChange(f"{ENVS_DIR}/{g}.yaml", "create", ground_file(g)))
    cs.append(Step("app-repo", f"Declare ground environments {', '.join(ground)}", f"{owner}/{app}",
                   "human-merge" if critical else "auto-merge", "T2" if critical else "T1",
                   depends_on=["repos-ready"], files=app_files,
                   effects=[f"creates namespace app-{app}-{g} for each ground environment" for g in ground]
                   + ["changing cicd.yaml regenerates .tekton/ in a follow-up PR"],
                   checks=["cicd.yaml validates against glidepath's cicd.schema.json"]))
    if final_doc != ground_doc:
        cs.append(Step("cicd-flight", "Declare the flight environments in cicd.yaml" if flight else "Retire the pre-ADR-0019 deploy keys",
                       f"{owner}/{app}", "human-merge", "T2", depends_on=["tenants", "app-repo"],
                       files=[FileChange("cicd.yaml", "merge-patch", _cicd_patch(final_doc, ground_doc), before=copy.deepcopy(ground_doc))],
                       effects=[f"declares flight environment {e['name']} on {e['cluster']}" for e in flight]
                       + ([f"adds the release step into {flight[0]['name']}"] if flight else [])
                       + ["Flight environments and their clusters are human-only cicd.yaml fields (GP-SCOPE-003, GP-SCOPE-001)"],
                       checks=["cicd.yaml validates against glidepath's cicd.schema.json"]))
    cs.append(Step("tekton-resync", "Merge the regenerated .tekton/ PR", f"{owner}/{app}", "auto-merge", "T1",
                   depends_on=["app-repo"], wait_for="the resync PR opened by the platform"))

    # 3. gitops repo: flight environment config
    if flight:
        gfiles = []
        if features.base_layer:
            gfiles.append(FileChange("base/values.yaml", "create", {k: v for k, v in cfg.items()}))
        for e in flight:
            patch = (spec.get("overrides") or {}).get(e["name"]) if features.base_layer else env_config(spec, e["name"])
            if patch:
                gfiles.append(FileChange(f"{e['cluster']}/{e['name']}/values.yaml", "merge-patch", patch))
        if gfiles:
            cs.append(Step("gitops", "Configure the flight environments", f"{owner}/gitops-{app}", "human-merge", "T2",
                           depends_on=["tenants", "repos-ready"], files=gfiles,
                           wait_for="each environment's values.yaml exists (the composition bootstraps it)",
                           effects=[f"changes what runs in {', '.join(e['name'] for e in flight)} after the next release"]))

    # 4. verify
    checks = [f"{STACK_KIND[spec['stack']]}/{app} Ready and CicdOnboarded",
              *[f"ApplicationEnvironment {app}-{e['cluster']}-{e['name']} Ready" for e in flight],
              *[f"namespace app-{app}-{g} exists and its Application is Synced" for g in ground]]
    for env in all_envs:
        checks += env_checks(env, env_config(spec, env))
    cs.append(Step("verify", "Verify", "", "verify", "T0", depends_on=[s.id for s in cs], checks=checks))
    return ChangeSet(app, cs, assumptions, warnings)
