# Autopilot

Any AI agent workload, bounded and audited.

Autopilot runs agents on [Hangar](https://github.com/jfillman/hangar) the way Hangar runs services: durable changes
are commits that people review, and ephemeral runs are direct requests to Crossplane. An agent is one more author of git
commits, held to a tier of tools, a path scope and a budget that can only narrow at runtime.

- **Defining or running an agent?** Start with the [user guide](user/README.md).
- **Installing or operating Clearance?** See the [admin guide](admin/README.md).
- **Design, plan, decisions and diagrams** live in Hangar's
  [`docs/autopilot/`](https://github.com/jfillman/hangar/tree/main/docs/autopilot): the roadmap (M0 to M5), the
  design, 37 one-page diagrams and the decision log.
- **The mark** and its files: [brand](brand/README.md).

## What is here
| Piece | What it is |
|---|---|
| Clearance | Policy (CEL), hash-chained audit, tiers, path scope, narrow-only limits, the gateway and MCP surface |
| Agent definitions | `agents/`: durable, schema-validated, one per workload shape |
| Preflight | `preflight/cases/`: cases that score an agent definition before it is promoted |
| AppSpec planner | Turns one statement of intent into an ordered change set across the tenants, app and gitops repos |
| `AgentRun` drafts | `airframe-drafts/`: XRD, composition, function and the RBAC it needs. Drafts: never applied by a pipeline |

Status: **v0.1, pre-release.** See the README for what is built and tested, and what is not.
