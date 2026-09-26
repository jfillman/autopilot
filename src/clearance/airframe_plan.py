"""AppSpec -> ChangeSet: the planner.

One statement of intent ("a Python app called parachute with dev and test ground environments and
staging and prod flight environments, one 100% canary step, and a URL env var") becomes an ordered
set of repo changes, each with its gate, its dependencies and what it will do. A pure function: no
network, no clock, same input gives the same output, so it is tested without a cluster and safe to
re-run.

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
STACK_KIND = {"nodejs": "NodeJSApplication", "springboot": "SpringBootApplication",
              "go": "GoApplication", "python": "PythonApplication"}
PIPELINE_STAGES = {"build", "test", "deploy", "release"}
TENANTS_REPO = "gitops-cluster-dev-tenants"


class NeedsInput(Exception):
    """The intent is ambiguous in a way a default should not paper over."""
    def __init__(self, question: str, options: list[str]):
        self.question, self.options = question, options
        super().__init__(question)


class SpecError(ValueError):
    pass


@dataclass(frozen=True)
class Registry:
    dev_cluster: str                      # the value to put in devCluster (kind-dev, even on kiac-dev)
    upper_clusters: tuple[str, ...]
    owner: str = "jfillman"


@dataclass(frozen=True)
class Features:
    """What the current Airframe can do. False = today. True = after the A+ program."""
    rollout_guard: bool = False           # chart renders no Rollout until an image exists
    base_layer: bool = False              # a shared base values file under the per-env files
    release_split: bool = False           # machine-owned values live in their own file


@dataclass
class FileChange:
    path: str
    op: str                               # create | merge-patch
    content: Any                          # text for create, dict for merge-patch


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


def xr_annotations(app: str, filename: str, owner: str) -> dict:
    src = {"pushToGit": True, "gitBranch": "main", "gitRepo": f"github.com?owner={owner}&repo={TENANTS_REPO}",
           "gitLayout": "custom", "basePath": ""}
    return {
        "terasky.backstage.io/source-info": json.dumps(src, separators=(",", ":")),
        "terasky.backstage.io/add-to-catalog": "true",
        "terasky.backstage.io/owner": f"group:default/{owner}",
        "terasky.backstage.io/system": f"app-{app}-cicd",
        "terasky.backstage.io/source-file-url": f"https://github.com/{owner}/{TENANTS_REPO}/blob/main/tenants/{app}/xr-requests/{filename}",
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
    return fn, {"apiVersion": "catalog.idp.io/v1alpha1", "kind": kind,
                "metadata": {"annotations": xr_annotations(app, fn, reg.owner), "name": app, "namespace": f"app-{app}-cicd"},
                "spec": body}


def env_xr(app: str, cluster: str, env: str, owner: str) -> tuple[str, dict]:
    name = f"{app}-{cluster}-{env}"
    fn = f"{name}.yaml"
    return fn, {"apiVersion": "catalog.idp.io/v1alpha1", "kind": "ApplicationEnvironment",
                "metadata": {"annotations": xr_annotations(app, fn, owner), "name": name, "namespace": f"app-{app}-cicd"},
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


def patch_cicd(base: dict, spec: dict, flight: list[dict]) -> dict:
    """Apply the environment declaration to a scaffolded cicd.yaml. The rest of the file is left alone."""
    ground = spec["environments"]["ground"]
    out = copy.deepcopy(base)
    dep = out.setdefault("deploy", {})
    dep["lowerEnvironments"] = list(ground)
    dep["upperEnvironments"] = [dict(e) for e in flight]
    dep["promotionOrder"] = list(ground) + [e["name"] for e in flight]
    steps = [{"stage": "build"}] + [{"stage": "deploy", "env": g} for g in ground]
    if flight:
        steps.append({"stage": "release", "env": flight[0]["name"]})
    ci = out.setdefault("pipelines", {}).setdefault("ci", {"trigger": {"source": "git", "event": "push", "branch": "main"}})
    ci["steps"] = steps
    return out


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
    for e in all_envs:
        if e in PIPELINE_STAGES:
            warnings.append(f"environment '{e}' has the same name as a pipeline stage; cicd.yaml will read `stage: {e}, env: {e}`. Legal, but easy to misread.")
    cfg = spec.get("config", {})
    steps_cfg = (cfg.get("rollout") or {}).get("steps")
    if steps_cfg and len(steps_cfg) == 1 and steps_cfg[0].get("setWeight") == 100:
        warnings.append("a single 100% step promotes immediately: it is a valid canary but has no gradual ramp")

    cs: list[Step] = []
    # 1. tenants repo: the app XR and every flight environment, in one PR
    files = []
    fn, xr = app_xr(spec, reg)
    files.append(FileChange(f"tenants/{app}/xr-requests/{fn}", "create", xr))
    for e in flight:
        fn2, xr2 = env_xr(app, e["cluster"], e["name"], owner)
        files.append(FileChange(f"tenants/{app}/xr-requests/{fn2}", "create", xr2))
    cs.append(Step("tenants", f"Create {app} and its flight environments", TENANTS_REPO, "human-merge", "T2", files=files,
                   effects=[f"creates the source repo {owner}/{app} and the deploy repo {owner}/gitops-{app}",
                            "onboards the CI/CD pipeline", "commits per-app SecretStore XRs (the compositions do this themselves)"]
                   + [f"creates flight environment {e['name']} on {e['cluster']}" for e in flight]))
    cs.append(Step("repos-ready", "Wait for the repos and onboarding", TENANTS_REPO, "wait", "T0", depends_on=["tenants"],
                   wait_for=f"{STACK_KIND[spec['stack']]}/{app} CicdOnboarded=True and repos {app}, gitops-{app} exist"))

    # 2. app repo: cicd.yaml + ground environments
    def ground_file(env: str) -> dict:
        c = env_config(spec, env)
        d: dict[str, Any] = {"envName": env}
        if features.base_layer:
            d.update({k: v for k, v in (spec.get("overrides", {}).get(env) or {}).items()})
        else:
            if "env" in c:
                d["env"] = c["env"]
            if features.rollout_guard:
                if c.get("rollout"):
                    d["rollout"] = c["rollout"]
            else:
                d["rollout"] = None   # explicit: omitting it renders an empty-image rollout today
        return d

    app_files = []
    cicd_patch = patch_cicd(base_cicd or {}, spec, flight)
    app_files.append(FileChange("cicd.yaml", "merge-patch", {"deploy": cicd_patch["deploy"], "pipelines": cicd_patch["pipelines"]}))
    if features.base_layer:
        app_files.append(FileChange("platform/base.yaml", "create", {k: v for k, v in cfg.items()}))
    for g in ground:
        app_files.append(FileChange(f"platform/envs/{g}.yaml", "create", ground_file(g)))
    cs.append(Step("app-repo", f"Declare ground environments {', '.join(ground)}", f"{owner}/{app}", "auto-merge", "T1",
                   depends_on=["repos-ready"], files=app_files,
                   effects=[f"creates namespace app-{app}-{g} for each ground environment" for g in ground]
                   + ["changing cicd.yaml regenerates .tekton/ in a follow-up PR"],
                   checks=["cicd.yaml validates against glidepath's cicd.schema.json"]))
    cs.append(Step("tekton-resync", "Merge the regenerated .tekton/ PR", f"{owner}/{app}", "auto-merge", "T1",
                   depends_on=["app-repo"], wait_for="the resync PR opened by the platform"))

    # 3. gitops repo: flight environment config
    if flight:
        gfiles = []
        if features.base_layer:
            gfiles.append(FileChange("base/values.yaml", "create", {k: v for k, v in cfg.items()}))
        else:
            for e in flight:
                c = env_config(spec, e["name"])
                patch = {k: v for k, v in c.items() if k in ("rollout", "env")}
                if patch:
                    gfiles.append(FileChange(f"{e['cluster']}/{e['name']}/values.yaml", "merge-patch", patch))
        if gfiles:
            cs.append(Step("gitops", "Configure the flight environments", f"{owner}/gitops-{app}", "human-merge", "T2",
                           depends_on=["tenants", "repos-ready"], files=gfiles,
                           wait_for="each environment's values.yaml exists (the composition bootstraps it)",
                           effects=["changes what runs in staging and prod after the next release"]))

    # 4. what the guard-less chart forces: rollout config only after an image exists
    if not features.rollout_guard and (cfg.get("rollout") or any((spec.get("overrides", {}).get(g) or {}).get("rollout") for g in ground)):
        gpatch = [FileChange(f"platform/envs/{g}.yaml", "merge-patch", {"rollout": env_config(spec, g).get("rollout")})
                  for g in ground if env_config(spec, g).get("rollout")]
        cs.append(Step("ground-rollout", "Set rollout config once the first image is deployed", f"{owner}/{app}", "auto-merge", "T1",
                       depends_on=["tekton-resync"], files=gpatch,
                       wait_for="the deploy stage has committed rollout.image into each ground environment",
                       effects=["needed only because the chart renders an empty-image Rollout if rollout config exists before an image"]))
        warnings.append("Airframe's chart has no empty-image guard, so ground rollout config is applied after the first deploy")

    # 5. verify
    checks = [f"{STACK_KIND[spec['stack']]}/{app} Ready and CicdOnboarded",
              *[f"ApplicationEnvironment {app}-{e['cluster']}-{e['name']} Ready" for e in flight],
              *[f"namespace app-{app}-{g} exists and its Application is Synced" for g in ground]]
    for env in all_envs:
        c = env_config(spec, env)
        if c.get("env"):
            checks.append(f"{env}: rendered config contains " + ", ".join(f"{e['name']}={e['value']}" for e in c["env"]))
        if (c.get("rollout") or {}).get("steps"):
            checks.append(f"{env}: rollout steps == {json.dumps(c['rollout']['steps'], separators=(',', ':'))}")
    cs.append(Step("verify", "Verify", "", "verify", "T0", depends_on=[s.id for s in cs], checks=checks))
    return ChangeSet(app, cs, assumptions, warnings)
