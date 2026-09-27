# Autopilot: admin guide

For people **running** Autopilot. Nothing here is deployed by a pipeline yet.

## What an install will need
- **Clearance**, a standalone deployment with one Kubernetes permission: a namespaced Role on `agentruns` in
  `autopilot-runs`. It never holds a cluster-wide grant.
- **provider-kubernetes RBAC** for the objects an `AgentRun` renders (Namespace, ResourceQuota, ServiceAccount,
  two NetworkPolicies, Job). Proven on kiac-dev with a scratch role and drafted in
  `airframe-drafts/rbac/provider-kubernetes-agentrun.yaml`; review it as a privilege grant before applying.
- **Expiry** cannot come from Crossplane re-invocation: on Crossplane 2.3 a function's response TTL is only honoured
  with the beta realtime-compositions flag, and the poll floor is one minute. Use the Sandbox `shutdownTime`
  (Kubernetes SIG Agent Sandbox v1.0.x, measured to expire within a second) or the Job's `activeDeadlineSeconds`.
- **A separate GitHub App** for agents, so their API budget cannot starve CI.

## Reading
The measured answers to the open questions (U1 to U12), the decision log and the M0 to M5 plan are in Hangar's
[`docs/autopilot/roadmap.md`](https://github.com/jfillman/hangar/blob/main/docs/autopilot/roadmap.md) and
[`decisions.md`](https://github.com/jfillman/hangar/blob/main/docs/autopilot/decisions.md).
