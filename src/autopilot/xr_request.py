"""xr.request: one Airframe XR request, as a pull request on the dev cluster's tenants repo, opened as the agents' App.

Airframe's write path is a commit into tenants/<app>/xr-requests/ of the dev cluster's tenants repo that a person
merges (airframe docs/admin/governance.md, "The write path"); Argo CD's <app>-xr-requests Application applies it and
the composition does the rest. xr.request writes exactly that file and nothing else:

  tenants/<app>/xr-requests/<name>.yaml     one new XR, apiVersion catalog.hangar.io/v1alpha1, in app-<app>-cicd

The core has already decided the call (T1, target lower from `env`, the definition's tool grant); this module is the
second check and builds every byte of the file itself, so an agent supplies a kind, an optional name and the XR's
spec, never a path, namespace, annotation or apiVersion:

  - `kind` is one an app requests for itself: Airframe's contract (schemas/airframe-contract.json) lists it with role
    dependency, observability or environment. Bootstrap kinds (a new repo), platform kinds (Agent, AgentRun,
    SecretStore, TektonCICD) and anything unknown are refused, so an agent never pins an agent or makes a repo.
  - `env` is a lower environment (dispatch.LOWER_ENVS, pr-*, agent-*). An ApplicationEnvironment is that env on this
    cluster, named <app>-<cluster>-<env> as Airframe's own planner names it (airframe_plan.env_xr), its appName,
    cluster and env set here.
  - The app is onboarded (tenants/<app>/ exists), the file does not exist yet (a request is new; changing one is a
    repo.pr.open a person reviews like any other), and the spec is a mapping under MAX_SPEC_BYTES with no metadata.

The PR is opened by github_pr.GitHubPRs.open_pr on the tenants repo with the broker token minted for <app>, carrying
the agent, task, session and run like every agent PR; the tenants repo's own checks and a person's merge follow.
"""
from __future__ import annotations

import json
import re
from typing import Any

import yaml

from .engine.dispatch import LOWER_ENVS, Denied
from . import airframe_plan

API_VERSION = "catalog.hangar.io/v1alpha1"
REQUESTABLE_ROLES = frozenset({"dependency", "observability", "environment"})
MAX_SPEC_BYTES = 32 << 10
_LABEL = re.compile(r"^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$")


def requestable_kinds(contract: dict[str, Any]) -> dict[str, str]:
    """{kind: role} for every kind an app may request for itself, from Airframe's contract."""
    return {k["kind"]: k["role"] for k in (contract.get("kinds") or {}).values()
            if isinstance(k, dict) and k.get("role") in REQUESTABLE_ROLES and isinstance(k.get("kind"), str)}


def is_lower(env: str) -> bool:
    return env in LOWER_ENVS or env.startswith(("pr-", "agent-"))


def build(app: str, env: str, kind: str, spec: Any, name: str | None, reg: airframe_plan.Registry,
          contract: dict[str, Any]) -> tuple[str, str, dict[str, Any]]:
    """(path, YAML text, the XR) for one request, or Denied with a hint the agent can act on."""
    if not isinstance(app, str) or not _LABEL.match(app):
        raise Denied("XR-APP", f"{app!r} is not an app name", "Pass the app's name as in tenants/<app>/ (e.g. flight-api).")
    if not isinstance(env, str) or not _LABEL.match(env) or not is_lower(env):
        raise Denied("XR-ENV", f"{env!r} is not a lower environment",
                     "xr.request asks for lower environments only (dev, pr-*, agent-*); an upper one is a Flight "
                     "change a person makes.")
    kinds = requestable_kinds(contract)
    if kind not in kinds:
        raise Denied("XR-KIND", f"{kind!r} is not a kind an app requests through xr.request",
                     f"Request one of {', '.join(sorted(kinds))} (airframe.capabilities describes each).")
    if not isinstance(spec, dict) or "metadata" in spec:
        raise Denied("XR-SPEC", "spec is the XR's spec mapping, without metadata",
                     "Pass only the fields under spec: (airframe.capabilities lists them for the kind).")
    if len(json.dumps(spec, default=str)) > MAX_SPEC_BYTES:
        raise Denied("XR-SPEC", f"spec is larger than {MAX_SPEC_BYTES} bytes", "Request a smaller XR.")
    spec = dict(spec)
    if kind == "ApplicationEnvironment":
        fixed = {"appName": app, "cluster": reg.dev_cluster, "env": env}
        clash = [k for k, v in fixed.items() if k in spec and spec[k] != v]
        if clash:
            raise Denied("XR-SPEC", f"spec.{clash[0]} must be {fixed[clash[0]]!r} for this request",
                         "An ApplicationEnvironment from xr.request is the app's env on this cluster; leave those "
                         "fields out.")
        spec = {**fixed, **{k: v for k, v in spec.items() if k not in fixed}}
        want = f"{app}-{reg.dev_cluster}-{env}"
        if name not in (None, "", want):
            raise Denied("XR-NAME", f"an ApplicationEnvironment for {app} {env} is named {want}", "Leave name out.")
        name = want
    else:
        name = name or f"{app}-{env}-{kind.lower()}"
    if not isinstance(name, str) or not _LABEL.match(name):
        raise Denied("XR-NAME", f"{name!r} is not an XR name", "Use a DNS label: lowercase, digits and '-', at most 63.")
    filename = f"{name}.yaml"
    xr = {"apiVersion": API_VERSION, "kind": kind,
          "metadata": {"name": name, "namespace": f"app-{app}-cicd",
                       "annotations": airframe_plan.xr_annotations(app, filename, reg)},
          "spec": spec}
    text = ("# Requested by a Hangar agent through xr.request; merged by a person.\n"
            + yaml.safe_dump(xr, sort_keys=False, default_flow_style=False))
    return f"tenants/{app}/xr-requests/{filename}", text, xr
