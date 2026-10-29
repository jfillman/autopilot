#!/usr/bin/env python3
"""Glidepath API spec phase 4, done when: an agent proposes a staging promotion, a human merges it, the release record
reaches healthy, and the agent had no way to merge it.

Runs Clearance's MCP server in process, with the real Glidepath backend (src/autopilot/glidepath_api.py) pointed at a
live glidepath-api, opens a `release-proposer` session and, through MCP calls only: reads the app's release plan,
checks the target is the Flight environment right after the source, dry-runs `glidepath.promote` (every read and
check, and a server-side dry run of the release PipelineRun; nothing is stored), and, only with --propose, really
proposes it. It prints the release run (or the release PR already open) and the call that follows the release record.
Every other backend is a fake. The agent's definition holds no tool that reaches GitHub, so the merge is a person's.

    kubectl --context hangar-dev -n platform-system port-forward svc/glidepath-api 18080:8080 &
    kubectl --context hangar-dev -n platform-system create token clearance --duration=10m > /tmp/clearance-token
    tools/glidepath_release_smoke.py --url http://127.0.0.1:18080 --token-file /tmp/clearance-token            # dry run
    tools/glidepath_release_smoke.py --url http://127.0.0.1:18080 --token-file /tmp/clearance-token --propose  # for real

The token's identity needs `list`/`get` on environments.glidepath.hangar.io (glidepath-api-reader) and `promote`
(glidepath-api-releaser, api.releasers or a RoleBinding) in the app's namespace. Pass --on-behalf-of when it is one of
glidepath-api's api.delegates: a delegate's write must name the person (GP-AUTH-004), who becomes the run's
initiator. The same session sends the same Idempotency-Key for the dry run and the real call, so the dry run names
the run --propose starts. The token is read from the file on every call and never printed.

Exit 0 when the promotion was dry-run (and, with --propose, proposed); 1 when glidepath-api refused it (a denial:
its rule and hint are printed); 2 when the plan has no such promotion or a call fails.
"""
from __future__ import annotations

import argparse
import asyncio
import json
import sys
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from clearance import profile  # noqa: E402
from clearance.audit import AuditLog  # noqa: E402
from autopilot.backends import Backends, FakeArgo, FakeArtifacts, FakeGit, FakeHuman, FakeRead, FakeRuns  # noqa: E402
from autopilot.gateway import hangar_gateway  # noqa: E402
from clearance.dispatch import Principal  # noqa: E402
from autopilot.glidepath_api import GlidepathAPI  # noqa: E402
from clearance.server import create_server  # noqa: E402
from clearance.session import SessionStore  # noqa: E402
from clearance.tiers import Tier  # noqa: E402


class LocalUser:
    """The person running this, as a delegated principal at T1: a propose-only (T2) tool needs T1 to open a proposal
    (R007), and the definition lists nothing else that writes. Clearance's identity toward glidepath-api is the token."""
    def __init__(self, who: str):
        self.p = Principal(f"user:{who}", "user", Tier.T1)

    def authenticate(self, token: str) -> Principal:
        return self.p


