"""function-agentrun: the AgentRun composition logic, as a pure function.

Everything the AgentRun composition decides is decided here, from four inputs: the XR, what has been observed (the
run's Sandbox, the XR's last status, the cluster registry record), the current time and the cluster's config. No
I/O, so it is tested without a cluster (tests/test_compose.py). fn.py is a thin protobuf wrapper around compose().

Three properties matter more than the rest:

1. Expiry needs no Clearance and no Crossplane. The run's pod is an agent-sandbox `Sandbox` whose shutdownTime is
   spec.expiresAt and whose shutdownPolicy is Delete, so the agent-sandbox controller deletes it within about a
   second of the deadline (roadmap U11). The pod's activeDeadlineSeconds, anchored to the XR's creation, is a
   kubelet-enforced backstop if that controller is down. Past expiresAt this function also renders NOTHING, so
   Crossplane removes the namespace on its next reconcile (about every 60 s, U2). A dead gateway, a dead function
   pod or a slow Crossplane cannot keep a run alive past its deadline.
2. It fails closed. A request this cluster cannot honour exactly (no autopilotReady in the registry, a hardened
   sandbox with no runtime class, a compute class the cluster lacks, an oversized TTL or input, an over-broad
   allowlist) is Rejected and renders nothing. It is never quietly downgraded.
3. Final phases are final. Expired, Drained and Rejected are read back from the XR's own status and never render
   again: a drained run whose Sandbox is gone must not look "not created yet" and start a second time.

The run namespace never carries hangar.io/ephemeral-env: the PR-namespace TTL sweep deletes namespaces with that
label whose owning Argo CD Application is gone, and would reap live runs. It carries hangar.io/agent-run instead,
which the cluster's admission policy for provider-kubernetes requires (gitops <cluster>/55-autopilot/agentrun-grants).
"""
from __future__ import annotations

import ipaddress
import json
import re
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any

PROVIDER_CONFIG = {"kind": "ClusterProviderConfig", "name": "default"}
OBJECT_API = "kubernetes.m.crossplane.io/v1alpha1"
SANDBOX_API = "agents.x-k8s.io/v1beta1"
LABEL_RUN = "hangar.io/agent-run"
# Deliberately NOT hangar.io/ephemeral-env (see the module docstring). tests/test_compose.py asserts it is absent.
FORBIDDEN_LABELS = ("hangar.io/ephemeral-env",)
RUN_NAME = re.compile(r"^r-[a-f0-9]{6,32}$")
MAX_INPUT_BYTES = 4096
SANDBOX_NAME = "agent"
RUN_SERVICE_ACCOUNT = "run"
SIDECAR_REQUESTS = {"cpu": "50m", "memory": "64Mi"}
SIDECAR_LIMITS = {"cpu": "500m", "memory": "512Mi"}
FINAL_PHASES = ("Expired", "Drained", "Rejected")

# RunReady: the XR's own condition, a closed reason set (xrds/agentrun.meta.yaml lists the same set; the sidecar
# test holds them together). Status True means the agent is up or has finished well.
RUN_READY = "RunReady"
REASONS = {
    "Provisioning": ("AgentRunProvisioning", "False"),
    "Running": ("AgentRunRunning", "True"),
    "Succeeded": ("AgentRunSucceeded", "True"),
    "Failed": ("AgentRunFailed", "False"),
    "Frozen": ("AgentRunFrozen", "False"),
    "Expired": ("AgentRunExpired", "False"),
    "Drained": ("AgentRunDrained", "False"),
    "Rejected": ("AgentRunRejected", "False"),
}


@dataclass
class ComputeClassCfg:
    requests: dict[str, str]
    limits: dict[str, str]
    node_selector: dict[str, str] = field(default_factory=dict)
    tolerations: list[dict[str, Any]] = field(default_factory=list)
    pods: int = 2


