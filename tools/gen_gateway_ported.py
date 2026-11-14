"""The drift guard for the Go gateway (A-9): which Python source files the Go gateway ports, and the SHA-256 each had when
the Go code was last brought level with it. gateway/PORTED.json is written by this tool and read by
tests/test_gateway_go.py, which fails when a listed file changed.

Why: the Python gateway (src/autopilot) is the deployed one and the reference; gateway/ is the Go port. Between the first
slice (366442a) and the catch-up that added this guard, the Python hot path moved by 1,600 lines and the Go code did not
notice. A hash cannot tell whether a change matters to the port, so the rule is: when a listed file changes, the author
either ports the change to Go (in the same pull request) or reads it and confirms it does not touch what the Go gateway
carries, and then runs this tool, which records the new hash.

  python tools/gen_gateway_ported.py            show what changed since the manifest, then rewrite it
  python tools/gen_gateway_ported.py --check    exit 1 if a listed file's hash differs (what the test does)

Every Python file under src/autopilot and src/autopilot/engine is classified here: PORTED (watched), or NOT_PORTED with the
slice of docs/admin/go-gateway.md that will port it (or OUT_OF_SCOPE with the reason). A new Python file that is in none of
the three fails the test, so a new module cannot be forgotten. The tables below are the source of truth; the manifest is
their output with the hashes.
"""
from __future__ import annotations

import hashlib
import json
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
MANIFEST = ROOT / "gateway" / "PORTED.json"
SCAN = ("src/autopilot", "src/autopilot/engine")

