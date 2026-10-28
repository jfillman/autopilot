#!/usr/bin/env python3
"""AF-8a, done when: the airframe.* tools answer from a live cluster through MCP, read-only, and refuse a Secret.

Runs Clearance's MCP server in process, with the real Airframe backend (src/clearance/airframe_tools.py over the
GET-only adapter in src/clearance/airframe_api.py) pointed at a live API server as Clearance's own identity, opens an
`airframe-operator` session and, through MCP calls only:

  1. airframe.plan for examples/parachute.appspec.yaml, on a cicd.yaml that already carries a check-ride step: the
     app-repo step's cicd.yaml patch has no GP-SCOPE problem (R021), and opening it as a pull request through the
     coding-agent's repo.pr.open is allowed (the git backend is a fake: nothing is pushed);
  2. airframe.status and airframe.describe for APP/ENV, compared field by field with `kubectl --context CTX get`;
  3. a Secret read: refused by the adapter before any request is made (counted at the transport);
  4. airframe.verify on one live component: pass/skipped results, each skip with its reason.

    kubectl --context hangar-dev -n platform-system create token clearance --duration=10m > $SCRATCH/token; chmod 600 $SCRATCH/token
    kubectl config view --minify --raw --context hangar-dev -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' \
        | base64 -d > $SCRATCH/ca.crt
    tools/airframe_smoke.py --server https://192.168.64.240:6443 --ca-file $SCRATCH/ca.crt --token-file $SCRATCH/token \
        --context hangar-dev --app boarding-api --env dev --verify Redis/redis

The token is read from the file on every call and never printed. kubectl (for the comparison) runs `get` only, as
whoever the context is. Exit 0 when every check holds, 1 when one does not, 2 when a call fails.
"""
from __future__ import annotations

import argparse
import asyncio
import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from clearance import airframe_plan, policy, profile  # noqa: E402
from clearance.airframe_api import KubeReader, PathRefused, _urlopen  # noqa: E402
from clearance.airframe_tools import AirframeTools, ValidatorCommand  # noqa: E402
from clearance.audit import AuditLog  # noqa: E402
from clearance.backends import Backends, FakeArgo, FakeArtifacts, FakeGit, FakeHuman, FakeRead, FakeRuns  # noqa: E402
from clearance.dispatch import Gateway, Principal  # noqa: E402
from clearance.server import create_server  # noqa: E402
from clearance.session import SessionStore  # noqa: E402
from clearance.tiers import Tier  # noqa: E402

# The scaffold's cicd.yaml with the check ride Skyport apps now carry: the case the old planner broke (GP-SCOPE-002).
BASE_CICD = yaml.safe_load("""
apiVersion: platform/v1
kind: PipelineConfig
build: {agent: python-3.12, unitTest: {enabled: false}}
pipelines:
  ci:
    trigger: {source: git, event: push, branch: main}
    steps: [{stage: build}, {stage: deploy, env: dev}, {stage: test, env: dev, role: check-ride}]
deploy:
  environments: [{name: dev, tier: ground}]
  strategy: rollout
""")


class LocalUser:
    """The person running this, as a delegated principal (T1 so the coding-agent's repo.pr.open can be shown allowed;
    its git backend is a fake). Clearance's identity toward the API server is the token."""
    def __init__(self, who: str):
        self.p = Principal(f"user:{who}", "user", Tier.T1)

    def authenticate(self, token: str) -> Principal:
        return self.p


class Counting:
    """The real transport, counting what it is asked to send."""
    def __init__(self):
        self.paths: list[str] = []

    def __call__(self, req, timeout, ctx):
        self.paths.append(req.full_url)
        return _urlopen(req, timeout, ctx)