@dataclass
class ClusterConfig:
    """The Composition's function input (snake_case keys), with the defaults a dev cluster gets."""
    max_ttl_minutes: int = 240
    runs_namespace: str = "autopilot-runs"
    registry_namespace: str = "crossplane-system"
    clearance_namespace: str = "autopilot-system"
    clearance_selector: dict[str, str] = field(default_factory=lambda: {"app.kubernetes.io/name": "clearance"})
    clearance_port: int = 8080
    model_proxy_selector: dict[str, str] = field(default_factory=lambda: {"app.kubernetes.io/name": "model-proxy"})
    model_proxy_port: int = 8081
    dns_namespace: str = "kube-system"
    dns_selector: dict[str, str] = field(default_factory=lambda: {"k8s-app": "kube-dns"})
    runtime_classes: dict[str, str] = field(default_factory=dict)     # {"hardened": "gvisor"}; empty on the home lab
    compute: dict[str, ComputeClassCfg] = field(default_factory=lambda: {
        "small": ComputeClassCfg({"cpu": "250m", "memory": "512Mi"}, {"cpu": "1", "memory": "1Gi"}),
        "medium": ComputeClassCfg({"cpu": "500m", "memory": "1Gi"}, {"cpu": "2", "memory": "4Gi"}),
        "large": ComputeClassCfg({"cpu": "2", "memory": "4Gi"}, {"cpu": "4", "memory": "16Gi"}),
    })
    workspace_size: str = "1Gi"
    completion_grace_minutes: int = 5

    @classmethod
    def from_input(cls, d: dict[str, Any] | None) -> "ClusterConfig":
        c = cls()
        d = ints(dict(d or {}))
        d.pop("apiVersion", None)
        d.pop("kind", None)
        for k in ("max_ttl_minutes", "runs_namespace", "registry_namespace", "clearance_namespace", "clearance_port",
                  "model_proxy_port", "dns_namespace", "workspace_size", "completion_grace_minutes"):
            if k in d:
                setattr(c, k, d.pop(k))
        for k in ("clearance_selector", "model_proxy_selector", "dns_selector", "runtime_classes"):
            if k in d:
                setattr(c, k, dict(d.pop(k)))
        if "compute" in d:
            c.compute = {k: ComputeClassCfg(dict(v["requests"]), dict(v["limits"]), dict(v.get("nodeSelector", {})),
                                            list(v.get("tolerations", [])), int(v.get("pods", 2)))
                         for k, v in d.pop("compute").items()}
        if d:
            # An unknown key is a typo in the Composition: fail loudly rather than run with a default.
            raise ValueError(f"function-agentrun input: unknown keys {sorted(d)}")
        return c


@dataclass
class Composition:
    resources: dict[str, dict[str, Any]]
    status: dict[str, Any]
    ttl_seconds: int
    condition: tuple[str, str, str, str] = ("", "", "", "")   # (type, status, reason, message)
    ready: dict[str, bool] = field(default_factory=dict)       # composition-resource-name -> ready


def ints(v: Any) -> Any:
    """Integral floats back to ints, recursively. Crossplane hands the XR and the input over as protobuf Structs,
    whose only number type is a double: without this, limits {toolCalls: 100} reach the run as 100.0."""
    if isinstance(v, float) and v.is_integer():
        return int(v)
    if isinstance(v, dict):
        return {k: ints(x) for k, x in v.items()}
    if isinstance(v, list):
        return [ints(x) for x in v]
    return v


def parse_ts(s: str) -> datetime:
    return datetime.fromisoformat(s.replace("Z", "+00:00")).astimezone(timezone.utc)


def iso(dt: datetime) -> str:
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def ns_name(xr: dict[str, Any]) -> str:
    return "agent-" + xr["metadata"]["name"]


