"""tools/netpol_canary.py without a cluster: the check plan, the verdict logic and the rendered policies."""
import importlib.util
import sys
from datetime import datetime, timezone
from pathlib import Path

spec = importlib.util.spec_from_file_location("netpol_canary", Path(__file__).resolve().parents[1] / "tools" / "netpol_canary.py")
nc = importlib.util.module_from_spec(spec)
sys.modules["netpol_canary"] = nc  # dataclasses resolve types through sys.modules
spec.loader.exec_module(nc)

NOW = datetime(2026, 10, 9, 12, 0, tzinfo=timezone.utc)


def results(**overrides):
    """A fully passing run: allows allowed, denies denied; override by check name."""
    checks = nc.plan()
    for c in checks:
        c.result = overrides.get(c.name, "allowed" if c.expect == "allow" else "denied")
    return checks


def test_every_deny_names_an_existing_allow_control():
    checks = nc.plan()
    names = {c.name: c for c in checks}
    denies = [c for c in checks if c.expect == "deny"]
    assert denies, "a canary with no denials proves nothing"
    for c in denies:
        assert c.control in names and names[c.control].expect == "allow", c.name
        assert names[c.control].target == c.target or c.target in ("run-probe",), c.name


def test_all_expectations_held_passes():
    assert nc.evaluate(results()) is True


def test_traffic_that_gets_through_a_deny_fails():
    checks = results(**{"clearance mode: model-proxy denied": "allowed"})
    assert nc.evaluate(checks) is False
    assert next(c for c in checks if c.name == "clearance mode: model-proxy denied").verdict == "fail"


def test_a_blocked_allow_fails():
    assert nc.evaluate(results(**{"clearance mode: clearance": "denied"})) is False


def test_a_deny_whose_control_failed_proves_nothing_and_fails():
    checks = results(**{"control: other service reachable": "denied"})
    assert nc.evaluate(checks) is False
    assert next(c for c in checks if c.name == "clearance mode: other service denied").verdict == "inconclusive"


def test_no_internet_on_the_cluster_fails_the_run_for_a_person_to_judge():
    checks = results(**{"control: internet reachable": "denied"})
    # the control itself is an allow that failed, which fails the run: an air-gapped cluster
    # has to be judged by a person, not passed silently
    assert nc.evaluate(checks) is False
    assert next(c for c in checks if c.name == "clearance mode: internet denied").verdict == "inconclusive"


def test_clearance_mode_renders_default_deny_and_only_dns_and_clearance_egress():
    docs = nc.render_run("canary-x", "clearance", "canary-svc-x", NOW)
    kinds = {(d["kind"], d["metadata"]["name"]) for d in docs}
    assert ("Namespace", "agent-canary-x") in kinds
    deny = next(d for d in docs if d["metadata"]["name"] == "default-deny")
    assert deny["spec"] == {"podSelector": {}, "policyTypes": ["Ingress", "Egress"]}
    egress = next(d for d in docs if d["metadata"]["name"] == "allow-egress")["spec"]["egress"]
    selectors = [r["to"][0].get("podSelector", {}).get("matchLabels") for r in egress]
    assert {"app.kubernetes.io/name": "clearance"} in selectors
    assert {"app.kubernetes.io/name": "model-proxy"} not in selectors
    assert all(r["to"][0]["namespaceSelector"]["matchLabels"]["kubernetes.io/metadata.name"] in ("kube-system", "canary-svc-x")
               for r in egress)


def test_none_mode_renders_no_egress_policy_at_all():
    docs = nc.render_run("canary-none-x", "none", "canary-svc-x", NOW)
    assert not any(d["metadata"]["name"] == "allow-egress" for d in docs)


def test_probe_pods_satisfy_restricted_pod_security():
    p = nc.pod("probe", "ns", {}, 8080)
    sc = p["spec"]["containers"][0]["securityContext"]
    assert sc["runAsNonRoot"] and sc["allowPrivilegeEscalation"] is False and sc["capabilities"] == {"drop": ["ALL"]}
    assert "@sha256:" in p["spec"]["containers"][0]["image"]
