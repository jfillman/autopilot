# Changelog

Autopilot's gateway and runtime images are tagged `gateway-v*`, `runtime-v*` and `runtime-<name>-v*`; this file
says what each gateway tag adds. The manager cuts the tags.

## Unreleased

### Added
- `airframe.apply` (A-3; T1; [README](README.md), "airframe.apply and the parachute"): the planner's change set opened as
  pull requests by the agents' GitHub App, one per call, each with a receipt (repo, pr, commit, branch, paths) that
  Clearance's provenance reads (`pr_tools` in the Hangar bundle, now 1.1.0). It opens the tenants-repo PR, then the app
  repo's (cicd.yaml checked by glidepath-api as an agent's change), resumes from GitHub after an interruption (branches
  are fixed by the plan; nothing is kept in the gateway), never opens the Flight half (cicd.yaml's Flight fields,
  `gitops-<app>`) and has no way to merge. `agents/parachute.yaml` is its definition; `examples/parachute-bad.appspec.yaml`
  is the seeded bad run. R023 scopes it by `spec.app`.
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