def obj(manifest: dict[str, Any], readiness: str | None = None) -> dict[str, Any]:
    spec: dict[str, Any] = {"forProvider": {"manifest": manifest}, "providerConfigRef": dict(PROVIDER_CONFIG)}
    if readiness:
        spec["readiness"] = {"policy": readiness}
    return {"apiVersion": OBJECT_API, "kind": "Object", "spec": spec}


def _labels(xr: dict[str, Any], extra: dict[str, str] | None = None) -> dict[str, str]:
    spec = xr["spec"]
    base = {"hangar.io/component": "autopilot", LABEL_RUN: "true",
            "hangar.io/agent": spec["agent"], "hangar.io/task": spec["taskId"],
            "hangar.io/session": spec["sessionId"]}
    base.update(extra or {})
    return base


# ---- quantities (only the units the compute classes use) ----------------------------------------------------
_MEM = {"Ki": 2**10, "Mi": 2**20, "Gi": 2**30, "Ti": 2**40, "": 1}


def _cpu_milli(q: str) -> int:
    return int(q[:-1]) if q.endswith("m") else int(float(q) * 1000)


def _mem_bytes(q: str) -> int:
    m = re.fullmatch(r"(\d+)(Ki|Mi|Gi|Ti|)", q)
    if not m:
        raise ValueError(f"memory quantity {q!r}: use Ki, Mi, Gi or Ti")
    return int(m.group(1)) * _MEM[m.group(2)]


def _fmt_cpu(milli: int) -> str:
    return f"{milli // 1000}" if milli % 1000 == 0 else f"{milli}m"


def _fmt_mem(b: int) -> str:
    for unit in ("Gi", "Mi", "Ki"):
        if b % _MEM[unit] == 0:
            return f"{b // _MEM[unit]}{unit}"
    return str(b)


def _sum(kind: str, qs: list[str]) -> str:
    if kind == "cpu":
        return _fmt_cpu(sum(_cpu_milli(q) for q in qs))
    return _fmt_mem(sum(_mem_bytes(q) for q in qs))


# ---- admission: what the cluster can honour exactly ---------------------------------------------------------
def registry_problem(record: dict[str, Any] | None, cluster: str) -> str | None:
    """The cluster registry gate. record is the ConfigMap's data, or None when the lookup found nothing."""
    if not cluster:
        return "function-agentrun does not know which cluster it runs on (HANGAR_CLUSTER_NAME is unset in its runtime config)"
    if record is None:
        return f"no cluster registry record named {cluster!r}: AgentRuns run only on a registered control-plane cluster"
    if record.get("type") != "dev":
        return f"cluster {cluster!r} is type {record.get('type')!r}: AgentRuns run only on the control-plane (dev) cluster"
    if record.get("autopilotReady") != "true":
        return (f"cluster {cluster!r} is not autopilotReady: set airframe.autopilotReady in clusters.yaml only after "
                "autopilot's tools/netpol_canary.py passes on it")
    return None


def validate(xr: dict[str, Any], cfg: ClusterConfig, created: datetime) -> str | None:
    md = xr["metadata"]
    spec = xr["spec"]
    if not RUN_NAME.match(md.get("name", "")):
        return "an AgentRun is named r-<6 to 32 hex> (the XRD enforces it; this is the function's own check)"
    if md.get("namespace") != cfg.runs_namespace:
        return f"AgentRuns live in the namespace {cfg.runs_namespace!r}, not {md.get('namespace')!r}"
    expires = parse_ts(spec["expiresAt"])
    if expires - created > timedelta(minutes=cfg.max_ttl_minutes):
        return f"ttl exceeds this cluster's cap of {cfg.max_ttl_minutes} minutes"
    if spec["sandbox"] == "hardened" and "hardened" not in cfg.runtime_classes:
        return "hardened sandbox requested but this cluster has no runtime class for it"
    if spec["compute"] not in cfg.compute:
        return f"compute class {spec['compute']!r} is not available on this cluster"
    net = spec["network"]
    if net["mode"] == "allowlist":
        cidrs = net.get("allowCidrs") or []
        if not cidrs:
            return "allowlist network mode needs allowCidrs"
        for c in cidrs:
            n = ipaddress.ip_network(c, strict=False)
            if n.prefixlen == 0 or n.is_loopback or n.is_link_local:
                return f"allowCidr {c} is too broad or not routable"
    elif net.get("allowCidrs"):
        return "allowCidrs is only valid with network mode allowlist"
    if len(json.dumps(spec.get("input", {}), separators=(",", ":"))) > MAX_INPUT_BYTES:
        return "input is larger than 4 KiB; pass a reference instead"
    return None