# Python file -> (Go paths that carry it, what of it the Go gateway carries, what is left in it for a later slice).
PORTED: dict[str, tuple[tuple[str, ...], str, str]] = {
    "src/autopilot/engine/dispatch.py": (
        ("gateway/internal/dispatch",),
        "Gateway.call (decision, charge, handler, outcome, backend-failure text and evidence), open_session, the outage "
        "path, tools_list, agents_for; the run tree: launch, _spawn (the child is the core's grant, checked), run.close, "
        "finish_run, close_session, expire and _backstop_outcomes, _delete_runs, _acting (a run acts as its session), _record "
        "and the flight recorder's fields, _def, _candidate's clamp and run_agent from the person's own session on",
        "S3 person controls, the person-facing run_agent and Preflight routes, inbox, holds, open_triggered, triggers; "
        "S4 publish_definitions and the schema validation a candidate document needs"),
    "src/autopilot/engine/core.py": (
        ("gateway/internal/core",),
        "the decision path: evaluation, outcome, records, outbox, outage policy, canonical JSON, the client's timeout "
        "(DECISION_TIMEOUT), retries and breaker",
        "S4 publish_definitions (and the bundled definitions)"),
    "src/autopilot/engine/outbox.py": (("gateway/internal/core", "gateway/internal/pgstate"), "the outbox", ""),
    "src/autopilot/engine/session.py": (
        ("gateway/internal/session",), "the session record (every field, signals included), budgets, breaker, expiry, the run "
        "tree (MAX_DEPTH, close, the cut of a child, the reservation a spawn makes on its parent)", ""),
    "src/autopilot/engine/limits.py": (("gateway/internal/session",), "budget fields, limits, LimitRequest and narrow (a request "
                                       "can only narrow)", ""),
    "src/autopilot/engine/results.py": (
        ("gateway/internal/dispatch", "gateway/internal/pgstate"), "observe, warnings, clip_text, tool_call_summary, the result "
        "store (memory, and gateway_results) and its retention", ""),
    "src/autopilot/kube_runs.py": (("gateway/internal/runs",), "the AgentRun store over the Kubernetes API: the three requests, "
                                   "the allowlist, KubeRunError, RunRefused", ""),
    "src/autopilot/manifests.py": (("gateway/internal/runs",), "the AgentRun manifest and the run name", ""),
    "src/autopilot/vendor/agentrun_compose.py": (
        ("gateway/internal/runs",), "what the gateway reads of the composition: RUN_NAME, the run namespace's name "
        "(agent-<run>) and its service account `run` (the run's identity), the input's size limit. The rendering itself runs in "
        "the cluster, not in the gateway", ""),
    "src/autopilot/engine/control.py": (
        ("gateway/internal/dispatch",), "ControlError and check_reason (what session_close and run_finish answer with)",
        "S3 holds, may_manage, check_hours and the person controls built on them"),
    "src/autopilot/engine/pg.py": (
        ("gateway/internal/pgstate",), "the tables, the writer lock, sessions, run results and the outbox (schema.sql is generated)",
        "S3 use of the inbox, holds and trigger-ledger tables"),
    "src/autopilot/engine/definitions.py": (
        ("gateway/internal/definitions",), "reading the bundled set and the pinned set",
        "S4 publishing the set, bundled_set, the Agent XR loader"),
    "src/autopilot/engine/profile.py": (
        ("gateway/internal/definitions",), "parse: scope, agent, runs_in", "S4 JSON-schema validation"),
    "src/autopilot/engine/scope.py": (("gateway/internal/dispatch",), "normalize (repository-relative paths)", ""),
    "src/autopilot/engine/tiers.py": (("gateway/internal/tools", "gateway/internal/dispatch"),
                                      "ToolSpec, tiers, the target and effect of a call", ""),
    "src/autopilot/hangar_tools.py": (("gateway/internal/tools",), "the Hangar tool pack (registry.json is generated)", ""),
    "src/autopilot/engine/modelproxy.py": (("gateway/internal/modelproxy",), "the model proxy: decision, metering, charge", ""),
    "src/autopilot/engine/auth.py": (
        ("gateway/internal/auth",), "WorkloadAuthenticator (TokenReview, the audience set), CachingAuthenticator",
        "S1 OIDCAuthenticator (Dex), GitHub Actions, ForeignWorkloadAuthenticator (Tower), RoutingAuthenticator"),
    "src/autopilot/engine/server.py": (
        ("gateway/internal/mcpserver",), "the MCP server surface: tools_list, agents_list, session_open, call, run_finish, "
        "session_close, healthz, protected-resource metadata", "S3 the people routes (/v1/...)"),
    "src/autopilot/engine/backends.py": (
        ("gateway/internal/dispatch", "gateway/internal/backends"), "BackendError (evidence, agent message), Handler, RunBackend "
        "(and a fake)", "S3/S5 the core tools' handlers (repo.*, human.request, artifacts, chat)"),
    "src/autopilot/reads.py": (("gateway/internal/backends",), "PrometheusAPI and LokiAPI (metrics.query, logs.query)",
                               "S5a catalog.read, argo.app.get, app.api.get"),
    "src/autopilot/gateway.py": (("gateway/internal/backends", "gateway/internal/runs", "gateway/cmd/autopilot-gateway"),
                                 "which tool has a handler in a deployment (metrics.query, logs.query, run.spawn, run.close), "
                                 "AgentRunBackend (the run's name, its identity, how it ended)", "S5 the other handlers"),
    "src/autopilot/serve.py": (("gateway/cmd/autopilot-gateway",), "the settings this slice carries, start-up, the expiry sweep "
                               "(closing, deleting runs, the backstop outcomes), the outbox drain, the run backend and result store",
                               "S1/S3/S4/S6 the settings and loops of what is not ported"),
    "src/autopilot/settings.py": (("gateway/cmd/autopilot-gateway",), "the retired CLEARANCE_* names", ""),
    "src/autopilot/conformance.py": (("gateway/internal/conformance", "gateway/cmd/autopilot-gateway"),
                                     "the conformance control interface", ""),
    "src/autopilot/version.py": (("gateway/cmd/autopilot-gateway",), "the gateway's version", ""),
}

