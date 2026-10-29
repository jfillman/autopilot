<div align="center">
  <img src="docs/brand/autopilot-tile.svg" width="88" height="88" alt="Autopilot mark" />
  <h1>Autopilot</h1>
  <p><i>Any AI agent workload, bounded and audited.</i></p>
</div>

Autopilot is the sixth [Hangar](https://github.com/jfillman/hangar) product: it runs AI agent workloads with authority
that is bounded and every action on the record. This repo holds **Clearance**, the policy, audit and session gateway
(`src/clearance/`), and the **Autopilot** code that plugs Hangar into it (`src/autopilot/`), plus agent definitions,
the Preflight evaluation cases, the AppSpec planner and drafts of the `AgentRun` composition.
Docs: [docs/](docs/index.md). Mark and brand files: [docs/brand/](docs/brand/README.md).

## Clearance

Policy, audit and session gateway for running AI agent workloads on Hangar. **v0.1, pre-release.**

Design, plan and diagrams: `../hangar/docs/autopilot/` (start at `README.md`). This is the `autopilot` repo (decision D8); Clearance is the package inside it.

## Layout: core, packs and Autopilot

Clearance is meant to move to its own repo and install where there is no Airframe and no Glidepath (D16). So the
package splits in three, and `tests/test_boundary.py` fails if the core imports anything Hangar's:

| Part | Where | What it owns |
|---|---|---|
| Clearance core | `src/clearance/` (all but `packs/hangar/`) | tiers and the core tool registry, path and field scope, narrow-only limits, CEL policy and the generic rules, sessions and the run tree, the gateway, the MCP surface, model-access decisions, audit. Imports nothing from Hangar; a Gateway with no packs is a working Clearance for the core tools |
| Packs | `src/clearance/pack.py`, `src/clearance/packs/` | a pack is the policy for a family of tools: ToolSpecs, CEL rules (each with an explicit `order` among the core rules, since first match wins), a context function the rules read, and optionally a decision for a rule that reports structured problems. `clearance.packs.hangar` holds the `glidepath.*` and `airframe.*` tools, R021 (Glidepath's cicd.yaml scope, vendored), R022 and R023 (per-agent app allowlists) |
| Autopilot | `src/autopilot/` | Hangar's tool implementations and everything around agents on Hangar: `glidepath_api.py`, `airframe_api.py`, `airframe_tools.py`, the planner `airframe_plan.py`, the AgentRun manifest `manifests.py`, `triggers.py`, `preflight.py`, the Hangar backends and fakes `backends.py`, the vendored airframe files `vendor/`, and `gateway.py`, which builds the Hangar Gateway: core + the Hangar pack + a handler per tool + AgentRun as the run backend |

A deployment names its parts when it builds the Gateway; nothing registers on import:

```python
Gateway(definitions, store, audit, auth, clock, packs=(HANGAR,), handlers=hangar_handlers(backends),
        runs=AgentRunBackend(backends.runs))       # what autopilot.gateway.hangar_gateway() does
```

## What is built and tested

`./.venv/bin/python -m pytest -q` runs 404 tests, no cluster needed (some skip without sibling airframe and glidepath checkouts under `HANGAR_TECH`, which CI clones).

| Piece | Where | What it does |
|---|---|---|
| Tool tiers and tripwires | `clearance/tiers.py` | T0 to T2 tools; T3 names are tripwires |
| Path and repo scope | `clearance/scope.py` | one implementation, shared with the CI `agent-scope` gate; traversal-safe |
| cicd.yaml field scope | `clearance/packs/hangar/` (R021, `cicd.py`), its `vendor/cicd_scope.py` + `vendor/glidepath-contract.json` (vendored byte for byte from glidepath, drift-tested) | an agent may change cicd.yaml in a PR within Glidepath's `open`/`tighten`/`human` field rules; a denial carries the `GP-SCOPE-00x` rule id and hint |
| Glidepath reads and run control | `clearance/packs/hangar/` (`glidepath.*`, R022), `autopilot/glidepath_api.py`, `autopilot/gateway.py` (handlers), `agents/cicd-explainer.yaml` | six T0 tools over glidepath-api: runs, one run, step logs, `explain` (a failed run's knownFailures id, hint and field), releases, the contract; and two T1 writes, `glidepath.run.rerun` and `glidepath.run.cancel` (spec phase 3), which retire `pipeline.rerun`: a re-run sends an Idempotency-Key made from the session and the run, so a retried call starts no second run, and Clearance names the session principal so glidepath-api audits both identities. `profile.glidepath.apps` (globs; none listed means none) scopes them per agent, arguments must be valid path segments, and the adapter calls as Clearance's own identity, re-reading its token file every call. A 400/404 from glidepath-api (no such run, no archived log) comes back as data with its `GP-*` rule; a 401/403 is a denial with that rule |
| Glidepath release proposals | `clearance/packs/hangar/` (`glidepath.environments.*`, `glidepath.promote`, `glidepath.rollback`), `autopilot/glidepath_api.py`, `autopilot/gateway.py` (`release_key`), `agents/release-proposer.yaml`, `tools/glidepath_release_smoke.py` | spec phase 4. Two T0 reads (the release plan; a Flight environment's rollback plan) and two T2 propose-only writes: `glidepath.promote` and `glidepath.rollback` start the app's release run, whose `open-release-pr` step opens the gitops PR with the platform's GitHub App, and a person merges it. Not capped by the tier ceiling (R007 admits propose-only at T1) but only for a definition that lists them. The Idempotency-Key is the session, app, target environment and cluster, and the source (or rollback target), never `dryRun` or the reason, so a retry replays and a dry run names the run the real call starts; `dryRun` goes as `X-Glidepath-Dry-Run`. A 422 (out of order, a Ground target, an ineligible target, admission) is a denial with glidepath-api's hint. `release-proposer` (gate-api only) holds these, the Glidepath reads, and nothing that reaches GitHub: no `repo.pr.open*`, `xr.request` or `run.spawn`, and zero GitHub, PR and child budgets; no tool in the registry merges or approves |
| Airframe reads | `clearance/packs/hangar/` (`airframe.*`, R023), `autopilot/airframe_tools.py`, `autopilot/airframe_api.py`, `schemas/airframe-contract.json` + `autopilot/vendor/airframe_sidecars.py` (vendored from airframe, drift-tested), `agents/airframe-operator.yaml`, `tools/airframe_smoke.py` | seven T0 tools. `capabilities` answers from Airframe's contract bundle; `validate` runs airframe's own `airframe-validate` (a configured command or container, parsed from `--format json`) and returns AF-* rule ids, JSON pointers and hints; `explain` turns an AF-* rule, a condition reason or a knownFailures id into a description and fix; `plan` is the AppSpec planner; `status` normalises an app's or environment's XR conditions (reason, meaning, hint, observedGeneration against generation); `describe` joins an environment's XRs, the Argo CD Application syncing it (sync, health, revision per source) and its Rollout (phase, ready replicas, image); `verify` runs a kind's sidecar verify checks from the XR alone and returns anything needing a Secret, ConfigMap, port-forward or cloud CLI as `skipped` with the reason. The Kubernetes adapter is GET-only against a hard path allowlist (`catalog.hangar.io` XRs, `argoproj.io` applications and rollouts; no core group, no subresource, no watch), refused before any request is built, with the token and CA re-read every call. `profile.airframe.apps` (globs; none listed means none) scopes every call that names an app |
| Narrow-only limits | `clearance/limits.py` | requested limits or a child can only narrow what they inherit |
| Agent definitions | `clearance/profile.py`, `schemas/` | durable, schema-validated; baseline deny paths cannot be removed |
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
| Network-policy canary (AP-A1) | `tools/netpol_canary.py` | renders a run's real policies with `compose()` on a live cluster and proves each denial against a no-policy control; its pass is what a person attests when setting the registry's `airframe.autopilotReady` |

## What is NOT built

* Real backends: GitHub through the token-review-interceptor, the ArgoCD API, the Kubernetes API
  for `AgentRun` XRs, Backstage MCP federation. `clearance/backends.py` (core tools) and `autopilot/backends.py` (Glidepath,
  Airframe, the AgentRun store) define the interfaces and fakes.
* Authentication adapters: Tower/OAuth introspection for delegated agents, TokenReview for workloads.
* The HTTP transport for MCP, and the model proxy's HTTP forwarder.
* The interceptor's `/agent-installation-token` route, the `agent-scope` and `agent-identity` CI gates.
* The AgentRun composition has not run under Crossplane on a cluster yet (AP-A3's copy-test, airframe
  `docs/admin/agentrun-copy-test.md`). Expiry does not depend on Crossplane re-invocation (U2: about every 60 s):
  the Sandbox's `shutdownTime` is the deadline.
