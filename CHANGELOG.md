# Changelog

Autopilot's gateway and runtime images are tagged `gateway-v*`, `runtime-v*` and `runtime-<name>-v*`; this file
says what each gateway tag adds. The manager cuts the tags.

## Unreleased

### Added (A-11, the `propose` scope: agents propose the Flight half, a person merges it)
- Owner decision 2026-11-12: when asked to configure an app, the agent configures every environment named, and a person approves
  anything that touches prod. Glidepath's contract version 2 adds the `propose` scope class; this repo's side (docs/admin in
  Glidepath's contract.md has the whole design):
  - **`airframe.apply` takes `proposeFlight: true`** and then opens the Flight half, the `cicd-flight` step (Flight environments and
    the release step in cicd.yaml, checked by glidepath-api as an agent's change) and the `gitops` step (the Flight values, merged into
    `<cluster>/<env>/values.yaml` once each exists, `waiting` before), as PRs marked for a person to merge: a `Hangar-Merge: person`
    commit trailer, a "Needs a person to merge" PR body, `merge: person` and `needsPerson: [paths]` in the result and the receipt,
    and step status `awaiting-merge`. A field that stays human in the Flight change (GP-SCOPE-001/002/003) makes that step a `humanStep`
    instead of failing the call. Without `proposeFlight` the Flight half is reported in `humanSteps`, as before.
  - **R025** (`policy/hangar/bundle.json`): a call with `proposeFlight: true` is refused unless the definition sets
    `profile.airframe.proposeFlight: true` (new in the agent-definition schema, byte-identical to Clearance's). `app-provisioner` sets it
    and its instructions pass it; `parachute` does not.
  - `glidepath.cicd.propose`: a change glidepath-api reports as `needsPerson` (a Flight environment added or changed, a release
    step into one, a new environment's cluster) is opened with the same mark (`Hangar-Merge: person`, `merge: person`,
    `needsPerson: [cicd.yaml]`).
  - **R021** refuses a `propose` field on a file-writing tool (`repo.pr.open`), which cannot mark its PR: rule `GP-SCOPE-004`,
    hint naming `glidepath.cicd.propose` and `airframe.apply`. The bundle names Glidepath contract versions 1 and 2; version 2
    needs a Clearance with `propose` support (docscope) and the `merge`/`needsPerson` receipt fields.
  - The receipt lists of `glidepath.cicd.propose` and `airframe.apply` gain `merge` and `needsPerson`. Rollout: Clearance, then this
    (bundle, pin, definitions), then Glidepath.

### Added (A-11, agent provisioning hardening)
- `airframe.capabilities {section: appspec}`: the AppSpec's top-level fields, required vs optional, how `config` and
  `overrides` map onto chart values, and one worked example (the parachute spec), derived from
  `schemas/appspec.schema.json` (under 12,000 characters, test-asserted). The default answer's `more` names it and now
  comes first (the default answer is ~19,000 characters, so the old trailing `more` was clipped away). The policy bundle's
  `section` allowlist admits `appspec` and the descriptions of `airframe.capabilities` and `airframe.plan` name it.
- Runtime: a clipped tool result ends with `[CLIPPED at N of M characters ... ]` and how to narrow (for
  `airframe.capabilities`: `section` or `kind`). Needs a new runtime tag and a pin bump in the agent definitions.
- `airframe.apply` merges into the files Glidepath's onboarding scaffolds (`glidepath/envs/*.yaml`, gitops `*/values.yaml`)
  instead of refusing `APPLY-EXISTS`: the plan's keys are deep-merged, the header comment and other keys kept. A key the
  plan sets that already holds a different value is `APPLY-CONFLICT` (naming the key paths).
- `airframe.apply` waits (`waiting`) while the app repo or `gitops-<app>` has an open `onboarding-resync` PR (the resync
  that CicdOnboarded=True does not wait for); `GitHubPRs.onboarding_prs`.
- Run results carry `warnings` (and the session outcome too): `airframe.plan` called more than 3 times, or
  `airframe.apply` answered ok but opened no PR (unless it ended at `agent-steps-merged`).
- `agents/parachute.yaml`: read `appspec` once, at most 3 plans, stop and ask (`human.request`) instead of proceeding with
  less than asked, wait for each merge in the same run, and end every stop with a resume prompt holding the AppSpec JSON.
  `ttlMinutes` 30 to 60 and `toolCalls` 60 to 80 so a run can wait for merges. `app-provisioner` is unchanged.

### Fixed
- The gateway publishes its bundled agents (`agents/`) to the Clearance core with the cluster pins (`PUT /v1/definitions`
  `bundled`: name, `AUTOPILOT_GATEWAY_VERSION`, document). Seen live on hangar-dev 2026-11-12: `parachute` opened
  gitops-hangar-dev-tenants#30 and `Clearance / agent-scope` failed AS004 "agent definition 'parachute' is not in this
  deployment", because since R-3 the core (no `CLEARANCE_AGENTS_DIR` on Hangar) had only the pins. A gateway without the
  Agent XR loader publishes the set at start and retries until the core takes it (`DefinitionsPublisher`). A core that
  predates the field refuses it (400); the gateway then publishes the pins without it and keeps running.
  Needs clearance v0.42.0 (`bundled`; the pins `pyproject.toml`, `CLEARANCE_REF`, `uv.lock` name it). Roll out the
  core first, then this gateway tag.
  The Go gateway (`gateway/`) publishes no definitions and stays on its v0.31.0 `go.mod` pin.

### Added
- `agents/app-provisioner.yaml`: a general provisioning agent beside `parachute` (owner decision 2026-11-11). Same tools
  (`airframe.apply` and the reads), T1, limits and runtime pin as parachute, but `airframe.apps: ["*"]` and
  `repos.allow: ["jfillman/*"]`, so it stands up any app from an AppSpec. This RAISES authority over parachute's single
  app; unchanged bounds: PRs only, no merge or push, Flight half and `gitops-<app>` human-only, R021 field scope.
  `examples/app-provisioner.appspec.yaml`; tests show its R023 scope admits another app and parachute's still refuses one.

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
