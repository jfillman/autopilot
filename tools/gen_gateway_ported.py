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
        "and the flight recorder's fields, _def, _candidate's clamp and run_agent from the person's own session on; S3: _person, "
        "_manager, freeze_session, end_session, hold_triggers, release_triggers, hold_for, human_requests, human_answer and the "
        "inbox's handlers, open_triggered, record_event, run_agent (the route's checks, holds, Preflight candidates as far as "
        "Candidate goes), preflight_case_list and preflight_score; S4: apply_cluster_definitions and publish_definitions "
        "(gateway/internal/definitions Manager), the live definition set (SetDefs) and the validated parse of a candidate document",
        ""),
    "src/autopilot/engine/core.py": (
        ("gateway/internal/core",),
        "the decision path: evaluation, outcome, records, outbox, outage policy, canonical JSON, the client's timeout "
        "(DECISION_TIMEOUT), retries and breaker; S4: publish_definitions (PUT /v1/definitions on its own client with "
        "DEFINITIONS_TIMEOUT, the bundled definitions with the pins, the retry without the field against a core that predates it)",
        ""),
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
        ("gateway/internal/dispatch",), "ControlError, check_reason, check_hours, Hold and its store, may_manage: the person controls", ""),
    "src/autopilot/engine/inbox.py": (("gateway/internal/dispatch",), "the human inbox: the request, its checks, who may answer, the store", ""),
    "src/autopilot/engine/events.py": (("gateway/internal/dispatch",), "the event history: from_summary and the record", ""),
    "src/autopilot/engine/trigger_ledger.py": (("gateway/internal/triggers", "gateway/internal/pgstate"),
                                               "the trigger ledger and the cursors: claim, decide, advance, prune, the keys", ""),
    "src/autopilot/triggers.py": (("gateway/internal/triggers", "gateway/internal/definitions"),
                                  "the topic matcher (as a table, not a recursion), the Glidepath event matcher, interval parsing, "
                                  "the task ids, the storm brake; alert and RabbitMQ matchers have no caller in Python and are not carried", ""),
    "src/autopilot/event_bridge.py": (("gateway/internal/triggers",), "the event bridge: the stream, the catch-up, the cursor, the brake, "
                                      "the summary, the history (on a client of its own, with limits)", ""),
    "src/autopilot/reconcile.py": (("gateway/internal/triggers",), "the reconciliation sweep", ""),
    "src/autopilot/scheduler.py": (("gateway/internal/triggers",), "interval triggers: slots, grace, missed slots, once per slot", ""),
    "src/autopilot/preflight.py": (("gateway/internal/dispatch",), "the cases, the checks, the score, the ledger and the fingerprint "
                                   "(Go's own digest) behind the Preflight routes; the scripted-run harness is Python's local tooling", ""),
    "src/autopilot/engine/pg.py": (
        ("gateway/internal/pgstate",), "the tables, the writer lock, sessions, run results, artifacts (PgArtifacts), the outbox, the inbox, holds, the event history "
        "and the trigger ledger and cursors (schema.sql is generated)", ""),
    "src/autopilot/engine/definitions.py": (
        ("gateway/internal/definitions",), "Source, Loaded, Rejected, from_pin (the pin rules), Definitions (the live set: bundled "
        "names first and never shadowed, replace_cluster and its Changes, records, cluster_set, bundled_set); S6: apply_records, as the "
        "read API's refresher (readapi.Refresher: the rows through the same pin decision `serve` makes)", ""),
    "src/autopilot/engine/profile.py": (
        ("gateway/internal/definitions",), "parse: the JSON-schema validation in Python's words and order (best_match), the "
        "placeholder digest, scope, agent, runs_in, the interval and Glidepath event triggers, load_dir, the baseline deny lists", ""),
    "src/autopilot/engine/schemas/agent-definition.schema.json": (
        ("gateway/internal/definitions/agent-definition.schema.json",), "the schema the Go validator embeds (a generated copy)", ""),
    "src/autopilot/agent_xrs.py": (
        ("gateway/internal/definitions", "gateway/cmd/autopilot-gateway"), "AgentXRs: the poll, the once-per-commit fetch, rejections with "
        "reasons, the transient-failure rules, the seed from the store; DefinitionsPublisher (the bundled set published at start until "
        "the core takes it); S6: DefinitionsRefresher (readapi.Refresher)", ""),
    "src/autopilot/engine/scope.py": (("gateway/internal/dispatch", "gateway/internal/backends"),
                                      "normalize (repository-relative paths); S5b: glob_to_regex, repo_allowed and the path deny match, "
                                      "which the write tools decide again from the agent's definition (backends/scope.go)",
                                      "the AF-9a field scope (diff_pointers, field_violations), which stays the core's"),
    "src/autopilot/engine/tiers.py": (("gateway/internal/tools", "gateway/internal/dispatch"),
                                      "ToolSpec, tiers, the target and effect of a call", ""),
    "src/autopilot/hangar_tools.py": (("gateway/internal/tools",), "the Hangar tool pack (registry.json is generated)", ""),
    "src/autopilot/engine/modelproxy.py": (("gateway/internal/modelproxy",), "the model proxy: decision, metering, charge", ""),
    "src/autopilot/engine/auth.py": (
        ("gateway/internal/auth",), "WorkloadAuthenticator (TokenReview, the audience set), CachingAuthenticator; S1: OIDCAuthenticator (Dex), "
        "ForeignWorkloadAuthenticator (Tower), GitHubActionsAuthenticator (ported and tested, not wired: serve.py does not "
        "wire it), RoutingAuthenticator, JWKS",
        ""),
    "src/autopilot/engine/server.py": (
        ("gateway/internal/mcpserver",), "the MCP server surface: tools_list, agents_list, session_open, call, run_finish, "
        "session_close, healthz, protected-resource metadata, the people routes (/v1/...: the inbox, freeze, end, hold, release, run, "
        "Preflight)", ""),
    "src/autopilot/engine/backends.py": (
        ("gateway/internal/dispatch", "gateway/internal/backends"), "BackendError (evidence, agent message), Handler, RunBackend "
        "(and a fake), core_handlers for catalog.read, argo.app.get, app.api.get and artifact.put/get (S5a), the human inbox's handlers (human.request, human.request.get, S3)",
        "(S5b: the repo.* handlers and argo.sync.lower; S5c: xr.request) chat"),
    "src/autopilot/reads.py": (("gateway/internal/backends",), "PrometheusAPI and LokiAPI (metrics.query, logs.query); S5a: "
                               "BackstageCatalog (catalog.read), AppAPIs (app.api.get), KubeArgo.get_app and app_summary (argo.app.get), "
                               "ReadError; S5b: KubeArgo.sync (argo.sync.lower, over backends.ArgoSync); S6: BackstageDirectory and BackstageOwnership "
                               "(gateway/internal/readapi Backstage: groups and apps from the catalog, cached, failing closed)",
                               ""),
    "src/autopilot/airframe_api.py": (("gateway/internal/backends",), "S5a: KubeReader's GET and list of Argo CD Applications "
                                      "(the path allowlist, the token and CA read on every call, AirframeError, argo_path); S5c: the XR kinds "
                                      "(a closed list of the contract's plurals) and Rollouts the airframe.* tools read through it, per namespace only", ""),
    "src/autopilot/airframe_plan.py": (("gateway/internal/airframe",), "the AppSpec planner: Registry, plan() and every step, file and warning it makes, "
                                       "ground_cicd, flight_cicd, patch_cicd, the XR annotations; the schema check in Python's words", ""),
    "src/autopilot/airframe_tools.py": (("gateway/internal/airframe", "gateway/internal/backends"),
                                        "capabilities (every section and kind), explain, plan/changeset, status, describe, verify and normalise; "
                                        "no validate (the image carries no airframe-validate, so serve drops the tool)", ""),
    "src/autopilot/vendor/airframe_sidecars.py": (("gateway/internal/airframe",), "dig, render, when_matches: the sidecar templates verify reads", ""),
    "src/autopilot/airframe_apply.py": (("gateway/internal/backends",), "airframe.apply: the steps, the resume from GitHub, the Flight half "
                                        "as a proposal marked for a person, merge_onboarding_file; every repository on repos.allow before the first request; "
                                        "airframe.configure: the env file found by envName, the named-list merge, the whole-file check, the branch derived from the change", ""),
    "src/autopilot/xr_request.py": (("gateway/internal/airframe",), "xr.request's build: the kinds, the lower environments, the names, the path and the "
                                    "annotations, with a field table per kind (Go's own, from the XRDs)", ""),
    "src/autopilot/gateway.py": (("gateway/internal/backends", "gateway/internal/runs", "gateway/cmd/autopilot-gateway"),
                                 "which tool has a handler in a deployment (metrics.query, logs.query, catalog.read, app.api.get, argo.app.get, "
                                 "artifact.put/get, run.spawn, run.close; S5b: every glidepath.* tool, repo.file.get, repo.pr.open, "
                                 "repo.pr.open_upper, argo.sync.lower; S5c: airframe.capabilities, explain, plan, status, describe, verify, apply and xr.request; W-45: airframe.configure), pr_meta, rerun_key, release_key, promote_source, AgentRunBackend "
                                 "(the run's name, its identity, how it ended)",
                                 ""),
    "src/autopilot/serve.py": (("gateway/cmd/autopilot-gateway",), "the settings this slice carries (S1: AUTOPILOT_OIDC_*, TOWER_*, the "
                               "authenticator chain), start-up, the expiry sweep (closing, deleting runs, the backstop outcomes), "
                               "the outbox drain, the run backend and result store; S5a: BACKSTAGE_URL, BACKSTAGE_TOKEN_FILE, APP_API_SERVICES, ARGO_ENABLED, "
                               "ARGO_APP_NAMESPACES; S5b: GLIDEPATH_API_URL, GLIDEPATH_TOKEN_FILE, GLIDEPATH_ON_BEHALF_OF, GITHUB_PR_ENABLED, "
                               "AGENT_TOKEN_FILE, BROKER_URL (no default in Go), ARGO_SYNC_ENABLED; S5c: AIRFRAME_ENABLED, AIRFRAME_CLUSTER, AIRFRAME_UPPER_CLUSTERS, AIRFRAME_TENANTS_REPO, XR_REQUEST_ENABLED (AIRFRAME_OWNER is Go's); S3: EVENTS_ENABLED (with GLIDEPATH_API_URL), "
                               "SCHEDULES_ENABLED, RECONCILE_SECONDS, RECONCILE_WINDOW_HOURS, AUTOPILOT_DELEGATES, AUTOPILOT_ADMINS, "
                               "RUNTIME_IMAGES_EXTRA, PREFLIGHT_CASES_DIR, the bridge, the sweep and the scheduler; S4: AGENT_XRS_ENABLED, "
                               "AGENT_XRS_INTERVAL (and the Go-only AGENT_XRS_NAMESPACES and AGENT_XRS_MAX_TIER), the Agent XR loader and the "
                               "bundled publish at start; S6: METRICS_PORT and the trigger metrics endpoint, OTEL_EXPORTER_OTLP_LOGS_ENDPOINT (and Go's "
                               "OTEL_EXPORTER_OTLP_LOGS_ALLOW_HTTP), the Backstage directory the inbox reads (BACKSTAGE_URL)",
                               ""),
    "src/autopilot/settings.py": (("gateway/cmd/autopilot-gateway",), "the retired CLEARANCE_* names", ""),
    # S5b: the Glidepath and repository tools
    "src/autopilot/glidepath_api.py": (
        ("gateway/internal/backends",), "GlidepathAPI: every glidepath.* read and write over HTTP (runs, run, log, explain, releases, contract, "
        "live, live log, watch, release run, evidence, tests, test report, events, previews, environments, rollback plan, rerun, cancel, "
        "promote, rollback, validate_cicd), the bearer token read per call, X-Glidepath-Authorization and the on-behalf-of header, the "
        "Idempotency-Key and dry-run headers, _refusal (denials and answers as data), GlidepathError and its cause, the 256 KiB tail", ""),
    "src/autopilot/glidepath_live.py": (("gateway/internal/backends",), "summarize, the SSE reader, Watch and the polling fallback "
                                        "(glidepath.live.list and glidepath.live.watch)", ""),
    "src/autopilot/github_pr.py": (
        ("gateway/internal/backends",), "GitHubPRs: the per-call broker token, open_pr over the Git data API, ensure_pr, find_pr, read_file "
        "(repo.file.get), GitHubError with its agent message and evidence, app_of; S5c: commit_xr, pr_paths, repo_ready, onboarding_prs, "
        "find_pr and ensure_pr as airframe.apply calls them", ""),
    "src/autopilot/argo_sync.py": (("gateway/internal/backends",), "ArgoSync (argo.sync.lower): the lower-project and Application checks, "
                                   "the one merge PATCH, ArgoSyncError and its evidence", ""),
    "src/autopilot/cicd_propose.py": (("gateway/internal/backends",), "validate, propose, check_files (the cicd.yaml check on repo.pr.open), "
                                      "pr_body, branch_for, needs_person", ""),
    "src/autopilot/conformance.py": (("gateway/internal/conformance", "gateway/cmd/autopilot-gateway"),
                                     "the conformance control interface", ""),
    "src/autopilot/version.py": (("gateway/cmd/autopilot-gateway",), "the gateway's version", ""),
    # S6: the read API, the metrics and the log export
    "src/autopilot/api_serve.py": (
        ("gateway/cmd/autopilot-gateway",), "`api`: GATEWAY_API_DSN, GATEWAY_API_TABLES (retired, `gateway` accepted), GATEWAY_API_POOL, "
        "GATEWAY_API_AUDITORS, GATEWAY_API_NOTIFIERS, GATEWAY_API_DEFINITIONS_INTERVAL, AUTOPILOT_DELEGATES, AUTOPILOT_AGENTS_DIR, "
        "AUTOPILOT_GATEWAY_URL, CLEARANCE_API_URL, RUNTIME_IMAGES_EXTRA, PORT, the auth and catalog settings; the build, the definitions "
        "refresher and the runtime catalog", ""),
    "src/autopilot/read_api.py": (
        ("gateway/internal/readapi",), "the whole read API: ROUTES (the embedded contract), Access.viewer and Viewer (auditor, person, delegate "
        "for a person, notifier), every route and its answers, PgReadStore's queries (scoped by the viewer in SQL, bounded), the "
        "read-only pool", ""),
    "src/autopilot/metrics.py": (
        ("gateway/internal/metrics",), "the registry, counter and gauge, the exposition (text 0.0.4: names, help, labels, ordering, escaping, "
        "%g), TriggerMetrics' twelve series, GET /metrics on its own port", ""),
    "src/autopilot/otlp_logs.py": (
        ("gateway/internal/otlplogs",), "the OTLP/HTTP JSON logs export: the resource attributes, scope, severity numbers, body, the bounded "
        "queue, batches and the drop-not-block rule", ""),
    "src/autopilot/engine/runtimes.py": (
        ("gateway/internal/readapi",), "the catalog's parse (load) and Runtime.view for GET /v1/runtimes; allowed_images is the core's", ""),
}

# Python file -> the slice that ports it (the plan in docs/admin/go-gateway.md: S1 auth, S2 runs, S3 people and triggers,
# S4 definitions publish, S5a/b/c backends, S6 api/metrics/logs). A change to one of these is not watched until its slice
# moves it to PORTED.
NOT_PORTED: dict[str, str] = {}

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
