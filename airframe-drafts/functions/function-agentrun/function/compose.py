"""function-agentrun: the composition logic, as a pure function.

Everything the AgentRun composition decides is decided here, from three inputs: the XR,
what has been observed, and the current time. No I/O, so it is tested without a cluster
(tests/test_compose.py). fn.py is a thin protobuf wrapper around compose().

Two properties matter more than the rest:

1. Expiry needs no Clearance. Past spec.expiresAt the function renders NOTHING, and Crossplane
   garbage-collects everything it had composed, including the Namespace. A dead or compromised
   gateway therefore cannot leave a run alive.
2. It fails closed. A request the cluster cannot honour exactly (a hardened sandbox on a cluster
   with no runtime class, a GPU on a cluster with none, an oversized TTL, an over-broad
   allowlist) is Rejected and renders nothing. It is never quietly downgraded.
"""
from __future__ import annotations

import ipaddress
import json
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any

PROVIDER_CONFIG = {"kind": "ClusterProviderConfig", "name": "default"}
LABEL_RUN = "hangar.io/agent-run"
# NOTE: deliberately NOT hangar.io/ephemeral-env. The platform's PR-namespace TTL sweep
# deletes namespaces carrying that label whose owning ArgoCD Application is gone; reusing it
# would have that sweep reap live agent runs.
FORBIDDEN_LABELS = ("hangar.io/ephemeral-env",)
MAX_INPUT_BYTES = 4096
DEFAULT_COMPLETION_GRACE = timedelta(minutes=5)


@dataclass
class ComputeClassCfg:
    requests: dict[str, str]
    limits: dict[str, str]
    node_selector: dict[str, str] = field(default_factory=dict)
    tolerations: list[dict[str, Any]] = field(default_factory=list)
    pods: int = 4


@dataclass
class ClusterConfig:
    max_ttl_minutes: int = 240
    clearance_namespace: str = "autopilot-system"
    clearance_selector: dict[str, str] = field(default_factory=lambda: {"app.kubernetes.io/name": "clearance"})
    clearance_port: int = 8080
    model_proxy_selector: dict[str, str] = field(default_factory=lambda: {"app.kubernetes.io/name": "model-proxy"})
    model_proxy_port: int = 8081
    dns_namespace: str = "kube-system"
    dns_selector: dict[str, str] = field(default_factory=lambda: {"k8s-app": "kube-dns"})
    runtime_classes: dict[str, str] = field(default_factory=dict)     # {"hardened": "gvisor"}
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
        d = d or {}
        for k in ("max_ttl_minutes", "clearance_namespace", "clearance_port", "model_proxy_port",
                  "workspace_size", "completion_grace_minutes"):
            if k in d:
                setattr(c, k, d[k])
        if "runtime_classes" in d:
            c.runtime_classes = dict(d["runtime_classes"])
        if "compute" in d:
            c.compute = {k: ComputeClassCfg(v["requests"], v["limits"], v.get("nodeSelector", {}),
                                             v.get("tolerations", []), v.get("pods", 4))
                         for k, v in d["compute"].items()}
        return c


@dataclass
class Composition:
    resources: dict[str, dict[str, Any]]
    status: dict[str, Any]
    ttl_seconds: int
    ready: bool = False
    warnings: list[str] = field(default_factory=list)


def parse_ts(s: str) -> datetime:
    return datetime.fromisoformat(s.replace("Z", "+00:00")).astimezone(timezone.utc)


def iso(dt: datetime) -> str:
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def ns_name(xr: dict[str, Any]) -> str:
    return "agent-" + xr["metadata"]["name"]


def obj(manifest: dict[str, Any]) -> dict[str, Any]:
    return {
        "apiVersion": "kubernetes.m.crossplane.io/v1alpha1", "kind": "Object",
        "spec": {"forProvider": {"manifest": manifest}, "providerConfigRef": dict(PROVIDER_CONFIG)},
    }


def _labels(xr: dict[str, Any], extra: dict[str, str] | None = None) -> dict[str, str]:
    spec = xr["spec"]
    base = {"hangar.io/component": "autopilot", LABEL_RUN: "true",
            "hangar.io/agent": spec["agent"], "hangar.io/task": spec["taskId"],
            "hangar.io/session": spec["sessionId"]}
    base.update(extra or {})
    return base


def validate(xr: dict[str, Any], cfg: ClusterConfig, created: datetime) -> str | None:
    spec = xr["spec"]
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


def _restricted_security(user: int = 10001) -> dict[str, Any]:
    return {"runAsNonRoot": True, "runAsUser": user, "runAsGroup": user, "readOnlyRootFilesystem": True,
            "allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]},
            "seccompProfile": {"type": "RuntimeDefault"}}


def _dns_rule(cfg: ClusterConfig) -> dict[str, Any]:
    return {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": cfg.dns_namespace}},
                    "podSelector": {"matchLabels": cfg.dns_selector}}],
            "ports": [{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}]}


def egress_rules(net: dict[str, Any], cfg: ClusterConfig) -> list[dict[str, Any]]:
    mode = net["mode"]
    if mode == "none":
        return []
    rules = [_dns_rule(cfg)]
    rules.append({"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": cfg.clearance_namespace}},
                          "podSelector": {"matchLabels": cfg.clearance_selector}}],
                  "ports": [{"protocol": "TCP", "port": cfg.clearance_port}]})
    if mode in ("clearance+model", "allowlist"):
        rules.append({"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": cfg.clearance_namespace}},
                              "podSelector": {"matchLabels": cfg.model_proxy_selector}}],
                      "ports": [{"protocol": "TCP", "port": cfg.model_proxy_port}]})
    if mode == "allowlist":
        rules.append({"to": [{"ipBlock": {"cidr": c}} for c in net["allowCidrs"]]})
    return rules


