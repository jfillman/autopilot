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
| Glidepath cicd.yaml changes | `clearance/packs/hangar/` (`glidepath.cicd.validate`, `glidepath.cicd.propose`, a PR tool: `provenance.PR_TOOLS`), `autopilot/cicd_propose.py`, `autopilot/glidepath_api.py` (`validate_cicd`), `agents/coding-agent.yaml` | validate asks glidepath-api what a merge patch would mean (the cicd-validate check's verdict, every field it touches, the resulting file); propose asks the same and is refused with the GP rule when invalid, else opens one commit of cicd.yaml on the app's own repo as the agents' App, the verdict in the PR body, a human merges; its receipt names repo, PR, commit and path, so the agent gates vouch for it like `repo.pr.open`. A `repo.pr.open` that carries the app's root `cicd.yaml` gets the same verdict first (`cicd_propose.check_files`) |
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
| `AGENT_XRS_ENABLED` | = `GITHUB_PR_ENABLED` | `1`: Agent XR pins become definitions (`agent_xrs.py`, Autopilot M4): every `AGENT_XRS_INTERVAL` s (60) the gateway lists `Agent` XRs (airframe `xrds/agent.yaml`), fetches each pinned definition file once through the agents' App (the broker's repository rule applies) and loads it beside the bundled `agents/` (clearance `definitions.py`: a bundled name is never shadowed; every change is `definition.*` evidence once). Needs `GITHUB_PR_ENABLED=1` |
| `CLEARANCE_VERIFIERS` | (none) | comma-separated principal globs that may call `POST /v1/provenance` (the agent-identity and agent-scope gates' pipelines, e.g. `app-*-cicd/pipeline-runner`); with none, every caller is refused 403 |
| `AGENT_LOGINS` | `hangar-autopilot-agents[bot]` | the GitHub logins agents open PRs as; such a PR with no Clearance record fails agent-identity (AI002) |
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
| Argo CD syncs (`ARGO_SYNC_ENABLED`) | `argo.sync.lower` | Role `autopilot-clearance-argo-sync` in `argocd-apps` (get, patch applications) and the ValidatingAdmissionPolicy of the same name: for this identity an Application update must be in a `-lower` project, change only `operation`, start a sync and never override its source. The adapter checks first that the Application's own project is the `project` argument and ends in `-lower` and that nothing is in flight, then sends one merge PATCH with the resourceVersion it read (`argo_sync.py`). The operation's `initiatedBy` is `clearance:<agent>`, its `info` the task, session and principal |
| Backstage catalog (`BACKSTAGE_URL`) | `catalog.read` | a projected service-account token for audience `backstage` at `BACKSTAGE_TOKEN_FILE`, which Backstage verifies against this cluster's public issuer (`backend.auth.externalAccess` type `jwks`, `accessRestrictions` catalog `catalog.entity.read`); no stored secret. A full-text search, or a Backstage filter when the query has `=`; at most 25 entities, each a summary (ref, kind, title, description, type, owner, lifecycle, system), never annotations |
| Prometheus (`PROMETHEUS_URL`) | `metrics.query` | network reach to the query API. An instant query; at most 200 series returned (`truncated` says when more matched) |
| Loki (`LOKI_URL`) | `logs.query` | network reach to the query API. A range query over the last hour, newest first; at most 200 lines of at most 2000 characters. On hangar-dev the labels are the OpenTelemetry ones (`k8s_namespace_name`, `k8s_pod_name`, `service_name`) |
| Postgres (`CLEARANCE_DSN`) | everything, and `artifact.put`/`artifact.get` (clearance `PgArtifacts`: per task, 256 KiB each, 32 and 2 MiB per task, 7 days) | a database the gateway owns; it creates its tables and verifies the evidence chain from genesis at start. Losing the connection ends the process (exit 70): after a failed commit the sessions in memory are ahead of the database, and only a restart, which reloads them, puts the two back in step. The expiry sweep pings the database first, so this is noticed within one interval when no agent is calling |

| GitHub PRs (`GITHUB_PR_ENABLED`) | `repo.pr.open`, `repo.pr.open_upper`, `repo.file.get` | a projected token for audience `agent-installation-token` at `AGENT_TOKEN_FILE`, and `autopilot-system/clearance` in the broker's `agentToken.callers`. Per PR the broker mints a token for the agents' App on that one repo (contents and pull_requests write, never workflows), only for an onboarded app's own repo (`<app>`, `gitops-<app>` or `gitops-infra-<app>`; `repo.pr.open_upper` is T2 and propose-only: a human merges it). The PR is built with the Git data API (no clone), on a new branch only (an existing one is refused, never forced); its commit trailer and body carry the agent, task, session and run. `repo.file.get` reads a file (or lists a directory) at a ref with the same per-call token, GET only; over 1 MiB or binary is named, not returned |
| Model proxy (`MODEL_KEY_FILE`) | a run's model calls | the provider key file, egress to `api.anthropic.com`, and the Service `model-proxy` on `MODEL_PORT` selecting this pod. A run sends its model-proxy token as `x-api-key` and its session as `x-hangar-session`; Clearance checks the session's model allowlist, network mode, breaker and token budget, forwards the body only, and charges the actual usage to the session in the same evidence chain (clearance `server.create_model_app`) |
| Event triggers (`EVENTS_ENABLED`) | Glidepath event triggers | glidepath-api's reader role (it already lists this identity). One run per (agent, event id) while it is live, a storm brake per agent (`maxPerHour`, default 6), the event summarised into the run's input beside the definition's `runtime.instructions` (`event_bridge.py`) |

A launched run's pod authenticates as `agent-<run>/run` (function-agentrun's run ServiceAccount). Clearance lets that
caller act as the run's session, and only that one (`AgentRunBackend.principal`); its records name the run.

