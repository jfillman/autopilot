#!/usr/bin/env python3
"""Autopilot AP-A1: the network-policy canary that gates a cluster's `airframe.autopilotReady`.

An AgentRun's isolation is its NetworkPolicies. On a cluster that does not enforce them, every
egress claim in the design is false, so the registry flag that lets runs onto a cluster is set
only after this canary passes there. It is read-only toward everything but its own scratch
namespaces, which it always deletes.

What it does, on the cluster named by --context:
  1. Renders the run namespace and NetworkPolicies with function-agentrun's own compose(), for
     the `clearance` network mode and for `none`, so it tests the policy shape a real run gets.
  2. Stands up a stand-in service namespace (pods labelled as Clearance, as the model proxy and
     as an unrelated service) and a control namespace with no policies at all.
  3. Probes from inside each run namespace and from the control namespace. Every expected denial
     must be paired with a control probe that succeeds, or the denial proves nothing (a pass
     can mean the gate is off, or the network is broken).

    tools/netpol_canary.py --context kiac-dev [--json result.json] [--keep]

Exit 0: every expectation held. Exit 1: a policy was not enforced, or a control failed.
Run it with your own kubeconfig: the result is what a person attests when setting the flag.
"""
from __future__ import annotations

import argparse
import json
import secrets
import subprocess
import sys
import time
from dataclasses import asdict, dataclass, field
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Callable

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "airframe-drafts" / "functions" / "function-agentrun"))
from function.compose import ClusterConfig, compose, iso  # noqa: E402

# busybox 1.37, multi-arch index digest (kiac-dev is arm64, kind-prod amd64).
IMAGE = "docker.io/library/busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
INTERNET = ("1.1.1.1", 443)
PROBE_TIMEOUT = 3
POLICY_SETTLE_SECONDS = 5


@dataclass
class Check:
    name: str
    source: str            # which namespace's probe pod runs it: "run", "none" or "control" (or "svc")
    target: str            # "dns", a pod key ("clearance", "model-proxy", "other", "run-probe", "control-probe"),
                           # "internet" or "apiserver"
    expect: str            # "allow" or "deny"
    control: str | None = None   # for a deny: the name of the check proving the target is reachable without policy
    result: str = ""       # "allowed" or "denied" once run
    verdict: str = ""      # "pass", "fail" or "inconclusive"
    detail: str = ""


def plan() -> list[Check]:
    """The checks, in order. Controls first so a deny can cite them."""
    c = [
        Check("control: dns", "control", "dns", "allow"),
        Check("control: model-proxy reachable", "control", "model-proxy", "allow"),
        Check("control: other service reachable", "control", "other", "allow"),
        Check("control: clearance reachable", "control", "clearance", "allow"),
        Check("control: internet reachable", "control", "internet", "allow"),
        Check("control: apiserver reachable", "control", "apiserver", "allow"),
        Check("control: pod ingress across namespaces", "svc", "control-probe", "allow"),
        # network mode `clearance`: DNS and Clearance only
        Check("clearance mode: dns", "run", "dns", "allow"),
        Check("clearance mode: clearance", "run", "clearance", "allow"),
        Check("clearance mode: model-proxy denied", "run", "model-proxy", "deny", "control: model-proxy reachable"),
        Check("clearance mode: other service denied", "run", "other", "deny", "control: other service reachable"),
        Check("clearance mode: internet denied", "run", "internet", "deny", "control: internet reachable"),
        Check("clearance mode: apiserver denied", "run", "apiserver", "deny", "control: apiserver reachable"),
        Check("clearance mode: ingress denied", "svc", "run-probe", "deny", "control: pod ingress across namespaces"),
        # network mode `none`: nothing at all, not even DNS
        Check("none mode: dns denied", "none", "dns", "deny", "control: dns"),
        Check("none mode: clearance denied", "none", "clearance", "deny", "control: clearance reachable"),
    ]
    return c


def evaluate(checks: list[Check]) -> bool:
    """Set each check's verdict from its result. A deny only passes when its control was allowed; with a
    failed control it is inconclusive, and the failed control fails the run. (An air-gapped cluster fails
    the internet control: a person judges that case, the canary does not pass it.)"""
    by_name = {c.name: c for c in checks}
    ok = True
    for c in checks:
        if c.expect == "allow":
            c.verdict = "pass" if c.result == "allowed" else "fail"
        else:
            ctl = by_name[c.control] if c.control else None
            if c.result != "denied":
                c.verdict = "fail"
                c.detail = c.detail or "traffic got through: the policy is not enforced"
            elif ctl is not None and ctl.result != "allowed":
                c.verdict = "inconclusive"
                c.detail = f"denied, but its control ({ctl.name}) also failed, so this proves nothing"
            else:
                c.verdict = "pass"
        if c.verdict == "fail":
            ok = False
    return ok


