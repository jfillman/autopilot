import copy
import json
from datetime import datetime, timedelta, timezone

import pytest

from function.compose import ClusterConfig, ComputeClassCfg, compose, iso

T0 = datetime(2026, 9, 26, 12, 0, 0, tzinfo=timezone.utc)
DIGEST = "ghcr.io/jfillman/agent-x@sha256:" + "0" * 64


def xr(**spec):
    base = {
        "agent": "coding-agent", "image": DIGEST, "framework": "generic", "sandbox": "standard", "compute": "small",
        "network": {"mode": "clearance+model"}, "expiresAt": iso(T0 + timedelta(minutes=30)),
        "taskId": "t-1a2b3c", "sessionId": "s-1a2b3c",
        "limits": {"toolCalls": 100, "githubCalls": 50, "modelTokens": 1000},
    }
    base.update(spec)
    return {"apiVersion": "catalog.idp.io/v1alpha1", "kind": "AgentRun",
            "metadata": {"name": "r-1a2b3c", "namespace": "autopilot-runs", "creationTimestamp": iso(T0)},
            "spec": base}


def manifest(comp, key):
    return comp.resources[key]["spec"]["forProvider"]["manifest"]


def run(x=None, observed=None, now=T0, cfg=None):
    return compose(x or xr(), observed or {}, now, cfg or ClusterConfig())


def test_a_normal_run_renders_the_sandbox_and_is_provisioning_until_the_job_is_seen():
    c = run()
    assert set(c.resources) == {"namespace", "quota", "netpol-default-deny", "netpol-egress", "serviceaccount", "job"}
    assert c.status["phase"] == "Provisioning" and not c.ready
    assert all(r["kind"] == "Object" and r["apiVersion"].startswith("kubernetes.m.crossplane.io") for r in c.resources.values())
    c2 = run(observed={"job": {"status": {"active": 1}}})
    assert c2.status["phase"] == "Running" and c2.ready


def test_namespace_is_pod_security_restricted_and_never_carries_the_pr_sweep_label():
    ns = manifest(run(), "namespace")
    assert ns["metadata"]["name"] == "agent-r-1a2b3c"
    assert ns["metadata"]["labels"]["pod-security.kubernetes.io/enforce"] == "restricted"
    assert "hangar.io/ephemeral-env" not in ns["metadata"]["labels"], "the PR TTL sweep would reap live runs"
    assert ns["metadata"]["labels"]["hangar.io/agent-run"] == "true"


def test_default_deny_always_present_and_quota_forbids_secrets_and_services():
    c = run()
    assert manifest(c, "netpol-default-deny")["spec"]["policyTypes"] == ["Ingress", "Egress"]
    hard = manifest(c, "quota")["spec"]["hard"]
    assert hard["count/secrets"] == "0" and hard["count/services"] == "0" and hard["count/jobs.batch"] == "1"


def test_job_is_locked_down():
    pod = manifest(run(), "job")["spec"]["template"]["spec"]
    ctr = pod["containers"][0]
    assert pod["automountServiceAccountToken"] is False and pod["restartPolicy"] == "Never"
    assert ctr["securityContext"]["runAsNonRoot"] is True and ctr["securityContext"]["readOnlyRootFilesystem"] is True
    assert ctr["securityContext"]["capabilities"] == {"drop": ["ALL"]} and ctr["securityContext"]["allowPrivilegeEscalation"] is False
    assert "runtimeClassName" not in pod
    assert ctr["image"].endswith("@sha256:" + "0" * 64)
    assert not any("KEY" in e["name"] or "SECRET" in e["name"] or "TOKEN" == e["name"] for e in ctr["env"])


def test_env_contract():
    env = {e["name"]: e["value"] for e in manifest(run(), "job")["spec"]["template"]["spec"]["containers"][0]["env"]}
    for k in ("HANGAR_RUN_ID", "HANGAR_TASK_ID", "HANGAR_SESSION_ID", "HANGAR_AGENT", "HANGAR_EXPIRES_AT",
              "HANGAR_OUTPUT_DIR", "HANGAR_WORKSPACE", "HANGAR_LIMITS", "CLEARANCE_URL", "MODEL_PROXY_URL"):
        assert k in env, k
    assert json.loads(env["HANGAR_LIMITS"])["toolCalls"] == 100