# ---- rendering ----------------------------------------------------------------------------------------------
def _restricted_security(user: int = 10001) -> dict[str, Any]:
    return {"runAsNonRoot": True, "runAsUser": user, "runAsGroup": user, "readOnlyRootFilesystem": True,
            "allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]},
            "seccompProfile": {"type": "RuntimeDefault"}}


def _dns_rule(cfg: ClusterConfig) -> dict[str, Any]:
    return {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": cfg.dns_namespace}},
                    "podSelector": {"matchLabels": dict(cfg.dns_selector)}}],
            "ports": [{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}]}


def egress_rules(net: dict[str, Any], cfg: ClusterConfig) -> list[dict[str, Any]]:
    mode = net["mode"]
    if mode == "none":
        return []
    rules = [_dns_rule(cfg)]
    rules.append({"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": cfg.clearance_namespace}},
                          "podSelector": {"matchLabels": dict(cfg.clearance_selector)}}],
                  "ports": [{"protocol": "TCP", "port": cfg.clearance_port}]})
    if mode in ("clearance+model", "allowlist"):
        rules.append({"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": cfg.clearance_namespace}},
                              "podSelector": {"matchLabels": dict(cfg.model_proxy_selector)}}],
                      "ports": [{"protocol": "TCP", "port": cfg.model_proxy_port}]})
    if mode == "allowlist":
        rules.append({"to": [{"ipBlock": {"cidr": c}} for c in net["allowCidrs"]]})
    return rules


