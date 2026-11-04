<div align="center">
  <img src="docs/brand/autopilot-tile.svg" width="88" height="88" alt="Autopilot mark" />
  <h1>Autopilot</h1>
  <p><i>Any AI agent workload, bounded and audited.</i></p>
</div>

Autopilot is the sixth [Hangar](https://github.com/jfillman/hangar) product: it runs AI agent workloads with authority
that is bounded and every action on the record. This repo holds the **AI gateway** (the MCP surface agents call, sessions, the run
tree, budgets, tool dispatch, the model proxy: `src/autopilot/engine/`), the code that plugs Hangar into it and into
**Clearance** (`src/autopilot/`), plus agent definitions, the Preflight evaluation cases and the AppSpec planner.
Docs: [docs/](docs/index.md). Mark and brand files: [docs/brand/](docs/brand/README.md).

## The gateway here, Clearance in its own repo

Clearance is its own product and repo (https://github.com/jfillman/clearance): it decides and records; the gateway
enforces (Hangar's boundary of 2026-11-02, amending D16). Its core installs where there is no Airframe and no
Glidepath. Hangar's policy over the `glidepath.*` and `airframe.*` tools (R021 to R023 and their argument checks) is a
**policy bundle** that ships here, `policy/hangar/bundle.json`, beside the names Hangar passes the core,
`policy/hangar/core-env.json`; Apron deploys both to the core, which loads them from configuration (Clearance v0.29.0,
R-5) and reads R021's cicd.yaml scope from glidepath-api's `GET /v1/contract` at start. A Glidepath contract change
needs no Clearance release.

**R-5 (Built, live on hangar-dev since R-4):** the bundle replaces Clearance's compiled Hangar pack. `tests/test_hangar_bundle.py`
proves it governs exactly the gateway's Hangar tools (`hangar_tools.py`) and replays Clearance's golden corpus
identically; the suite's test core reads its scope from a glidepath-api stand-in (`tests/clearance_core/contractsrv.py`).

**R-3a (Built):** the AI gateway's Python moved here from clearance at v0.26.1, with its history (`git filter-repo`),
behaviour unchanged: `src/autopilot/engine/` is clearance's `server.py`, `dispatch.py`, `session.py`, `limits.py`,
`tiers.py`, `modelproxy.py`, `backends.py`, `profile.py` (and `schemas/agent-definition.schema.json`), `runtimes.py`,
`inbox.py`, `control.py`, `results.py` and `events.py`, and `tests/engine/` their tests.

**R-3b (Built, live on hangar-dev since R-4, 2026-11-03):** the gateway stopped deciding and recording in process. Every tool call,
model call, session open and spawn is an evaluation the Clearance core decides and records
(`POST /access/v1/evaluation`, through `clearance-core-client`, pinned at clearance v0.29.0 until A-4, which pins the commit of clearance's
`w2-a4-cost-counts` branch until the release that carries it); the gateway applies the
spend the core names to its own session, acts only on an allow, and reports the result
(`POST /v1/decisions/{id}/outcome`). Its runtime records (`run.launch`, `run.delete`, `human.answer`,
`session.freeze`, `trigger.hold`; from R-3f also `run.finish`, `session.end`, `trigger.release`) go to
`POST /v1/records`. `engine/core.py` is the one module that talks to the
core. The gateway keeps its runtime state in its own database (`GATEWAY_DSN`) and has no evidence DSN; when the core
is unreachable it follows [the outage policy](#the-outage-policy). The deployment gate, PR provenance, definition
review and the GitHub App are the core's routes now, so the gateway serves none of them. Deployed at R-4: hangar-dev
runs gateway-v0.4.0 against clearance v0.29.0's core.

**R-3f (Built, live on hangar-dev since R-4):** the gateway adopts the Clearance core of v0.28.0 (the client pinned at
clearance v0.28.0). The core takes every record kind the gateway writes, so nothing
waits in the outbox for a core kind any more: `run.finish`, `session.end`, `trigger.release` and the outage policy's
`tool.call` (which now names the tool that ran) go to `POST /v1/records`. A person's run (`Gateway.run_agent`) asks
for `session.open` with `run: true` and `via`, so the core's record is clearance v0.26.1's (`opened to run via
<delegate>`). A handler's refusal after an allow (R001, HR-*, ART-*, GP-*) goes on the outcome as `refusal {rule,
message}`. The cluster-pinned definition set is published whole to the core (`PUT /v1/definitions`), which decides
each pin again and writes `definition.load`, `.unload` and `.reject` itself; the gateway writes none of them. The
core registers the gateway with `"definitions": true` in `CLEARANCE_PROVIDERS`.

**R-3d (Built):** nothing imports the `clearance` package, and it is no dependency: not of the image, not of the
tests (it left clearance in v0.30.0). The only Clearance dependency is the generated client. What the tests compare
with clearance v0.26.1 (the tables it created, its read API's operations) is vendored in `tests/fixtures/clearance-v0.26.1-*`.
`tests/engine/test_boundary.py` fails on any `clearance` import under `src`, `tools`, `tests` or `runtime`, and on a
`clearance` requirement in `pyproject.toml`.

**R-3c (Built, live on hangar-dev since R-4):** the gateway serves its own read API, the `autopilot-api` Deployment
(two replicas, on the gateway's own database `autopilot-pg`, `gateway_*` only); Tower reads it through
`AUTOPILOT_API_URL`. `autopilot-gateway api` is
`src/autopilot/read_api.py`, no longer clearance's `api.py`: the nine runtime reads the Go `clearance api` (clearance
v0.27.0) left for the gateway (agents with their trigger hold, sessions, a run's result, the run tree, the human inbox,
trigger history, the runtime catalog), ported with the same paths, parameters, answers, status codes and roles, and
its contract `contract/autopilot-api.json`. Clearance's reads (evidence, receipts, provenance, clearances, policy) are
clearance-api's; `/v1/contract` names it (`CLEARANCE_API_URL`). See [Running the read API](#running-the-read-api).

| Part | Where | What it owns |
|---|---|---|
| The Clearance core (a service) | `clearance core` (ghcr.io/jfillman/clearance), called through `clearance-core-client @ git+https://github.com/jfillman/clearance@<ref>#subdirectory=clients/python` (`pyproject.toml`: the A-4 branch commit until the release that carries its outcome and record fields) | the decision (CEL rules, the core's and Hangar's policy bundle, scope, tiers, the narrow-only rule, spawn and open checks, the model decision), the evidence chain and its store, the cluster-pinned definitions the gateway publishes and their `definition.*` records, the deployment gate, PR provenance, definition review and the GitHub App |
| The gateway engine | `src/autopilot/engine/` (from clearance v0.26.1; R-3b adds `core.py`, `outbox.py`, `pg.py`, and the copied `auth.py`, `definitions.py`, `scope.py`) | the MCP surface over stdio and streamable HTTP (`server.py`), sessions, the run tree and `Gateway.expire` (`session.py`, `dispatch.py`), budgets and requested limits (`limits.py`), the tool registry and tier vocabulary (`tiers.py`), dispatch to backends (`dispatch.py`, the protocols and fakes in `backends.py`), the model proxy (`modelproxy.py`, streaming in `server.py`), agent definitions and their schema (`profile.py`), the runtime catalog (`runtimes.py`), the human inbox, person controls, run results and trigger history (`inbox.py`, `control.py`, `results.py`, `events.py`), callers' authentication (`auth.py`), the cluster pin set (`definitions.py`), the gateway's own database (`pg.py`) and outbox (`outbox.py`); `core.py` is the core's client |
| Autopilot | `src/autopilot/` | Hangar's tool implementations and everything around agents on Hangar: `glidepath_api.py`, `airframe_api.py`, `airframe_tools.py`, the planner `airframe_plan.py`, the AgentRun manifest `manifests.py`, `triggers.py`, `preflight.py`, the Hangar backends and fakes `backends.py`, the AgentRun store over the Kubernetes API `kube_runs.py`, the T0 read adapters `reads.py`, the deployable server `serve.py`, the vendored airframe files `vendor/`, `hangar_tools.py` (the `glidepath.*` and `airframe.*` tool specs: the gateway's registry, which `policy/hangar/bundle.json` governs), and `gateway.py`, which builds the Hangar Gateway: the engine's Gateway + Hangar's tools + a handler per tool + AgentRun as the run backend + the core |

```python
Gateway(definitions, store, core, auth, clock, packs=(HANGAR,), handlers=hangar_handlers(backends),
        runs=AgentRunBackend(backends.runs))       # what autopilot.gateway.hangar_gateway() does
```

A change to Clearance's policy (a rule, a re-vendored Glidepath contract) is a clearance release, deployed as the
core; the client pin here moves with it (`pyproject.toml` and `CLEARANCE_REF` in `.github/workflows/test.yaml`, which
builds the core the tests run against from the same ref, and the core container from its released image, or from the
same checkout while the pin is a commit). A new tool is a spec here (`engine/tiers.py`,
`hangar_tools.py`) and in the core's bundle. `tests/test_cicd_scope.py` checks the vendored Glidepath files against
glidepath.

### The outage policy

Decided 2026-11-02 (Hangar spec R-3). When the core is unreachable (a connection failure, a timeout, a 5xx: the
client's `CoreUnavailable`), the gateway:

| Call | Outcome |
|---|---|
| a T1, T2 tool call; `run.spawn`; a model call; a session open (MCP, a trigger, a person's run) | denied, rule `CORE-UNAVAILABLE`, with a hint; nothing runs, nothing is charged |
| a T0 read | denied the same way, unless `GATEWAY_OUTAGE_ALLOW_READS=true` |
| a T0 read with the switch on | runs if the gateway can stand behind it alone: a live, untripped session of the caller's that was granted the tool, budget left, and arguments that name no `repo`, `app` or `service` (scope only the core checks; those stay denied). It is charged, and written to the outbox as a `tool.call` record with `coverage: coverage-unavailable` |
| a person control (freeze, end, hold, release), a run's `run_finish`, a run deletion | takes effect; its record waits in the outbox |
| a human answer, a run launch | refused (the answer is not given, the run is deleted again): each stands only with its record |

The outbox (`gateway_outbox`, `engine/outbox.py`) is posted in order once the core answers: by the expiry sweep, and
before any new record, so nothing overtakes what waits. The client's timeout is 250 ms per attempt with one retry,
and its breaker opens after 5 consecutive failures, so later calls are denied at once without a request (it lets one
probe through every 5 s). A core that answers but refuses a request (a 4xx, an unreadable answer) is a deny too,
rule `CORE-REFUSED`. `GET /healthz` shows the breaker (`"core": "closed" | "open" | "half-open"`).

Since clearance v0.28.0 (R-3f) the core takes every kind the gateway writes, so no entry waits for a core kind. The
`held` reason stays for a rolling upgrade (a client that does not list a kind holds it rather than send it), and a
`run.finish`, `session.end` or `trigger.release` row an older gateway held is posted by the next drain. An outage
read is posted as the core's `tool.call` record, naming the tool, `tier` 0, `coverage: coverage-unavailable`, and no
rule. The definition set is never in the outbox: the gateway publishes it whole (`PUT /v1/definitions`) after every
poll that changed it and on its first poll; a publish the core does not take (unreachable, or refused) is made again
on the next poll, whole. That call has its own client, a 5 s timeout and its own breaker, so a set of up to 256 pins
never trips the decisions' 250 ms breaker. Held `definition.*` rows an older gateway wrote are deleted when the
gateway opens its database (they can never be posted: the core is their one writer).

Design, plan and diagrams: `../hangar/docs/autopilot/` (start at `README.md`).

## What is built and tested

`./.venv/bin/python -m pytest -q` runs 763 tests (A-4, 2026-11-03: 755 pass and 8 skip without `AUTOPILOT_TEST_PG` and the core container), no cluster needed, against a real Clearance core:
`tests/conftest.py` builds the unmodified clearance binary from a checkout (`go build ./cmd/clearance`;
`CLEARANCE_SRC`, or `$HANGAR_TECH/clearance`; CI checks out the ref the client is pinned at) and runs it as
`clearance core --memory --insecure-dev-auth --max-clock-skew 0 --listen 127.0.0.1:0` (the tests run on a fixed
clock), reading its address from the core's "clearance core serving" log line and the chain it wrote from
`GET /v1/dev/records` (`tests/clearance_core/coreenv.py`). `CLEARANCE_CORE_BIN` names a built one. Three tests need more and skip without it:
`tests/test_gateway_pg.py` (`AUTOPILOT_TEST_PG`, a Postgres server for scratch databases) and
`tests/test_core_container.py` (`AUTOPILOT_CORE_CONTAINER_URL`, the released core image, unmodified); CI runs both.
One more skips without a gitops-cluster-dev checkout.

`clearance/...` paths below are in the clearance repo, under `src/clearance/`; `engine/...` paths are here, under `src/autopilot/engine/`.

| Piece | Where | What it does |
|---|---|---|
| Tool tiers and tripwires | `engine/tiers.py`, `autopilot/hangar_tools.py` | the registry the gateway dispatches against; T0 to T2 tools; T3 names are tripwires. The core's bundle names the same tools and tiers |
| Path and repo scope | the core (`internal/scope`); `engine/scope.py` for Preflight | traversal-safe |
| cicd.yaml field scope | `policy/hangar/bundle.json` (R021: the `cicd_scope_problems` document, its pointer rules read from glidepath-api's `GET /v1/contract` at the core's start; Glidepath's structural GP-SCOPE-002/003 rules as the bundle's invariants), evaluated by the Clearance core | an agent may change cicd.yaml in a PR within Glidepath's `open`/`tighten`/`human` field rules; a denial carries the `GP-SCOPE-00x` rule id and hint |
| Glidepath reads and run control | `policy/hangar/bundle.json` (`glidepath.*`, R022), `autopilot/glidepath_api.py`, `autopilot/gateway.py` (handlers), `agents/cicd-explainer.yaml` | six T0 tools over glidepath-api: runs, one run, step logs, `explain` (a failed run's knownFailures id, hint and field), releases, the contract; and two T1 writes, `glidepath.run.rerun` and `glidepath.run.cancel` (spec phase 3), which retire `pipeline.rerun`: a re-run sends an Idempotency-Key made from the session and the run, so a retried call starts no second run, and Clearance names the session principal so glidepath-api audits both identities. `profile.glidepath.apps` (globs; none listed means none) scopes them per agent, arguments must be valid path segments, and the adapter calls as Clearance's own identity, re-reading its token file every call. A 400/404 from glidepath-api (no such run, no archived log) comes back as data with its `GP-*` rule; a 401/403 is a denial with that rule |
| Glidepath live runs, release runs, evidence, tests, events and previews | `policy/hangar/bundle.json` (Clearance v0.24.0), `autopilot/glidepath_live.py`, `autopilot/glidepath_api.py`, `autopilot/gateway.py` | nine T0 reads over glidepath-api. `glidepath.live.list` is the runs in the cluster now (`runs.list` reads Results, finished runs only), summarised to each run's tasks and step containers; `glidepath.live.logs.get` is a running step's log (its last 256 KiB). `glidepath.live.watch` is the live stream as one bounded call: it follows one PipelineRun until it finishes or `timeoutSeconds` (1-120, default 60) passes and returns the run then and every task change it saw; a stream that cannot open (503 at a replica's limit) or drops is replaced by polling `live` every 3 s, so the answer is the same. `glidepath.releaseRun.get` follows a promote or rollback; `glidepath.evidence.get`, `glidepath.tests.list`, `glidepath.tests.report`, `glidepath.events.recent` (the app's CDEvents from glidepath-api's buffer) and `glidepath.previews.list` (preview environments, read only) round out the reads. Fleet-wide reads have no tool: agents are app-scoped |
| Glidepath release proposals | `policy/hangar/bundle.json` (`glidepath.environments.*`, `glidepath.promote`, `glidepath.rollback`), `autopilot/glidepath_api.py`, `autopilot/gateway.py` (`release_key`), `agents/release-proposer.yaml`, `tools/glidepath_release_smoke.py` | spec phase 4. Two T0 reads (the release plan; a Flight environment's rollback plan) and two T2 propose-only writes: `glidepath.promote` and `glidepath.rollback` start the app's release run, whose `open-release-pr` step opens the gitops PR with the platform's GitHub App, and a person merges it. Not capped by the tier ceiling (R007 admits propose-only at T1) but only for a definition that lists them. The Idempotency-Key is the session, app, target environment and cluster, and the source (or rollback target), never `dryRun` or the reason, so a retry replays and a dry run names the run the real call starts; `dryRun` goes as `X-Glidepath-Dry-Run`. A 422 (out of order, a Ground target, an ineligible target, admission) is a denial with glidepath-api's hint. `release-proposer` (gate-api only) holds these, the Glidepath reads, and nothing that reaches GitHub: no `repo.pr.open*`, `xr.request` or `run.spawn`, and zero GitHub, PR and child budgets; no tool in the registry merges or approves |
| Glidepath cicd.yaml changes | `policy/hangar/bundle.json` (`glidepath.cicd.validate`, `glidepath.cicd.propose`, a PR tool: `provenance.PR_TOOLS`), `autopilot/cicd_propose.py`, `autopilot/glidepath_api.py` (`validate_cicd`), `agents/coding-agent.yaml` | validate asks glidepath-api what a merge patch would mean (the cicd-validate check's verdict, every field it touches, the resulting file); propose asks the same and is refused with the GP rule when invalid, else opens one commit of cicd.yaml on the app's own repo as the agents' App, the verdict in the PR body, a human merges; its receipt names repo, PR, commit and path, so the agent gates vouch for it like `repo.pr.open`. A `repo.pr.open` that carries the app's root `cicd.yaml` gets the same verdict first (`cicd_propose.check_files`) |
| Airframe reads | `policy/hangar/bundle.json` (`airframe.*`, R023), `autopilot/airframe_tools.py`, `autopilot/airframe_api.py`, `schemas/airframe-contract.json` + `autopilot/vendor/airframe_sidecars.py` (vendored from airframe, drift-tested), `agents/airframe-operator.yaml`, `tools/airframe_smoke.py` | seven T0 tools. `capabilities` answers from Airframe's contract bundle; `validate` runs airframe's own `airframe-validate` (a configured command or container, parsed from `--format json`) and returns AF-* rule ids, JSON pointers and hints; `explain` turns an AF-* rule, a condition reason or a knownFailures id into a description and fix; `plan` is the AppSpec planner; `status` normalises an app's or environment's XR conditions (reason, meaning, hint, observedGeneration against generation); `describe` joins an environment's XRs, the Argo CD Application syncing it (sync, health, revision per source) and its Rollout (phase, ready replicas, image); `verify` runs a kind's sidecar verify checks from the XR alone and returns anything needing a Secret, ConfigMap, port-forward or cloud CLI as `skipped` with the reason. The Kubernetes adapter is GET-only against a hard path allowlist (`catalog.hangar.io` XRs, `argoproj.io` applications and rollouts; no core group, no subresource, no watch), refused before any request is built, with the token and CA re-read every call. `profile.airframe.apps` (globs; none listed means none) scopes every call that names an app |
| Narrow-only limits | `engine/limits.py` | requested limits or a child can only narrow what they inherit |
| Agent definitions | `engine/profile.py` and its `engine/schemas/agent-definition.schema.json` (cluster-pinned definitions are still parsed by clearance's copy, `clearance/definitions.py`) | durable, schema-validated; baseline deny paths cannot be removed |
| Sessions and run tree | `engine/session.py` | budgets, expiry, breaker, parent-reserved child budgets |
| Policy and the record | the Clearance core | CEL deny rules with ids, an explicit order and fix hints, core plus Hangar's policy bundle; the evidence chain; fails closed |
| Gateway core | `engine/dispatch.py`, `engine/backends.py`, `engine/core.py` | authenticate, ask the core, apply its spend, call, report the outcome; handlers are registered per tool, the run tree goes through an injected run backend |
| Outbox and the gateway's database | `engine/outbox.py`, `engine/pg.py`, `tools/cutover/copy_runtime_tables.py` | what the core is owed, posted in order; sessions, inbox, results, artifacts, events, holds, pins; the R-4 copy from clearance-pg ([docs/admin/gateway-database.md](docs/admin/gateway-database.md)) |
| Model access | `engine/modelproxy.py` | the core decides each call (allowlist, network mode, breaker, budget); the gateway charges the usage |
| Flight recorder and cost (A-4: AP-C1, AP-C2) | `engine/server.py` (`create_model_app`), `engine/dispatch.py` (`model_usage`, `_record`, `pinned_model`, `preflight_of`, `run_cost`), `engine/modelproxy.py` (`usage_record`), `engine/session.py` (`Session.usage`) | every allowed model call's outcome goes to the core however it ended (`ok`, `error:http-<status>`, `error:ProviderUnreachable`), with `model_version` (the model that answered: the response's `model`, a stream's `message_start`, else the route's upstream model) and `usage` (the four token kinds and `charged_tokens`, what the session's `model_tokens` budget was charged by `USAGE_WEIGHTS`). `run.launch`, `run.finish` and `run.delete` carry `model_version` (the definition's first model through its route) and `preflight` (`Gateway.preflight`, a callable A-6 fills; `not-run` until then); `run.finish` carries `cost` (the session's usage, model and tool calls, wall seconds, compute class). The session keeps its running cost as `usage` (the read API's sessions show it; `GET /v1/tasks/{id}` sums it as `cost`). clearance-api's `GET /v1/evidence/counts?by=cost&per=task|session|agent|principal` sums the records |
| Triggers | `autopilot/triggers.py` | interval and alert triggers for scheduled and event agents |
| Preflight | `autopilot/preflight.py`, `preflight/cases/` | deterministic scoring, self-check that a case can fail |
| MCP surface | `engine/server.py` | three tools on the mcp SDK v2; `tools_list` lists the Gateway's own registry |
| AppSpec and the planner | `autopilot/airframe_plan.py`, `schemas/appspec.schema.json` (vendored from airframe's contract) | the parachute test: one statement of intent to an ordered change set; its cicd.yaml patch is split by Glidepath's scope rules: the agent's half (Ground environments and their deploy steps, added without dropping a step) passes R021, and Flight environments, the release step and retiring pre-ADR-0019 keys are a separate `cicd-flight` step a human merges; `config`/`overrides` take any human-owned chart value (second sentence: `examples/mynewpythonapp.appspec.yaml`, five environments, canary with pauses, HPA, PDB, a prod-only route); validated against the real XRD schemas, Glidepath's cicd schema and a real `helm template` of every environment |
| Skyport agents | `agents/skyport/`, `preflight/cases/skyport-*.yaml` | one definition per workload shape, plus team members; event trigger, app-API allowlist, chat, artifacts, idempotent triggered runs |
| AgentRun composition | lives in airframe (`xrds/agentrun.yaml`, `compositions/agentrun/`, `functions/function-agentrun/`); `src/autopilot/vendor/agentrun_compose.py` and `schemas/agentrun.xrd.yaml` are byte-identical copies | the canary renders with the vendored `compose()`; Clearance's AgentRun manifests are validated against the vendored XRD; `tests/test_agentrun_vendor.py` fails on drift |
| AgentRun store | `autopilot/kube_runs.py` | the real `AgentRunStore`: POST to `/apis/catalog.hangar.io/v1alpha1/namespaces/autopilot-runs/agentruns` and DELETE of one named AgentRun there, nothing else (any other method, path, namespace, kind or a non-DNS-label name is refused before a request is built); a 404 on delete is success, a 409 on create an error; token and CA re-read every call |
| Read adapters | `autopilot/reads.py` | `argo.app.get` over the Application objects (the GET-only Kubernetes reader), `metrics.query` on a Prometheus-compatible API, `logs.query` on Loki; summaries and bounds, a rejected query as an answer |
| The deployable gateway | `autopilot/serve.py` (`autopilot-gateway`), `Dockerfile` | builds the Hangar Gateway from environment variables and serves the engine's HTTP app (`engine/server.py`) with uvicorn; real backends only; the expiry sweep on a thread under the same lock as every gateway call. See [Running the gateway](#running-the-gateway) |
| The agent runtime | `runtime/` (`hangar_agent`, `runtime/Dockerfile`), `tests/test_runtime_*.py` | the program an AgentRun pod runs: a tool-use loop that offers Clearance's tools to the model, sends every call through Clearance in the run's existing session, writes `result.json` and hands it to Clearance (`run_finish`). See [The agent runtime](#the-agent-runtime) |
| Network-policy canary (AP-A1) | `tools/netpol_canary.py` | renders a run's real policies with `compose()` on a live cluster and proves each denial against a no-policy control; its pass is what a person attests when setting the registry's `airframe.autopilotReady` |

## Running the gateway

`autopilot-gateway` (`src/autopilot/serve.py`) is the deployable gateway for Hangar: the gateway engine, the Clearance
core's client, Hangar's tools and Autopilot's real backends, served as the engine's
streamable-HTTP MCP app (`/mcp`, bearer auth on every request,
an unauthenticated `GET /healthz`). The image is the repo's `Dockerfile` (python:3.13-slim, uid 65532, nothing
written at runtime, so it runs with a read-only root filesystem; linux/amd64 and linux/arm64):

```shell
container build --arch arm64 -t autopilot-gateway:dev .         # or --arch amd64
container run --rm --read-only -e CLEARANCE_EPHEMERAL=1 -e CLEARANCE_CORE_URL=http://<core>:8080 -p 8080:8080 autopilot-gateway:dev
curl -s localhost:8080/healthz                                   # {"ok":true,"tools":35}
```

| Variable | Default | What it does |
|---|---|---|
| `GATEWAY_DSN` | required unless ephemeral | the gateway's own Postgres (`engine/pg.py`): sessions, the inbox, results, artifacts, trigger history, holds, the pinned definition set and the outbox; never evidence. One writer: a second replica on the same database stops at start (advisory lock `apg0`), so run one |
| `CLEARANCE_CORE_URL` | (required) | the Clearance core, e.g. `http://clearance-core.clearance-system:8080` |
| `CLEARANCE_CORE_TOKEN_FILE` | `/var/run/secrets/tokens/clearance-core` | the gateway's token for the core, re-read on every request: a projected service-account token whose audience is the core's `CLEARANCE_AUDIENCE` (default `clearance`). The core must register this identity as provider `autopilot-gateway` with `"definitions": true` (`CLEARANCE_PROVIDERS`), or it refuses the gateway's definition set |
| `GATEWAY_OUTAGE_ALLOW_READS` | `false` | `true`: during a core outage, unscoped T0 reads still run and are recorded later as coverage-unavailable ([the outage policy](#the-outage-policy)) |
| `CLEARANCE_EPHEMERAL` | `0` | `1`: in-memory sessions and outbox, lost on exit. Local runs only; `GATEWAY_DSN` is ignored |
| `CLEARANCE_AGENTS_DIR` | the repo's `agents/` (`/app/agents` in the image) | the agent definitions loaded at start (that directory only, not its subdirectories) |
| `CLEARANCE_RESOURCE_URL` | `http://localhost:$PORT/mcp` | this server's public URL, for the OAuth protected-resource metadata |
| `CLEARANCE_OIDC_ISSUER`, `CLEARANCE_OIDC_AUDIENCE` | unset | set both to accept people's OIDC ID tokens from that issuer for that audience (Dex in the lab); unset, only workload tokens are accepted. The issuer is also the authorization server the metadata names (else the resource URL) |
| `CLEARANCE_OIDC_JWKS_URL` | `$CLEARANCE_OIDC_ISSUER/keys` | the issuer's JWKS (Dex's path) |
| `CLEARANCE_OIDC_USER_CLAIM` | `email` | the claim that names the person (`user:<claim>`) |
| `CLEARANCE_OIDC_ALLOWED_USERS` | (none) | comma-separated globs on that claim: who may sign in at all. Required with `CLEARANCE_OIDC_ISSUER`, because an issuer that federates GitHub signs in anyone with a GitHub account |
| `CLEARANCE_AUDIENCE` | `clearance` | the audience a workload's service-account token must carry (TokenReview) |
| `PORT` | `8080` | listen port, all interfaces |
| `CLEARANCE_EXPIRE_SECONDS` | `60` | how often `Gateway.expire()` closes sessions past their deadline and deletes their AgentRuns (and their children's), on a background thread under the gateway lock; a failed sweep is logged and retried |
| `GLIDEPATH_API_URL` | unset | glidepath-api's base URL (e.g. `http://glidepath-api.platform-system:8080`); unset, the `glidepath.*` tools have no backend |
| `GLIDEPATH_TOKEN_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | Clearance's token toward glidepath-api, re-read every call |
| `GLIDEPATH_ON_BEHALF_OF` | `0` | `1`: send the session principal in `X-Glidepath-On-Behalf-Of` (only when Clearance is one of glidepath-api's `api.delegates`; a non-delegate sending it is refused) |
| `AIRFRAME_ENABLED` | `1` | `0`: the `airframe.*` tools have no backend |
| `AIRFRAME_CLUSTER` | `hangar-dev` | the cluster the Airframe reader reads, and the planner's dev cluster |
| `AIRFRAME_UPPER_CLUSTERS` | `hangar-prod` | comma-separated: the clusters the planner may put a Flight environment on |
| `AIRFRAME_TENANTS_REPO` | `gitops-hangar-dev-tenants` | the dev cluster's tenants repo, where the planner commits XR requests |
| `ARGO_ENABLED` | `1` | `0`: `argo.app.get` has no backend |
| `ARGO_APP_NAMESPACES` | `argocd` | comma-separated: the namespaces `argo.app.get` and `argo.sync.lower` look for an Application in, in order |
| `BACKSTAGE_URL` | unset | Tower's Backstage, e.g. `http://backstage.hangar-prod.local`: `catalog.read` searches its catalog |
| `BACKSTAGE_TOKEN_FILE` | `/var/run/clearance/backstage-token` | a projected token for audience `backstage`, re-read per call |
| `TOWER_ISSUER` | unset | another cluster's public service-account issuer (Tower's, e.g. `https://hangarplatform.dev/oidc/hangar-prod`): its tokens for audience `CLEARANCE_AUDIENCE` are verified against `TOWER_JWKS_URL` (default `<issuer>/jwks.json`) as `<TOWER_CLUSTER>/<ns>/<sa>` |
| `TOWER_CLUSTER`, `TOWER_SERVICE_ACCOUNTS` | unset | required with `TOWER_ISSUER`: the name prefix, and the `namespace/name` globs that may call (the issuer signs a token for every pod there) |
| `CLEARANCE_DELEGATES` | unset | comma-separated principal globs that may answer human requests for a person they name (`X-Hangar-On-Behalf-Of`), e.g. Tower's backend |
| `ARGO_SYNC_ENABLED` | `0` | `1`: `argo.sync.lower` starts a sync of a lower Application (needs the grant below) |
| `PROMETHEUS_URL` | unset | a Prometheus-compatible query API (e.g. `http://thanos-query.observability.svc:9090`); unset, `metrics.query` has no backend |
| `LOKI_URL` | unset | Loki's query API (e.g. `http://loki-gateway.observability.svc`); unset, `logs.query` has no backend |
| `GITHUB_PR_ENABLED` | `0` | `1`: `repo.pr.open` opens PRs as the agents' GitHub App through Glidepath's broker (`github_pr.py`) |
| `AGENT_TOKEN_FILE` | `/var/run/clearance/agent-installation-token` | a projected token for the broker's `agent-installation-token` audience, re-read per PR |
| `BROKER_URL` | `http://cdevents-broker-auth.platform-system.svc:8080` | Glidepath's token broker |
| `MODEL_KEY_FILE` | unset | the model provider's API key (a mounted Secret file, re-read per call); set, the model proxy listens on `MODEL_PORT` |
| `MODEL_PORT` | `8081` | the model proxy's port (function-agentrun's `MODEL_PROXY_URL` port) |
| `MODEL_ROUTES` | unset | comma-separated `alias=upstream-model`, e.g. `claude-sonnet-5-5=claude-sonnet-5-5`: the models a definition may name (`profile.models`), all on Anthropic's API |
| `MODEL_TOKEN_AUDIENCE` | `model-proxy` | the audience of a run's model-proxy token (TokenReview) |
| `EVENTS_ENABLED` | `0` | `1`: the event trigger bridge holds glidepath-api's `/v1/events` stream and launches each agent whose Glidepath event trigger matches (needs `GLIDEPATH_API_URL`) |
| `SCHEDULES_ENABLED` | `0` | `1`: interval triggers (`src/autopilot/scheduler.py`): each scheduled workload agent starts once per interval, at its UTC-aligned start (every 24h: 00:00 UTC), within 10 minutes of it, with its instructions |
| `AGENT_XRS_ENABLED` | = `GITHUB_PR_ENABLED` | `1`: Agent XR pins become definitions (`agent_xrs.py`, Autopilot M4): every `AGENT_XRS_INTERVAL` s (60) the gateway lists `Agent` XRs (airframe `xrds/agent.yaml`), fetches each pinned definition file once through the agents' App (the broker's repository rule applies) and loads it beside the bundled `agents/` (clearance `definitions.py`: a bundled name is never shadowed; every change is `definition.*` evidence once). Needs `GITHUB_PR_ENABLED=1` |
| `LOG_LEVEL` | `info` | |

The core's settings are refused on the gateway, so a cutover that leaves one behind fails at start rather than
running half-configured: `CLEARANCE_DSN`, `CLEARANCE_VERIFIERS`, `AGENT_LOGINS`, `CLEARANCE_GITHUB_APP_*` and
`CLEARANCE_GITHUB_OIDC_*` (the evidence DSN, provenance, the gate, the GitHub App and GitHub Actions callers are the
core's; set them there).

The start line names what is enabled (store kind, agent and tool counts, auth, Glidepath URL, Airframe cluster, expiry
interval, the core's URL and the outage switch) and never a DSN, token or file content.

What each backend needs. The gateway runs as one ServiceAccount (`autopilot-system/clearance` on hangar-dev) with its
projected token and CA at the standard path; every adapter re-reads both on each call.

| Backend | Tools | Needs |
|---|---|---|
| Authentication (always) | every MCP request | `create` on `tokenreviews.authentication.k8s.io`: a ClusterRoleBinding to `system:auth-delegator`. Callers bring a projected token with audience `CLEARANCE_AUDIENCE`. For people, network reach to the OIDC issuer's JWKS |
| AgentRuns (always) | `run.spawn`, `run.close`, the expiry sweep | Role `clearance-agentruns` in `autopilot-runs`: `create` and `delete` on `agentruns.catalog.hangar.io` (the adapter sends nothing else) |
| Airframe reads (`AIRFRAME_ENABLED`) | `airframe.capabilities`, `explain`, `plan`, `status`, `describe`, `verify` | ClusterRole `clearance-airframe-reader`: get/list/watch on `catalog.hangar.io` and `argoproj.io` applications and rollouts, nothing else (gitops-hangar-dev#40). `airframe.validate` has no backend in the image |
| Glidepath (`GLIDEPATH_API_URL`) | `glidepath.*` | network reach to glidepath-api and RBAC there for Clearance's identity: glidepath-api checks it with a SubjectAccessReview (ClusterRoles `glidepath-api-reader`, `-operator`, `-releaser` for reads, run control and release proposals) |
| Argo CD reads (`ARGO_ENABLED`) | `argo.app.get` | the same `clearance-airframe-reader` ClusterRole: it reads the Application object through the Kubernetes API (no Argo CD account, no token) and returns a summary: sources, destination, sync, health, the last operation, conditions and the resources not synced or not healthy. Never the Helm values |
| Argo CD syncs (`ARGO_SYNC_ENABLED`) | `argo.sync.lower` | Role `autopilot-clearance-argo-sync` in `argocd-apps` (get, patch applications) and the ValidatingAdmissionPolicy of the same name: for this identity an Application update must be in a `-lower` project, change only `operation`, start a sync and never override its source. The adapter checks first that the Application's own project is the `project` argument and ends in `-lower` and that nothing is in flight, then sends one merge PATCH with the resourceVersion it read (`argo_sync.py`). The operation's `initiatedBy` is `clearance:<agent>`, its `info` the task, session and principal |
| Backstage catalog (`BACKSTAGE_URL`) | `catalog.read` | a projected service-account token for audience `backstage` at `BACKSTAGE_TOKEN_FILE`, which Backstage verifies against this cluster's public issuer (`backend.auth.externalAccess` type `jwks`, `accessRestrictions` catalog `catalog.entity.read`); no stored secret. A full-text search, or a Backstage filter when the query has `=`; at most 25 entities, each a summary (ref, kind, title, description, type, owner, lifecycle, system), never annotations |
| Prometheus (`PROMETHEUS_URL`) | `metrics.query` | network reach to the query API. An instant query; at most 200 series returned (`truncated` says when more matched) |
| Loki (`LOKI_URL`) | `logs.query` | network reach to the query API. A range query over the last hour, newest first; at most 200 lines of at most 2000 characters. On hangar-dev the labels are the OpenTelemetry ones (`k8s_namespace_name`, `k8s_pod_name`, `service_name`) |
| The Clearance core (always) | every decision and record | network reach to `CLEARANCE_CORE_URL`, and a projected token for the core's audience at `CLEARANCE_CORE_TOKEN_FILE`; the core lists this identity in `CLEARANCE_PROVIDERS` as `autopilot-gateway`, with `"definitions": true` so the gateway may publish its cluster-pinned definitions (`PUT /v1/definitions`) |
| Postgres (`GATEWAY_DSN`) | sessions, the inbox, results, the outbox, and `artifact.put`/`artifact.get` (`PgArtifacts`: per task, 256 KiB each, 32 and 2 MiB per task, 7 days) | a database the gateway owns (not clearance-pg); it creates its tables at start. Losing the connection ends the process (exit 70): after a failed commit the sessions in memory are ahead of the database, and only a restart, which reloads them, puts the two back in step. The expiry sweep pings the database first, so this is noticed within one interval when no agent is calling |

| GitHub PRs (`GITHUB_PR_ENABLED`) | `repo.pr.open`, `repo.pr.open_upper`, `repo.file.get` | a projected token for audience `agent-installation-token` at `AGENT_TOKEN_FILE`, and `autopilot-system/clearance` in the broker's `agentToken.callers`. Per PR the broker mints a token for the agents' App on that one repo (contents and pull_requests write, never workflows), only for an onboarded app's own repo (`<app>`, `gitops-<app>` or `gitops-infra-<app>`; `repo.pr.open_upper` is T2 and propose-only: a human merges it). The PR is built with the Git data API (no clone), on a new branch only (an existing one is refused, never forced); its commit trailer and body carry the agent, task, session and run. `repo.file.get` reads a file (or lists a directory) at a ref with the same per-call token, GET only; over 1 MiB or binary is named, not returned |
| Model proxy (`MODEL_KEY_FILE`) | a run's model calls | the provider key file, egress to `api.anthropic.com`, and the Service `model-proxy` on `MODEL_PORT` selecting this pod. A run sends its model-proxy token as `x-api-key` and its session as `x-hangar-session`; Clearance checks the session's model allowlist, network mode, breaker and token budget, forwards the body only, and charges the actual usage to the session in the same evidence chain (clearance `server.create_model_app`) |
| Event triggers (`EVENTS_ENABLED`) | Glidepath event triggers | glidepath-api's reader role (it already lists this identity). One run per (agent, event id) while it is live, a storm brake per agent (`maxPerHour`, default 6), the event summarised into the run's input beside the definition's `runtime.instructions` (`event_bridge.py`) |

A launched run's pod authenticates as `agent-<run>/run` (function-agentrun's run ServiceAccount). Clearance lets that
caller act as the run's session, and only that one (`AgentRunBackend.principal`); its records name the run.

A query Prometheus or Loki rejects (a parse error, a timeout, too many samples) comes back as data with `error` and
`message`, so the agent can fix it; any other failure is a "backend error".

`human.request` and `human.request.get` are the gateway's own inbox (`engine/inbox.py`), durable on Postgres
(`PgInbox`). A request is answered by the session's owner or a member of the definition's `owner` group, whose
membership is the person's Tower catalog `memberOf` (`reads.BackstageDirectory`, the catalog.read token), either signed in
themselves or through a `CLEARANCE_DELEGATES` principal (Tower) at `GET/POST /v1/human/requests`. Every definition in
`agents/` names its `owner` (team-platform; the Skyport ones team-skyport).

Everything else (`xr.request`, `app.api.get`, `chat.*`) has no handler: a call is audited and refused with "no backend for this tool in this
deployment", charged nothing. `tools_list` still lists them, with their policy.

## Running the read API

`autopilot-gateway api` is the gateway's own read API (`src/autopilot/read_api.py`, R-3c): the runtime's state as a
separate Deployment from the same image. It reads the gateway's tables through its own read-only DSN, never the writer
lock, so Tower's reads never queue behind agents and it can run several replicas. Writes (answering a human request,
the person controls, running an agent) stay on the gateway.

| Operation | Path | |
|---|---|---|
| listAgents, getAgent | `GET /v1/agents`, `/v1/agents/{name}` | the definitions with each one's trigger `hold`, `source` and the `rejected` pins; `full=1` (auditors) adds the instructions |
| listSessions, getSession | `GET /v1/sessions`, `/v1/sessions/{id}` | sessions you may see, newest first (`agent`, `principal`, `task`, `state`, `limit`, `after`); each with `usage`, its model calls' tokens and charge (A-4; zeros on a session saved before it) |
| getSessionResult | `GET /v1/sessions/{id}/result` | a finished run's answer, kept 30 days |
| getTask | `GET /v1/tasks/{task_id}` | the run tree: the task's sessions and their totals, `used` (budgets) and `cost` (the sessions' model usage summed, A-4) (sessions only, no evidence: nothing is asked of Clearance) |
| listHumanRequests | `GET /v1/human/requests` | the human inbox, and a notifier's feed of every pending request |
| listEvents | `GET /v1/events` | the trigger bridge's history |
| listRuntimes | `GET /v1/runtimes` | the runtime catalog and the custom-image globs |
| getContract | `GET /v1/contract` | `contract/autopilot-api.json`, plus `gateway` (where writes go) and `clearance` (where Clearance's reads are) |

`GET /healthz` is the process; `GET /readyz` reads `gateway_sessions`, so a DSN without the gateway's tables is not
ready. Clearance's reads (`/v1/evidence*`, `/v1/receipts`, `/v1/provenance`, `/v1/clearances*`, `/v1/clearance-policy`,
`/v1/policy*`, `/v1/definitions`) are 404 here: they are clearance-api's (the Go `clearance api`, clearance
`docs/clearance-api.md`). The contract is generated from the route table: `python3 tools/gen_api_contract.py` writes
it, `--check` (and `tests/engine/test_read_api.py`) fails when it is stale.

| Variable | Default | |
|---|---|---|
| `GATEWAY_API_DSN` | (required) | the read-only role's DSN on the gateway's own database; it reads `gateway_*` only (connections are forced read-only, 5 s statement timeout) |
| `GATEWAY_API_TABLES` | unset | retired at R-4: `gateway` (what apron's Deployment still sets) is accepted and ignored for one release; any other value, `clearance` included, is refused at start. Apron drops it before the next release |
| `GATEWAY_API_POOL` | `5` | connections per replica (1 to 50) |
| `GATEWAY_API_AUDITORS` | unset | principal globs that read everything |
| `GATEWAY_API_NOTIFIERS` | unset | principal globs that read the human-inbox feed only (Tower's poller) |
| `CLEARANCE_DELEGATES` | unset | as for the gateway: may read for the person named in `X-Hangar-On-Behalf-Of` |
| `CLEARANCE_GATEWAY_URL` | unset | where writes go, named in `/v1/contract` as `gateway` |
| `CLEARANCE_API_URL` | unset | where Clearance's reads are (clearance-api), named in `/v1/contract` as `clearance` |
| `GATEWAY_API_DEFINITIONS_INTERVAL` | `30` | how often the cluster-pinned definitions are re-read from the gateway's store (`gateway_definitions`); this process fetches nothing |

The pre-R-3c `CLEARANCE_API_*` names (`CLEARANCE_API_DSN` and so on) configure nothing since R-4; apron's Deployment
sets the `GATEWAY_API_*` names (`CLEARANCE_API_URL` is a different variable and stays). Auth
(`CLEARANCE_AUDIENCE`, `CLEARANCE_OIDC_*`, `TOWER_*`) and the catalog (`BACKSTAGE_URL`, `BACKSTAGE_TOKEN_FILE`) are the
gateway's variables. A person sees their own sessions and their teams' agents' sessions (`BackstageDirectory`), and an
app's events when they own it (`BackstageOwnership`: Components whose `spec.owner` is them or their team).

## The agent runtime

`hangar-agent` (`runtime/src/hangar_agent/`, its own package in `runtime/pyproject.toml`) is the generic program an
AgentRun pod runs. The session already exists: Clearance opened it and launched the run. The runtime never calls
`session_open`; it calls `tools_list` once and `call` for every action, and talks to the model only through the model
proxy. It holds no authority of its own.

The loop: it asks Clearance for `tools_list(session)` (clearance v0.7.0: only the tools this session may call and the
deployment backs; an older Clearance refuses the argument and the runtime falls back to the whole registry) and
offers those to the model (`glidepath.run.get`
becomes `glidepath__run__get`, since Anthropic tool names allow no dots, and is mapped back through a table), asks the
model for the next step, sends each `tool_use` to Clearance and hands the answer back as a `tool_result`. A refusal
goes back with `is_error` and Clearance's rule, message and hint; the system prompt tells the model to adapt, not
retry. The system prompt also names the run's task id, run id and session, so instructions can use them (a branch
named `agent/<task id>`). Each request carries prompt-cache breakpoints (after the tools,
the system prompt and the newest message), so a turn reads the conversation so far from the cache; Clearance charges
a cache read at a tenth of an input token (clearance `USAGE_WEIGHTS`). A tool result is cut at 16k characters (60k for
`repo.file.get`, since a fix PR carries whole files), and a cut result says it is incomplete. Each turn's log line ends
with `usage=<input>/<cache read>/<cache write>/<output>`. It stops on `end_turn` (done), after `HANGAR_MAX_TURNS` model calls (budget; the last turn's tool calls are
not run), when the session has less than 60 s left (expired), on a 429 from the proxy (budget), and on a 403 or any
failure a retry did not fix (error). stdout gets one line per turn with the tool names and decisions
(`turn 2: stop=tool_use tools=[catalog.read:ok, repo.pr.open:deny(R006)]`), never a token or an argument.

```shell
python -m hangar_agent             # the image's entrypoint; `hangar-agent` when installed from runtime/
```

The environment is airframe's function-agentrun's (`_pod_spec`):

| Variable | Used for |
|---|---|
| `HANGAR_SESSION_ID` | the session every `call` names; also sent to the proxy as `x-hangar-session` |
| `HANGAR_EXPIRES_AT` | RFC 3339; the loop stops 60 s before it |
| `CLEARANCE_URL` | the gateway; the runtime POSTs JSON-RPC `tools/call` to `$CLEARANCE_URL/mcp` (stateless, no initialize) |
| `MODEL_PROXY_URL` | the proxy; `POST $MODEL_PROXY_URL/v1/messages`, the Anthropic Messages API, non-streaming. Without it (network mode `clearance` only) the runtime does not start |
| `HANGAR_TOKEN_DIR` | `clearance-token` (`Authorization: Bearer`, to Clearance) and `model-proxy-token` (`x-api-key`, to the proxy; `Authorization` is not sent), both read again on every request because the kubelet rotates them |
| `HANGAR_INPUT` | optional JSON: `prompt` (the user turn), `event` (appended to it as JSON), `instructions` (appended to the system prompt), `model`. With neither `prompt` nor `event` the run ends as an error without calling the model |
| `HANGAR_MODEL` | the model alias, over `HANGAR_INPUT.model`; default `claude-sonnet-5-5`. The proxy maps it upstream |
| `HANGAR_MAX_TURNS`, `HANGAR_MAX_TOKENS` | model calls per run (default 30) and `max_tokens` per call (default 8192) |
| `HANGAR_OUTPUT_DIR` | where `result.json` goes (`/run/output`) |
| `HANGAR_LIMITS` | read; if it ever carries `tools` (the session's grant), only those are offered |
| `HANGAR_RUN_ID`, `HANGAR_TASK_ID`, `HANGAR_AGENT` | the start line and the system prompt |

`result.json`, written atomically in every case the process can still write, a bad environment included:

```json
{"status": "done", "turns": 2, "final_text": "flight-api is owned by ...", "reason": "",
 "tool_calls": [{"tool": "catalog.read", "ok": true, "rule": null},
                {"tool": "repo.pr.open", "ok": false, "rule": "R006"}]}
```

`status` is `done`, `budget`, `expired` or `error`, and `reason` says why it is not `done`. The exit code is 0 for
`done`, 1 otherwise, 2 for an unusable environment.

The pod's `/run/output` goes with its namespace, so after writing `result.json` the runtime hands the same result to
Clearance (`run_finish`, clearance v0.25.0): Clearance keeps the answer 30 days for Tower's run detail
(`GET /v1/sessions/{id}/result`), puts the outcome on the session and closes it. A failure there is logged and changes
nothing else; a run that ends without handing one back is closed by Clearance's sweep from its AgentRun's
`status.outcome` (`AgentRunBackend.ended`, a GET of that one AgentRun), with the outcome `no-result`.

The image is `runtime/Dockerfile`: python:3.13-slim pinned by its multi-arch digest, the dependencies (the anthropic
SDK only) installed from the hashed `runtime/requirements.lock`, uid 65532, nothing written outside `/run/output` and
`/tmp`, so it runs with a read-only root filesystem under any uid (function-agentrun sets 10001). The
`runtime-image` workflow builds it for linux/amd64 and linux/arm64 on a `runtime-v*` tag and pushes
`ghcr.io/jfillman/autopilot-agent-runtime:<tag>`.

```shell
container build --arch arm64 -t autopilot-agent-runtime:dev runtime   # or --arch amd64
```

### The runtime image library

`runtime/catalog.yaml` lists the agent images Hangar offers (decided 2026-10-31). Every one speaks the contract
above, so any fits any definition. The gateway loads the catalog (`engine/runtimes.py`): an
**available `runtime`** image is what a definition's `runtime.image` may pin (with `RUNTIME_IMAGES_EXTRA` globs for
images Glidepath builds for a code agent's own repo); a **`base`** image is a `FROM` image for code agents, never run
as is; autopilot-api (the read API) lists all of it at `GET /v1/runtimes` for the New agent picker.

| Name | Kind | State |
|---|---|---|
| `generic` | runtime | available: this loop, `runtime-v0.1.3` (hands its result to Clearance) |
| `base-python` | base | available: `runtime-base-python-v0.1.1` (its Clearance client has `finish()`; call it after `result.json`), from `runtime/base-python/Dockerfile` |
| `claude-agent-sdk` | runtime | available: `runtime-claude-agent-sdk-v0.1.0`, from `runtime/claude-agent-sdk/` (needs clearance v0.26.0's streaming model proxy) |
| `langgraph` | runtime | available: `runtime-langgraph-v0.1.0`, from `runtime/langgraph/` |

**`langgraph`** (`hangar_langgraph`) is this loop as a LangGraph graph: a `model` node (the model proxy) and a `tools`
node (Clearance), looping until the model ends its turn or the turns or the session's time run out. The tests run
every scenario through both and require the same outcome. It is the image to `FROM` for a graph of your own
(`hangar_langgraph.graph.build`).

**`claude-agent-sdk`** (`hangar_claude`) runs the Claude Agent SDK's bundled Claude Code CLI as the loop, with **no
built-in tools** (`tools=[]`: no Bash, no file edits, no web), no settings files and no MCP config but its own: each
tool the session may call is an in-process MCP tool (`mcp__clearance__<name>`) that forwards to Clearance, so every
action is decided and recorded there. The CLI's API is the model proxy (`ANTHROPIC_BASE_URL`), with the run's
model-proxy token as its key and `x-hangar-session` as a custom header; every model alias is pinned to the run's model
and nonessential traffic is off. The CLI streams, and its requests carry betas their bodies depend on, which is why it
needs clearance v0.26.0 (streaming relay, `anthropic-beta` forwarded less OAuth). Checked against a logging fake API:
it calls only `POST /v1/messages`.

Both use `hangar_agent.main.run_pod`, so `result.json`, `run_finish` and the exit codes are the generic runtime's.

The `runtime-image` workflow builds each multi-arch from its tag. A code agent: `FROM` the base, add its code and an
entrypoint, `from hangar_agent import env, clearance, model` for the contract, and let its own Glidepath build publish
the image its definition pins.

## The Clearance GitHub App on Hangar

From R-3b the App runs in the Clearance core (`CLEARANCE_GITHUB_APP_*` on the core; the gateway refuses them), and
since R-4 the webhook route reaches the core (apron `55-clearance/clearance-config/github-app.yaml`, cluster.yaml
`clearance.githubApp`). The steps below are as they were set up with the in-process gateway.

Clearance's own check runs (`Clearance / agent-identity`, `Clearance / agent-scope`; clearance
`docs/provenance-app.md`) run on hangar-dev beside Glidepath's `agent-gates-` check until their verdicts agree.
Setting it up:

1. **Register the App** as `Hangar Clearance` (GitHub reserves `Clearance`) from clearance `contract/github-app-manifest.json`, with the webhook URL
   `https://clearance-webhook.hangarplatform.dev/v1/github/webhook`. It asks only for checks write and contents,
   pull requests and metadata read. Install it on the repos agents open PRs on.
2. **Store its id, private key (PEM) and webhook secret** in hangar-dev's Infisical project as
   `clearance-github-app-id`, `clearance-github-app-private-key` and `clearance-github-app-webhook-secret`.
3. **Route the hostname**: `cloudflared tunnel route dns pac-webhook clearance-webhook.hangarplatform.dev` (needs a
   `cloudflared tunnel login`), then `~/tech/pac-tunnel/install.sh pac-webhook.hangarplatform.dev
   clearance-webhook.hangarplatform.dev`. The tunnel forwards `POST /v1/github/webhook` only; every other path is a
   404 at Cloudflare.
4. **Turn it on**: `autopilot.githubApp: true` in gitops-hangar-dev `cluster.yaml`, then render (apron
   `55-autopilot/clearance/github-app.yaml`). The gateway logs `github-app=on (app <id>, comments off)` at start.

## Signing in as a person

On hangar-dev, people sign in through people-dex (apron `55-autopilot/people-dex`), a Dex whose only connector is GitHub. This is Hangar D19's interim answer until an IdP issues ID-JAG. Clearance accepts its ID tokens for audience `clearance`, as `user:<GitHub login>`, and only for the logins in cluster.yaml `autopilot.people`. ID tokens last an hour, and no refresh token is issued.

```shell
python3 tools/hangar_login.py login        # opens the sign-in page; the token goes to the macOS Keychain
claude mcp add-json clearance '{"type":"http","url":"http://clearance.hangar-dev.local/mcp",
  "headersHelper":"python3 '"$PWD"'/tools/hangar_login.py headers"}'
```

`headers` never signs in: Claude Code gives a headersHelper 10 seconds. When the hour is up, a call fails 401, and you run `login` again.

## What is NOT built

Checked against gateway-v0.4.0 (clearance v0.29.0's core), the image on hangar-dev, on 2026-11-03.

* Tools with no backend in the image: `chat.recv` and `chat.send` (no human chat backend; `human.request` is the
  inbox), `app.api.get` (no adapter) and `xr.request`. `serve.build_handlers` drops them, so a call is refused as
  "no backend". The nine Skyport definitions under `agents/skyport/` depend on them and are not loaded.
* `airframe.validate` in the image: it needs airframe's `airframe-validate` (helm included), which the image does not
  carry, so the deployment drops it.
* Backstage MCP federation.
* A durable event cursor: the event bridge holds glidepath-api's `/v1/events` stream, which is an in-memory buffer
  (glidepath known gap #44), and keeps no cursor of its own, so events sent while it is down are not replayed.
* More than one gateway replica: the Postgres store has one writer (advisory lock). clearance-api, the read side,
  runs two.
* Stable agent identity by ID-JAG: people sign in through people-dex (Hangar D19's interim) and runs authenticate by
  TokenReview until an issuer exists.
* Anything on hangar-prod: Clearance and Autopilot run on hangar-dev only; Tower on hangar-prod reads autopilot-api
  (`AUTOPILOT_API_URL`, the runtime's reads) and clearance-api (Clearance's) across clusters.
* The deployment gate's enforcement at admission (the pulled grant and its policy): Clearance's
  `docs/gate.md`. On Hangar the `clearance-gate-` check is advisory.
* Dropping the `clearance_*` runtime tables in clearance-pg: the R-4 copy moved them to the gateway's database
  (docs/admin/gateway-database.md) and nothing reads them now; dropping them is a separate step. The read
  API keeps api.py's behaviour on unknown query parameters (ignored, where the Go API answers 400) and runs on uvicorn
  without per-phase server timeouts; both come with A-9. The core changes R-3b needed (record kinds for `run.finish`,
  `session.end`, `trigger.release` and coverage-unavailable `tool.call`; `PUT /v1/definitions`; a `via` on a
  person's run; a handler refusal on an outcome; a clock-skew setting and a chain read for tests) are clearance
  v0.28.0, which R-3f adopts, deployed at R-4. The Go port of the gateway is A-9.