# ---- network modes -------------------------------------------------------------------
def egress(c):
    return manifest(c, "netpol-egress")["spec"]["egress"] if "netpol-egress" in c.resources else []


def sources(c):
    vols = manifest(c, "job")["spec"]["template"]["spec"]["volumes"]
    return [s["serviceAccountToken"]["audience"] for v in vols if "projected" in v for s in v["projected"]["sources"]]


def env_names(c):
    return {e["name"] for e in manifest(c, "job")["spec"]["template"]["spec"]["containers"][0]["env"]}


def test_network_none_has_no_egress_and_no_urls_or_tokens():
    c = run(xr(network={"mode": "none"}))
    assert "netpol-egress" not in c.resources and sources(c) == []
    assert not {"CLEARANCE_URL", "MODEL_PROXY_URL"} & env_names(c)


def test_network_clearance_reaches_dns_and_the_gateway_only():
    c = run(xr(network={"mode": "clearance"}))
    assert len(egress(c)) == 2 and sources(c) == ["clearance"]
    assert "MODEL_PROXY_URL" not in env_names(c)


def test_network_clearance_model_adds_the_model_proxy_and_its_token():
    c = run()
    assert len(egress(c)) == 3 and sources(c) == ["clearance", "model-proxy"]


def test_allowlist_adds_only_the_listed_cidrs():
    c = run(xr(network={"mode": "allowlist", "allowCidrs": ["203.0.113.0/24"]}))
    assert egress(c)[-1] == {"to": [{"ipBlock": {"cidr": "203.0.113.0/24"}}]}


@pytest.mark.parametrize("net", [
    {"mode": "allowlist"}, {"mode": "allowlist", "allowCidrs": []},
    {"mode": "allowlist", "allowCidrs": ["0.0.0.0/0"]}, {"mode": "allowlist", "allowCidrs": ["127.0.0.0/8"]},
    {"mode": "allowlist", "allowCidrs": ["169.254.0.0/16"]}, {"mode": "clearance", "allowCidrs": ["10.0.0.0/8"]},
])
def test_bad_network_requests_are_rejected_and_render_nothing(net):
    c = run(xr(network=net))
    assert c.status["phase"] == "Rejected" and c.resources == {}


# ---- expiry: the dead-man property ---------------------------------------------------
def test_expiry_renders_nothing_so_crossplane_removes_everything():
    e = T0 + timedelta(minutes=30)
    assert run(now=e - timedelta(seconds=1)).resources
    c = run(now=e)
    assert c.resources == {} and c.status["phase"] == "Expired"
    assert run(now=e + timedelta(days=3)).resources == {}


def test_expiry_does_not_depend_on_anything_but_the_clock_and_the_xr():
    """No gateway, no observed state: an XR that nobody is tending still dies on schedule."""
    c = compose(xr(), {}, T0 + timedelta(hours=1), ClusterConfig())
    assert c.resources == {}


def test_job_deadline_is_anchored_to_creation_so_it_does_not_drift_between_reconciles():
    a = manifest(run(now=T0), "job")["spec"]["activeDeadlineSeconds"]
    b = manifest(run(now=T0 + timedelta(minutes=10)), "job")["spec"]["activeDeadlineSeconds"]
    assert a == b == 30 * 60


def test_output_is_deterministic():
    assert json.dumps(run().resources, sort_keys=True) == json.dumps(run().resources, sort_keys=True)


def test_ttl_over_the_cluster_cap_is_rejected():
    c = run(xr(expiresAt=iso(T0 + timedelta(minutes=241))))
    assert c.status["phase"] == "Rejected" and "cap" in c.status["reason"] and c.resources == {}
    assert run(xr(expiresAt=iso(T0 + timedelta(minutes=240)))).resources