# ---- rendering -----------------------------------------------------------------------------------
def render_run(name: str, mode: str, svc_ns: str, now: datetime) -> list[dict]:
    """The namespace, quota and NetworkPolicies a real AgentRun gets, from function-agentrun's compose()."""
    xr = {
        "apiVersion": "catalog.hangar.io/v1alpha1", "kind": "AgentRun",
        "metadata": {"name": name, "namespace": "autopilot-runs", "creationTimestamp": iso(now)},
        "spec": {
            "agent": "netpol-canary", "image": IMAGE.replace(":1.37@", "@"), "framework": "generic",
            "sandbox": "standard", "compute": "small", "network": {"mode": mode},
            "expiresAt": iso(now + timedelta(minutes=30)), "taskId": "t-canary", "sessionId": "s-canary",
            "limits": {"toolCalls": 1, "githubCalls": 0, "modelTokens": 0},
        },
    }
    comp = compose(xr, {}, now, ClusterConfig.from_input({"clearance_namespace": svc_ns}))
    if not comp.resources:
        raise RuntimeError(f"compose() rendered nothing for mode {mode}: {comp.status}")
    keep = ("namespace", "quota", "netpol-default-deny", "netpol-egress")
    return [comp.resources[k]["spec"]["forProvider"]["manifest"] for k in keep if k in comp.resources]


def _sec() -> dict:
    return {"runAsNonRoot": True, "runAsUser": 10001, "runAsGroup": 10001, "readOnlyRootFilesystem": True,
            "allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]},
            "seccompProfile": {"type": "RuntimeDefault"}}


def pod(name: str, ns: str, labels: dict, port: int) -> dict:
    """A busybox pod serving HTTP on `port` (so it can be a target) that can also run probes."""
    return {
        "apiVersion": "v1", "kind": "Pod",
        "metadata": {"name": name, "namespace": ns, "labels": labels},
        "spec": {
            "automountServiceAccountToken": False,
            "securityContext": {"runAsNonRoot": True, "seccompProfile": {"type": "RuntimeDefault"}},
            "containers": [{
                "name": "c", "image": IMAGE, "command": ["httpd", "-f", "-p", str(port), "-h", "/etc"],
                "securityContext": _sec(),
                "resources": {"requests": {"cpu": "10m", "memory": "16Mi"}, "limits": {"cpu": "100m", "memory": "32Mi"}},
            }],
        },
    }


# ---- cluster I/O ---------------------------------------------------------------------------------
class Kube:
    def __init__(self, context: str):
        self.context = context

    def _run(self, args: list[str], stdin: str | None = None, check: bool = True) -> subprocess.CompletedProcess:
        return subprocess.run(["kubectl", "--context", self.context, *args], input=stdin, text=True,
                              capture_output=True, check=check)

    def apply(self, manifests: list[dict]) -> None:
        self._run(["apply", "-f", "-"], stdin="\n---\n".join(json.dumps(m) for m in manifests))

    def create_ns(self, name: str) -> None:
        self.apply([{"apiVersion": "v1", "kind": "Namespace",
                     "metadata": {"name": name, "labels": {"hangar.io/component": "autopilot-canary"}}}])

    def wait_ready(self, ns: str, names: list[str], timeout: int = 180) -> None:
        self._run(["-n", ns, "wait", "--for=condition=Ready", *[f"pod/{n}" for n in names], f"--timeout={timeout}s"])

    def pod_ip(self, ns: str, name: str) -> str:
        return self._run(["-n", ns, "get", "pod", name, "-o", "jsonpath={.status.podIP}"]).stdout.strip()

    def service_ip(self, ns: str, name: str) -> str:
        return self._run(["-n", ns, "get", "svc", name, "-o", "jsonpath={.spec.clusterIP}"]).stdout.strip()

    def exec_ok(self, ns: str, name: str, cmd: list[str]) -> tuple[bool, str]:
        r = self._run(["-n", ns, "exec", name, "--", *cmd], check=False)
        return r.returncode == 0, (r.stdout + r.stderr).strip()[-200:]

    def delete_ns(self, names: list[str]) -> None:
        self._run(["delete", "ns", *names, "--ignore-not-found", "--wait=false"], check=False)


