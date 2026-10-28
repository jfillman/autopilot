<div align="center">
  <img src="docs/brand/autopilot-tile.svg" width="88" height="88" alt="Autopilot mark" />
  <h1>Autopilot</h1>
  <p><i>Any AI agent workload, bounded and audited.</i></p>
</div>

Autopilot is the sixth [Hangar](https://github.com/jfillman/hangar) product: it runs AI agent workloads with authority
that is bounded and every action on the record. This repo holds **Clearance**, the policy, audit and session gateway,
plus agent definitions, the Preflight evaluation cases, the AppSpec planner and drafts of the `AgentRun` composition.
Docs: [docs/](docs/index.md). Mark and brand files: [docs/brand/](docs/brand/README.md).

## Clearance

Policy, audit and session gateway for running AI agent workloads on Hangar. **v0.1, pre-release.**

Design, plan and diagrams: `../hangar/docs/autopilot/` (start at `README.md`). This is the `autopilot` repo (decision D8); Clearance is the package inside it.

## What is built and tested

`./.venv/bin/python -m pytest -q` runs 368 tests, no cluster needed (some skip without sibling airframe and glidepath checkouts under `HANGAR_TECH`, which CI clones).

| Piece | Where | What it does |
|---|---|---|
| Tool tiers and tripwires | `tiers.py` | T0 to T2 tools; T3 names are tripwires |
| Path and repo scope | `scope.py` | one implementation, shared with the CI `agent-scope` gate; traversal-safe |
| cicd.yaml field scope | `policy.py` (R021), `vendor/cicd_scope.py` + `vendor/glidepath-contract.json` (vendored byte for byte from glidepath, drift-tested) | an agent may change cicd.yaml in a PR within Glidepath's `open`/`tighten`/`human` field rules; a denial carries the `GP-SCOPE-00x` rule id and hint |
| Glidepath reads and run control | `tiers.py` (`glidepath.*`), `glidepath_api.py`, `policy.py` (R022), `agents/cicd-explainer.yaml` | six T0 tools over glidepath-api: runs, one run, step logs, `explain` (a failed run's knownFailures id, hint and field), releases, the contract; and two T1 writes, `glidepath.run.rerun` and `glidepath.run.cancel` (spec phase 3), which retire `pipeline.rerun`: a re-run sends an Idempotency-Key made from the session and the run, so a retried call starts no second run, and Clearance names the session principal so glidepath-api audits both identities. `profile.glidepath.apps` (globs; none listed means none) scopes them per agent, arguments must be valid path segments, and the adapter calls as Clearance's own identity, re-reading its token file every call. A 400/404 from glidepath-api (no such run, no archived log) comes back as data with its `GP-*` rule; a 401/403 is a denial with that rule |
| Airframe reads | `tiers.py` (`airframe.*`), `airframe_tools.py`, `airframe_api.py`, `policy.py` (R023), `schemas/airframe-contract.json` + `vendor/airframe_sidecars.py` (vendored from airframe, drift-tested), `agents/airframe-operator.yaml`, `tools/airframe_smoke.py` | seven T0 tools. `capabilities` answers from Airframe's contract bundle; `validate` runs airframe's own `airframe-validate` (a configured command or container, parsed from `--format json`) and returns AF-* rule ids, JSON pointers and hints; `explain` turns an AF-* rule, a condition reason or a knownFailures id into a description and fix; `plan` is the AppSpec planner; `status` normalises an app's or environment's XR conditions (reason, meaning, hint, observedGeneration against generation); `describe` joins an environment's XRs, the Argo CD Application syncing it (sync, health, revision per source) and its Rollout (phase, ready replicas, image); `verify` runs a kind's sidecar verify checks from the XR alone and returns anything needing a Secret, ConfigMap, port-forward or cloud CLI as `skipped` with the reason. The Kubernetes adapter is GET-only against a hard path allowlist (`catalog.hangar.io` XRs, `argoproj.io` applications and rollouts; no core group, no subresource, no watch), refused before any request is built, with the token and CA re-read every call. `profile.airframe.apps` (globs; none listed means none) scopes every call that names an app |
| Narrow-only limits | `limits.py` | requested limits or a child can only narrow what they inherit |
| Agent definitions | `profile.py`, `schemas/` | durable, schema-validated; baseline deny paths cannot be removed |
| Sessions and run tree | `session.py` | budgets, expiry, breaker, parent-reserved child budgets |
| Policy | `policy.py` | CEL deny rules with ids and fix hints; fails closed |
| Audit | `audit.py` | hash chain; detects edit, delete, reorder; truncation via an anchored checkpoint |
| Gateway core | `dispatch.py` | authenticate, decide, audit, spend, call, audit |
| Model access | `modelproxy.py` | model allowlist, network mode, token budget; the decision core only |
| Triggers | `triggers.py` | interval and alert triggers for scheduled and event agents |
| Preflight | `preflight.py`, `preflight/cases/` | deterministic scoring, self-check that a case can fail |
| MCP surface | `server.py` | three tools on the mcp SDK v2 |
| AppSpec and the planner | `airframe_plan.py`, `schemas/appspec.schema.json` (vendored from airframe's contract) | the parachute test: one statement of intent to an ordered change set; its cicd.yaml patch is split by Glidepath's scope rules: the agent's half (Ground environments and their deploy steps, added without dropping a step) passes R021, and Flight environments, the release step and retiring pre-ADR-0019 keys are a separate `cicd-flight` step a human merges; `config`/`overrides` take any human-owned chart value (second sentence: `examples/mynewpythonapp.appspec.yaml`, five environments, canary with pauses, HPA, PDB, a prod-only route); validated against the real XRD schemas, Glidepath's cicd schema and a real `helm template` of every environment |
| Skyport agents | `agents/skyport/`, `preflight/cases/skyport-*.yaml` | one definition per workload shape, plus team members; event trigger, app-API allowlist, chat, artifacts, idempotent triggered runs |
| AgentRun composition | `airframe-drafts/functions/function-agentrun/` | pure `compose()`; expiry, fail-closed validation, per-network-mode policy |
| Network-policy canary (AP-A1) | `tools/netpol_canary.py` | renders a run's real policies with `compose()` on a live cluster and proves each denial against a no-policy control; its pass is what a person attests when setting the registry's `airframe.autopilotReady` |

## What is NOT built

* Real backends: GitHub through the token-review-interceptor, the ArgoCD API, the Kubernetes API
  for `AgentRun` XRs, Backstage MCP federation. `backends.py` defines the interfaces and fakes.
* Authentication adapters: Tower/OAuth introspection for delegated agents, TokenReview for workloads.
* The HTTP transport for MCP, and the model proxy's HTTP forwarder.
* The interceptor's `/agent-installation-token` route, the `agent-scope` and `agent-identity` CI gates.
* Anything in `airframe-drafts/` has never been applied to a cluster or rendered by Crossplane.
  Unit tests prove the composition logic, not that provider-kubernetes accepts what it renders.
* Time-driven re-invocation: the function sets a response TTL so Crossplane calls it again near
  expiry, but that behaviour is **unverified** on your Crossplane version. Test it first.
