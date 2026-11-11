# Changelog

Autopilot's gateway and runtime images are tagged `gateway-v*`, `runtime-v*` and `runtime-<name>-v*`; this file
says what each gateway tag adds. The manager cuts the tags.

## Unreleased

### Renamed (R-10a)
- The settings the gateway owns are `AUTOPILOT_*` now: `CLEARANCE_EPHEMERAL`, `_AGENTS_DIR`, `_RESOURCE_URL`,
  `_OIDC_ISSUER`, `_OIDC_AUDIENCE`, `_OIDC_JWKS_URL`, `_OIDC_USER_CLAIM`, `_OIDC_ALLOWED_USERS`, `_AUDIENCE`,
  `_EXPIRE_SECONDS`, `_DELEGATES`, `_ADMINS`, `_GATEWAY_URL` (read API) and `_ISSUER_URL` (Go gateway) become
  `AUTOPILOT_<same suffix>`. The old names keep working for one release: the new name wins when both are set, and a
  legacy name logs one deprecation warning at start naming its replacement (`src/autopilot/settings.py`; the Go gateway
  has the same in `settings.go`). Unchanged: `CLEARANCE_CORE_URL`, `CLEARANCE_CORE_TOKEN_FILE`, `CLEARANCE_API_URL`
  (they address the Clearance product), the core-only refuse-list, and every audience value (R-10b). The images no longer
  set `CLEARANCE_AGENTS_DIR`: the default is `/app/agents`. Apron renames the Deployments' variables in the same release.

### Changed (R-10e)
- Every pinned runtime image moves to the re-released images that read `AUTOPILOT_URL` and the `autopilot`-audience token
  first (falling back to the old names): `autopilot-agent-runtime` runtime-v0.1.6, `autopilot-agent-base-python`
  runtime-base-python-v0.1.4, `autopilot-agent-langgraph` runtime-langgraph-v0.1.3 and `autopilot-agent-claude-agent-sdk`
  runtime-claude-agent-sdk-v0.1.3 (`agents/*.yaml`, `agents/skyport/*.yaml`, `runtime/catalog.yaml`, the gateway test
  fixture, the README table). Nothing was left pinned to an older runtime.

### Changed
- Run-pod contract, backward compatible for one release (R-10b; Airframe's function-agentrun renders both sides):
  - The gateway accepts run-pod tokens for the `autopilot` audience as well as `clearance`. `AUTOPILOT_RUN_AUDIENCES`
    (comma list; default `autopilot,` + `CLEARANCE_AUDIENCE`, so `autopilot,clearance`) names them, and the TokenReview
    asks for all of them and accepts a token carrying any.
  - The runtime reads `AUTOPILOT_URL` first and falls back to `CLEARANCE_URL`; it uses the `autopilot-token` file
    when the pod has one, else `clearance-token`. A pod rendered before the new function still runs.
- The gateway image carries only the platform's core definitions (`agents/*.yaml`). `agents/skyport/` was copied in but never loaded; it stays in the repository until those agents move to their own repo.

### Added
- `airframe.apply` (A-3; T1; [README](README.md), "airframe.apply and the parachute"): the planner's change set opened as
  pull requests by the agents' GitHub App, one per call, each with a receipt (repo, pr, commit, branch, paths) that
  Clearance's provenance reads (`pr_tools` in the Hangar bundle, now 1.1.0). It opens the tenants-repo PR, then the app
  repo's (cicd.yaml checked by glidepath-api as an agent's change), resumes from GitHub after an interruption (branches
  are fixed by the plan; nothing is kept in the gateway), never opens the Flight half (cicd.yaml's Flight fields,
  `gitops-<app>`) and has no way to merge. `agents/parachute.yaml` is its definition; `examples/parachute-bad.appspec.yaml`
  is the seeded bad run. R023 scopes it by `spec.app`.
- Preflight (A-6; [docs/admin/preflight.md](docs/admin/preflight.md)):
  - `RunRecord`s from real runs (`record_from_run`: a run's result plus its evidence), checks per verifier in the case
    files, and the local harness `autopilot preflight run --case <id> [--seeded-bad | --run file | --all]`. The six
    Skyport cases have a scripted good run and a seeded bad run in `preflight/runs/`; CI runs `--all`.
  - `POST /v1/agents/{name}/runs` takes a `document` (a PR's candidate definition) and a `budget`: a Preflight run as a
    clamped task named `pf-<agent>-<fingerprint>`. `GET /v1/preflight/cases?agent=` lists an agent's cases and `POST /v1/preflight/score` scores the runs against the agent's cases and
    records the definition's result (`pass`/`fail`, cases) on its run records while it is unchanged.
  - The image carries `preflight/cases` (`PREFLIGHT_CASES_DIR`).
- The Go AI gateway, first slice (A-9; [docs/admin/go-gateway.md](docs/admin/go-gateway.md)): `gateway/`, one binary with
  `serve`, `conformance-control` and `version` (`api` exits "not ported yet"), on the official MCP Go SDK, Clearance's Go
  client, a streaming model proxy that never buffers a response, and the same gateway database. Off by default and not
  deployed: the Python gateway stays the gateway and the reference. Proven equal by the R-3 replay corpus (identical
  evaluations, decisions, spends, grants and record fields), the conformance harness, and a load test (p99 gateway
  overhead under 1 ms at 200 concurrent sessions). `tools/gen_gateway_registry.py` generates what it takes from the
  Python gateway; `tests/test_gateway_go.py` keeps the two in step. What remains is listed in the doc.
- Trigger durability (A-5; [docs/admin/trigger-durability.md](docs/admin/trigger-durability.md)):
  - The trigger ledger, `gateway_trigger_launches`: one row per (agent, trigger key), claimed before a triggered
    session opens, so an event or slot launches once across restarts, replays and the reconciliation sweep.
    A `pipelinerun.*` event is keyed by its routing key and PipelineRun, so two events for one run launch once.
  - The event bridge's cursor, `gateway_event_cursors`: saved after each event; on start the bridge catches up with
    `GET /v1/events/recent?since=<cursor>`, then resumes the stream with `Last-Event-ID`.
  - The reconciliation sweep (`reconcile.py`, `RECONCILE_SECONDS`, `RECONCILE_WINDOW_HOURS`): finished runs from
    glidepath-api's `GET /v1/runs` whose `pipelinerun.finished` never arrived are launched once, as `via reconcile`.
  - Prometheus metrics on `METRICS_PORT` (9090), among them `autopilot_trigger_missed_total{agent,result}` (the gap) and
    `autopilot_schedule_missed_total{agent}`; a PodMonitor, NetworkPolicy and PrometheusRule for apron in
    `docs/admin/monitoring/autopilot-triggers.yaml`.
  - `EVENTS_RABBITMQ_ENABLED`: reserved; the RabbitMQ source is designed, not built, and the flag refuses to start.

### Changed
- `xr.request` asks the broker for the tenants repo with no `app` (Glidepath broker, glidepath#236: it refuses that repo
  when an app is named), so it no longer 403s as `XR-TENANTS-TOKEN` once the broker allows the repo.
- `airframe.validate` is removed from every definition rather than restored to the image (it needs `airframe-validate`
  and helm); `tests/test_airframe_apply.py` fails a definition that names it again.
- The scheduler claims each slot in the ledger: a restart inside the grace window after a slot fired no longer fires
  it again, and a slot whose window passed with the gateway down is recorded `missed` (not run late).
- Event history outcomes carry `via` (stream, recent, reconcile); a replay of an event already kept is not written
  again; the bridge's history writes hold the gateway lock and commit.
- An event that matched while held, braked or refused stays decided: no later path launches it.
