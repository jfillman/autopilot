# The Go gateway: release, rollout and cutover (A-9 S7)

What it takes to run the Go gateway (`gateway/`, [go-gateway.md](go-gateway.md)) on a cluster: the image and how it is released, the
database steps, the settings, and the ordered cutover and rollback. **Nothing here deploys anything**: merging to `main` ships no image and
changes no cluster. The Python gateway stays the deployed one and the rollback target until the cutover (S9) is done and accepted.

## The image

| | Python (today) | Go (S7) |
|---|---|---|
| Dockerfile | `Dockerfile` | `gateway/Dockerfile` |
| Workflow | `.github/workflows/gateway-image.yaml` | `.github/workflows/gateway-go-image.yaml` |
| Tag | `gateway-v*` (live: `gateway-v0.9.1`) | `gateway-go-v<major>.<minor>.<patch>` |
| Package | `ghcr.io/jfillman/autopilot-gateway` | `ghcr.io/jfillman/autopilot-gateway-go` |

The tag namespaces and packages are disjoint (`gateway-v*` does not match `gateway-go-v0.1.0` and the reverse), so a Go release cannot
replace, or be mistaken for, a Python one, and the Python image is still there to roll back to.

The Go image is one static binary on distroless static, nonroot (uid 65532), no shell, nothing written at runtime (a read-only root filesystem
works), for `linux/amd64` and `linux/arm64`. The entrypoint is the binary and the default command is `serve`; the same image is the gateway
(`serve`), the read API (`api`) and the migration (`migrate`). It carries what the Go gateway reads next to `agents/`: `runtime/catalog.yaml`
(`GET /v1/runtimes`) and `preflight/cases/` (the Preflight score endpoint).

### What the workflow gates on

A pull request that touches the Go gateway runs every gate and **builds the image for both architectures without pushing it**. A
`gateway-go-v*` tag runs the same gates and then pushes. Nothing is pushed unless all of these pass:

1. the tag is `gateway-go-vMAJOR.MINOR.PATCH`;
2. the generators are current: `gen_gateway_registry.py`, `gen_gateway_ported.py`, `gen_gateway_readapi.py`, `gen_gateway_env_table.py` (each `--check`);
3. `go vet ./...` and `go test -race ./...` over a real Postgres (the database, read-only-role and migrate tests) and the real Clearance core binary;
4. the replay of Clearance's golden corpus through the Go gateway (`TestCorpusReplaysThroughGateway`);
5. the environment table matches the Python source (`tests/test_gateway_env_table.py`);
6. the image, built for the runner (amd64) and loaded locally, passes **the released core image's runtime conformance suite run against the
   container** (read-only root filesystem, no capabilities, `tests/test_gateway_go.py`) and the image checks: nonroot, no shell, the bundled
   agents, runtime catalog and Preflight cases are in it, `version`, `migrate` and `api` start and refuse a missing setting by name;
7. the multi-arch build (amd64 and arm64) succeeds; on a tag it is pushed and started on both architectures.

