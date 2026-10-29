<div align="center">
  <img src="docs/brand/autopilot-tile.svg" width="88" height="88" alt="Autopilot mark" />
  <h1>Autopilot</h1>
  <p><i>Any AI agent workload, bounded and audited.</i></p>
</div>

Autopilot is the sixth [Hangar](https://github.com/jfillman/hangar) product: it runs AI agent workloads with authority
that is bounded and every action on the record. This repo holds the **Autopilot** code that plugs Hangar into
**Clearance** (`src/autopilot/`), plus agent definitions, the Preflight evaluation cases and the AppSpec planner.
Docs: [docs/](docs/index.md). Mark and brand files: [docs/brand/](docs/brand/README.md).

## Clearance lives in its own repo

Clearance, the policy, audit and session gateway, is its own product and repo since 2026-10-29 (Hangar decision D16):
https://github.com/jfillman/clearance. Its core installs where there is no Airframe and no Glidepath; its Hangar pack
(`clearance.packs.hangar`: the `glidepath.*` and `airframe.*` tool policy, R021 to R023) ships with it. Autopilot
pins a tagged release in `pyproject.toml` and supplies what only Hangar has:

| Part | Where | What it owns |
|---|---|---|
| Clearance (pinned) | `clearance[serve,pg] @ git+https://github.com/jfillman/clearance@v0.4.0` | tiers, scope, limits, CEL policy and packs, sessions and the run tree (`Gateway.expire`), the gateway, the MCP surface over stdio and streamable HTTP, authentication (TokenReview, OIDC), the Postgres store, model-access decisions, audit |
| Autopilot | `src/autopilot/` | Hangar's tool implementations and everything around agents on Hangar: `glidepath_api.py`, `airframe_api.py`, `airframe_tools.py`, the planner `airframe_plan.py`, the AgentRun manifest `manifests.py`, `triggers.py`, `preflight.py`, the Hangar backends and fakes `backends.py`, the AgentRun store over the Kubernetes API `kube_runs.py`, the T0 read adapters `reads.py`, the deployable server `serve.py`, the vendored airframe files `vendor/`, and `gateway.py`, which builds the Hangar Gateway: Clearance's core + the Hangar pack + a handler per tool + AgentRun as the run backend |

```python
Gateway(definitions, store, audit, auth, clock, packs=(HANGAR,), handlers=hangar_handlers(backends),
        runs=AgentRunBackend(backends.runs))       # what autopilot.gateway.hangar_gateway() does
```

A change to Clearance's policy (a new tool spec, a rule, a re-vendored Glidepath contract) is a clearance release and
a pin bump here. `tests/test_cicd_scope.py` checks the pinned release's vendored Glidepath files against glidepath, so
a Glidepath contract change fails here until the pin moves.

Design, plan and diagrams: `../hangar/docs/autopilot/` (start at `README.md`).

## What is built and tested

`./.venv/bin/python -m pytest -q` runs 411 tests, no cluster needed; Clearance's own tests are in its repo (some skip without sibling airframe and glidepath checkouts under `HANGAR_TECH`, which CI clones).

`clearance/...` paths below are in the clearance repo, under `src/clearance/`.

| Piece | Where | What it does |
|---|---|---|
| Tool tiers and tripwires | `clearance/tiers.py` | T0 to T2 tools; T3 names are tripwires |
| Path and repo scope | `clearance/scope.py` | one implementation, shared with the CI `agent-scope` gate; traversal-safe |
| cicd.yaml field scope | `clearance/packs/hangar/` (R021, `cicd.py`), its `vendor/cicd_scope.py` + `vendor/glidepath-contract.json` (vendored byte for byte from glidepath, drift-tested) | an agent may change cicd.yaml in a PR within Glidepath's `open`/`tighten`/`human` field rules; a denial carries the `GP-SCOPE-00x` rule id and hint |
| Glidepath reads and run control | `clearance/packs/hangar/` (`glidepath.*`, R022), `autopilot/glidepath_api.py`, `autopilot/gateway.py` (handlers), `agents/cicd-explainer.yaml` | six T0 tools over glidepath-api: runs, one run, step logs, `explain` (a failed run's knownFailures id, hint and field), releases, the contract; and two T1 writes, `glidepath.run.rerun` and `glidepath.run.cancel` (spec phase 3), which retire `pipeline.rerun`: a re-run sends an Idempotency-Key made from the session and the run, so a retried call starts no second run, and Clearance names the session principal so glidepath-api audits both identities. `profile.glidepath.apps` (globs; none listed means none) scopes them per agent, arguments must be valid path segments, and the adapter calls as Clearance's own identity, re-reading its token file every call. A 400/404 from glidepath-api (no such run, no archived log) comes back as data with its `GP-*` rule; a 401/403 is a denial with that rule |
| Glidepath release proposals | `clearance/packs/hangar/` (`glidepath.environments.*`, `glidepath.promote`, `glidepath.rollback`), `autopilot/glidepath_api.py`, `autopilot/gateway.py` (`release_key`), `agents/release-proposer.yaml`, `tools/glidepath_release_smoke.py` | spec phase 4. Two T0 reads (the release plan; a Flight environment's rollback plan) and two T2 propose-only writes: `glidepath.promote` and `glidepath.rollback` start the app's release run, whose `open-release-pr` step opens the gitops PR with the platform's GitHub App, and a person merges it. Not capped by the tier ceiling (R007 admits propose-only at T1) but only for a definition that lists them. The Idempotency-Key is the session, app, target environment and cluster, and the source (or rollback target), never `dryRun` or the reason, so a retry replays and a dry run names the run the real call starts; `dryRun` goes as `X-Glidepath-Dry-Run`. A 422 (out of order, a Ground target, an ineligible target, admission) is a denial with glidepath-api's hint. `release-proposer` (gate-api only) holds these, the Glidepath reads, and nothing that reaches GitHub: no `repo.pr.open*`, `xr.request` or `run.spawn`, and zero GitHub, PR and child budgets; no tool in the registry merges or approves |
| Airframe reads | `clearance/packs/hangar/` (`airframe.*`, R023), `autopilot/airframe_tools.py`, `autopilot/airframe_api.py`, `schemas/airframe-contract.json` + `autopilot/vendor/airframe_sidecars.py` (vendored from airframe, drift-tested), `agents/airframe-operator.yaml`, `tools/airframe_smoke.py` | seven T0 tools. `capabilities` answers from Airframe's contract bundle; `validate` runs airframe's own `airframe-validate` (a configured command or container, parsed from `--format json`) and returns AF-* rule ids, JSON pointers and hints; `explain` turns an AF-* rule, a condition reason or a knownFailures id into a description and fix; `plan` is the AppSpec planner; `status` normalises an app's or environment's XR conditions (reason, meaning, hint, observedGeneration against generation); `describe` joins an environment's XRs, the Argo CD Application syncing it (sync, health, revision per source) and its Rollout (phase, ready replicas, image); `verify` runs a kind's sidecar verify checks from the XR alone and returns anything needing a Secret, ConfigMap, port-forward or cloud CLI as `skipped` with the reason. The Kubernetes adapter is GET-only against a hard path allowlist (`catalog.hangar.io` XRs, `argoproj.io` applications and rollouts; no core group, no subresource, no watch), refused before any request is built, with the token and CA re-read every call. `profile.airframe.apps` (globs; none listed means none) scopes every call that names an app |
| Narrow-only limits | `clearance/limits.py` | requested limits or a child can only narrow what they inherit |
| Agent definitions | `clearance/profile.py` and its `schemas/agent-definition.schema.json` | durable, schema-validated; baseline deny paths cannot be removed |
| Sessions and run tree | `clearance/session.py` | budgets, expiry, breaker, parent-reserved child budgets |
| Policy | `clearance/policy.py`, `clearance/pack.py` | CEL deny rules with ids, an explicit order and fix hints, core plus each pack's; fails closed |
| Audit | `clearance/audit.py` | hash chain; detects edit, delete, reorder; truncation via an anchored checkpoint |
| Gateway core | `clearance/dispatch.py`, `clearance/backends.py` | authenticate, decide, audit, spend, call, audit; handlers are registered per tool, the run tree goes through an injected run backend |
| Model access | `clearance/modelproxy.py` | model allowlist, network mode, token budget; the decision core only |
| Triggers | `autopilot/triggers.py` | interval and alert triggers for scheduled and event agents |
| Preflight | `autopilot/preflight.py`, `preflight/cases/` | deterministic scoring, self-check that a case can fail |
| MCP surface | `clearance/server.py` | three tools on the mcp SDK v2; `tools_list` lists the Gateway's own registry |
| AppSpec and the planner | `autopilot/airframe_plan.py`, `schemas/appspec.schema.json` (vendored from airframe's contract) | the parachute test: one statement of intent to an ordered change set; its cicd.yaml patch is split by Glidepath's scope rules: the agent's half (Ground environments and their deploy steps, added without dropping a step) passes R021, and Flight environments, the release step and retiring pre-ADR-0019 keys are a separate `cicd-flight` step a human merges; `config`/`overrides` take any human-owned chart value (second sentence: `examples/mynewpythonapp.appspec.yaml`, five environments, canary with pauses, HPA, PDB, a prod-only route); validated against the real XRD schemas, Glidepath's cicd schema and a real `helm template` of every environment |
| Skyport agents | `agents/skyport/`, `preflight/cases/skyport-*.yaml` | one definition per workload shape, plus team members; event trigger, app-API allowlist, chat, artifacts, idempotent triggered runs |
| AgentRun composition | lives in airframe (`xrds/agentrun.yaml`, `compositions/agentrun/`, `functions/function-agentrun/`); `src/autopilot/vendor/agentrun_compose.py` and `schemas/agentrun.xrd.yaml` are byte-identical copies | the canary renders with the vendored `compose()`; Clearance's AgentRun manifests are validated against the vendored XRD; `tests/test_agentrun_vendor.py` fails on drift |
| AgentRun store | `autopilot/kube_runs.py` | the real `AgentRunStore`: POST to `/apis/catalog.hangar.io/v1alpha1/namespaces/autopilot-runs/agentruns` and DELETE of one named AgentRun there, nothing else (any other method, path, namespace, kind or a non-DNS-label name is refused before a request is built); a 404 on delete is success, a 409 on create an error; token and CA re-read every call |
| Read adapters | `autopilot/reads.py` | `argo.app.get` over the Application objects (the GET-only Kubernetes reader), `metrics.query` on a Prometheus-compatible API, `logs.query` on Loki; summaries and bounds, a rejected query as an answer |
| The deployable gateway | `autopilot/serve.py` (`autopilot-gateway`), `Dockerfile` | builds the Hangar Gateway from environment variables and serves Clearance's HTTP app with uvicorn; real backends only; the expiry sweep on a thread under the same lock as every gateway call. See [Running the gateway](#running-the-gateway) |
| The agent runtime | `runtime/` (`hangar_agent`, `runtime/Dockerfile`), `tests/test_runtime_*.py` | the program an AgentRun pod runs: a tool-use loop that offers Clearance's tools to the model, sends every call through Clearance in the run's existing session, and writes `result.json`. See [The agent runtime](#the-agent-runtime) |
| Network-policy canary (AP-A1) | `tools/netpol_canary.py` | renders a run's real policies with `compose()` on a live cluster and proves each denial against a no-policy control; its pass is what a person attests when setting the registry's `airframe.autopilotReady` |

## Running the gateway

`autopilot-gateway` (`src/autopilot/serve.py`) is the deployable Clearance for Hangar: core Clearance, the Hangar pack
and Autopilot's real backends, served as Clearance's streamable-HTTP MCP app (`/mcp`, bearer auth on every request,
an unauthenticated `GET /healthz`). The image is the repo's `Dockerfile` (python:3.13-slim, uid 65532, nothing
written at runtime, so it runs with a read-only root filesystem; linux/amd64 and linux/arm64):

```shell
container build --arch arm64 -t autopilot-gateway:dev .         # or --arch amd64
container run --rm --read-only -e CLEARANCE_EPHEMERAL=1 -p 8080:8080 autopilot-gateway:dev
curl -s localhost:8080/healthz                                   # {"ok":true,"tools":35}
```

| Variable | Default | What it does |
|---|---|---|
| `CLEARANCE_DSN` | required unless ephemeral | Postgres for sessions and evidence (`clearance.pg.open_durable`). One writer: a second replica on the same database stops at start (advisory lock), so run one |
| `CLEARANCE_EPHEMERAL` | `0` | `1`: in-memory sessions and audit log, lost on exit. Local runs only; `CLEARANCE_DSN` is ignored |
| `CLEARANCE_AGENTS_DIR` | the repo's `agents/` (`/app/agents` in the image) | the agent definitions loaded at start (that directory only, not its subdirectories) |
| `CLEARANCE_RESOURCE_URL` | `http://localhost:$PORT/mcp` | this server's public URL, for the OAuth protected-resource metadata |
| `CLEARANCE_OIDC_ISSUER`, `CLEARANCE_OIDC_AUDIENCE` | unset | set both to accept people's OIDC ID tokens from that issuer for that audience (Dex in the lab); unset, only workload tokens are accepted. The issuer is also the authorization server the metadata names (else the resource URL) |
| `CLEARANCE_OIDC_JWKS_URL` | `$CLEARANCE_OIDC_ISSUER/keys` | the issuer's JWKS (Dex's path) |
| `CLEARANCE_OIDC_USER_CLAIM` | `email` | the claim that names the person (`user:<claim>`) |
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
| `ARGO_APP_NAMESPACES` | `argocd` | comma-separated: the namespaces `argo.app.get` looks for an Application in, in order |
| `PROMETHEUS_URL` | unset | a Prometheus-compatible query API (e.g. `http://thanos-query.observability.svc:9090`); unset, `metrics.query` has no backend |
| `LOKI_URL` | unset | Loki's query API (e.g. `http://loki-gateway.observability.svc`); unset, `logs.query` has no backend |
| `LOG_LEVEL` | `info` | |

The start line names what is enabled (store kind, agent and tool counts, auth, Glidepath URL, Airframe cluster, expiry
interval) and never a DSN, token or file content.

What each backend needs. The gateway runs as one ServiceAccount (`autopilot-system/clearance` on hangar-dev) with its
projected token and CA at the standard path; every adapter re-reads both on each call.

| Backend | Tools | Needs |
|---|---|---|
| Authentication (always) | every MCP request | `create` on `tokenreviews.authentication.k8s.io`: a ClusterRoleBinding to `system:auth-delegator`. Callers bring a projected token with audience `CLEARANCE_AUDIENCE`. For people, network reach to the OIDC issuer's JWKS |
| AgentRuns (always) | `run.spawn`, `run.close`, the expiry sweep | Role `clearance-agentruns` in `autopilot-runs`: `create` and `delete` on `agentruns.catalog.hangar.io` (the adapter sends nothing else) |
| Airframe reads (`AIRFRAME_ENABLED`) | `airframe.capabilities`, `explain`, `plan`, `status`, `describe`, `verify` | ClusterRole `clearance-airframe-reader`: get/list/watch on `catalog.hangar.io` and `argoproj.io` applications and rollouts, nothing else (gitops-hangar-dev#40). `airframe.validate` has no backend in the image |
| Glidepath (`GLIDEPATH_API_URL`) | `glidepath.*` | network reach to glidepath-api and RBAC there for Clearance's identity: glidepath-api checks it with a SubjectAccessReview (ClusterRoles `glidepath-api-reader`, `-operator`, `-releaser` for reads, run control and release proposals) |
| Argo CD reads (`ARGO_ENABLED`) | `argo.app.get` | the same `clearance-airframe-reader` ClusterRole: it reads the Application object through the Kubernetes API (no Argo CD account, no token) and returns a summary: sources, destination, sync, health, the last operation, conditions and the resources not synced or not healthy. Never the Helm values |
| Prometheus (`PROMETHEUS_URL`) | `metrics.query` | network reach to the query API. An instant query; at most 200 series returned (`truncated` says when more matched) |
| Loki (`LOKI_URL`) | `logs.query` | network reach to the query API. A range query over the last hour, newest first; at most 200 lines of at most 2000 characters. On hangar-dev the labels are the OpenTelemetry ones (`k8s_namespace_name`, `k8s_pod_name`, `service_name`) |
| Postgres (`CLEARANCE_DSN`) | everything | a database the gateway owns; it creates its tables and verifies the evidence chain from genesis at start. Losing the connection ends the process (exit 70): after a failed commit the sessions in memory are ahead of the database, and only a restart, which reloads them, puts the two back in step. The expiry sweep pings the database first, so this is noticed within one interval when no agent is calling |

A query Prometheus or Loki rejects (a parse error, a timeout, too many samples) comes back as data with `error` and
`message`, so the agent can fix it; any other failure is a "backend error".

Everything else (`repo.pr.open*`, `xr.request`, `argo.sync.lower`, `catalog.read`, `app.api.get`, `chat.*`,
`human.request`, `artifact.*`) has no handler: a call is audited and refused with "no backend for this tool in this
deployment", charged nothing. `tools_list` still lists them, with their policy.

## The agent runtime

`hangar-agent` (`runtime/src/hangar_agent/`, its own package in `runtime/pyproject.toml`) is the generic program an
AgentRun pod runs. The session already exists: Clearance opened it and launched the run. The runtime never calls
`session_open`; it calls `tools_list` once and `call` for every action, and talks to the model only through the model
proxy. It holds no authority of its own.

The loop: it offers every tool in `tools_list` to the model (the deployment's whole registry; `glidepath.run.get`
becomes `glidepath__run__get`, since Anthropic tool names allow no dots, and is mapped back through a table), asks the
model for the next step, sends each `tool_use` to Clearance and hands the answer back as a `tool_result`. A refusal
goes back with `is_error` and Clearance's rule, message and hint; the system prompt tells the model to adapt, not
retry. It stops on `end_turn` (done), after `HANGAR_MAX_TURNS` model calls (budget; the last turn's tool calls are
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

The image is `runtime/Dockerfile`: python:3.13-slim pinned by its multi-arch digest, the dependencies (the anthropic
SDK only) installed from the hashed `runtime/requirements.lock`, uid 65532, nothing written outside `/run/output` and
`/tmp`, so it runs with a read-only root filesystem under any uid (function-agentrun sets 10001). The
`runtime-image` workflow builds it for linux/amd64 and linux/arm64 on a `runtime-v*` tag and pushes
`ghcr.io/jfillman/autopilot-agent-runtime:<tag>`.

```shell
container build --arch arm64 -t autopilot-agent-runtime:dev runtime   # or --arch amd64
```

## What is NOT built

* Real backends: GitHub through the token-review-interceptor, Argo CD sync, the Backstage catalog (`catalog.read`;
  Backstage runs on hangar-prod only, and its catalog API needs a credential), allowlisted app APIs (`app.api.get`;
  no definition uses it yet), human chat, artifacts, Backstage MCP federation. `clearance/backends.py` (core tools) defines the
  interfaces and fakes; the deployable gateway registers no handler for these, so their calls are refused as having no
  backend.
* `airframe.validate` in the image: it needs airframe's `airframe-validate` (helm included), which the image does not
  carry.
* The model proxy's HTTP forwarder.
* A deployment of the gateway (manifests, the Postgres database, the TokenReview binding): nothing here deploys it.
* The interceptor's `/agent-installation-token` route, the `agent-scope` and `agent-identity` CI gates.
* The AgentRun composition has not run under Crossplane on a cluster yet (AP-A3's copy-test, airframe
  `docs/admin/agentrun-copy-test.md`). Expiry does not depend on Crossplane re-invocation (U2: about every 60 s):
  the Sandbox's `shutdownTime` is the deadline.
