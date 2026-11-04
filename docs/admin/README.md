# Autopilot: admin guide

For people **running** Autopilot. On Hangar it is installed by apron's `templates/55-autopilot/` (rendered into the
cluster repo, synced by Argo CD): the gateway, its read API autopilot-api, the gateway's CNPG database `autopilot-pg`,
people-dex, the AgentRun grants, agent-sandbox and the run reaper, all on hangar-dev. The `cluster.yaml` `autopilot:`
keys turn each feature on. The image is pinned by tag and digest: gateway-v0.4.0, calling clearance v0.29.0's core
(apron `55-clearance`), since the R-4 cutover on 2026-11-03.

## The gateway image
`autopilot-gateway` (`src/autopilot/serve.py`, the repo's `Dockerfile`) is the deployable gateway for Hangar. Its code
is this repo's `src/autopilot/engine/` (moved from clearance v0.26.1 in R-3a). From R-3b it asks the Clearance core
(`CLEARANCE_CORE_URL`) for every decision and writes every record through it; it holds no evidence. It serves the
streamable-HTTP MCP app on `PORT` (8080) at `/mcp`, `GET /healthz` unauthenticated for probes. Every variable, and the
RBAC each backend needs, is in the README's [Running the gateway](../../README.md#running-the-gateway). In short:
- **One replica of the gateway.** The read API (`autopilot-gateway api`, [The read API](#the-read-api); the Deployment
  is still named clearance-api on hangar-dev) is a separate Deployment with two replicas on a read-only role. The gateway's own Postgres (`GATEWAY_DSN`, not
  clearance-pg: [gateway-database.md](gateway-database.md)) takes an advisory lock; a second writer on the same
  database exits at start. The gateway holds one connection and does not reconnect: a database error from any
  operation ends the process (exit 70) and the kubelet restarts it, which reloads the sessions. The expiry
  sweep pings the database first, so a lost connection is found within `CLEARANCE_EXPIRE_SECONDS` with no traffic.
- **The Clearance core.** `CLEARANCE_CORE_URL` (required), and a projected service-account token at
  `CLEARANCE_CORE_TOKEN_FILE` (default `/var/run/secrets/tokens/clearance-core`) whose audience is the core's
  `CLEARANCE_AUDIENCE` (default `clearance`). The core registers the gateway's identity as provider
  `autopilot-gateway` in `CLEARANCE_PROVIDERS`, with `"definitions": true` (clearance v0.28.0): the gateway publishes
  its cluster-pinned definitions to the core (`PUT /v1/definitions`), which records each change, and a provider
  without it is refused. For example
  `[{"name": "autopilot-gateway", "version": "<gateway tag>", "principals": ["autopilot-system/clearance"],
  "definitions": true}]`. A NetworkPolicy must let the gateway reach the core, and nothing but
  the core and clearance-api reach clearance-pg.
- **Outage policy.** When the core is unreachable the gateway denies every T1-and-above call, session open, spawn
  and model call (rule `CORE-UNAVAILABLE`); T0 reads too, unless `GATEWAY_OUTAGE_ALLOW_READS=true`, which lets
  unscoped reads run and records them later as coverage-unavailable. 250 ms per attempt, one retry, a breaker after 5
  failures. The README's [outage policy](../../README.md#the-outage-policy) has every branch. `GET /healthz` shows the
  breaker.
- **Not the gateway's any more.** The evidence DSN, the deployment gate, provenance, definition review and the GitHub
  App are the core's: `CLEARANCE_DSN`, `CLEARANCE_VERIFIERS`, `AGENT_LOGINS`, `CLEARANCE_GITHUB_APP_*` and
  `CLEARANCE_GITHUB_OIDC_*` set on the gateway stop it at start.
- **Read-only root, uid 65532.** Nothing is written at runtime; no volume is needed.
- **Identity.** One ServiceAccount (`autopilot-system/clearance`): `system:auth-delegator` (TokenReview, for every
  caller), Role `clearance-agentruns` in `autopilot-runs` (AgentRuns), ClusterRole `clearance-airframe-reader`
  (Airframe reads, and Argo CD Applications for `argo.app.get`), and glidepath-api's ClusterRoles when
  `GLIDEPATH_API_URL` is set.
- **Only real backends.** Each is on when configured: glidepath-api (`GLIDEPATH_API_URL`), Airframe and Argo CD reads,
  Argo CD syncs of lower Applications (`ARGO_SYNC_ENABLED`), PRs as the agents' App through Glidepath's broker
  (`GITHUB_PR_ENABLED`), `catalog.read` (`BACKSTAGE_URL`), `metrics.query` and `logs.query`, the model proxy
  (`MODEL_KEY_FILE`), event and schedule triggers, `Agent` XR pins, and artifacts on the durable store.
  `xr.request` (with GitHub PRs; `XR_REQUEST_ENABLED=0` turns it off) and `app.api.get` (`APP_API_SERVICES`) too.
  `chat.*` and `airframe.validate` have no handler in the image, so their calls are refused as "no backend", and no
  bundled definition names them (`tests/test_definitions_backed.py`); nothing is faked.
- **Logs to Loki.** `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` sends a copy of every log line to the cluster's OpenTelemetry
  collector over OTLP/HTTP JSON (`otlp_logs.py`, standard library only; a bounded queue, batches with a 5 s deadline,
  and a collector outage drops lines, never blocks a call). The read API (`autopilot-gateway api`) does the same as `autopilot-api`. On Hangar apron sets it with `OTEL_SERVICE_NAME` and the
  pod's namespace and name, so `{service_name="autopilot-gateway"}` and `{k8s_namespace_name="autopilot-system"}`
  find them in Loki.
- **Expiry.** Every `CLEARANCE_EXPIRE_SECONDS` (60) the gateway closes sessions past their deadline and deletes their
  AgentRuns, each deletion a record; a failed deletion is recorded and retried next sweep. The same sweep posts what
  waits in the outbox once the core answers.
- **Local run:** `CLEARANCE_EPHEMERAL=1` (in-memory store, nothing kept) and a core, e.g.
  `clearance core --memory --insecure-dev-auth` with `CLEARANCE_PACKS=hangar` and `CLEARANCE_PROVIDERS` naming the
  gateway (with `"definitions": true`); tool calls that need the cluster fail as backend errors.

## The read API
Status: **live on hangar-dev** since the R-4 cutover (2026-11-03): the `autopilot-api` Deployment, two replicas;
Tower reads it through `AUTOPILOT_API_URL`. `autopilot-gateway api` (`src/autopilot/read_api.py`, `api_serve.py`) is the
gateway's own read API: agents and their trigger holds, sessions, run results, the run tree, the human inbox, trigger
history and the runtime catalog, with the same paths, answers and roles clearance's `api.py` had (contract:
`contract/autopilot-api.json`, generated by `tools/gen_api_contract.py`). The README's
[Running the read API](../../README.md#running-the-read-api) lists every route and variable.
- **Read only.** Its own DSN (`GATEWAY_API_DSN`), every connection forced read-only with a 5 s statement timeout, a
  pool of `GATEWAY_API_POOL` (5). No writer lock, no migration: it runs as many replicas as reads need.
- **Which tables.** Only the gateway's own database's (`gateway_*`, R-3b). `GATEWAY_API_TABLES=clearance` (clearance
  v0.26.1's `clearance_*` tables on clearance-pg) is retired at R-4 and refused at start; `GATEWAY_API_TABLES=gateway`,
  which apron's Deployment still sets, is accepted and ignored for one release. `/readyz` reads `gateway_sessions`, so
  a DSN without the gateway's tables is not ready rather than serving empty lists.
- **Not Clearance's reads.** Evidence, receipts, provenance, the deployment gate's changes and policy, the decision
  policy and its explainer are clearance-api's (the Go `clearance api`); here they are 404, and `/v1/contract` names
  clearance-api (`CLEARANCE_API_URL`) as `clearance`. A caller (Tower's Clearance backend) routes each operation by the
  two contracts.
- **Who sees what.** An auditor (`GATEWAY_API_AUDITORS`) everything; a person their own sessions and their teams'
  agents' (the Backstage directory), an app's events when they own it; a delegate (`CLEARANCE_DELEGATES`, Tower) what
  the person named in `X-Hangar-On-Behalf-Of` may; a notifier (`GATEWAY_API_NOTIFIERS`) the inbox feed only.
- **Cutover (done, R-4, 2026-11-03).** autopilot-api serves the runtime's reads and the Go `clearance api` serves
  Clearance's. The pre-R-3c `CLEARANCE_API_*` names (`CLEARANCE_API_DSN` and the rest) no longer configure it.

## What an install needs
- **Clearance**, a standalone deployment (https://github.com/jfillman/clearance: `clearance core`, its image and
  chart). It needs no Airframe and no Glidepath; on Hangar it runs with the Hangar pack (`CLEARANCE_PACKS=hangar`),
  and the gateway (`autopilot.gateway.hangar_gateway`, deployed as `autopilot-gateway`, above) calls it.
  `tools_list` shows exactly the tools the deployment was built with. Its write permission is one namespaced Role on `agentruns` in
  `autopilot-runs` (used by `src/autopilot/kube_runs.py`: create and delete, nothing else). For the `airframe.*` tools it reads, cluster-wide and read-only, `catalog.hangar.io/*` and
  `argoproj.io` applications and rollouts (ClusterRole `clearance-airframe-reader`, gitops-hangar-dev#40), and
  nothing else: no Secrets, ConfigMaps or pods. The adapter (`src/autopilot/airframe_api.py`) enforces the same
  allowlist itself, GET only, so an RBAC mistake alone still reads nothing more. It needs the API server URL, the
  CA file and a projected ServiceAccount token file (re-read every call). `airframe.validate` needs airframe's
  `airframe-validate`: the script from an airframe checkout with python3, pyyaml, jsonschema and helm, or the
  `tools/validate.Containerfile` image run with the content directory mounted read-only.
- **Live check**: `tools/airframe_smoke.py` drives the tools through MCP against a cluster with a 10-minute token
  (`kubectl create token clearance -n autopilot-system --duration=10m`) and compares them with `kubectl get`.
- **provider-kubernetes RBAC** for the objects an `AgentRun` renders (Namespace, ResourceQuota, ServiceAccount,
  two NetworkPolicies, the agent-sandbox Sandbox): the cluster repo's `55-autopilot/agentrun-grants/` (apron
  template), shipped only with the ValidatingAdmissionPolicy that confines it to run namespaces (decision D15).
- **Expiry** cannot come from Crossplane re-invocation: on Crossplane 2.3 a function's response TTL is only honoured
  with the beta realtime-compositions flag, and the poll floor is one minute. Use the Sandbox `shutdownTime`
  (Kubernetes SIG Agent Sandbox v1.0.x, measured to expire within a second) or the Job's `activeDeadlineSeconds`.
- **A separate GitHub App** for agents, so their API budget cannot starve CI.

## Reading
The measured answers to the open questions (U1 to U12), the decision log and the M0 to M5 plan are in Hangar's
[`docs/autopilot/roadmap.md`](https://github.com/jfillman/hangar/blob/main/docs/autopilot/roadmap.md) and
[`decisions.md`](https://github.com/jfillman/hangar/blob/main/docs/autopilot/decisions.md).
