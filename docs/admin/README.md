# Autopilot: admin guide

For people **running** Autopilot. Nothing here is deployed by a pipeline yet.

## What an install will need
- **Clearance**, a standalone deployment. Its core (https://github.com/jfillman/clearance, `src/clearance/` all but `packs/hangar/`) needs no Airframe
  and no Glidepath; on Hangar it runs with the Hangar pack and Autopilot's backends (`autopilot.gateway.hangar_gateway`).
  `tools_list` shows exactly the tools the deployment was built with. Its write permission is one namespaced Role on `agentruns` in
  `autopilot-runs`. For the `airframe.*` tools it reads, cluster-wide and read-only, `catalog.hangar.io/*` and
  `argoproj.io` applications and rollouts (ClusterRole `clearance-airframe-reader`, gitops-hangar-dev#40), and
  nothing else: no Secrets, ConfigMaps or pods. The adapter (`src/autopilot/airframe_api.py`) enforces the same
  allowlist itself, GET only, so an RBAC mistake alone still reads nothing more. It needs the API server URL, the
  CA file and a projected ServiceAccount token file (re-read every call). `airframe.validate` needs airframe's
  `airframe-validate`: the script from an airframe checkout with python3, pyyaml, jsonschema and helm, or the
  `tools/validate.Containerfile` image run with the content directory mounted read-only.
- **Live check**: `tools/airframe_smoke.py` drives the tools through MCP against a cluster with a 10-minute token
  (`kubectl create token clearance -n platform-system --duration=10m`) and compares them with `kubectl get`.
- **provider-kubernetes RBAC** for the objects an `AgentRun` renders (Namespace, ResourceQuota, ServiceAccount,
  two NetworkPolicies, the agent-sandbox Sandbox): the cluster repo's `55-autopilot/agentrun-grants/` (apron
  template), shipped only with the ValidatingAdmissionPolicy that confines it to run namespaces (decision D15).
- **Expiry** cannot come from Crossplane re-invocation: on Crossplane 2.3 a function's response TTL is only honoured
  with the beta realtime-compositions flag, and the poll floor is one minute. Use the Sandbox `shutdownTime`
  (Kubernetes SIG Agent Sandbox v1.0.x, measured to expire within a second) or the Job's `activeDeadlineSeconds`.
- **A separate GitHub App** for agents, so their API budget cannot starve CI.

## Reading
The measured answers to the open questions (U1 to U12), the decision log and the M0 to M5 plan are in Hangar's
[`docs/autopilot/roadmap.md`](https://github.com/jfillman/hangar/blob/main/docs/autopilot/roadmap.md) and
[`decisions.md`](https://github.com/jfillman/hangar/blob/main/docs/autopilot/decisions.md).
