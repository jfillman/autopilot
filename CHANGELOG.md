# Changelog

Autopilot's gateway and runtime images are tagged `gateway-v*`, `runtime-v*` and `runtime-<name>-v*`; this file
says what each gateway tag adds. The manager cuts the tags.

## Unreleased

### Added
- Preflight (A-6; [docs/admin/preflight.md](docs/admin/preflight.md)):
  - `RunRecord`s from real runs (`record_from_run`: a run's result plus its evidence), checks per verifier in the case
    files, and the local harness `autopilot preflight run --case <id> [--seeded-bad | --run file | --all]`. The six
    Skyport cases have a scripted good run and a seeded bad run in `preflight/runs/`; CI runs `--all`.
  - `POST /v1/agents/{name}/runs` takes a `document` (a PR's candidate definition) and a `budget`: a Preflight run as a
    clamped task named `pf-<agent>-<fingerprint>`. `POST /v1/preflight/score` scores the runs against the agent's cases and
    records the definition's result (`pass`/`fail`, cases) on its run records while it is unchanged.
  - The image carries `preflight/cases` (`PREFLIGHT_CASES_DIR`).
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
- The scheduler claims each slot in the ledger: a restart inside the grace window after a slot fired no longer fires
  it again, and a slot whose window passed with the gateway down is recorded `missed` (not run late).
- Event history outcomes carry `via` (stream, recent, reconcile); a replay of an event already kept is not written
  again; the bridge's history writes hold the gateway lock and commit.
- An event that matched while held, braked or refused stays decided: no later path launches it.