The supply-chain steps are the Python workflow's, copied: actions by version tag (that workflow does not pin by SHA either), QEMU and buildx,
the `GITHUB_TOKEN` to ghcr.io, `provenance: false`; there is no SBOM and no signature on either image. A new ghcr.io package is private:
make it public once, or give the cluster a pull secret. Pin the deployment by tag **and digest** (the digest is in the run's summary).

## The database

The Go gateway uses the Python gateway's database (`autopilot-pg`, the `gateway_*` tables): same tables, same advisory lock, either implementation
reads the other's rows. Three things are Go's own, all additive:

- three columns on `gateway_trigger_launches` (`failures`, `first_failed_at`, `last_failed_at`), added by `serve` and `migrate`
  (`ADD COLUMN IF NOT EXISTS`, with defaults, so Python's statements are unaffected);
- seven indexes: six the read API's queries need and one (`gateway_outbox_refused`, a partial index on `gateway_outbox (created_at, id) WHERE state = 'refused'`) the Preflight score's outbox guard reads under a lock (below);
- a read-only role for `api`.

### `autopilot-gateway migrate`

`serve` **no longer creates indexes** (a plain `CREATE INDEX` blocks the table's writes for as long as it builds). They are made by an explicit
step, the same image with the command `migrate`:

```
autopilot-gateway migrate            create what is missing (the schema, then each index)
autopilot-gateway migrate --check    change nothing; exit 1 if an index is missing or invalid
```

- **`CREATE INDEX CONCURRENTLY IF NOT EXISTS`**, one statement per index, so no write to a gateway table waits for a build. It may run while
  the Python or the Go gateway is writing.
- **A routine re-run takes no table lock**: the schema step (`ALTER TABLE ... ADD COLUMN IF NOT EXISTS`, which needs an ACCESS EXCLUSIVE lock even when the column is
  there) is skipped when the Go-only columns already exist, and a valid, correct index is not touched (tested with a reader holding ACCESS SHARE and a 300 ms lock timeout).
- **The definition is checked, not only the name**: an index with the right name and another definition is rebuilt **beside the old one** (new under a temporary
  name, concurrently; then the old one dropped, the new renamed), so the table is never unindexed and no write waits. The expected text is not hard-coded: the same definition is
  built on an empty TEMP table and its `pg_get_indexdef` compared, so the check follows Postgres's own printing across versions. `migrate --check` fails on it too.
- **Idempotent and repairable**: a valid index is left alone; an `INVALID` one (what an interrupted concurrent build leaves) is dropped
  (`DROP INDEX CONCURRENTLY`) and built again. A failure leaves nothing a second run does not repair.
- **Lock and statement timeouts**: `lock_timeout` 5 s (`MIGRATE_LOCK_TIMEOUT_SECONDS`) so a statement never queues behind a long transaction (it
  fails, and a re-run completes it; the concurrent build waits only for transactions older than itself); `statement_timeout` 15 minutes
  (`MIGRATE_STATEMENT_TIMEOUT_SECONDS`) per index (the tables are some thousands of rows and build in a second; a table far past that wants a decision,
  not a wait).
- **One at a time**: its own advisory lock (`apg1`; the writer's is `apg0`), so two migrates do not race, and it does not need the writer's lock.
- It uses `GATEWAY_DSN` (the owner's DSN: it creates indexes). Exit 0 done, 1 failed or missing (`--check`), 2 configuration.
- **`serve` and `api` check at start** that the seven exist and are valid (they only read the system catalogs) and **refuse to start** when one is not (exit 1 / 2),
  naming it and saying to run `autopilot-gateway migrate`: a silent slow read path in production is what `migrate` exists to prevent.
  `GATEWAY_ALLOW_MISSING_INDEXES=1` (default off) starts anyway, with an error line each start. **Ordering in the cutover**: `migrate` and `migrate --check` run
  **The release that adds `gateway_outbox_refused` (Preflight paths, strip W-16) needs the `migrate` step before `serve` and `api` roll**, like the
  first six: until it has run both refuse to start, naming `gateway_outbox_refused (missing)`. The index is small (it covers refused rows only) and is built concurrently.
  BEFORE the switch (step 4), so the first Go start finds the indexes; the override is for an emergency rollforward, not a procedure.

**Deployment choice, and why**: `migrate` is a **manual step in the cutover runbook** unless the render emits it (`autopilot.gateway.go.migrateJob: true`), and it is
**not** an Argo CD hook (a hook is dropped from a partial sync, argo-cd#28701). The runbook runs it once as a one-off Job (below), or lets the render's
tracked Job do it, then `migrate --check` is a gate before the switch. (`kubectl create job --from` takes a CronJob, so it is not used.)

```
kubectl -n autopilot-system apply -f - <<'EOF'
apiVersion: batch/v1
kind: Job
metadata: {name: autopilot-gateway-migrate-1, labels: {app.kubernetes.io/name: autopilot-gateway-migrate}}
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 3600
  template:
    metadata: {labels: {app.kubernetes.io/name: autopilot-gateway-migrate}}
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: migrate
          image: <the Go image, tag@digest>
          args: [migrate]          # then a second Job with args: [migrate, --check]
          env: [{name: GATEWAY_DSN, valueFrom: {secretKeyRef: {name: autopilot-pg-app, key: uri}}}]
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}}
EOF
kubectl -n autopilot-system wait --for=condition=complete job/autopilot-gateway-migrate-1 --timeout=20m && kubectl -n autopilot-system logs job/autopilot-gateway-migrate-1
```

The database admits only the gateway and read API pods (`networkpolicy.yaml`); the policy that admits the Job's label (`autopilot-pg-migrate`) is rendered only with
`autopilot.gateway.go.migrateJob: true`, so for the first cutover set it, render and sync. Since 2026-11-16 both are **ordinary tracked resources ordered by sync waves**,
not hooks: the NetworkPolicy `autopilot-pg-migrate` is wave 0, the migrate Job is wave 1, and the Go Deployments are wave 2. Argo CD waits for each wave before the next
(a Job is Healthy when Complete and Degraded when Failed), so the Go pods change only after `migrate` has succeeded. The hand-run Job above is then only needed for
`migrate --check` and re-runs; its pod label `app.kubernetes.io/name: autopilot-gateway-migrate` is the one the policy admits (the policy's name, `autopilot-pg-migrate`, is not a label). Notes:

- The Job is named for its image (`autopilot-gateway-migrate-v0-1-0-fce8be95`: the tag without `gateway-go-`, then the first 8 hex of the digest). A Job's pod template is
  immutable, so a new migration (a new image) is a new Job name, and it runs once per image.
- There is no `ttlSecondsAfterFinished` on it: a finished Job that was deleted would be re-created, and `migrate` would run again, on the next sync. Older Jobs stay until
  deleted by hand.
- **Never start a sync by patching the Application's `.operation` with `kubectl patch`.** It can inherit a stale partial resource list and silently skip steps (upstream
  argo-cd#28701). Trigger the sync from git or the Argo CD UI.

**If the tracked Job Failed** (read from the manifests, not tested live): the Job is Degraded, so the Application is Degraded (with `implementation: go` the sync also
fails at wave 1) and the Go Deployments, which are wave 2, are not rolled. The Job is kept for its log (`kubectl -n autopilot-system logs job/<name>`). Then either
fix the cause and ship a new image, which is a new Job name through git and so a fresh run; or delete the failed Job, and Argo CD re-creates and re-runs it on the next
sync (a deleted tracked Job is re-created, see above). Never start that sync with a `.operation` patch. The hand-run Job above does not change the tracked Job's
status, so it cannot clear a Degraded Application.

### The read-only role (`GATEWAY_API_DSN`)

**The default is the role the cluster already has.** The Python deployment's CNPG-managed role `autopilot_reader` (`pg_read_all_data`, its password
generated by an ESO generator into the Secret `autopilot-pg-reader`, key `uri`) is read-only and passes the same check: the real binary starts as it
(`TestTheRealAPIRefusesEveryRoleShapeThatCouldWrite` includes the `pg_read_all_data` shape). The Apron switch therefore defaults to that secret: the
cutover needs no new role and no new secret.

**The option, for other installs or the narrower grant** (only `gateway_*` tables readable, no other table): `gateway/sql/gateway-api-reader.sql` makes the
NOLOGIN group role `gateway_api_reader` (`SELECT` on every `gateway_*` table and nothing else: no write, no sequence, no `CREATE`, no default
privileges, no membership). `TestTheRoleFileHasOnlyTheStatementsItClaims` fails on any statement outside an allowlist and
`TestTheRoleFileGrantsNothingButSelect` checks the live role. The owner then makes a login role in the group. **Never put the password on a command
line** (it lands in shell history and the process list): create the role without one and set it with `\password`, which sends the hash, then put the
DSN in Infisical:

```
psql "$OWNER_DSN" -v ON_ERROR_STOP=1 -f gateway/sql/gateway-api-reader.sql
psql "$OWNER_DSN" -c "CREATE ROLE autopilot_reader2 LOGIN IN ROLE gateway_api_reader"
psql "$OWNER_DSN"            # then, at the prompt:  \password autopilot_reader2
```

On a CNPG cluster prefer the managed-role path the cluster already uses (`spec.managed.roles` with `inRoles: [gateway_api_reader]` and a
`passwordSecret`, as `postgres.yaml` does for `autopilot_reader`), so no human types a password at all. Point `autopilot.gateway.go.readerSecret` at the
secret that holds the DSN under the key `uri`.

`api` refuses to start on a role that could write (`CheckRole`: superuser, `pg_write_all_data`, owner or writer of any table, view, materialized view,
foreign table or partitioned table, a column or sequence write grant, `CREATE` anywhere, or any membership that can become one whatever its
INHERIT and SET options). The real binary is tested against each shape, including a writer granted `WITH INHERIT FALSE, SET TRUE` and `INSERT` on an
auto-updatable view over a gateway table: all exit 2.

## Settings the Go gateway refuses, and what changed

Every refusal in `serve` and `api` was read against the Python gateway. Result:

| Setting | Go | Why |
|---|---|---|
| `EVENTS_RABBITMQ_ENABLED` | still refused | the source is designed, not built; Python refuses it too |
| `AUTOPILOT_EPHEMERAL`, `CLEARANCE_EPHEMERAL` | still refused | an in-memory store for local runs; Go has no memory store (Python's `api` ignores them; so does Go's) |
| the Clearance core's nine settings (`CLEARANCE_DSN`, `CLEARANCE_VERIFIERS`, `AGENT_LOGINS`, `CLEARANCE_GITHUB_*`) | **now refused** (`serve` and `api`) | Python refuses them (`CORE_ONLY`); Go ignored them silently |
| the thirteen retired `CLEARANCE_*` names | refused (as before) | renamed `AUTOPILOT_*` at R-10a, fallbacks removed at R-10e |
| `GATEWAY_API_TABLES` other than `gateway` | refused (as before) | retired at R-4 |
| the settings S3 to S6 carried (`EVENTS_ENABLED`, `GLIDEPATH_API_URL`, `SCHEDULES_ENABLED`, `AGENT_XRS_*`, `XR_*`, `METRICS_PORT`, ...) | no longer refused | carried and tested in their slices; `unported` lists only the three above |

The full environment table (Python against Go, every variable) is generated: [go-gateway-env.md](go-gateway-env.md) (124 variables: 69 the same,
26 new in Go, 25 refused by both, 2 kubelet-only, **2 not carried**: `CLEARANCE_CORE_TOKEN` and `CLEARANCE_TOKEN`, dev-only tokens of the Python
smoke tools). `TestEveryVariableThePythonDeploymentSetsIsReadByGo` fails the build when a variable the Python Deployment sets would be silently
ignored by Go.

## Fixes made for the release

- **The read API shows what `serve` decided.** `serve` holds every stored pin to the strict policy at start; a pin it rejects is now written back
  as rejected (it was only dropped in memory), and `readapi.Refresher` shows the stored decision instead of deciding with a weaker policy.
- **The catalog read is paged** (500 to a page, a cursor, at most 20 pages): a person with more, or a catalog that does not stop paging, is an
  error and is held to their own sessions (as an outage is), never given a partial list as if it were whole.
- **Options that differ only in surrounding whitespace are duplicates** (`CheckQuestion`, `dispatch/inbox.go`). Python checked for duplicates on the strings as sent and stored
  them trimmed, so `["prod", " prod "]` was accepted and stored as two `prod`; Go trims first, then dedupes (`HR-OPTIONS`). The length limits still apply to the string as sent.
  Test: `TestCheckQuestionDedupesTheTrimmedOptions`. (The question itself has no duplicate rule.)
- **A human answer is recorded once even when the inbox save fails** (`HumanAnswer`, `dispatch/inbox.go`). The order stays record, then save, because an answer must stand only
  once its audit record does; the trade-off is that a failed save leaves a record for a request that still shows pending. So the record is made idempotent on the request id: the
  gateway remembers it (in memory, 1000 at most) and a retry of the same answer (same person, route and text) reuses the record and its `audit_seq` instead of writing a second.
  A different answer after a failed save is recorded as its own; a gateway restart between the failed save and the retry still writes a second record (Python always did).
  Test: `TestAnAnswerWhoseSaveFailedIsRecordedOnceOnRetry`.
- **Dropped metric series are counted**: `autopilot_metric_series_dropped_total{metric}` (Go only, after Python's twelve).

## The cutover (S9) on hangar-dev, in order

Owner decisions that bind it: hard cutover on hangar-dev, the Go gateway replaces the Python one on the **same database**, the Python image is kept for
rollback; no hostname-based exceptions in code, only explicit settings, off by default. hangar-dev is a small lab VM: nothing below is run in a loop, and
the replay and conformance runs happen in CI against the release image, not on the cluster.

Before: a `gateway-go-v*` tag was pushed by the owner and the workflow is green; its digest is in the summary.

1. **Record the baseline** (Python live): `GET /v1/agents`, the last 20 sessions' records for each live agent, the trigger history, the metrics
   exposition, the Clearance core's last 50 records for the gateway. Keep them: step 9 compares against them.
2. **[owner] Prepare the cluster values** in the cluster's `cluster.yaml` (the apron `autopilot.gateway.*` keys): `implementation: python` still; the Go
   image tag and digest; `go.brokerUrl`, `go.githubOwners`, `go.airframeOwner`, `go.agentXRs.namespaces` and `.names` (`gate-reader=app-gate-api-cicd`),
   `go.allowPlainHttp` for what this lab serves over http (glidepath-api, the log collector, the catalog, the issuer's keys). Render (apron, the
   default is byte-identical). No change to the cluster yet.
3. **[owner] The reader secret**: nothing to do with the default (`autopilot-pg-reader`); for the narrower grant, run the SQL file and put the DSN in
   Infisical (the owner creates it, no one pastes it).
4. **Migrate**: first make sure the NetworkPolicy `autopilot-pg-migrate` is in the cluster (`kubectl -n autopilot-system get networkpolicy autopilot-pg-migrate`): it
   renders only with `autopilot.gateway.go.migrateJob: true` (set in step 2's values), and syncing that is the first change to the cluster (with `implementation: python`
   it adds only the policy and the tracked migrate Job, no traffic change); where the tracked Job has already run, the policy is already there. Then apply the one-off
   migrate Job (or `autopilot-gateway migrate` from a pod with `GATEWAY_DSN`); read its log; `migrate --check` exits 0.
   Python keeps serving throughout (the build is concurrent).
5. **Set the switch**: `autopilot.gateway.implementation: go`; render; read the diff (the Python Deployments are replaced by the Go ones,
   same Service names and ports); open the gitops PR; the owner merges.
6. **Sync**. The gateway Deployment is `Recreate`, so the Python pod stops (releasing the writer lock) before the Go pod starts; the read API rolls (two
   replicas, `minAvailable: 1`). Runs in flight: sessions are rows, the Go gateway loads the live ones at start; a run pod that calls while it is down
   retries.
7. **Verify** (live, not assumed): `serve` start line (version, agents, loader on, metrics port), `/healthz` and `/readyz` of both Deployments;
   `GET /v1/agents` against the baseline (same agents, same holds, the same rejected pins); `autopilot-gateway migrate --check` from the pod; a person
   signs in through Tower and sees their sessions; the metrics endpoint answers on the metrics port and Prometheus scrapes it; the log lines reach Loki;
   `kubectl get clusterrolebinding` shows the namespace-read binding; the definitions publish reached the core (`GET /v1/definitions` on the core).
8. **Open the canary run**: one scheduled agent and one event-triggered agent fire on the Go gateway; each finishes; their sessions, results and the core's
   records carry the same fields as the baseline's (the replay proves the shapes; this proves the wiring).
9. **Record the comparison** for the first runs after the switch against the step-1 baseline (decisions, outcome records, run results, trigger ledger
   states), and attach it to the S9 report. Differences are explained or the cutover is rolled back.
10. **Close**: keep `implementation: go`; the Python image tag stays in the values (the rollback). Removing the Python gateway and its workflow is a later
    change, not this one.

### Rollback

Set `autopilot.gateway.implementation: python`, render, merge, sync. The Python image is still pinned in the same values, and nothing about
the database has to be undone. What the Go gateway may have written that the Python one must tolerate, and why it does:

| Go-written state | Python tolerance |
|---|---|
| three extra columns on `gateway_trigger_launches` | they have defaults and Python names its columns (`INSERT ... (agent, key, via, event_id, state, claimed_at)`, `SELECT agent, key, ...`): never `SELECT *`, so extra columns are not seen |
| seven extra indexes | invisible to Python's statements; they cost a little on writes, and can be dropped (`DROP INDEX CONCURRENTLY`) if they are unwanted |
| `gateway_sessions`, `gateway_results`, `gateway_inbox`, holds, events | the Go gateway writes Python's record shapes (the replay and the read-API equivalence tests hold that); a session open at the switch is a row Python loads at start |
| `gateway_definitions` rows (loaded and rejected pins) | Python's shape `{name, state, source, doc \| reason}`, read by `Definitions.apply_records` |
| `gateway_outbox` rows the core has not taken | the same `op`/`kind`/`body` columns; Python's sweep posts them |
| `claimed` trigger rows | Python's claim expiry re-claims them |
| the writer advisory lock `apg0` | released when the Go pod stops (`Recreate`), before Python starts |

This is argued from the schemas and Python's SQL, not tested against a database that a Go gateway has written for a day; the first rollback rehearsal
belongs to the canary. **Unverified until then.** Pins the Go policy rejected stay rejected in the table; Python's loader re-decides them on its next poll.

## Open owner decisions

- **The coding agent's `airframe.apps: ["*"]`** (`agents/coding-agent.yaml`, [go-gateway.md](go-gateway.md) S5c): the Go gateway scopes `xr.request` by
  `profile.airframe.apps`, so `["*"]` equals Python's unscoped behaviour for that tool (the core never scoped it by app). It is the owner's call whether the
  coding agent keeps `*` at the cutover or is narrowed to named apps first; the cutover does not change it.
- **`RecordFromRuns` leaves `PathsTouched` empty** (the third of the three Python defects the owner chose to fix before the cutover; the other two are under "Fixes made for
  the release"). Not fixed: the data is not available where the record is built. A run's `tool_calls` carry only `{tool, ok, rule}` (`ToolCallSummary`), the evidence the score
  route builds has no paths, and the paths of a write (`effect()` in `dispatch.go`) go to Clearance on the outcome and are not kept by the gateway. Python's `record_from_run`
  takes `paths_touched` as a parameter that its live caller never passes. Filling it needs a decision: keep the effect paths on the session (new persisted state), or read
  them back from the core's session records. Until then a case with `minPathsTouched` above 0 cannot pass through the score route, in Go as in Python.
- **Turn `autopilot.gateway.go.migrateJob` off** once the cutover has held (it is a tracked wave-1 Job, not a hook, so it needs no turning off between syncs; the flag only
  stops it rendering).
- **Narrow the gateway's cluster-wide read RBAC**: the Go gateway needs `get` on `namespaces` (added by the Go render) and today still rides the Python
  ClusterRole that lists every `catalog.hangar.io` kind; a Role per `AGENT_XRS_NAMESPACES` entry is the least-privilege shape (go-gateway.md, S4).
- **SBOM and signature** for both images, together.