def _pod_spec(xr: dict[str, Any], cfg: ClusterConfig, created: datetime) -> dict[str, Any]:
    spec = xr["spec"]
    cc = cfg.compute[spec["compute"]]
    expires = parse_ts(spec["expiresAt"])
    mode = spec["network"]["mode"]
    env = [
        {"name": "HANGAR_RUN_ID", "value": xr["metadata"]["name"]},
        {"name": "HANGAR_TASK_ID", "value": spec["taskId"]},
        {"name": "HANGAR_SESSION_ID", "value": spec["sessionId"]},
        {"name": "HANGAR_AGENT", "value": spec["agent"]},
        {"name": "HANGAR_EXPIRES_AT", "value": spec["expiresAt"]},
        {"name": "HANGAR_OUTPUT_DIR", "value": "/run/output"},
        {"name": "HANGAR_WORKSPACE", "value": "/workspace"},
        {"name": "HANGAR_LIMITS", "value": json.dumps(spec["limits"], sort_keys=True, separators=(",", ":"))},
    ]
    volumes: list[dict[str, Any]] = [
        {"name": "workspace", "emptyDir": {"sizeLimit": cfg.workspace_size}},
        {"name": "output", "emptyDir": {"sizeLimit": "256Mi"}},
        {"name": "tmp", "emptyDir": {"sizeLimit": "256Mi"}},
    ]
    mounts = [{"name": "workspace", "mountPath": "/workspace"}, {"name": "output", "mountPath": "/run/output"},
              {"name": "tmp", "mountPath": "/tmp"}]
    sources: list[dict[str, Any]] = []
    if mode != "none":
        env.append({"name": "CLEARANCE_URL",
                    "value": f"http://clearance.{cfg.clearance_namespace}.svc:{cfg.clearance_port}"})
        sources.append({"serviceAccountToken": {"audience": "clearance", "expirationSeconds": 3600,
                                                "path": "clearance-token"}})
    if mode in ("clearance+model", "allowlist"):
        env.append({"name": "MODEL_PROXY_URL",
                    "value": f"http://model-proxy.{cfg.clearance_namespace}.svc:{cfg.model_proxy_port}"})
        sources.append({"serviceAccountToken": {"audience": "model-proxy", "expirationSeconds": 3600,
                                                "path": "model-proxy-token"}})
    if sources:
        volumes.append({"name": "hangar-tokens", "projected": {"sources": sources}})
        mounts.append({"name": "hangar-tokens", "mountPath": "/var/run/hangar", "readOnly": True})
        env.append({"name": "HANGAR_TOKEN_DIR", "value": "/var/run/hangar"})
    if spec.get("input"):
        env.append({"name": "HANGAR_INPUT", "value": json.dumps(spec["input"], sort_keys=True, separators=(",", ":"))})
    container = {
        "name": "agent", "image": spec["image"], "imagePullPolicy": "IfNotPresent", "env": env,
        "resources": {"requests": dict(cc.requests), "limits": dict(cc.limits)},
        "securityContext": _restricted_security(), "volumeMounts": mounts,
    }
    sidecars = [{
        "name": s["name"], "image": s["image"], "imagePullPolicy": "IfNotPresent",
        "resources": {"requests": dict(SIDECAR_REQUESTS), "limits": dict(SIDECAR_LIMITS)},
        "securityContext": _restricted_security(), "volumeMounts": [{"name": "tmp", "mountPath": "/tmp"}],
    } for s in spec.get("sidecars", [])]
    pod: dict[str, Any] = {
        "serviceAccountName": RUN_SERVICE_ACCOUNT, "automountServiceAccountToken": False, "restartPolicy": "Never",
        "terminationGracePeriodSeconds": 30, "enableServiceLinks": False,
        # Backstop only: the kubelet ends the pod this long after it starts even if the agent-sandbox controller is
        # down. Anchored to the XR's creation, so the template is identical on every reconcile (no drift, no
        # spurious Object updates). shutdownTime is the real deadline.
        "activeDeadlineSeconds": max(1, int((expires - created).total_seconds())),
        "securityContext": {"runAsNonRoot": True, "seccompProfile": {"type": "RuntimeDefault"}},
        "containers": [container] + sidecars, "volumes": volumes,
    }
    if spec["sandbox"] == "hardened":
        pod["runtimeClassName"] = cfg.runtime_classes["hardened"]
    if cc.node_selector:
        pod["nodeSelector"] = dict(cc.node_selector)
    if cc.tolerations:
        pod["tolerations"] = list(cc.tolerations)
    return pod


def _sandbox(xr: dict[str, Any], cfg: ClusterConfig, ns: str, created: datetime) -> dict[str, Any]:
    return {
        "apiVersion": SANDBOX_API, "kind": "Sandbox",
        "metadata": {"name": SANDBOX_NAME, "namespace": ns, "labels": _labels(xr)},
        "spec": {
            # The hard deadline. The agent-sandbox controller deletes the pod at this instant, then (Delete) the
            # Sandbox itself, independent of Crossplane and of this function.
            "shutdownTime": xr["spec"]["expiresAt"],
            "shutdownPolicy": "Delete",
            # A failed agent is a result to report, not something to restart.
            "failedPodPolicy": "Retain",
            # No headless Service: the quota forbids Services and nothing calls into a run.
            "service": False,
            "operatingMode": "Running",
            "podTemplate": {"metadata": {"labels": _labels(xr, {"app.kubernetes.io/name": "agent-run"})},
                            "spec": _pod_spec(xr, cfg, created)},
        },
    }


