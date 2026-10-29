#!/usr/bin/env python3
"""Glidepath API spec phase 2, done when: an agent starting cold names the right failure id for a failed flow,
using only MCP.

Runs Clearance's MCP server in process, with the real Glidepath backend (src/autopilot/glidepath_api.py) pointed at
a live glidepath-api, opens a `cicd-explainer` session and, through MCP calls only, lists the app's runs, picks the
newest failed one and asks `glidepath.explain` why it failed. It prints each failed TaskRun's knownFailures id and the
line that matched. Every other backend is a fake; nothing here can write.

    kubectl --context hangar-dev -n platform-system port-forward svc/glidepath-api 18080:8080 &
    kubectl --context hangar-dev -n platform-system create token clearance --duration=10m > /tmp/clearance-token
    tools/glidepath_smoke.py --url http://127.0.0.1:18080 --token-file /tmp/clearance-token --app gate-api

The token is read from the file on every call and never printed. Exit 0 when the newest failed run's failures all
carry a knownFailures id; 1 when one doesn't (the evidence is printed, for a new knownFailures entry); 2 when there is
no failed run in the window or a call fails.
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
from autopilot.glidepath_api import GlidepathAPI  # noqa: E402
from clearance.dispatch import Principal  # noqa: E402
from clearance.server import create_server  # noqa: E402
from clearance.session import SessionStore  # noqa: E402
from clearance.tiers import Tier  # noqa: E402


class LocalUser:
    """The person running this, as a delegated principal. Clearance's identity toward glidepath-api is the token."""
    def __init__(self, who: str):
        self.p = Principal(f"user:{who}", "user", Tier.T0)

    def authenticate(self, token: str) -> Principal:
        return self.p


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--url", required=True, help="glidepath-api base URL (a port-forward, or the Service in cluster)")
    ap.add_argument("--token-file", required=True, help="a token for Clearance's identity (re-read per call)")
    ap.add_argument("--app", required=True)
    ap.add_argument("--since-hours", type=int, default=168)
    ap.add_argument("--who", default="smoke")
    a = ap.parse_args()

    api = GlidepathAPI(a.url, a.token_file)
    backends = Backends(FakeGit(), FakeArgo(), FakeRuns(), FakeRead(), FakeHuman(), FakeArtifacts(), api)
    gw = hangar_gateway(profile.load_dir(ROOT / "agents"), SessionStore(), AuditLog(), backends, LocalUser(a.who),
                 lambda: datetime.now(timezone.utc))
    mcp = create_server(gw, lambda: "local")

    def call(name: str, args: dict) -> dict:
        r = asyncio.run(mcp.call_tool(name, args))
        sc = getattr(r, "structured_content", None) or getattr(r, "structuredContent", None)
        return sc.get("result", sc) if sc else json.loads(r.content[0].text)

    opened = call("session_open", {"agent": "cicd-explainer"})
    if not opened["ok"]:
        print(f"session refused: {opened['decision']}", file=sys.stderr)
        return 2
    sid = opened["data"]["session"]
    runs = call("call", {"session": sid, "tool": "glidepath.runs.list",
                         "args": {"app": a.app, "sinceHours": a.since_hours, "limit": 100}})
    if not runs["ok"]:
        print(f"glidepath.runs.list: {runs['decision']} {runs['error']}", file=sys.stderr)
        return 2
    failed = [r for r in runs["data"].get("runs", []) if r.get("status") == "failed"]
    print(f"{a.app}: {len(runs['data'].get('runs', []))} runs in {a.since_hours} h, {len(failed)} failed")
    if not failed:
        return 2
    run = failed[0]
    ex = call("call", {"session": sid, "tool": "glidepath.explain", "args": {"app": a.app, "result": run["result"]}})
    if not ex["ok"]:
        print(f"glidepath.explain: {ex['decision']} {ex['error']}", file=sys.stderr)
        return 2
    print(f"newest failed run: {run.get('name')} ({run['result']}), contract v{ex['data']['contractVersion']}")
    unknown = 0
    for f in ex["data"]["failures"]:
        k = f.get("knownFailure")
        print(f"  {f['taskRun']} step={f.get('step')} log={f['log']}: {k['id'] if k else 'NO MATCH'}")
        print(f"    {(f.get('evidence') or '').splitlines()[-1][:200] if f.get('evidence') else ''}")
        unknown += k is None
    print(f"audit: {len(gw.audit.records)} decisions, all through MCP")
    return 1 if unknown else 0


if __name__ == "__main__":
    sys.exit(main())