def kubectl(ctx: str, *args: str) -> dict:
    r = subprocess.run(["kubectl", "--context", ctx, "get", *args, "-o", "json"], capture_output=True, text=True, timeout=60)
    if r.returncode != 0:
        raise SystemExit(f"kubectl get {' '.join(args)}: {r.stderr.strip()[-200:]}")
    return json.loads(r.stdout)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--server", required=True, help="the API server URL")
    ap.add_argument("--ca-file", required=True)
    ap.add_argument("--token-file", required=True, help="a token for Clearance's identity (re-read per call)")
    ap.add_argument("--context", required=True, help="kubectl context to compare against (get only)")
    ap.add_argument("--cluster", default="hangar-dev", help="the cluster's registry name")
    ap.add_argument("--app", default="boarding-api")
    ap.add_argument("--env", default="dev")
    ap.add_argument("--verify", default="Redis/redis", help="KIND/NAME of a component in the environment")
    ap.add_argument("--validator", help="path to airframe's tools/airframe-validate (optional; airframe.validate is skipped without it)")
    ap.add_argument("--who", default="smoke")
    a = ap.parse_args()

    transport = Counting()
    reader = KubeReader(a.server, a.token_file, a.ca_file, opener=transport)
    tools = AirframeTools(reader, ValidatorCommand([sys.executable, a.validator]) if a.validator else None,
                          airframe_plan.Registry("hangar-dev", ("hangar-prod",), "gitops-hangar-dev-tenants"), cluster=a.cluster)
    git = FakeGit()
    backends = Backends(git, FakeArgo(), FakeRuns(), FakeRead(), FakeHuman(), FakeArtifacts(), None, tools)
    gw = Gateway(profile.load_dir(ROOT / "agents"), SessionStore(), AuditLog(), backends, LocalUser(a.who),
                 lambda: datetime.now(timezone.utc))
    mcp = create_server(gw, lambda: "local")

    def call(name: str, args: dict) -> dict:
        r = asyncio.run(mcp.call_tool(name, args))
        sc = getattr(r, "structured_content", None) or getattr(r, "structuredContent", None)
        return sc.get("result", sc) if sc else json.loads(r.content[0].text)

    def tool(sid: str, name: str, args: dict) -> dict:
        r = call("call", {"session": sid, "tool": name, "args": args})
        if not r["ok"]:
            print(f"{name}: {r['decision']} {r['error']}", file=sys.stderr)
            raise SystemExit(2)
        return r["data"]

    failures: list[str] = []

    def check(ok: bool, what: str) -> None:
        print(f"  {'ok  ' if ok else 'FAIL'} {what}")
        if not ok:
            failures.append(what)

    opened = call("session_open", {"agent": "airframe-operator"})
    if not opened["ok"]:
        print(f"session refused: {opened['decision']}", file=sys.stderr)
        return 2
    sid = opened["data"]["session"]

    # 1. plan
    print("airframe.plan examples/parachute.appspec.yaml (base cicd.yaml carries a check ride)")
    spec = yaml.safe_load((ROOT / "examples" / "parachute.appspec.yaml").read_text())
    cs = tool(sid, "airframe.plan", {"spec": spec, "baseCicd": yaml.safe_dump(BASE_CICD)})
    steps = {s["id"]: s for s in cs["steps"]}
    print("  steps: " + ", ".join(f"{s['id']}({s['gate']})" for s in cs["steps"]))
    agent = next(f for f in steps["app-repo"]["files"] if f["path"] == "cicd.yaml")
    as_file = {"path": "cicd.yaml", "op": agent["op"], "before": yaml.safe_dump(agent["before"]), "content": agent["content"]}
    check(policy.cicd_scope_problems([as_file]) == [], "app-repo cicd.yaml patch: no GP-SCOPE problem (R021)")
    coder = call("session_open", {"agent": "coding-agent"})["data"]["session"]
    pr = call("call", {"session": coder, "tool": "repo.pr.open",
                       "args": {"repo": "jfillman/parachute", "branch": "agent/plan", "title": "plan", "files": [as_file]}})
    check(pr["ok"] and git.names() == ["git.open_pr"], f"repo.pr.open with that patch through MCP: {'allowed' if pr['ok'] else pr['decision']}")
    human = steps["cicd-flight"]
    hrules = sorted({p["rule"] for p in policy.cicd_scope_problems([{**human["files"][0], "before": yaml.safe_dump(human["files"][0]["before"])}])})
    check(human["gate"] == "human-merge" and "GP-SCOPE-003" in hrules, f"flight half is a human-merge step ({', '.join(hrules)} if an agent sent it)")
    check({"stage": "test", "env": "dev", "role": "check-ride"} in policy.merge_patch(BASE_CICD, agent["content"])["pipelines"]["ci"]["steps"],
          "the check-ride step survives the patch")

    # 2. status / describe against kubectl
    print(f"airframe.status / airframe.describe {a.app} {a.env} vs kubectl --context {a.context}")
    st = tool(sid, "airframe.status", {"app": a.app, "env": a.env})
    d = tool(sid, "airframe.describe", {"app": a.app, "env": a.env})
    if "error" in st or "error" in d:
        print(f"  {st.get('error') or d.get('error')}", file=sys.stderr)
        return 2
    print(f"  {st['summary']}")
    check(len(st["xrs"]) > 0 and len(d["applications"]) > 0 and len(d["rollouts"]) > 0, "status and describe are non-empty")
    for x in st["xrs"]:
        plural = next(v["plural"] for k, v in tools.contract["xrds"].items() if k == x["kind"])
        live = kubectl(a.context, f"{plural}.catalog.hangar.io", "-n", x["namespace"], x["name"])
        lc = {c["type"]: c for c in live["status"]["conditions"]}
        same = x["generation"] == live["metadata"]["generation"] and all(
            lc[c["type"]]["status"] == c["status"] and lc[c["type"]].get("reason") == c["reason"] for c in x["conditions"]) \
            and set(lc) == {c["type"] for c in x["conditions"]}
        check(same, f"{x['kind']}/{x['name']} ({x['namespace']}): generation {x['generation']}, "
                    + ", ".join(f"{c['type']}={c['status']}/{c['reason']}" for c in x["conditions"]))
    for app in d["applications"]:
        live = kubectl(a.context, "applications.argoproj.io", "-n", app["namespace"], app["name"])
        revs = live["status"]["sync"].get("revisions") or [live["status"]["sync"].get("revision")]
        check(app["sync"]["status"] == live["status"]["sync"]["status"] and app["health"]["status"] == live["status"]["health"]["status"]
              and [r["revision"] for r in app["sync"]["revisions"]] == revs,
              f"Application {app['namespace']}/{app['name']}: {app['sync']['status']}/{app['health']['status']} at "
              + ", ".join(f"{(r['repoURL'] or '').rsplit('/', 1)[-1]}@{(r['revision'] or '')[:7]}" for r in app["sync"]["revisions"]))
    for ro in d["rollouts"]:
        live = kubectl(a.context, "rollouts.argoproj.io", "-n", ro["namespace"], ro["name"])
        images = [c["image"] for c in live["spec"]["template"]["spec"]["containers"]]
        check(ro["phase"] == live["status"]["phase"] and ro["readyReplicas"] == live["status"].get("readyReplicas") and ro["images"] == images,
              f"Rollout {ro['namespace']}/{ro['name']}: {ro['phase']}, {ro['readyReplicas']}/{ro['replicas']} ready, {', '.join(ro['images'])}")

    # 3. a Secret read
    print("a Secret read")
    before = len(transport.paths)
    try:
        reader.get(f"/api/v1/namespaces/app-{a.app}-{a.env}/secrets/redis-connection")
        check(False, "the adapter refused the Secret path")
    except PathRefused as e:
        check(len(transport.paths) == before, f"refused by the adapter ({e.rule}), {len(transport.paths) - before} requests sent")
    via = call("call", {"session": sid, "tool": "airframe.verify", "args": {"app": a.app, "env": a.env, "kind": "Secret", "name": "redis-connection"}})
    check(via["ok"] and via["data"]["error"]["rule"] == "AF-KIND-404" and len(transport.paths) == before,
          f"airframe.verify kind=Secret through MCP: {via['data']['error']['rule'] if via['ok'] else via['decision']['rule']}, no request")

    # 4. verify
    kind, name = a.verify.split("/", 1)
    print(f"airframe.verify {kind}/{name} in {a.env}")
    v = tool(sid, "airframe.verify", {"app": a.app, "env": a.env, "kind": kind, "name": name})
    for s in v["steps"]:
        print(f"    {s['status']:8} {s['id']:16} {s.get('detail') or s.get('reason')}")
    check(v["counts"]["pass"] > 0 and v["counts"]["fail"] == 0 and all(s.get("reason") for s in v["steps"] if s["status"] == "skipped"),
          f"verify: {v['counts']}")

    if a.validator:
        print("airframe.validate (the real airframe-validate)")
        val = tool(sid, "airframe.validate", {"app": a.app, "documents": [
            {"path": "glidepath/envs/dev.yaml", "content": "envName: dev\nrolout: {replicas: 2}\n"}]})
        p = val["files"][0]["problems"][0]
        check(p["rule"] == "AF-SCHEMA-001" and p["pointer"] == "/rolout", f"{p['rule']} at {p['pointer']}: {p['hint']}")

    off = sorted({u.split("?")[0].split(a.server.rstrip("/"))[-1].split("/")[1:4][0] for u in transport.paths})
    print(f"requests: {len(transport.paths)}, every one a GET under /{', /'.join(off)}; audit: {len(gw.audit.records)} decisions, all through MCP")
    check(all("/apis/catalog.hangar.io/" in u or "/apis/argoproj.io/" in u for u in transport.paths), "every request is on the allowlist")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