def _job(xr: dict[str, Any], cfg: ClusterConfig, ns: str, created: datetime) -> dict[str, Any]:
    spec = xr["spec"]
    cc = cfg.compute[spec["compute"]]
    expires = parse_ts(spec["expiresAt"])
    deadline = max(1, int((expires - created).total_seconds()))
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
        "resources": {"requests": {"cpu": "50m", "memory": "64Mi"}, "limits": {"cpu": "500m", "memory": "512Mi"}},
        "securityContext": _restricted_security(), "volumeMounts": [{"name": "tmp", "mountPath": "/tmp"}],
    } for s in spec.get("sidecars", [])]
    pod: dict[str, Any] = {
        "serviceAccountName": "run", "automountServiceAccountToken": False, "restartPolicy": "Never",
        "terminationGracePeriodSeconds": 30, "enableServiceLinks": False,
        "securityContext": {"runAsNonRoot": True, "seccompProfile": {"type": "RuntimeDefault"}},
        "containers": [container] + sidecars, "volumes": volumes,
    }
    if spec["sandbox"] == "hardened":
        pod["runtimeClassName"] = cfg.runtime_classes["hardened"]
    if cc.node_selector:
        pod["nodeSelector"] = dict(cc.node_selector)
    if cc.tolerations:
        pod["tolerations"] = list(cc.tolerations)
    return {"apiVersion": "batch/v1", "kind": "Job",
            "metadata": {"name": "agent", "namespace": ns, "labels": _labels(xr)},
            "spec": {"backoffLimit": 0, "activeDeadlineSeconds": deadline, "ttlSecondsAfterFinished": 3600,
                     "template": {"metadata": {"labels": _labels(xr, {"app.kubernetes.io/name": "agent-run"})}, "spec": pod}}}


def compose(xr: dict[str, Any], observed: dict[str, Any], now: datetime, cfg: ClusterConfig) -> Composition:
    """observed: {"job": <Job manifest with status, or None>, "status": <XR status so far>}"""
    spec = xr["spec"]
    ns = ns_name(xr)
    prev = observed.get("status") or {}
    created = parse_ts(xr["metadata"].get("creationTimestamp") or prev.get("createdAt") or iso(now))
    expires = parse_ts(spec["expiresAt"])
    base_status = {"namespace": ns, "expiresAt": spec["expiresAt"], "createdAt": iso(created)}

    def done(phase: str, reason: str, ttl: int = 60, **extra) -> Composition:
        return Composition({}, {**base_status, "phase": phase, "reason": reason, **extra}, ttl)

    problem = validate(xr, cfg, created)
    if problem:
        return done("Rejected", problem)
    if now >= expires:
        return done("Expired", "past expiresAt; everything this run created is being removed")

    frozen = bool(spec.get("frozen"))
    frozen_at = parse_ts(prev["frozenAt"]) if prev.get("frozenAt") else (now if frozen else None)
    if frozen and frozen_at is not None:
        hold = timedelta(minutes=spec.get("freezeHoldMinutes", 15))
        if now >= frozen_at + hold:
            return done("Expired", "freeze hold ended", frozenAt=iso(frozen_at))

    job = observed.get("job")
    jstatus = (job or {}).get("status", {}) or {}
    finished = None
    phase = "Provisioning"
    if job is not None:
        phase = "Running"
        if jstatus.get("succeeded"):
            phase, finished = "Succeeded", parse_ts(jstatus["completionTime"]) if jstatus.get("completionTime") else now
        elif jstatus.get("failed"):
            phase = "Failed"
            fin = [c for c in jstatus.get("conditions", []) if c.get("type") == "Failed"]
            finished = parse_ts(fin[0]["lastTransitionTime"]) if fin and fin[0].get("lastTransitionTime") else now
    if finished is not None and now >= finished + timedelta(minutes=cfg.completion_grace_minutes):
        return done("Drained", f"job {phase.lower()}; grace period over")
    if frozen:
        phase = "Frozen"

    resources: dict[str, dict[str, Any]] = {}
    lab = _labels(xr)
    cc = cfg.compute[spec["compute"]]
    resources["namespace"] = obj({
        "apiVersion": "v1", "kind": "Namespace",
        "metadata": {"name": ns, "labels": {**lab, "pod-security.kubernetes.io/enforce": "restricted",
                                             "pod-security.kubernetes.io/enforce-version": "latest"}}})
    resources["quota"] = obj({
        "apiVersion": "v1", "kind": "ResourceQuota", "metadata": {"name": "run", "namespace": ns, "labels": lab},
        "spec": {"hard": {"pods": str(cc.pods), "requests.cpu": cc.limits["cpu"], "requests.memory": cc.limits["memory"],
                           "limits.cpu": cc.limits["cpu"], "limits.memory": cc.limits["memory"],
                           "count/jobs.batch": "1", "count/secrets": "0", "count/services": "0",
                           "persistentvolumeclaims": "0"}}})
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
        "metadata": {"name": "run", "namespace": ns, "labels": lab}, "automountServiceAccountToken": False})
    resources["job"] = obj(_job(xr, cfg, ns, created))

    nxt = min(expires, frozen_at + timedelta(minutes=spec.get("freezeHoldMinutes", 15)) if frozen_at else expires)
    if finished is not None:
        nxt = min(nxt, finished + timedelta(minutes=cfg.completion_grace_minutes))
    ttl = int(min(60, max(10, (nxt - now).total_seconds())))
    status = {**base_status, "phase": phase, "reason": ""}
    if frozen_at:
        status["frozenAt"] = iso(frozen_at)
    return Composition(resources, status, ttl, ready=(phase in ("Running", "Succeeded")))
