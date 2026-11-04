# The gateway's database

**Status: Live on hangar-dev** since the R-4 cutover (2026-11-03): gateway-v0.4.0 writes this database and calls
clearance v0.29.0's core; autopilot-api serves the runtime reads from it (`GATEWAY_API_DSN`, `gateway_*` only).

The gateway keeps runtime state, not evidence, in a Postgres database of its own (`GATEWAY_DSN`). The evidence chain,
the deployment gate's changes and the core's definitions stay in clearance-pg, which only the Clearance core (the one
writer) and clearance-api (read-only) open. The gateway has no DSN for it.

| Table | What it holds | Was (clearance-pg) |
|---|---|---|
| `gateway_sessions` | every session: grant, budgets used and reserved, breaker, run, input, outcome | `clearance_sessions` |
| `gateway_human_requests` | `human.request` questions and their answers | `clearance_human_requests` |
| `gateway_results` | a finished run's answer, 30 days | `clearance_results` |
| `gateway_artifacts` | `artifact.put` text per task, 7 days | `clearance_artifacts` |
| `gateway_events` | what the trigger bridge received and did, 30 days | `clearance_events` |
| `gateway_holds` | people's holds on an agent's triggers | `clearance_holds` |
| `gateway_definitions` | the cluster-pinned definition set (the gateway's copy) | `clearance_definitions` (the core keeps its own) |
| `gateway_outbox` | records and outcomes owed to the core, in order (`engine/outbox.py`) | new |
| `gateway_trigger_launches` | the trigger ledger: one row per (agent, trigger key), claimed before a triggered session opens, with the decision; 30 days ([trigger-durability.md](trigger-durability.md)) | new (A-5) |
| `gateway_event_cursors` | the event bridge's cursor (newest event id handled) and the ledger's floor, per source | new (A-5) |

Same columns as before (`src/autopilot/engine/pg.py` SCHEMA), created at start if missing. One writer: the gateway
takes advisory lock `apg0` and a second one stops at start.

## The outbox

An entry is a record (`POST /v1/records`) or an outcome (`POST /v1/decisions/{id}/outcome`), with why it waits:
`outage` (a T0 read allowed during a core outage, posted as a coverage-unavailable `tool.call`), `core-unavailable`
(the core could not take it when it was written), `held` (a kind the core does not accept yet) and `refused` (the
core answered and refused it; kept with the error, never retried). Since clearance v0.28.0 the core takes every kind
the gateway writes (`run.finish`, `session.end`, `trigger.release`, the outage `tool.call` among them), so nothing new
is `held`; a row an older gateway held is posted by the next drain. The definition set is not in the outbox: the
gateway publishes it whole to the core (`PUT /v1/definitions`) and makes the publish again on the next poll if the
core did not take it. At start the gateway deletes pending `held` `definition.*` rows an older gateway left (the
core writes those records itself and refuses them from a provider). States: `pending`, `done` (with the record's seq),
`refused`. The expiry sweep posts pending entries oldest first and stops at the first the core cannot take; a new
record waits behind them.

To see what waits:

```sql
SELECT id, op, kind, reason, state, created_at, error FROM gateway_outbox WHERE state <> 'done' ORDER BY id;
```

## The cutover copy (R-4)

**Used once**, at the hangar-dev cutover on 2026-11-03: it copied 24 sessions, 2 human requests, 145 events, 2 results
and 1 definition. Kept as the record of the move; the steps below are what was run. The read API no longer reads the
`clearance_*` runtime tables (its `GATEWAY_API_TABLES=clearance` option is retired), so they can be dropped.

`tools/cutover/copy_runtime_tables.py` copies the seven runtime tables from clearance-pg into the gateway's database,
row for row: `clearance_<name>` to `gateway_<name>`. It deletes and changes nothing in clearance-pg.

1. Scale the running (Python) gateway to 0, so nothing writes the runtime tables.
2. Create the gateway's database and role (apron's template), then:

   ```shell
   python tools/cutover/copy_runtime_tables.py --from "$CLEARANCE_DSN" --to "$GATEWAY_DSN" --dry-run
   python tools/cutover/copy_runtime_tables.py --from "$CLEARANCE_DSN" --to "$GATEWAY_DSN"
   ```

   It reads clearance-pg in one read-only snapshot, writes the target in one transaction holding the gateway's
   writer lock (it refuses to run beside a gateway), refuses a target table that already has rows (`--replace`
   overwrites), checks every row count, and prints counts only.
3. Start the new gateway with `GATEWAY_DSN` and `CLEARANCE_CORE_URL`.
4. Once the run is verified, the `clearance_*` runtime tables in clearance-pg can be dropped (not by this script).

Tested on scratch databases (`tests/test_gateway_pg.py`, CI's postgres service): rows as clearance v0.26.1 wrote
them, a dry run that writes nothing, an exact copy of every table, the evidence left behind, both refusals, and the
gateway opening the copied tables.