def show(r: dict) -> str:
    d = r.get("data") or {}
    if d.get("mode") == "pr":
        return f"release PR already open: {d.get('prUrl')}"
    flags = ", ".join(k for k in ("dryRun", "replayed") if d.get(k))
    return f"release run {d.get('pipelineRun')}" + (f" ({flags})" if flags else "")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--url", required=True, help="glidepath-api base URL (a port-forward, or the Service in cluster)")
    ap.add_argument("--token-file", required=True, help="a token for Clearance's identity (re-read per call)")
    ap.add_argument("--app", default="gate-api", help="must be in release-proposer's profile.glidepath.apps")
    ap.add_argument("--env", default="staging", help="the Flight environment to promote to")
    ap.add_argument("--cluster", default="hangar-prod", help="the target environment's cluster")
    ap.add_argument("--source-env", default="dev")
    ap.add_argument("--source-cluster", default="hangar-dev")
    ap.add_argument("--propose", action="store_true", help="really propose it (default: dry run only)")
    ap.add_argument("--on-behalf-of", action="store_true",
                    help="send X-Glidepath-On-Behalf-Of (only when Clearance's identity is one of api.delegates)")
    ap.add_argument("--who", default="smoke")
    a = ap.parse_args()

    api = GlidepathAPI(a.url, a.token_file, on_behalf_of=a.on_behalf_of)
    backends = Backends(FakeGit(), FakeArgo(), FakeRuns(), FakeRead(), FakeHuman(), FakeArtifacts(), api)
    gw = hangar_gateway(profile.load_dir(ROOT / "agents"), SessionStore(), AuditLog(), backends, LocalUser(a.who),
                 lambda: datetime.now(timezone.utc))
    mcp = create_server(gw, lambda: "local")

    def call(name: str, args: dict) -> dict:
        r = asyncio.run(mcp.call_tool(name, args))
        sc = getattr(r, "structured_content", None) or getattr(r, "structuredContent", None)
        return sc.get("result", sc) if sc else json.loads(r.content[0].text)

    def refused(tool: str, r: dict) -> int:
        d = r["decision"]
        print(f"{tool}: refused {d.get('rule')}: {d.get('message') or r.get('error')}", file=sys.stderr)
        if d.get("hint"):
            print(f"  hint: {d['hint']}", file=sys.stderr)
        # A rule glidepath-api (or Clearance's policy) applied is an answer; a backend failure has no rule.
        return 1 if d.get("rule") else 2

    opened = call("session_open", {"agent": "release-proposer"})
    if not opened["ok"]:
        print(f"session refused: {opened['decision']}", file=sys.stderr)
        return 2
    sid = opened["data"]["session"]

    plan = call("call", {"session": sid, "tool": "glidepath.environments.list", "args": {"app": a.app}})
    if not plan["ok"]:
        return refused("glidepath.environments.list", plan)
    p = plan["data"]
    if p.get("error"):
        print(f"glidepath.environments.list: {p['error']}", file=sys.stderr)
        return 2
    upper = {u["name"]: u for u in p.get("upper", [])}
    flight = ", ".join(f"{n}@{u.get('cluster', '-')}" for n, u in upper.items())
    print(f"{a.app}: order {' -> '.join(p.get('order', []))} ({p.get('orderSource')}); "
          f"Ground {', '.join(p.get('lower', []))}; Flight {flight}")
    order = [e.lower() for e in p.get("order", [])]
    if a.source_env.lower() not in order or order.index(a.source_env.lower()) + 1 >= len(order) \
            or order[order.index(a.source_env.lower()) + 1] != a.env.lower():
        print(f"{a.env} is not the environment right after {a.source_env} in {a.app}'s plan", file=sys.stderr)
        return 2
    if a.env not in upper:
        print(f"{a.env} is not a Flight environment; glidepath.promote proposes only Flight promotions", file=sys.stderr)
        return 2

    args = {"app": a.app, "env": a.env, "cluster": a.cluster, "sourceEnv": a.source_env, "sourceCluster": a.source_cluster}
    dry = call("call", {"session": sid, "tool": "glidepath.promote", "args": {**args, "dryRun": True}})
    if not dry["ok"]:
        return refused("glidepath.promote (dry run)", dry)
    if (dry["data"] or {}).get("error"):
        print(f"glidepath.promote (dry run): {dry['data']['error']}", file=sys.stderr)
        return 2
    print(f"dry run: {a.source_env}@{a.source_cluster} -> {a.env}@{a.cluster}: {show(dry)}")
    if not a.propose:
        print("dry run only; pass --propose to start it")
    else:
        real = call("call", {"session": sid, "tool": "glidepath.promote", "args": args})
        if not real["ok"]:
            return refused("glidepath.promote", real)
        if (real["data"] or {}).get("error"):
            print(f"glidepath.promote: {real['data']['error']}", file=sys.stderr)
            return 2
        print(f"proposed: {show(real)}")
        print(f"a person merges the release PR; then follow it with glidepath.releases.get {{app: {a.app}}} "
              f"until the {a.env} record reaches healthy")
    denied = [r for r in gw.audit.records if r["decision"] == "deny"]
    print(f"audit: {len(gw.audit.records)} decisions ({len(denied)} denied), all through MCP")
    return 0


if __name__ == "__main__":
    sys.exit(main())
