# Trigger durability

**Status: Built, not yet live** (A-5, 2026-11-03). In the gateway from the first gateway tag after this change; on
hangar-dev once that tag is pinned and apron wires the metrics port ([monitoring](#metrics-and-alerts)). The live
checks are [below](#live-verification-on-hangar-dev).

Before A-5 the event bridge kept its place in glidepath-api's stream in memory, and the scheduler kept the slots it
had fired in memory. A gateway restart forgot both: events that came while it was down were replayed only as far as
glidepath-api's in-memory buffer reached (glidepath known gap #44), a restart inside a slot's grace window could fire
the slot again, and nothing said when an event had been lost. Now:

| What | How | Where |
|---|---|---|
| A trigger launches once | Every firing has a key; one row per (agent, key) is claimed before a session is opened, and the decision is written to it. A second claim loses, from any path. | `gateway_trigger_launches`, `engine/trigger_ledger.py` |
| The bridge resumes where it stopped | The newest event id handled is saved after each event. At start the bridge reads `GET /v1/events/recent?since=<cursor>` and handles what it missed, then holds `GET /v1/events` with `Last-Event-ID`. | `gateway_event_cursors`, `event_bridge.py` |
| A lost `pipelinerun.finished` still launches | Every 5 minutes the sweep reads glidepath-api's run records (`GET /v1/runs`, Tekton Results) for the apps a trigger watches, rebuilds each finished run's event and hands it to the bridge. A run whose event came is already claimed; one whose event never came launches now, once. | `reconcile.py` |
| The gap is visible | `autopilot_trigger_missed_total{agent}` counts what the sweep launched; alert `AutopilotTriggerGap`. | `metrics.py`, [monitoring manifest](monitoring/autopilot-triggers.yaml) |
| A slot fires once, on time | The slot's key is claimed in the same ledger, so a restart inside the 10-minute grace window after the slot fired does nothing. A slot whose window passed with the gateway down is recorded `missed` and alerted, not run late. | `scheduler.py` |

ADR-0030 (AMQP fan-out inside Glidepath) stays deferred by the user's 2026-11-01 decision: none of this needs
glidepath-api to change.

## The trigger ledger

`gateway_trigger_launches`: primary key (agent, key). The key is:

| Firing | Key | Why |
|---|---|---|
| A `pipelinerun.*` event | `<routingKey>/<subject id>`, e.g. `gate-api.pipelinerun.finished/gate-api-push-abc12` | The routing key and the CDEvent subject id (the PipelineRun's name) are both in glidepath-api's documented envelope, and a run record gives the same pair. The stream and the sweep agree on "this run's finished event" without knowing how glidepath-api forms event ids. Two events for one PipelineRun launch once. |
| Any other Glidepath event | `event:<id>` | glidepath-api's id, the same on every replica |
| An interval slot | `slot:<every>:<n>` | the UTC-aligned slot number (`scheduler.slot_of`) |

`state` is the decision: `launched`, `deduped` (Gateway.open_triggered found the task's run still live), `braked`
(over `maxPerHour`), `held` (a person's hold, R024), `refused`, `launch-failed`, `missed` (a slot), or `claimed` while
it is being made. Every decision is final: a braked or held event is never launched later by the sweep. A row left
`claimed` (the process died between claim and decision) is taken again by the next process at once (one writer holds
the database, so a claim older than the process is an orphan) or after 5 minutes; if its run had in fact started and
is still live, the open is answered as a dedupe. `via` says which path claimed it: `stream`, `recent` (the start-up
catch-up), `reconcile`, `schedule` (`rabbitmq` is reserved). Rows are kept 30 days; the sweep prunes them.

The AgentRun's task id is a hash of the agent and the key (`triggers.event_task_id`), so a live duplicate is also
caught by the gateway's own dedupe.

## The cursor and the catch-up

`gateway_event_cursors`: one row per source. `glidepath` holds `last_id`, the newest event id handled (moved forward
only; ids order by receive time, compared bytewise), and `floor_at`, when the ledger first ran. `schedule` holds only a
`floor_at`. On the first start there is no cursor: the bridge starts live, with no backlog. On every later start:

1. `GET /v1/events/recent?since=<last_id>&limit=10000`: what glidepath-api's buffer holds after the cursor (plus its
   10-second overlap), handled oldest first as `via recent`.
2. `GET /v1/events` with `Last-Event-ID: <newest id handled>`; a reconnect after a dropped stream sends it too.

glidepath-api's buffer is 2000 events and 24 hours, in memory, and empty after every replica restarts at once. A
cursor older than 24 hours counts as `stale-cursor` in `autopilot_events_resume_total` (alert
`AutopilotEventCursorStale`); a failed catch-up as `failed` (the stream still resumes with `Last-Event-ID`). Neither
says which events were lost: the sweep is what finds a lost `pipelinerun.finished`.

## The reconciliation sweep

Every `RECONCILE_SECONDS` (300; the first one interval after start, after the catch-up), for each app a Glidepath
trigger's `bindingKey` can match as `<app>.pipelinerun.finished` (literal apps by `app=`; a `*` or `#` app reads the
fleet), the sweep reads `GET /v1/runs?app=&sinceHours=RECONCILE_WINDOW_HOURS&limit=1000`. For each finished run it
rebuilds the event (`reconcile.envelope_from_run`): routing key `<app>.pipelinerun.finished`, subject id the run's
name, `subject.content.outcome` from its status (succeeded, failed, cancelled to success, failure, cancel; anything
else error, as glidepath's `cdevents_map_outcome`) and `pipelineName` from its Pipeline. It hands that to the bridge as
`via reconcile`, under the same storm brake and holds. What it launches is kept in the event history as
`reconcile:<app>/<run>` with `via: reconcile` on each outcome, and counted in `autopilot_trigger_missed_total{agent}`.

It skips runs that finished before the ledger's floor (turning A-5 on does not launch a day of old failures) and
runs that finished in the last 2 minutes (their event is sent from the run's finally block and normally arrives
first). It reads at most 1000 runs per app per sweep and logs when it hits that.

Limits, stated: a trigger that matches a CDEvent field a run record does not carry (anything but the outcome, the
pipeline and the subject id) will not match a rebuilt event; no definition does today (`triage-agent` matches
`subject.content.outcome`). Events other than `pipelinerun.finished` are not reconciled: there is no record to read
them back from. The storm brake's window is in memory, so a restart resets it.

## The scheduler

Unchanged: each interval is a UTC-aligned slot (every 24h: 00:00 UTC), fired within 10 minutes of its start, once.
What changed: the slot is claimed in the ledger, so a restart at 00:04 after the 00:00 slot fired (its run finished
or not) does not fire it again. A slot whose window passed with no firing (the gateway was down from 00:00 to past
00:10) is recorded `missed`, once, counted in `autopilot_schedule_missed_total{agent}` (alert
`AutopilotScheduleSlotMissed`) and not run late. `autopilot_schedule_lag_seconds{agent}` is how long after its slot's
start the last firing came: one 30-second tick at most while the gateway is up.

## Configuration

| Variable | Default | |
|---|---|---|
| `RECONCILE_SECONDS` | `300` | How often the sweep runs, with `EVENTS_ENABLED`; at least 30; `0` turns it off |
| `RECONCILE_WINDOW_HOURS` | `24` | How far back it reads run records (1..744; glidepath-api's `sinceHours`) |
| `METRICS_PORT` | `9090` | GET /metrics on its own port (not the MCP port, so the cluster Gateway's route never exposes it); `0` turns it off |
| `EVENTS_RABBITMQ_ENABLED` | `0` | Reserved: `1` refuses to start (see [A RabbitMQ source](#a-rabbitmq-source)) |

The sweep needs nothing new from glidepath-api: `GET /v1/runs` is authorized like the events routes (cluster-wide
without `app=`, in the app's namespace with it), and the gateway's identity already has glidepath-api's reader role.

## Metrics and alerts

| Series | Type | Means |
|---|---|---|
| `autopilot_events_received_total{via}` | counter | events handled, `stream` or `recent` |
| `autopilot_events_stream_connected` | gauge | 1 while the stream is held |
| `autopilot_events_last_received_timestamp_seconds` | gauge | when the last event was handled |
| `autopilot_events_cursor_timestamp_seconds` | gauge | the receive time in the saved cursor |
| `autopilot_events_resume_total{result}` | counter | start-up catch-ups: `caught-up`, `stale-cursor`, `failed` |
| `autopilot_trigger_outcomes_total{agent,via,result}` | counter | every decision, by path |
| `autopilot_trigger_missed_total{agent}` | counter | **the gap**: launches the sweep made because no event came |
| `autopilot_reconcile_sweeps_total{result}` | counter | sweeps, `ok` or `failed` |
| `autopilot_reconcile_last_success_timestamp_seconds` | gauge | the last good sweep |
| `autopilot_schedule_launches_total{agent,result}` | counter | slots fired |
| `autopilot_schedule_missed_total{agent}` | counter | slots whose window passed unfired |
| `autopilot_schedule_lag_seconds{agent}` | gauge | slot start to firing, last slot |

A gauge has no sample until it is set, so an alert on one never fires for a feature that is off.

[monitoring/autopilot-triggers.yaml](monitoring/autopilot-triggers.yaml) is the manifest for apron (not applied from
this repo): the container port `metrics` (9090), a NetworkPolicy admitting only Prometheus (`observability`,
`app.kubernetes.io/name: prometheus`) to it, a PodMonitor, and a PrometheusRule (`release: kube-prometheus-stack`)
with `AutopilotTriggerGap`, `AutopilotReconcileFailing`, `AutopilotReconcileStale`, `AutopilotEventStreamDown`,
`AutopilotEventCursorStale`, `AutopilotScheduleSlotMissed` and `AutopilotScheduleLate`. After wiring, confirm the
target is up (`up{namespace="autopilot-system"}`): app scrapes on these clusters were found down on 2026-10-23.

## A RabbitMQ source

**Proposal; the flag is a stub.** The Skyport agents (`agents/skyport/`, e.g. `disruption-responder`: `{type: event,
source: rabbitmq, brokerRef: skyport-broker, exchange: flights.events, bindingKey: "flight.*.delayed"}`) trigger on
messages from Skyport's own RabbitMQ, not Glidepath's events. They are not loaded by the gateway today (it reads
`agents/*.yaml`, not `agents/skyport/`), and nothing in the gateway reads AMQP. A reader is not small: an AMQP client
dependency (pika), broker credentials from `brokerRef` (a Secret per broker, named by the definition), a queue per
gateway with its bindings, TLS, and acks. So `EVENTS_RABBITMQ_ENABLED=1` refuses to start rather than look on.

The design, when it is built:

- One durable quorum queue per broker, `autopilot.<cluster>`, declared by the broker's owner in Git (not by the
  gateway), bound to each exchange and `bindingKey` a loaded definition names (`triggers.match_event` already has the
  topic semantics). The gateway's broker user may consume from that queue only.
- A consumer thread per broker, beside the SSE bridge, with manual acks and a prefetch of 10. Each message goes
  through the same ledger: key `amqp:<exchange>:<message-id>` (AMQP `message_id`; a message without one is
  rejected to a dead-letter queue, never keyed by its body), `via rabbitmq`, the same storm brake and holds. The ack
  is sent after the ledger decision commits, so a crash redelivers and the ledger dedupes.
- No cursor is needed: the queue is the durability. A consumer that is down leaves messages in the queue (bounded by
  the queue's own length and TTL policy, which the broker owner sets).
- Metrics: `autopilot_trigger_outcomes_total{via="rabbitmq"}` and a `autopilot_amqp_connected{broker}` gauge.

## Live verification on hangar-dev

For the manager, after a gateway tag with this change is pinned in apron (and the metrics port wired). Context
`hangar-dev`; the gateway is `deploy/clearance` in `autopilot-system` (one replica, Recreate), its database
`autopilot-pg` (database `autopilot`), glidepath-api `deploy/glidepath-api` in `platform-system` (2 replicas, label
`app=glidepath-api`). The trigger under test is `triage-agent` (`gate-api.pipelinerun.finished`, outcome failure, 4
an hour).

Shorthands:

```shell
K="kubectl --context hangar-dev"
PSQL="$K -n autopilot-system exec autopilot-pg-1 -c postgres -- psql -d autopilot -At -c"
# glidepath-api reads: a port-forward and a token for an identity in glidepath-api's api.readers ($GP_TOKEN, the
# manager's usual one). `kubectl get --raw` through the services proxy answers 401: the API server drops
# Authorization, and glidepath-api reads X-Glidepath-Authorization instead (as Tower does).
$K -n platform-system port-forward svc/glidepath-api 18080:8080 &
GP() { curl -sf -H "X-Glidepath-Authorization: Bearer $GP_TOKEN" "http://127.0.0.1:18080$1"; }
```

**0. The tables and the floor exist.** After the new gateway starts:

```shell
$PSQL "SELECT source, last_id, floor_at FROM gateway_event_cursors ORDER BY source"
$K -n autopilot-system logs deploy/clearance | grep -E "reconcile=|metrics="     # the start line
```

`glidepath` with a `floor_at` of the start (and a `last_id` once one event came), `schedule` once a scheduled agent
is loaded; the start line says `reconcile=every 300s over 24h metrics=:9090`.

**1. Restarting the gateway during a build loses no trigger.** Start a gate-api run that will fail (re-run a failed
one: pick a name from `GP "/v1/runs?app=gate-api&sinceHours=168" | jq -r '.runs[] | select(.status=="failed") | .name' | head -1`,
then `POST /v1/apps/gate-api/pipelineruns/<name>/rerun` with an `Idempotency-Key`, as glidepath docs/admin/api.md
"Re-run" says, or push a failing commit to a gate-api PR). While it runs, take the gateway down so its
`pipelinerun.finished` arrives with no bridge listening, and bring it back after the run has finished:

```shell
$K -n autopilot-system scale deploy/clearance --replicas=0
$K -n app-gate-api-cicd get pipelinerun <new run> -w        # until SUCCEEDED False
$K -n autopilot-system scale deploy/clearance --replicas=1
$K -n autopilot-system logs deploy/clearance -f | grep -E "catch-up|via recent"
```

Pass: `event catch-up from <id>: N event(s) (caught-up)` and `event <id> gate-api.pipelinerun.finished via recent:
triage-agent launched`; then

```shell
$PSQL "SELECT agent, key, via, state, session, run FROM gateway_trigger_launches WHERE key LIKE 'gate-api.pipelinerun.finished/%' ORDER BY claimed_at DESC LIMIT 3"
$K -n autopilot-runs get agentruns.catalog.hangar.io --sort-by=.metadata.creationTimestamp | tail -2
```

one row for the new run, `via recent`, `state launched`, with its AgentRun; the core's record for the session open
names trigger `glidepath:<event id>`. A second restart (`rollout restart deploy/clearance`) launches nothing new: the
replay is `deduped` in the log and no row changes.

**2. A seeded gap is reconciled.** Make glidepath-api lose the event: gateway down, a failing gate-api run, then every
glidepath-api replica restarted at once so the buffer is empty, then the gateway up:

```shell
$K -n autopilot-system scale deploy/clearance --replicas=0
# start a failing gate-api run as in 1 and wait for it to finish
$K -n platform-system delete pod -l app=glidepath-api       # both at once: no peer to copy the buffer from
$K -n platform-system rollout status deploy/glidepath-api
$K -n autopilot-system scale deploy/clearance --replicas=1
```

The catch-up finds nothing for that run. Within `RECONCILE_SECONDS` (5 min) after start plus the 2-minute settle:

```shell
$K -n autopilot-system logs deploy/clearance | grep -E "reconcile: [0-9]+ trigger"
$PSQL "SELECT agent, key, via, state FROM gateway_trigger_launches WHERE via = 'reconcile'"
$PSQL "SELECT id, record->'outcomes' FROM gateway_events WHERE id LIKE 'reconcile:%' ORDER BY received_at DESC LIMIT 3"
$K -n autopilot-system exec deploy/clearance -- python -c "import urllib.request;print(urllib.request.urlopen('http://127.0.0.1:9090/metrics').read().decode())" | grep -E "missed|reconcile_sweeps"
```

Pass: `reconcile: 1 trigger(s) no event delivered: triage-agent reconcile:gate-api/<run> launched`, one row `via
reconcile` `launched`, `autopilot_trigger_missed_total{agent="triage-agent"} 1`, and the next sweeps launch nothing.
With the PrometheusRule wired, `AutopilotTriggerGap` fires for an hour.

A cheaper seed, if no failing run can be made: delete the ledger row of a failed run the bridge already launched
(within the last 24 h and after the floor), which looks to the sweep exactly like an event that never came. It launches
`triage-agent` on that run a second time, so use it only in the lab:

```shell
$PSQL "DELETE FROM gateway_trigger_launches WHERE agent = 'triage-agent' AND key = 'gate-api.pipelinerun.finished/<run>'"
```

**3. The nightly slot runs on time.** After 00:00 UTC:

```shell
$PSQL "SELECT agent, key, state, session, run, claimed_at FROM gateway_trigger_launches WHERE via = 'schedule' ORDER BY claimed_at DESC LIMIT 3"
```

Pass: `nightly-reviewer`, key `slot:24h:<n>` with `n = floor(epoch / 86400)` of that day, `state launched`,
`claimed_at` within 30 s of 00:00:00 UTC, and `autopilot_schedule_lag_seconds{agent="nightly-reviewer"}` at most 30.
Optional: `rollout restart deploy/clearance` at 00:03; the slot does not fire again (no second row, no second
AgentRun).