A query Prometheus or Loki rejects (a parse error, a timeout, too many samples) comes back as data with `error` and
`message`, so the agent can fix it; any other failure is a "backend error".

`human.request` and `human.request.get` are Clearance's own inbox (clearance `inbox.py`), durable on Postgres
(`PgInbox`). A request is answered by the session's owner or a member of the definition's `owner` group, whose
membership is the person's Tower catalog `memberOf` (`reads.BackstageDirectory`, the catalog.read token), either signed in
themselves or through a `CLEARANCE_DELEGATES` principal (Tower) at `GET/POST /v1/human/requests`. Every definition in
`agents/` names its `owner` (team-platform; the Skyport ones team-skyport).

Everything else (`xr.request`, `app.api.get`, `chat.*`) has no handler: a call is audited and refused with "no backend for this tool in this
deployment", charged nothing. `tools_list` still lists them, with their policy.

## Running the read API

`autopilot-gateway api` is clearance-api (clearance `docs/clearance-api.md`): Clearance's read API as a separate
Deployment from the same image. It reads the gateway's tables through its own read-only DSN, never the writer lock,
so Tower's reads never queue behind agents and it can run several replicas. Writes (answering a human request) stay
on the gateway.

| Variable | Default | |
|---|---|---|
| `CLEARANCE_API_DSN` | (required) | the read-only role's DSN (connections are forced read-only, 5 s statement timeout) |
| `CLEARANCE_API_POOL` | `5` | connections per replica |
| `CLEARANCE_API_AUDITORS` | unset | principal globs that read everything (and the chain check) |
| `CLEARANCE_API_NOTIFIERS` | unset | principal globs that read the human-inbox feed only (Tower's poller) |
| `CLEARANCE_DELEGATES` | unset | as for the gateway: may read for the person named in `X-Hangar-On-Behalf-Of` |
| `CLEARANCE_GATEWAY_URL` | unset | where writes go, named in `/v1/contract` |
| `CLEARANCE_API_DEFINITIONS_INTERVAL` | `30` | how often the cluster-pinned definitions are re-read from the gateway's store (`clearance_definitions`); this process fetches nothing. `GET /v1/agents` carries each agent's `source` and the `rejected` pins |

Auth (`CLEARANCE_AUDIENCE`, `CLEARANCE_OIDC_*`, `TOWER_*`) and the catalog (`BACKSTAGE_URL`, `BACKSTAGE_TOKEN_FILE`)
are the gateway's variables. A person sees their own sessions, their teams' agents' sessions (`BackstageDirectory`),
and the evidence on apps they own (`BackstageOwnership`: Components whose `spec.owner` is them or their team).

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
above, so any fits any definition. The gateway hands the catalog to Clearance (clearance `runtimes.py`): an
**available `runtime`** image is what a definition's `runtime.image` may pin (with `RUNTIME_IMAGES_EXTRA` globs for
images Glidepath builds for a code agent's own repo); a **`base`** image is a `FROM` image for code agents, never run
as is; clearance-api lists all of it at `GET /v1/runtimes` for the New agent picker.

| Name | Kind | State |
|---|---|---|
| `generic` | runtime | available: this loop, `runtime-v0.1.2` |
| `base-python` | base | built from `runtime/base-python/Dockerfile` on a `runtime-base-python-v*` tag; available once its digest is pinned |
| `claude-agent-sdk` | runtime | planned: needs the model proxy to speak streaming Messages |
| `langgraph` | runtime | planned |

The `runtime-image` workflow builds each multi-arch from its tag. A code agent: `FROM` the base, add its code and an
entrypoint, `from hangar_agent import env, clearance, model` for the contract, and let its own Glidepath build publish
the image its definition pins.

## Signing in as a person

On hangar-dev, people sign in through people-dex (apron `55-autopilot/people-dex`), a Dex whose only connector is GitHub. This is Hangar D19's interim answer until an IdP issues ID-JAG. Clearance accepts its ID tokens for audience `clearance`, as `user:<GitHub login>`, and only for the logins in cluster.yaml `autopilot.people`. ID tokens last an hour, and no refresh token is issued.

```shell
python3 tools/hangar_login.py login        # opens the sign-in page; the token goes to the macOS Keychain
claude mcp add-json clearance '{"type":"http","url":"http://clearance.hangar-dev.local/mcp",
  "headersHelper":"python3 '"$PWD"'/tools/hangar_login.py headers"}'
```

`headers` never signs in: Claude Code gives a headersHelper 10 seconds. When the hour is up, a call fails 401, and you run `login` again.

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
