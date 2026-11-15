# Changelog

Autopilot's gateway and runtime images are tagged `gateway-v*`, `runtime-v*` and `runtime-<name>-v*`; this file
says what each gateway tag adds. The manager cuts the tags.

## Unreleased

### Added (A-9 S4: the agent definition set in the Go gateway)
- The Go gateway (`gateway/`, still off by default and not deployed) owns the agent definition set: it reads the bundled `agents/` and the Agent XRs,
  validates every definition against the JSON schema (a Go validator over the embedded schema, in Python's error order and words), decides each pin
  (`from_pin`: name, owner, workload identity, no bundled name), keeps the live set (replaced whole while serving: `Gateway.SetDefs`) and publishes the
  whole set to the policy core with `PUT /v1/definitions` through the Clearance Go client (v0.44.0): pins, rejections and the bundled definitions,
  atomic from the gateway's side, idempotent, never shrunk by a read error, logged with counts and a digest. `AGENT_XRS_ENABLED` and
  `AGENT_XRS_INTERVAL` are carried; a candidate's Preflight `document` is validated by the same schema (it answered 501 before).
- Differences from Python, each pinned in `gateway/internal/definitions/testdata/definitions.json` with a `go` and a `why`
  (`tools/gen_gateway_definitions.py` drives the Python loader over 301 definition files and 9 scenarios of its poll, 31 steps, 25 publish
  requests): YAML is read strictly (a repeated key, an anchor or alias, a merge key, a tag, `yes`, `0755`, `1:30`, `1e3`, a date, a control character,
  a file over 64 KiB are refused); a pattern's `$` does not match before a final newline; `20.0` is not an integer; the loader reads only the namespaces
  in the new required `AGENT_XRS_NAMESPACES` (Python lists every namespace), a 404 on the list is an error, and an empty list is believed only when it is
  given twice, and a deleted pin is not kept alive by another pin's outage; a pin must name a full commit id (not a branch or a tag), may not name a tool the gateway lacks, a `*/*`, `acme/*` or upper-case repository pattern, an app pattern of only wildcards, or a tier
  above `AGENT_XRS_MAX_TIER` (default `T2`). `docs/admin/go-gateway.md` lists each, the sources and precedence of definitions, the bad-definition and
  failed-reload rules, the publish contract and the loader's crash points.
- The Kubernetes reader's path allowlist gains one entry: a list of one namespace's Agent XRs (GET, no name, no cluster-wide list).
  `pgstate.ReplaceDefinitions` replaces the stored set in one transaction.

### Added (A-9 S3: the human inbox, person controls and triggers in the Go gateway)
- The Go gateway (`gateway/`, still off by default and not deployed) carries the human inbox (`human.request`, the pending list,
  answering, expiry), the person controls (freeze, end, hold, release) and a person's run behind the `/v1` routes, with Preflight's
  cases and scoring, as the Python gateway does: only a person acts (a user, or a listed delegate naming one), never a run,
  and each control is an evidence record. The trigger scheduler, the event bridge (the stream, the catch-up from the saved cursor
  with `Last-Event-ID`, the storm brake, the history) and the reconciliation sweep run when `SCHEDULES_ENABLED` and `EVENTS_ENABLED`
  are set, over a ledger in the gateway database that makes a trigger fire once across restarts, replays and a crash between the
  claim and the session. `serve` no longer refuses `EVENTS_ENABLED`; it reads `AUTOPILOT_DELEGATES`, `AUTOPILOT_ADMINS`,
  `RECONCILE_SECONDS`, `RECONCILE_WINDOW_HOURS`, `RUNTIME_IMAGES_EXTRA` and `PREFLIGHT_CASES_DIR`. The replay now takes the 31
  triggered `session.open` lines. A candidate definition `document` is refused (501) until the definition schema is carried (S4).
- Beyond Python, on purpose: the gateway checks the grant the core makes for a trigger or a person's run, authenticates before it
  answers anything, makes a hold cover the open and the launch without ever waiting for the core, rate limits holds and runs, bounds the event bridge's client (no
  redirect, capped answers and lines, bounded backoff) and its topic matcher, repairs a session a trigger opened and never launched,
  and closes a session whose launch failed. `docs/admin/go-gateway.md` lists each difference, who may do what and the trigger crash points.
