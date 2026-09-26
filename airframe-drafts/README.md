# airframe-drafts

Files that belong in `airframe/` but are NOT there yet, on purpose.

Airframe is ArgoCD-synced with selfHeal on dev, and a shared XRD or composition is a fleet-wide
blast radius. These are moved in through a git worktree, one at a time, and a new composition is
first exercised by pointing ONE XR at it through `compositionRef` (the test-via-copy rule).

| Draft | Goes to | Notes |
|---|---|---|
| `xrds/agentrun.yaml` | `airframe/xrds/` | namespaced XR, dev clusters only |
| `compositions/agentrun.yaml` | `airframe/compositions/agentrun/` | single Python-function step |
| `functions/function-agentrun/` | `airframe/functions/function-agentrun/` | pure `compose()` plus a thin gRPC wrapper; needs the usual package/Dockerfile from `function-template-python` |

Also required before any of it can run (none of this is drafted yet): grants for the composed
kinds in `provider-kubernetes-applied-resources` (Namespace, ResourceQuota, NetworkPolicy,
ServiceAccount, batch/Job), `function-agentrun` in `functions.yaml`, and a namespaced Role that
lets Clearance create, get, list, patch and delete `agentruns` in `autopilot-runs` and nothing else.