def _quota(xr: dict[str, Any], cfg: ClusterConfig, ns: str) -> dict[str, Any]:
    spec = xr["spec"]
    cc = cfg.compute[spec["compute"]]
    n = len(spec.get("sidecars", []))
    req = {k: _sum(k, [cc.requests[k]] + [SIDECAR_REQUESTS[k]] * n) for k in ("cpu", "memory")}
    lim = {k: _sum(k, [cc.limits[k]] + [SIDECAR_LIMITS[k]] * n) for k in ("cpu", "memory")}
    return {
        "apiVersion": "v1", "kind": "ResourceQuota", "metadata": {"name": "run", "namespace": ns, "labels": _labels(xr)},
        "spec": {"hard": {"pods": str(cc.pods), "requests.cpu": req["cpu"], "requests.memory": req["memory"],
                          "limits.cpu": lim["cpu"], "limits.memory": lim["memory"],
                          "count/sandboxes.agents.x-k8s.io": "1", "count/jobs.batch": "0",
                          "count/secrets": "0", "count/services": "0",
                          "persistentvolumeclaims": "0"}}}


def _condition_by_type(manifest: dict[str, Any] | None, typ: str) -> dict[str, Any] | None:
    for c in ((manifest or {}).get("status") or {}).get("conditions") or []:
        if c.get("type") == typ:
            return c
    return None


def sandbox_phase(sandbox: dict[str, Any] | None, now: datetime) -> tuple[str, datetime | None, str]:
    """(phase, finishedAt, outcome) from the observed Sandbox. Finished=True (PodSucceeded/PodFailed) wins over
    Ready; Ready=True is Running; anything else (absent, DependenciesNotReady, ...) is Provisioning."""
    if sandbox is None:
        return "Provisioning", None, ""
    fin = _condition_by_type(sandbox, "Finished")
    if fin and fin.get("status") == "True":
        at = parse_ts(fin["lastTransitionTime"]) if fin.get("lastTransitionTime") else now
        outcome = "Succeeded" if fin.get("reason") == "PodSucceeded" else "Failed"
        return outcome, at, outcome
    ready = _condition_by_type(sandbox, "Ready")
    if ready and ready.get("status") == "True":
        return "Running", None, ""
    return "Provisioning", None, ""