- `tools/gen_gateway_registry.py` also writes fixtures for the inbox, the controls, the rows they write and the triggers (matcher, keys,
  the bridge and the scheduler driven over a fake gateway), generated from the Python code.

- Round-6 review fixes of S3 (Go gateway only): an outage is no longer counted as poison (only a panic is; a hold store or a backend that is away for hours decides nothing and deletes nothing), a poison event whose run is recorded launched is decided `launched` and its run left alone, and the bound has a stated window (8 failures, each within an hour of the one before, the first and last 10 minutes apart; a third column `last_failed_at`). The saves of a session wait with their context (a hung save cannot block a person's end forever). The crash harness gained the faults it was missing (a person's end or freeze inside each save, the core unreachable, a delete that leaves the run, a 409, poison, a hold store down for 30 minutes, a check at every decision) and two real-goroutine `-race` tests (two deliveries of one key, an end racing the launch's save); ten mutations of the code were run against it and each is killed.
- Round-5 review fixes of S3 (Go gateway only): the crash-window test now drives the real code over a fault-injecting store, ledger and a
  stateful run backend (every durable call crashed at, for a plain launch, a hold, an end, a freeze and each class of backend answer,
  through the stream and through the catch-up). It found, and these fix: a run that outlived its `run_removed` marker (a takeover now
  closes the session, deletes the run and confirms it gone before deciding); a hold not read before a resumed create; create errors
  treated as final (a 409 on the session's own name is the run, a timeout or 5xx is resumable and deletes nothing, only a 4xx releases
  the name); a session a person ended before it was tied to its ledger row being followed by a fresh one; a stale save overwriting a later
  one (saves of a session are ordered); a holds reload overlapping a hold write (one lock with context-aware waiters, store calls bounded);
  a poison event blocking the cursor forever (decided `launch-failed`/`CT-POISON` after 8 attempts over 10 minutes, counted on the row, run
  resolved first). Go-only: two columns on the trigger ledger (`failures`, `first_failed_at`), with defaults. The rollback note is in
  `docs/admin/go-gateway.md`.
- Round-4 review fixes of S3 (Go gateway only): a trigger's takeover decides only from facts recorded durably when they happened, never from
  the clock or a missing field: a person's end is written with the close (who, when, which control), a session closed with no reason is
  never launched, the run's name is saved before the run is made and "created" right after, a run a hold took away is recorded as removed
  (with the reason) before the row is decided, and a run backend that cannot be asked leaves the trigger resumable instead of closing the
  session of a run that may exist. The holds copy is loaded at start and every triggered open and launch fails closed while it is not.
  Difference from Python: a session row carries three Go-only keys (`launched_at`, `close_reason`, `run_removed`), omitted when empty;
  Python ignores them and drops them at its next save of the session. The doc's crash-point table is written from the code and walked by a test.
- Review fixes of S3 (Go gateway only): a trigger records the session it opened in its ledger row before it launches, so a stale claim can
  never launch a second run; a trigger dedupes only onto a session it opened; a frozen session and a held agent start no run; freeze and end
  answer one 404 for an unknown and a foreign session; delegates are named by where they run (no `*`, no wildcard namespace, never a run, a
  header with one clean `user:` name); every `/v1` route checks the token before the body; `RECONCILE_WINDOW_HOURS` is at most 696;
  `GLIDEPATH_API_URL` is https unless `GLIDEPATH_API_ALLOW_HTTP=1`; a child spawned while its parent is being closed is closed too.

- Second review round (Go gateway only): a claim that is still `claimed` is taken over at once and resumed (never answered as a duplicate), so a
  trigger is neither dropped nor fired twice in any crash window; a taken-over session that names a run that was never made gets its run; a
  session records when its run was made (`launched_at`); an outcome that could not be written is written as it was; a hold is kept in memory
  so a stuck database cannot stall opens and launches; a workload definition with no service account is refused as a trigger.

### Changed (A-9 S3 hardening of S2: a candidate's image and a run's pod)
- A Preflight candidate's runtime image must end in `@sha256:` and 64 lowercase hex digits, an empty allow-list refuses every
  candidate (the Python gateway allows any registry) and a glob matches the whole image, so a bare prefix no longer matches across a
  path boundary (Go gateway only).
- A caller acts as a run's session only with a token bound to the run's pod: `auth.Workload` carries the token review's
  `authentication.kubernetes.io/pod-name` and the run's pod must be the composition's Sandbox name (`agent`). A token minted for the
  bare service account is that service account and nothing more. A token without a pod binding still authenticates (Go gateway only).

### Added (A-9 S5b: the Glidepath and repository tools in the Go gateway)
- The Go gateway (`gateway/`, still off by default and not deployed) now has handlers for every `glidepath.*` tool (reads, the
  live watch, `run.rerun`, `run.cancel`, `promote`, `rollback`, `cicd.validate`, `cicd.propose`), `repo.file.get`, `repo.pr.open`,
  `repo.pr.open_upper` (with the `cicd.yaml` check) and `argo.sync.lower`, each the Python behaviour for the same inputs: the same
  arguments, result shapes and clipping, refusals, `GitHubError`, `GlidepathError` and `ArgoSyncError` text and class, and the same
  receipt on the outcome. `GITHUB_PR_ENABLED`, `GLIDEPATH_API_URL` and `ARGO_SYNC_ENABLED` are no longer refused at start.
  `ensure_pr` and `find_pr` (for `airframe.apply`, S5c) are built and tested but no tool calls them yet.
- Decided again by the gateway, from the session's grant and the agent's definition, before any request: the repository
  allowlist, the glidepath app allowlist, protected paths (in any case), a lower tool on a release repository, the tool's grant and tier,
  and the owners the deployment names. The GitHub requests the gateway can send are a closed list of nine (GET and POST only: no
  merge, review, delete, protection change or move of a ref), the glidepath-api and Kubernetes ones too; branch names, paths, titles and
  every glidepath argument are validated before a request exists (fuzz targets with independent oracles for each builder).
- Credentials are the gateway's projected tokens, re-read per call, and one installation token per call from Glidepath's broker, never
  kept; an upstream that echoes a credential cannot put it in an error; no redirect is followed.
- A write whose answer is lost says the write may have been made, and a pull request is looked for once before anything is said;
  a replayed call opens no second pull request, starts no second run (the idempotency keys) and asks for no second sync.
- Differences from Python, each pinned in the fixtures with a `why` (`docs/admin/go-gateway.md`): `BROKER_URL` has no default
  (Python's names a lab Service), `GITHUB_OWNERS` (Go only) names the owners, `GITHUB_PR_ENABLED` needs `AGENT_XRS_ENABLED=0` and
  `XR_REQUEST_ENABLED=0`, `ARGO_SYNC_ENABLED` needs `ARGO_ENABLED`, stricter names, paths and titles, an `@mention` in text that
  goes to a pull request is defanged, a 120 s watch is cut to the call's deadline, and a read answered 429 or 5xx is tried again.
- After an independent security review: one lower-case canonical spelling of a repository for every check, request and receipt
  (`o/GITOPS-x` is a release repository); a file is never written where the base has a directory (the base's contents are read) or
  above a protected path; the base is the default branch and heads are under `agent/`; a resumed branch must carry this session's trailer;
  `GLIDEPATH_API_ALLOW_HTTP` and `BROKER_ALLOW_HTTP` (Go only, off) are needed for plain http, and a token under 12 characters is refused;
  pull request text neutralises mentions (also after a backtick), `#n` references, closing keywords, HTML comments, raw HTML and images;
  `argo.sync.lower` is scoped to the definition's apps. A ninth GitHub request (`GET /repos/{owner}/{repo}`, the default branch).
- `tools/gen_gateway_writes.py` (called by `tools/gen_gateway_registry.py`) writes the equivalence fixture (320 cases) by driving the
  Python handlers over canned upstream answers; the drift guard now watches `glidepath_api.py`, `glidepath_live.py`, `github_pr.py`,
  `argo_sync.py` and `cicd_propose.py`.

### Added (A-9 S5a: the read backends in the Go gateway)
- The Go gateway (`gateway/`, still off by default and not deployed) now has handlers for `catalog.read` (`BACKSTAGE_URL`,
  `BACKSTAGE_TOKEN_FILE`), `argo.app.get` (`ARGO_ENABLED`, `ARGO_APP_NAMESPACES`, through the Kubernetes API), `app.api.get`
  (`APP_API_SERVICES`) and `artifact.put` and `artifact.get` (the gateway's own database), each the Python behaviour for the same
  inputs: arguments, result shape and clipping, refusals, `ReadError` text and class. `BACKSTAGE_URL` and `APP_API_SERVICES` are no
  longer refused at start; a bad value of any of the five settings stops it.
- Stricter than Python, on purpose, each pinned in tests and listed in `docs/admin/go-gateway.md`: a redirect is never followed
  (Python follows one for the catalog and the API server and sends the bearer token to the new host), an application API path is
  refused for an encoded control character, a malformed `%` escape, a dot segment before a `;` or a trailing newline, artifact content
  with a NUL is refused, a session with no task has no artifacts, and the catalog answer is capped at 4 MiB. `app.api.get` paths are checked on the decoded form (`/..%3b/`, overlong and look-alike encodings are refused), `argo.app.get` strips credentials from `repoURL`, refusals repeat at most 100 characters of an argument, and a put deletes only its own task's expired artifacts (the sweep purges the rest in batches). Every URL is a
  configured base plus a validated path, with a fuzz test for each builder.
- `tools/gen_gateway_reads.py` (called by `tools/gen_gateway_registry.py`) writes the equivalence fixtures by driving the Python
  adapters over canned upstream answers; the drift guard now also watches `airframe_api.py`, which `argo.app.get` reads through.

### Added (A-9 S2: runs, the run tree and run identity in the Go gateway)
- The Go gateway (`gateway/`, still off by default and not deployed) creates, reads and deletes AgentRun XRs, spawns and closes
  runs, launches and finishes them (`run_finish`), closes a session with its runs (`session_close`), keeps a run's answer
  and its warnings, and its expiry sweep deletes runs and writes the backstop outcomes, as the Python gateway does. A run's
  own tool and model calls are made as its session (`acting_as`), only for the run the session names and only from the workload
  identity that run has. The replay now takes the 35 `session.spawn` lines and the 8 acting lines it skipped.
- The Go gateway also checks the core's spawn grant against the parent before it creates a child (a child can never be wider
  than the parent has left), saves a run's name before it creates the run so a crash leaves a session the sweep cleans, and
  refuses a second launch of a session. `docs/admin/go-gateway.md` lists each difference from Python and each crash point.
- `tools/gen_gateway_registry.py` also writes fixtures for the limit rules, the AgentRun manifests, who acts as a session,
  the run results helpers, the operations that end a run and the Preflight clamp, generated from the Python code.

### Changed (A-9 S0: the Go gateway is level with the Python one, and a guard keeps it so)
- The Go gateway (`gateway/`, still off by default and not deployed) now does what the Python gateway does on the part it
  carries: the decision path asks the core for 1 s per attempt (`CLEARANCE_CORE_TIMEOUT`), workload tokens are checked for
  `AUTOPILOT_RUN_AUDIENCES` (default `autopilot`), a backend failure is logged and its evidence is on the outcome record, a
  write's effect carries `merge` and `needsPerson`, and a session's `signals` survive a load and a save. Its Clearance client
  is v0.44.0 (it was v0.31.0). `docs/admin/go-gateway.md` says what changed and what each later slice still has to port.
- Drift guard: `gateway/PORTED.json` (written by `tools/gen_gateway_ported.py`) holds the hash of each Python file the Go
  gateway ports; `tests/test_gateway_go.py` fails when one changes until the change is ported or confirmed not to touch what
  Go carries. Every Python file under `src/autopilot` must be classified, so a new module cannot be forgotten.

### Removed (R-10e: the `clearance` aliases retire)
- The vendored `agentrun_compose.py` matches airframe's R-10e `compose.py` (airframe#147): run pods get `AUTOPILOT_URL` and
  the `autopilot`-audience token only, and the gateway Service is `autopilot-gateway`.
- The `CLEARANCE_*` names of the settings the gateway owns (`CLEARANCE_EPHEMERAL`, `_AGENTS_DIR`, `_RESOURCE_URL`,
  `_OIDC_*`, `_AUDIENCE`, `_EXPIRE_SECONDS`, `_DELEGATES`, `_ADMINS`, `_GATEWAY_URL`, `_ISSUER_URL`) are no longer read as
  fallbacks. One that is still set is refused at start (Python and Go gateways), naming its `AUTOPILOT_*` replacement.
  `CLEARANCE_CORE_URL`, `CLEARANCE_CORE_TOKEN_FILE` and `CLEARANCE_API_URL` address Clearance and keep their names.
- The gateway accepts run-pod tokens for the `autopilot` audience only (`AUTOPILOT_RUN_AUDIENCES` defaults to `autopilot`;
  it was `autopilot,clearance`). The `clearance` audience is still `AUTOPILOT_AUDIENCE`'s default, which is the audience of
  Tower's foreign tokens and the core's, but in-cluster workloads are checked for `autopilot` alone. A cluster that still
  runs a pre-R-10b runtime image (runtime-v0.1.3 and older read `CLEARANCE_URL` and `clearance-token`) sets
  `AUTOPILOT_RUN_AUDIENCES=autopilot,clearance` until it is re-pinned.
- The runtime reads `AUTOPILOT_URL` and the `autopilot-token` file only (no `CLEARANCE_URL` / `clearance-token` fallback):
  the next runtime release. Needs Airframe's function-agentrun that no longer sets the old names, or one that sets both.

### Changed (A-11 pins)
- CI and `pyproject.toml` pin Clearance v0.44.0 (`propose` and bulk resolutions). `GLIDEPATH_REF` is `main` again (glidepath#242, contract v2, merged).

### Added (A-11, the `propose` scope: agents propose the Flight half, a person merges it)
- Owner decision 2026-11-12: when asked to configure an app, the agent configures every environment named, and a person approves
  anything that touches prod. Glidepath's contract version 2 adds the `propose` scope class; this repo's side (docs/admin in
  Glidepath's contract.md has the whole design):
  - **`airframe.apply` takes `proposeFlight: true`** and then opens the Flight half, the `cicd-flight` step (Flight environments and
    the release step in cicd.yaml, checked by glidepath-api as an agent's change) and the `gitops` step (the Flight values, merged into
    `<cluster>/<env>/values.yaml` once each exists, `waiting` before), as PRs marked for a person to merge: a `Hangar-Merge: person`
    commit trailer, a "Needs a person to merge" PR body, `merge: person` and `needsPerson: [paths]` in the result and the receipt,
    and step status `awaiting-merge`. A field that stays human in the Flight change (GP-SCOPE-001/002/003) makes that step a `humanStep`
    instead of failing the call. Without `proposeFlight` the Flight half is reported in `humanSteps`, as before.
  - **R025** (`policy/hangar/bundle.json`): a call with `proposeFlight: true` is refused unless the definition sets
    `profile.airframe.proposeFlight: true` (new in the agent-definition schema, byte-identical to Clearance's). `app-provisioner` sets it
    and its instructions pass it; so does `parachute` (owner decision 2026-11-12), whose `repos.allow` gains `jfillman/gitops-parachute` for the Flight values PR. Opening the PRs stays T1: opening changes nothing, the person's merge is the control; no T2 sibling.
  Both definitions' instructions (within 1500 chars) keep the person informed while a run waits: the `human.request` names the PR link, what merging it does and what comes next; a detected merge is said (which PR, by whom if reported) before `airframe.apply` is called again; the final report lists every PR as merged or open (`tests/test_airframe_apply.py` asserts it). `policy/hangar/bundle.json` is version 1.2.0.
  - `glidepath.cicd.propose`: a change glidepath-api reports as `needsPerson` (a Flight environment added or changed, a release
    step into one, a new environment's cluster) is opened with the same mark (`Hangar-Merge: person`, `merge: person`,
    `needsPerson: [cicd.yaml]`).
  - **R021** refuses a `propose` field on a file-writing tool (`repo.pr.open`), which cannot mark its PR: rule `GP-SCOPE-004`,
    hint naming `glidepath.cicd.propose` and `airframe.apply`. The bundle names Glidepath contract versions 1 and 2; version 2
    needs a Clearance with `propose` support (docscope) and the `merge`/`needsPerson` receipt fields.
  - The receipt lists of `glidepath.cicd.propose` and `airframe.apply` gain `merge` and `needsPerson`. Rollout: Clearance, then this
    (bundle, pin, definitions), then Glidepath.
### Changed
- Every agent and the runtime catalog pin `autopilot-agent-runtime` runtime-v0.1.7 (sha256:b5c9832f…): a clipped tool
  result now says it was clipped and how to narrow the call (A-11 b).

### Added (A-11, agent provisioning hardening)
- `airframe.capabilities {section: appspec}`: the AppSpec's top-level fields, required vs optional, how `config` and
  `overrides` map onto chart values, and one worked example (the parachute spec), derived from
  `schemas/appspec.schema.json` (under 12,000 characters, test-asserted). The default answer's `more` names it and now
  comes first (the default answer is ~19,000 characters, so the old trailing `more` was clipped away). The policy bundle's
  `section` allowlist admits `appspec` and the descriptions of `airframe.capabilities` and `airframe.plan` name it.
- Runtime: a clipped tool result ends with `[CLIPPED at N of M characters ... ]` and how to narrow (for
  `airframe.capabilities`: `section` or `kind`). Needs a new runtime tag and a pin bump in the agent definitions.
- `airframe.apply` merges into the files Glidepath's onboarding scaffolds (`glidepath/envs/*.yaml`, gitops `*/values.yaml`)
  instead of refusing `APPLY-EXISTS`: the plan's keys are deep-merged, the header comment and other keys kept. A key the
  plan sets that already holds a different value is `APPLY-CONFLICT` (naming the key paths).
- `airframe.apply` waits (`waiting`) while the app repo or `gitops-<app>` has an open `onboarding-resync` PR (the resync
  that CicdOnboarded=True does not wait for); `GitHubPRs.onboarding_prs`.
- Run results carry `warnings` (and the session outcome too): `airframe.plan` called more than 3 times, or
  `airframe.apply` answered ok but opened no PR (unless it ended at `agent-steps-merged`).
- `agents/parachute.yaml`: read `appspec` once, at most 3 plans, stop and ask (`human.request`) instead of proceeding with
  less than asked, wait for each merge in the same run, and end every stop with a resume prompt holding the AppSpec JSON.
  `ttlMinutes` 30 to 60 and `toolCalls` 60 to 80 so a run can wait for merges. `app-provisioner` is unchanged.

### Fixed
- The gateway publishes its bundled agents (`agents/`) to the Clearance core with the cluster pins (`PUT /v1/definitions`
  `bundled`: name, `AUTOPILOT_GATEWAY_VERSION`, document). Seen live on hangar-dev 2026-11-12: `parachute` opened
  gitops-hangar-dev-tenants#30 and `Clearance / agent-scope` failed AS004 "agent definition 'parachute' is not in this
  deployment", because since R-3 the core (no `CLEARANCE_AGENTS_DIR` on Hangar) had only the pins. A gateway without the
  Agent XR loader publishes the set at start and retries until the core takes it (`DefinitionsPublisher`). A core that
  predates the field refuses it (400); the gateway then publishes the pins without it and keeps running.
  Needs clearance v0.42.0 (`bundled`; the pins `pyproject.toml`, `CLEARANCE_REF`, `uv.lock` name it). Roll out the
  core first, then this gateway tag.
  The Go gateway (`gateway/`) publishes no definitions and stays on its v0.31.0 `go.mod` pin.

### Added
- `agents/app-provisioner.yaml`: a general provisioning agent beside `parachute` (owner decision 2026-11-11). Same tools
  (`airframe.apply` and the reads), T1, limits and runtime pin as parachute, but `airframe.apps: ["*"]` and
  `repos.allow: ["jfillman/*"]`, so it stands up any app from an AppSpec. This RAISES authority over parachute's single
  app; unchanged bounds: PRs only, no merge or push, Flight half and `gitops-<app>` human-only, R021 field scope.
  `examples/app-provisioner.appspec.yaml`; tests show its R023 scope admits another app and parachute's still refuses one.

### Renamed (R-10a)
- The settings the gateway owns are `AUTOPILOT_*` now: `CLEARANCE_EPHEMERAL`, `_AGENTS_DIR`, `_RESOURCE_URL`,
  `_OIDC_ISSUER`, `_OIDC_AUDIENCE`, `_OIDC_JWKS_URL`, `_OIDC_USER_CLAIM`, `_OIDC_ALLOWED_USERS`, `_AUDIENCE`,
  `_EXPIRE_SECONDS`, `_DELEGATES`, `_ADMINS`, `_GATEWAY_URL` (read API) and `_ISSUER_URL` (Go gateway) become
  `AUTOPILOT_<same suffix>`. The old names keep working for one release: the new name wins when both are set, and a
  legacy name logs one deprecation warning at start naming its replacement (`src/autopilot/settings.py`; the Go gateway
  has the same in `settings.go`). Unchanged: `CLEARANCE_CORE_URL`, `CLEARANCE_CORE_TOKEN_FILE`, `CLEARANCE_API_URL`
  (they address the Clearance product), the core-only refuse-list, and every audience value (R-10b). The images no longer
  set `CLEARANCE_AGENTS_DIR`: the default is `/app/agents`. Apron renames the Deployments' variables in the same release.

### Changed (R-10e)
- Every pinned runtime image moves to the re-released images that read `AUTOPILOT_URL` and the `autopilot`-audience token
  first (falling back to the old names): `autopilot-agent-runtime` runtime-v0.1.6, `autopilot-agent-base-python`
  runtime-base-python-v0.1.4, `autopilot-agent-langgraph` runtime-langgraph-v0.1.3 and `autopilot-agent-claude-agent-sdk`
  runtime-claude-agent-sdk-v0.1.3 (`agents/*.yaml`, `agents/skyport/*.yaml`, `runtime/catalog.yaml`, the gateway test
  fixture, the README table). Nothing was left pinned to an older runtime.

### Changed
- Run-pod contract, backward compatible for one release (R-10b; Airframe's function-agentrun renders both sides):
  - The gateway accepts run-pod tokens for the `autopilot` audience as well as `clearance`. `AUTOPILOT_RUN_AUDIENCES`
    (comma list; default `autopilot,` + `CLEARANCE_AUDIENCE`, so `autopilot,clearance`) names them, and the TokenReview
    asks for all of them and accepts a token carrying any.
  - The runtime reads `AUTOPILOT_URL` first and falls back to `CLEARANCE_URL`; it uses the `autopilot-token` file
    when the pod has one, else `clearance-token`. A pod rendered before the new function still runs.
- The gateway image carries only the platform's core definitions (`agents/*.yaml`). `agents/skyport/` was copied in but never loaded; it stays in the repository until those agents move to their own repo.

### Added
- `airframe.apply` (A-3; T1; [README](README.md), "airframe.apply and the parachute"): the planner's change set opened as
  pull requests by the agents' GitHub App, one per call, each with a receipt (repo, pr, commit, branch, paths) that
  Clearance's provenance reads (`pr_tools` in the Hangar bundle, now 1.1.0). It opens the tenants-repo PR, then the app
  repo's (cicd.yaml checked by glidepath-api as an agent's change), resumes from GitHub after an interruption (branches
  are fixed by the plan; nothing is kept in the gateway), never opens the Flight half (cicd.yaml's Flight fields,
  `gitops-<app>`) and has no way to merge. `agents/parachute.yaml` is its definition; `examples/parachute-bad.appspec.yaml`
  is the seeded bad run. R023 scopes it by `spec.app`.
- Preflight (A-6; [docs/admin/preflight.md](docs/admin/preflight.md)):
  - `RunRecord`s from real runs (`record_from_run`: a run's result plus its evidence), checks per verifier in the case
    files, and the local harness `autopilot preflight run --case <id> [--seeded-bad | --run file | --all]`. The six
    Skyport cases have a scripted good run and a seeded bad run in `preflight/runs/`; CI runs `--all`.
  - `POST /v1/agents/{name}/runs` takes a `document` (a PR's candidate definition) and a `budget`: a Preflight run as a
    clamped task named `pf-<agent>-<fingerprint>`. `GET /v1/preflight/cases?agent=` lists an agent's cases and `POST /v1/preflight/score` scores the runs against the agent's cases and
    records the definition's result (`pass`/`fail`, cases) on its run records while it is unchanged.
  - The image carries `preflight/cases` (`PREFLIGHT_CASES_DIR`).
- The Go AI gateway, first slice (A-9; [docs/admin/go-gateway.md](docs/admin/go-gateway.md)): `gateway/`, one binary with
  `serve`, `conformance-control` and `version` (`api` exits "not ported yet"), on the official MCP Go SDK, Clearance's Go
  client, a streaming model proxy that never buffers a response, and the same gateway database. Off by default and not
  deployed: the Python gateway stays the gateway and the reference. Proven equal by the R-3 replay corpus (identical
  evaluations, decisions, spends, grants and record fields), the conformance harness, and a load test (p99 gateway
  overhead under 1 ms at 200 concurrent sessions). `tools/gen_gateway_registry.py` generates what it takes from the
  Python gateway; `tests/test_gateway_go.py` keeps the two in step. What remains is listed in the doc.
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
