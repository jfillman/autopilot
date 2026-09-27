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

`./.venv/bin/python -m pytest -q` runs 254 tests, no cluster needed.

| Piece | Where | What it does |
|---|---|---|
| Tool tiers and tripwires | `tiers.py` | T0 to T2 tools; T3 names are tripwires |
| Path and repo scope | `scope.py` | one implementation, shared with the CI `agent-scope` gate; traversal-safe |
| Narrow-only limits | `limits.py` | a claim or child can only narrow what it inherits |
| Agent definitions | `profile.py`, `schemas/` | durable, schema-validated; baseline deny paths cannot be removed |
| Sessions and run tree | `session.py` | budgets, expiry, breaker, parent-reserved child budgets |
| Policy | `policy.py` | 15 CEL deny rules with ids and fix hints; fails closed |
| Audit | `audit.py` | hash chain; detects edit, delete, reorder; truncation via an anchored checkpoint |
| Gateway core | `dispatch.py` | authenticate, decide, audit, spend, call, audit |
| Model access | `modelproxy.py` | model allowlist, network mode, token budget; the decision core only |
| Triggers | `triggers.py` | interval and alert triggers for scheduled and event agents |
| Preflight | `preflight.py`, `preflight/cases/` | deterministic scoring, self-check that a case can fail |
| MCP surface | `server.py` | three tools on the mcp SDK v2 |
| AppSpec and the planner | `airframe_plan.py`, `schemas/appspec.schema.json` | the parachute test: one statement of intent to an ordered change set; validated against the real XRD schemas, Glidepath's cicd schema and a real `helm template` |
| Skyport agents | `agents/skyport/`, `preflight/cases/skyport-*.yaml` | one definition per workload shape, plus team members; event trigger, app-API allowlist, chat, artifacts, idempotent triggered runs |
| AgentRun composition | `airframe-drafts/functions/function-agentrun/` | pure `compose()`; expiry, fail-closed validation, per-network-mode policy |

## What is NOT built

* Real backends: GitHub through the token-review-interceptor, the ArgoCD API, the Kubernetes API
  for `AgentRun` claims, Backstage MCP federation. `backends.py` defines the interfaces and fakes.
* Authentication adapters: Tower/OAuth introspection for delegated agents, TokenReview for workloads.
* The HTTP transport for MCP, and the model proxy's HTTP forwarder.
* The interceptor's `/agent-installation-token` route, the `agent-scope` and `agent-identity` CI gates.
* Anything in `airframe-drafts/` has never been applied to a cluster or rendered by Crossplane.
  Unit tests prove the composition logic, not that provider-kubernetes accepts what it renders.
* Time-driven re-invocation: the function sets a response TTL so Crossplane calls it again near
  expiry, but that behaviour is **unverified** on your Crossplane version. Test it first.
