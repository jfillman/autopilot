# The Go gateway's environment, against the Python gateway's

Generated from `gateway/envtable.json` by `tools/gen_gateway_env_table.py` (CI checks it is current); do not edit by hand.
The table is held to the Python source by `tests/test_gateway_env_table.py` and to the Go source by
`gateway/cmd/autopilot-gateway/envtable_test.go`, which also fails the build when a variable the Python Deployment sets today
would be silently ignored by Go.

Columns: **Python** and **Go** are the processes that read it (`serve` the gateway, `api` the read API, `migrate` the index step);
**Set today** is where the Python Deployment (apron `templates/55-autopilot/gateway`) sets it.

## Counts

| Class | Variables |
|---|---|
| Same name, same meaning (Go may be stricter: see the note) | 69 |
| New in Go (the Python gateway has no such setting) | 28 |
| Refused at start by both gateways | 12 |
| Renamed AUTOPILOT_* at R-10a; the old name is refused by both | 13 |
| Not carried: Python reads it, Go does not | 2 |
| Set on the pod and read by no process | 2 |
| Total | 126 |

## Same name, same meaning (Go may be stricter: see the note)

| Variable | Python | Go | Set today | Note |
|---|---|---|---|---|
| `AGENT_TOKEN_FILE` | serve | serve | serve |  |
| `AGENT_XRS_ENABLED` | serve | serve | - |  |
| `AGENT_XRS_INTERVAL` | serve | serve | - |  |
| `AIRFRAME_CLUSTER` | serve | serve | serve |  |
| `AIRFRAME_ENABLED` | serve | serve | - |  |
| `AIRFRAME_TENANTS_REPO` | serve | serve | serve |  |
| `AIRFRAME_UPPER_CLUSTERS` | serve | serve | - | Python's default is hangar-prod. |
| `APP_API_SERVICES` | serve | serve,api | - |  |
| `ARGO_APP_NAMESPACES` | serve | serve,api | serve |  |
| `ARGO_ENABLED` | serve | serve | - |  |
| `ARGO_SYNC_ENABLED` | serve | serve | serve | Go needs ARGO_ENABLED (Python ignores it when Argo reads are off). |
| `AUTOPILOT_ADMINS` | serve | serve | serve |  |
| `AUTOPILOT_AGENTS_DIR` | serve,api | serve,api | - | Default /app/agents (the image carries them). |
| `AUTOPILOT_AUDIENCE` | serve,api | serve,api | serve,api |  |
| `AUTOPILOT_DELEGATES` | serve,api | serve,api | serve,api | Go refuses `*`, a leading wildcard and a wildcard namespace (stricter; Python takes any glob). |
| `AUTOPILOT_EXPIRE_SECONDS` | serve | serve | - |  |
| `AUTOPILOT_GATEWAY_URL` | api | api | api |  |
| `AUTOPILOT_GATEWAY_VERSION` | both | serve,api,migrate | - | Set by the image (the release tag). |
| `AUTOPILOT_ISSUER_URL` | serve | serve | - |  |
| `AUTOPILOT_OIDC_ALLOWED_USERS` | serve,api | serve,api | serve,api |  |
| `AUTOPILOT_OIDC_AUDIENCE` | serve,api | serve,api | serve,api |  |
| `AUTOPILOT_OIDC_ISSUER` | serve,api | serve,api | serve,api | Go checks the issuer's key URL is https unless AUTOPILOT_JWKS_ALLOW_HTTP=1. |
| `AUTOPILOT_OIDC_JWKS_URL` | serve,api | serve,api | serve,api |  |
| `AUTOPILOT_OIDC_USER_CLAIM` | serve,api | serve,api | serve,api |  |
| `AUTOPILOT_RESOURCE_URL` | serve | serve | serve |  |
| `AUTOPILOT_RUN_AUDIENCES` | serve,api | serve,api | - | Default autopilot. |
| `BACKSTAGE_TOKEN_FILE` | serve,api | serve,api | serve,api |  |
| `BACKSTAGE_URL` | serve,api | serve,api | serve,api | Go needs https unless BACKSTAGE_ALLOW_HTTP=1. |
| `BROKER_URL` | serve | serve | - | Python's default names a lab Service; Go has no default and needs https unless BROKER_ALLOW_HTTP=1. |
| `CLEARANCE_API_URL` | api | serve,api | api | Named in /v1/contract. Go's serve also reads it (optional, Go-only): a Preflight score reads the paths a run's writes touched from clearance-api's outcome records with the caller's bearer token, and is refused (503) while it is unset. serve needs https unless CLEARANCE_API_ALLOW_HTTP=1; api only names it in /v1/contract and sends no token, so it keeps accepting http. |
| `CLEARANCE_CORE_TIMEOUT` | serve | serve | - | Seconds per attempt, 0.05 to 30. |
| `CLEARANCE_CORE_TOKEN_FILE` | serve | serve | - | Default /var/run/secrets/tokens/clearance. |
| `CLEARANCE_CORE_URL` | serve | serve | serve | Plain http is accepted for the core (it is a Service on the cluster network). |
| `EVENTS_ENABLED` | serve | serve | serve |  |
| `GATEWAY_API_AUDITORS` | api | api | api | Go refuses a catch-all glob. |
| `GATEWAY_API_DEFINITIONS_INTERVAL` | api | api | - |  |
| `GATEWAY_API_DSN` | api | api | api | Go refuses to start on a role that could write (CheckRole). |
| `GATEWAY_API_NOTIFIERS` | api | api | api | Go refuses a catch-all glob. |
| `GATEWAY_API_POOL` | api | api | - |  |
| `GATEWAY_API_TABLES` | api | api | - | Retired at R-4 in both: only `gateway` is accepted. |
| `GATEWAY_CONFORMANCE_CONTROL` | serve | serve | - | Test-only control interface; off unless set. |
| `GATEWAY_DSN` | serve | serve,migrate | serve | The writer's DSN (the database owner's). |
| `GATEWAY_OUTAGE_ALLOW_READS` | serve | serve | - |  |
| `GITHUB_PR_ENABLED` | serve | serve | serve | Go also needs BROKER_URL, GITHUB_OWNERS and (it turns the Agent XR loader on) AGENT_XRS_NAMESPACES. |
| `GLIDEPATH_API_URL` | serve | serve | serve | Go needs https unless GLIDEPATH_API_ALLOW_HTTP=1. |
| `GLIDEPATH_ON_BEHALF_OF` | serve | serve | serve |  |
| `GLIDEPATH_TOKEN_FILE` | serve | serve | - |  |
| `LOG_LEVEL` | both | serve,api,migrate | - |  |
| `LOKI_URL` | serve | serve | serve |  |
| `METRICS_PORT` | serve | serve | - | The api serves no metrics (Python's does not either). |
| `MODEL_KEY_FILE` | serve | serve | serve |  |
| `MODEL_PORT` | serve | serve | - |  |
| `MODEL_ROUTES` | serve | serve | serve |  |
| `MODEL_TOKEN_AUDIENCE` | serve | serve | - |  |
| `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` | serve,api | serve,api | serve,api | Go needs https unless OTEL_EXPORTER_OTLP_LOGS_ALLOW_HTTP=1. |
| `OTEL_RESOURCE_ATTRIBUTES` | serve,api | serve,api | serve,api |  |
| `OTEL_SERVICE_NAME` | serve,api | serve,api | serve,api |  |
| `PORT` | both | serve,api | - |  |
| `PREFLIGHT_CASES_DIR` | serve | serve | - |  |
| `PROMETHEUS_URL` | serve | serve | serve |  |
| `RECONCILE_SECONDS` | serve | serve | - |  |
| `RECONCILE_WINDOW_HOURS` | serve | serve | - | Go allows at most 720 (its ledger's retention); Python allows 744. |
| `RUNTIME_IMAGES_EXTRA` | serve,api | serve,api | serve,api |  |
| `SCHEDULES_ENABLED` | serve | serve | serve |  |
| `TOWER_CLUSTER` | serve,api | serve,api | serve,api |  |
| `TOWER_ISSUER` | serve,api | serve,api | serve,api |  |
| `TOWER_JWKS_URL` | serve,api | serve,api | - |  |
| `TOWER_SERVICE_ACCOUNTS` | serve,api | serve,api | serve,api |  |
| `XR_REQUEST_ENABLED` | serve | serve | - |  |

## New in Go (the Python gateway has no such setting)

| Variable | Python | Go | Set today | Note |
|---|---|---|---|---|
| `AGENT_XRS_MAX_TIER` | - | serve | - | T0, T1 or T2 (default T2). |
| `AGENT_XRS_NAMES` | - | serve | - | Agents whose names do not start with their app: name=namespace. |
| `AGENT_XRS_NAMESPACES` | - | serve | - | The namespaces whose Agent XRs may define agents. Required when the loader is on (Python reads every namespace). |
| `AGENT_XRS_TOOLS` | - | serve | - | Tools a pin may hold beyond the default set. |
| `AIRFRAME_OWNER` | - | serve | - | Owner of the tenants repository and the apps' repositories (Python assumes one lab user). One of GITHUB_OWNERS. |
| `AUTOPILOT_JWKS_ALLOW_HTTP` | - | serve,api | - | Plain http to an OIDC or Tower key set (default off). |
| `AUTOPILOT_JWKS_CA_FILE` | - | serve,api | - | PEM file of extra CA certificates the OIDC and Tower key-set fetches trust, added to the system roots (default none): an in-cluster issuer serving a certificate from the cluster's own CA. |
| `BACKSTAGE_ALLOW_HTTP` | - | serve,api | - | Plain http to the catalog (default off). |
| `BROKER_ALLOW_HTTP` | - | serve | - | Plain http to the installation-token broker (default off). |
| `CLEARANCE_API_ALLOW_HTTP` | - | serve | - | Plain http to clearance-api, where a Preflight score reads the paths a run touched with the caller's token (default off). Not read by api. |
| `GATEWAY_ALLOW_MISSING_INDEXES` | - | serve,api | - | Start although the read API's and the outbox guard's seven indexes are missing (default off: serve and api refuse and name `autopilot-gateway migrate`). |
| `GATEWAY_CALL_TIMEOUT_SECONDS` | - | serve | - |  |
| `GATEWAY_PG_CONNS` | - | serve | - |  |
| `GITHUB_OWNERS` | - | serve | - | The GitHub owners the pull requests go to (no argument chooses one). Required with GITHUB_PR_ENABLED. |
| `GLIDEPATH_API_ALLOW_HTTP` | - | serve | - | Plain http to glidepath-api (default off). |
| `MIGRATE_LOCK_TIMEOUT_SECONDS` | - | migrate | - | `migrate` only (default 5). |
| `MIGRATE_STATEMENT_TIMEOUT_SECONDS` | - | migrate | - | `migrate` only (default 900). |
| `MODEL_TIMEOUT_SECONDS` | - | serve | - |  |
| `OTEL_EXPORTER_OTLP_LOGS_ALLOW_HTTP` | - | serve,api | - | Plain http to the log collector (default off). |
| `XR_ALLOW_EMPTY_ON_DELETE` | - | serve | - | What xr.request may ask for (Python bounds nothing; each has a safe default). |
| `XR_ALLOW_UNPROTECTED_DB` | - | serve | - | What xr.request may ask for (Python bounds nothing; each has a safe default). |
| `XR_DB_SIZES` | - | serve | - | What xr.request may ask for (Python bounds nothing; each has a safe default). |
| `XR_MAX_CONSUMERS` | - | serve | - | What xr.request may ask for (Python bounds nothing; each has a safe default). |
| `XR_MAX_DB_STORAGE_GIB` | - | serve | - | What xr.request may ask for (Python bounds nothing; each has a safe default). |
| `XR_REGIONS` | - | serve | - | What xr.request may ask for (Python bounds nothing; each has a safe default). |
| `XR_SLACK_CHANNELS` | - | serve | - | What xr.request may ask for (Python bounds nothing; each has a safe default). |
| `XR_SLO_LABELS` | - | serve | - | What xr.request may ask for (Python bounds nothing; each has a safe default). |
| `XR_VPC_CIDRS` | - | serve | - | What xr.request may ask for (Python bounds nothing; each has a safe default). |

## Refused at start by both gateways

| Variable | Python | Go | Set today | Note |
|---|---|---|---|---|
| `AGENT_LOGINS` | both | serve,api | - | The Clearance core's setting (Python CORE_ONLY): both refuse it. |
| `AUTOPILOT_EPHEMERAL` | serve | serve | - | An in-memory store, local runs only: Go refuses it (no memory store). Python's api sets it for its own config parse and ignores the environment's. |
| `CLEARANCE_DSN` | both | serve,api | - | The Clearance core's setting (Python CORE_ONLY): both refuse it. |
| `CLEARANCE_EPHEMERAL` | serve | serve | - | The old name of the above. |
| `CLEARANCE_GITHUB_APP_COMMENTS` | both | serve,api | - | The Clearance core's setting (Python CORE_ONLY): both refuse it. |
| `CLEARANCE_GITHUB_APP_ID` | both | serve,api | - | The Clearance core's setting (Python CORE_ONLY): both refuse it. |
| `CLEARANCE_GITHUB_APP_KEY_FILE` | both | serve,api | - | The Clearance core's setting (Python CORE_ONLY): both refuse it. |
| `CLEARANCE_GITHUB_APP_WEBHOOK_SECRET_FILE` | both | serve,api | - | The Clearance core's setting (Python CORE_ONLY): both refuse it. |
| `CLEARANCE_GITHUB_OIDC_ALLOWED` | both | serve,api | - | The Clearance core's setting (Python CORE_ONLY): both refuse it. |
| `CLEARANCE_GITHUB_OIDC_AUDIENCE` | both | serve,api | - | The Clearance core's setting (Python CORE_ONLY): both refuse it. |
| `CLEARANCE_VERIFIERS` | both | serve,api | - | The Clearance core's setting (Python CORE_ONLY): both refuse it. |
| `EVENTS_RABBITMQ_ENABLED` | serve | serve | - | Designed, not built: both refuse it. |

## Renamed AUTOPILOT_* at R-10a; the old name is refused by both

| Variable | Python | Go | Set today | Note |
|---|---|---|---|---|
| `CLEARANCE_ADMINS` | both | serve,api,migrate | - | Renamed AUTOPILOT_* at R-10a; the old name is refused at start (R-10e) by both. |
| `CLEARANCE_AGENTS_DIR` | both | serve,api,migrate | - | Renamed AUTOPILOT_* at R-10a; the old name is refused at start (R-10e) by both. |
| `CLEARANCE_AUDIENCE` | both | serve,api,migrate | - | Renamed AUTOPILOT_* at R-10a; the old name is refused at start (R-10e) by both. |
| `CLEARANCE_DELEGATES` | both | serve,api,migrate | - | Renamed AUTOPILOT_* at R-10a; the old name is refused at start (R-10e) by both. |
| `CLEARANCE_EXPIRE_SECONDS` | both | serve,api,migrate | - | Renamed AUTOPILOT_* at R-10a; the old name is refused at start (R-10e) by both. |
| `CLEARANCE_GATEWAY_URL` | both | serve,api,migrate | - | Renamed AUTOPILOT_* at R-10a; the old name is refused at start (R-10e) by both. |
| `CLEARANCE_ISSUER_URL` | both | serve,api,migrate | - | Renamed AUTOPILOT_* at R-10a; the old name is refused at start (R-10e) by both. |
| `CLEARANCE_OIDC_ALLOWED_USERS` | both | serve,api,migrate | - | Renamed AUTOPILOT_* at R-10a; the old name is refused at start (R-10e) by both. |
| `CLEARANCE_OIDC_AUDIENCE` | both | serve,api,migrate | - | Renamed AUTOPILOT_* at R-10a; the old name is refused at start (R-10e) by both. |
| `CLEARANCE_OIDC_ISSUER` | both | serve,api,migrate | - | Renamed AUTOPILOT_* at R-10a; the old name is refused at start (R-10e) by both. |
| `CLEARANCE_OIDC_JWKS_URL` | both | serve,api,migrate | - | Renamed AUTOPILOT_* at R-10a; the old name is refused at start (R-10e) by both. |
| `CLEARANCE_OIDC_USER_CLAIM` | both | serve,api,migrate | - | Renamed AUTOPILOT_* at R-10a; the old name is refused at start (R-10e) by both. |
| `CLEARANCE_RESOURCE_URL` | both | serve,api,migrate | - | Renamed AUTOPILOT_* at R-10a; the old name is refused at start (R-10e) by both. |

## Not carried: Python reads it, Go does not

| Variable | Python | Go | Set today | Note |
|---|---|---|---|---|
| `CLEARANCE_CORE_TOKEN` | serve | - | - | A literal token for the local smoke tools (engine/core.py local_core): never a deployment. The Go gateway reads CLEARANCE_CORE_TOKEN_FILE only. |
| `CLEARANCE_TOKEN` | serve | - | - | The test MCP server's token provider (engine/server.py): never a deployment. |

## Set on the pod and read by no process

| Variable | Python | Go | Set today | Note |
|---|---|---|---|---|
| `POD_NAME` | - | - | both | Substituted by the kubelet into OTEL_RESOURCE_ATTRIBUTES ($(POD_NAME)); no process reads it. |
| `POD_NAMESPACE` | - | - | both | Substituted by the kubelet into OTEL_RESOURCE_ATTRIBUTES ($(POD_NAMESPACE)); no process reads it. |
