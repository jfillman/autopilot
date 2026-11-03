# Autopilot

Any AI agent workload, bounded and audited.

Autopilot runs agents on [Hangar](https://github.com/jfillman/hangar) the way Hangar runs services: durable changes
are commits that people review, and ephemeral runs are direct requests to Crossplane. An agent is one more author of git
commits, held to a tier of tools, a path scope and a budget that can only narrow at runtime.

- **Defining or running an agent?** Start with the [user guide](user/README.md).
- **Installing or operating the gateway?** See the [admin guide](admin/README.md).
- **Design, plan, decisions and diagrams** live in Hangar's
  [`docs/autopilot/`](https://github.com/jfillman/hangar/tree/main/docs/autopilot): the roadmap (M0 to M5), the
  design, 37 one-page diagrams and the decision log.
- **The mark** and its files: [brand](brand/README.md).

## What is here
| Piece | What it is |
|---|---|
| The AI gateway | `src/autopilot/engine/` (moved from clearance v0.26.1 with its history, R-3a, Built): the MCP surface, sessions, the run tree, budgets and requested limits, the tool registry and dispatch, the model proxy, agent definitions and their schema, the runtime catalog, the human inbox, person controls, run results and trigger history. `engine/core.py` is its one link to Clearance: from R-3b (Built) the Clearance core's HTTP API through its generated client, with the outage policy and the outbox; the gateway's runtime state is in its own database ([admin/gateway-database.md](admin/gateway-database.md)) |
| Clearance | Policy (CEL), path, repo and field scope, the hash-chained evidence and its Postgres store, authentication, the deployment gate, provenance and clearance-api (its own repo, https://github.com/jfillman/clearance): from R-3b a service, the Clearance core, which the gateway calls over HTTP (its client pinned in `pyproject.toml`); it imports nothing from Hangar, and its Hangar pack is the policy for the Glidepath and Airframe tools |
| Autopilot code | `src/autopilot/`: the Glidepath and Airframe tool backends, the AgentRun manifest, triggers, Preflight, and the builder of the Hangar Gateway (`gateway.py`: the engine plus the Hangar pack and backends) |
| Agent definitions | `agents/`: durable, schema-validated, one per workload shape |
| Preflight | `preflight/cases/`: cases that score an agent definition before it is promoted |
| AppSpec planner | Turns one statement of intent into an ordered change set across the tenants, app and gitops repos |
| `AgentRun` | built in airframe (XRD, composition, function-agentrun) and the cluster repos (grants, agent-sandbox); `airframe-drafts/README.md` points there |

Status: **running on hangar-dev** (gateway-v0.3.27 with Clearance v0.26.1, 2026-11-02). Releases are the image tags
(`gateway-v*`, `runtime-v*`, `runtime-<name>-v*`); the Python package itself is not versioned. See the README for
what is built and tested, and what is not.