# Python file -> the slice that ports it (the plan in docs/admin/go-gateway.md: S1 auth, S2 runs, S3 people and triggers,
# S4 definitions publish, S5a/b/c backends, S6 api/metrics/logs). A change to one of these is not watched until its slice
# moves it to PORTED.
NOT_PORTED: dict[str, str] = {
    "src/autopilot/engine/runtimes.py": "S6",      # the catalog file and GET /v1/runtimes; a candidate's image globs are Gateway.RuntimeImages
    "src/autopilot/preflight.py": "S3",            # the cases, the scoring and the ledger of the person-facing Preflight routes
    "src/autopilot/engine/events.py": "S3", "src/autopilot/engine/inbox.py": "S3",
    "src/autopilot/engine/trigger_ledger.py": "S3", "src/autopilot/event_bridge.py": "S3", "src/autopilot/reconcile.py": "S3",
    "src/autopilot/scheduler.py": "S3", "src/autopilot/triggers.py": "S3",
    "src/autopilot/agent_xrs.py": "S4",
    "src/autopilot/argo_sync.py": "S5b", "src/autopilot/cicd_propose.py": "S5b", "src/autopilot/github_pr.py": "S5b",
    "src/autopilot/glidepath_api.py": "S5b", "src/autopilot/glidepath_live.py": "S5b",
    "src/autopilot/airframe_api.py": "S5c", "src/autopilot/airframe_apply.py": "S5c", "src/autopilot/airframe_plan.py": "S5c",
    "src/autopilot/airframe_tools.py": "S5c", "src/autopilot/xr_request.py": "S5c",
    "src/autopilot/vendor/airframe_sidecars.py": "S5c",
    "src/autopilot/api_serve.py": "S6", "src/autopilot/metrics.py": "S6", "src/autopilot/otlp_logs.py": "S6",
    "src/autopilot/read_api.py": "S6",
}

# Python files with no Go counterpart, and why.
OUT_OF_SCOPE: dict[str, str] = {
    "src/autopilot/backends.py": "the test fakes (FakeGlidepath, Recorder): the Go gateway registers no fake",
    "src/autopilot/cli.py": "the Python command line (preflight, reads): not part of the gateway process",
}

SLICES = ("S1", "S2", "S3", "S4", "S5a", "S5b", "S5c", "S6")


def python_files() -> list[str]:
    out: list[str] = []
    for d in SCAN:
        out += [p.relative_to(ROOT).as_posix() for p in sorted((ROOT / d).glob("*.py")) if p.name != "__init__.py"]
    out += [p.relative_to(ROOT).as_posix() for p in sorted((ROOT / "src/autopilot/vendor").glob("*.py")) if p.name != "__init__.py"]
    return out


def sha256(rel: str) -> str:
    return hashlib.sha256((ROOT / rel).read_bytes()).hexdigest()


def head() -> str | None:
    r = subprocess.run(["git", "-C", str(ROOT), "rev-parse", "HEAD"], capture_output=True, text=True)
    return r.stdout.strip() if r.returncode == 0 else None


def build(level_with: str | None) -> dict:
    return {
        "_": ("Written by tools/gen_gateway_ported.py; do not edit by hand. Each ported Python file's SHA-256 is the one it had "
              "when the Go gateway was last brought level with it. tests/test_gateway_go.py fails when one differs: port the "
              "change to Go (or read it and confirm it does not touch what the Go gateway carries), then re-run the tool."),
        "level_with": level_with,
        "ported": {rel: {"sha256": sha256(rel), "go": list(go), "carries": carries, "left_for_later_slices": later}
                   for rel, (go, carries, later) in sorted(PORTED.items())},
        "not_ported": dict(sorted(NOT_PORTED.items())),
        "out_of_scope": dict(sorted(OUT_OF_SCOPE.items())),
    }


def changed(manifest: dict) -> list[str]:
    return [rel for rel, e in manifest.get("ported", {}).items()
            if not (ROOT / rel).is_file() or sha256(rel) != e.get("sha256")]


def main(argv: list[str]) -> int:
    old = json.loads(MANIFEST.read_text()) if MANIFEST.is_file() else {}
    if "--check" in argv:
        return 1 if (changed(old) or not old) else 0
    for rel in changed(old):
        print(f"changed since the Go gateway was last level with it: {rel}")
        base = old.get("level_with")
        if base:
            subprocess.run(["git", "-C", str(ROOT), "diff", "--stat", base, "--", rel])
    out = json.dumps(build(head()), indent=1) + "\n"
    MANIFEST.write_text(out)
    print(f"wrote {MANIFEST.relative_to(ROOT)}: {len(PORTED)} ported files at {head()}. If a change above was not ported "
          "to gateway/, you have just said it does not affect what the Go gateway carries.")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