def compose(xr: dict[str, Any], observed: dict[str, Any], now: datetime, cfg: ClusterConfig) -> Composition:
    """observed: {"sandbox": <Sandbox manifest with status, or None>, "status": <XR status so far>,
                  "cluster": <this cluster's registry name>, "registry": <its registry ConfigMap data, or None>,
                  "objects": {composition-resource-name: <provider-kubernetes Object Ready status, True/False>}}"""
    xr = {**xr, "spec": ints(xr["spec"])}
    spec = xr["spec"]
    ns = ns_name(xr)
    prev = observed.get("status") or {}
    created = parse_ts(xr["metadata"].get("creationTimestamp") or prev.get("createdAt") or iso(now))
    expires = parse_ts(spec["expiresAt"])
    cluster = observed.get("cluster") or ""
    base_status: dict[str, Any] = {"namespace": ns, "sandbox": SANDBOX_NAME, "expiresAt": spec["expiresAt"],
                                   "createdAt": iso(created)}
    if cluster:
        base_status["cluster"] = cluster
    for k in ("outcome", "finishedAt", "frozenAt"):
        if prev.get(k):
            base_status[k] = prev[k]

    def done(phase: str, reason: str, **extra: Any) -> Composition:
        st = {**base_status, "phase": phase, "reason": reason, **extra}
        r, s = REASONS[phase]
        return Composition({}, st, 60, (RUN_READY, s, r, reason))

    # Final phases never render again, whatever else is true now (point 3 of the module docstring).
    if prev.get("phase") in FINAL_PHASES:
        return done(prev["phase"], prev.get("reason") or "")

    problem = registry_problem(observed.get("registry"), cluster) or validate(xr, cfg, created)
    if problem:
        return done("Rejected", problem)
    if now >= expires:
        return done("Expired", "past expiresAt; everything this run created is being removed")

    frozen = bool(spec.get("frozen"))
    frozen_at = parse_ts(prev["frozenAt"]) if prev.get("frozenAt") else (now if frozen else None)
    hold = timedelta(minutes=spec.get("freezeHoldMinutes", 15))
    if frozen and frozen_at is not None and now >= frozen_at + hold:
        return done("Expired", "freeze hold ended", frozenAt=iso(frozen_at))

    phase, finished, outcome = sandbox_phase(observed.get("sandbox"), now)
    if not finished and prev.get("finishedAt"):
        # The Sandbox can vanish after the agent finished (its pod deleted, a node lost): the outcome stands.
        finished, outcome = parse_ts(prev["finishedAt"]), prev.get("outcome") or "Failed"
        phase = outcome
    if finished is not None:
        base_status["finishedAt"] = iso(finished)
        base_status["outcome"] = outcome
        if now >= finished + timedelta(minutes=cfg.completion_grace_minutes):
            return done("Drained", f"agent {outcome.lower()}; completion grace period over")
    if frozen:
        phase = "Frozen"
        base_status["frozenAt"] = iso(frozen_at)  # type: ignore[arg-type]

    lab = _labels(xr)
    resources: dict[str, dict[str, Any]] = {}
    resources["namespace"] = obj({
        "apiVersion": "v1", "kind": "Namespace",
        "metadata": {"name": ns, "labels": {**lab, "pod-security.kubernetes.io/enforce": "restricted",
                                            "pod-security.kubernetes.io/enforce-version": "latest"}}})
    resources["quota"] = obj(_quota(xr, cfg, ns))
    resources["netpol-default-deny"] = obj({
        "apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
        "metadata": {"name": "default-deny", "namespace": ns, "labels": lab},
        "spec": {"podSelector": {}, "policyTypes": ["Ingress", "Egress"]}})
    egress = [] if frozen else egress_rules(spec["network"], cfg)
    if egress:
        resources["netpol-egress"] = obj({
            "apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
            "metadata": {"name": "allow-egress", "namespace": ns, "labels": lab},
            "spec": {"podSelector": {}, "policyTypes": ["Egress"], "egress": egress}})
    resources["serviceaccount"] = obj({
        "apiVersion": "v1", "kind": "ServiceAccount",
        "metadata": {"name": RUN_SERVICE_ACCOUNT, "namespace": ns, "labels": lab}, "automountServiceAccountToken": False})
    # DeriveFromObject: the Object is Ready only when the Sandbox reports Ready, so the XR's Ready tracks the pod.
    resources["sandbox"] = obj(_sandbox(xr, cfg, ns, created), readiness="DeriveFromObject")

    nxt = expires
    if frozen_at is not None:
        nxt = min(nxt, frozen_at + hold)
    if finished is not None:
        nxt = min(nxt, finished + timedelta(minutes=cfg.completion_grace_minutes))
    ttl = int(min(60, max(10, (nxt - now).total_seconds())))
    status = {**base_status, "phase": phase, "reason": ""}
    r, s = REASONS[phase]
    msg = {"Provisioning": f"waiting for Sandbox {ns}/{SANDBOX_NAME} to report Ready",
           "Running": f"Sandbox {ns}/{SANDBOX_NAME} is Ready; shutdownTime {spec['expiresAt']}",
           "Succeeded": "the agent exited 0; the run drains after the completion grace period",
           "Failed": "the agent failed; the run drains after the completion grace period",
           "Frozen": "egress cut by Clearance; kept for forensics until the freeze hold ends"}[phase]
    objects = observed.get("objects") or {}
    ready = {name: bool(objects.get(name)) for name in resources}
    return Composition(resources, status, ttl, (RUN_READY, s, r, msg), ready)