def run_checks(checks: list[Check], probe: Callable[[Check], tuple[bool, str]]) -> None:
    for c in checks:
        ok, out = probe(c)
        c.result = "allowed" if ok else "denied"
        if c.expect == "allow" and not ok:
            c.detail = out


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--context", required=True, help="kubectl context of the cluster to test")
    ap.add_argument("--json", help="write the full result here (evidence for the registry PR)")
    ap.add_argument("--keep", action="store_true", help="leave the scratch namespaces for inspection")
    a = ap.parse_args(argv)

    k = Kube(a.context)
    sfx = secrets.token_hex(3)
    now = datetime.now(timezone.utc)
    svc_ns, ctl_ns = f"canary-svc-{sfx}", f"canary-ctl-{sfx}"
    run_name, none_name = f"canary-{sfx}", f"canary-none-{sfx}"
    run_docs, none_docs = render_run(run_name, "clearance", svc_ns, now), render_run(none_name, "none", svc_ns, now)
    run_ns, none_ns = run_docs[0]["metadata"]["name"], none_docs[0]["metadata"]["name"]
    namespaces = [run_ns, none_ns, svc_ns, ctl_ns]
    checks = plan()
    try:
        k.create_ns(svc_ns)
        k.create_ns(ctl_ns)
        k.apply(run_docs + none_docs)
        k.apply([
            pod("clearance", svc_ns, {"app.kubernetes.io/name": "clearance"}, 8080),
            pod("model-proxy", svc_ns, {"app.kubernetes.io/name": "model-proxy"}, 8081),
            pod("other", svc_ns, {"app.kubernetes.io/name": "other"}, 8080),
            pod("probe", run_ns, {"app.kubernetes.io/name": "probe"}, 8080),
            pod("probe", none_ns, {"app.kubernetes.io/name": "probe"}, 8080),
            pod("probe", ctl_ns, {"app.kubernetes.io/name": "probe"}, 8080),
        ])
        k.wait_ready(svc_ns, ["clearance", "model-proxy", "other"])
        for ns in (run_ns, none_ns, ctl_ns):
            k.wait_ready(ns, ["probe"])
        time.sleep(POLICY_SETTLE_SECONDS)

        targets = {
            "clearance": (k.pod_ip(svc_ns, "clearance"), 8080),
            "model-proxy": (k.pod_ip(svc_ns, "model-proxy"), 8081),
            "other": (k.pod_ip(svc_ns, "other"), 8080),
            "run-probe": (k.pod_ip(run_ns, "probe"), 8080),
            "control-probe": (k.pod_ip(ctl_ns, "probe"), 8080),
            "internet": INTERNET,
            "apiserver": (k.service_ip("default", "kubernetes"), 443),
        }
        source = {"run": (run_ns, "probe"), "none": (none_ns, "probe"), "control": (ctl_ns, "probe"),
                  "svc": (svc_ns, "other")}

        def probe(c: Check) -> tuple[bool, str]:
            ns, name = source[c.source]
            if c.target == "dns":
                return k.exec_ok(ns, name, ["nslookup", "-timeout=3", "kubernetes.default.svc.cluster.local"])
            host, port = targets[c.target]
            return k.exec_ok(ns, name, ["nc", "-z", "-w", str(PROBE_TIMEOUT), host, str(port)])

        run_checks(checks, probe)
    finally:
        if not a.keep:
            k.delete_ns(namespaces)

    ok = evaluate(checks)
    width = max(len(c.name) for c in checks)
    for c in checks:
        print(f"{c.verdict.upper():12} {c.name:{width}}  expect {c.expect:5} got {c.result}"
              + (f"  ({c.detail})" if c.detail else ""))
    print()
    print(f"{a.context}: " + ("PASS - NetworkPolicy is enforced for AgentRun's policy shape." if ok
                              else "FAIL - do not set airframe.autopilotReady on this cluster."))
    if a.keep:
        print("kept namespaces: " + " ".join(namespaces))
    if a.json:
        Path(a.json).write_text(json.dumps({"context": a.context, "at": iso(now), "pass": ok,
                                            "image": IMAGE, "checks": [asdict(c) for c in checks]}, indent=2) + "\n")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