def test_response_ttl_is_bounded_and_shrinks_near_expiry():
    assert run().ttl_seconds == 60
    assert run(now=T0 + timedelta(minutes=29, seconds=52)).ttl_seconds == 10


# ---- fail closed ---------------------------------------------------------------------
def test_hardened_without_a_runtime_class_is_rejected_never_downgraded():
    c = run(xr(sandbox="hardened"))
    assert c.status["phase"] == "Rejected" and "runtime class" in c.status["reason"] and c.resources == {}


def test_hardened_with_a_runtime_class_sets_it():
    cfg = ClusterConfig(runtime_classes={"hardened": "gvisor"})
    assert manifest(run(xr(sandbox="hardened"), cfg=cfg), "job")["spec"]["template"]["spec"]["runtimeClassName"] == "gvisor"


def test_a_compute_class_the_cluster_lacks_is_rejected():
    assert run(xr(compute="gpu")).status["phase"] == "Rejected"
    cfg = ClusterConfig()
    cfg.compute["gpu"] = ComputeClassCfg({"cpu": "4", "memory": "16Gi"}, {"cpu": "8", "memory": "32Gi"},
                                         {"nvidia.com/gpu.present": "true"},
                                         [{"key": "nvidia.com/gpu", "operator": "Exists"}])
    pod = manifest(run(xr(compute="gpu"), cfg=cfg), "job")["spec"]["template"]["spec"]
    assert pod["nodeSelector"] == {"nvidia.com/gpu.present": "true"} and pod["tolerations"]


def test_oversized_input_is_rejected_and_small_input_is_passed():
    assert run(xr(input={"blob": "x" * 5000})).status["phase"] == "Rejected"
    env = {e["name"]: e["value"] for e in manifest(run(xr(input={"q": "hello"})), "job")["spec"]["template"]["spec"]["containers"][0]["env"]}
    assert json.loads(env["HANGAR_INPUT"]) == {"q": "hello"}


def test_sidecars_get_the_same_lockdown_and_no_agent_env():
    side = [{"name": "tools", "image": DIGEST}]
    pod = manifest(run(xr(sidecars=side)), "job")["spec"]["template"]["spec"]
    sc = pod["containers"][1]
    assert sc["name"] == "tools" and sc["securityContext"]["readOnlyRootFilesystem"] is True and "env" not in sc


# ---- lifecycle -----------------------------------------------------------------------
def test_freeze_cuts_egress_keeps_the_run_for_forensics_then_expires():
    c = run(xr(frozen=True))
    assert c.status["phase"] == "Frozen" and "netpol-egress" not in c.resources and "job" in c.resources
    assert manifest(c, "netpol-default-deny")
    frozen_at = c.status["frozenAt"]
    later = run(xr(frozen=True), observed={"status": {"frozenAt": frozen_at}}, now=T0 + timedelta(minutes=16))
    assert later.status["phase"] == "Expired" and later.resources == {}


def test_a_finished_job_drains_after_the_grace_period():
    done = {"job": {"status": {"succeeded": 1, "completionTime": iso(T0 + timedelta(minutes=2))}}}
    c = run(observed=done, now=T0 + timedelta(minutes=3))
    assert c.status["phase"] == "Succeeded" and c.resources and c.ready
    d = run(observed=done, now=T0 + timedelta(minutes=8))
    assert d.status["phase"] == "Drained" and d.resources == {}


def test_a_failed_job_is_reported_and_drains():
    failed = {"job": {"status": {"failed": 1, "conditions": [{"type": "Failed", "lastTransitionTime": iso(T0 + timedelta(minutes=1))}]}}}
    assert run(observed=failed, now=T0 + timedelta(minutes=2)).status["phase"] == "Failed"
    assert run(observed=failed, now=T0 + timedelta(minutes=7)).status["phase"] == "Drained"


def test_rejection_takes_precedence_over_everything_and_nothing_is_created():
    c = run(xr(sandbox="hardened", compute="gpu"), now=T0)
    assert c.resources == {} and c.status["phase"] == "Rejected"
