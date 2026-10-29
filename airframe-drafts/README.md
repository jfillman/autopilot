# airframe-drafts (moved)

The AgentRun drafts that lived here moved into airframe in AP-A3 and are no longer drafts:

| Was | Now |
|---|---|
| `xrds/agentrun.yaml` | airframe `xrds/agentrun.yaml` (+ `xrds/agentrun.meta.yaml`) |
| `compositions/agentrun.yaml` | airframe `compositions/agentrun/composition.yaml`, with golden render cases |
| `functions/function-agentrun/` | airframe `functions/function-agentrun/` (renders an agent-sandbox `Sandbox`, not a Job) |
| `rbac/provider-kubernetes-agentrun.yaml` | apron `templates/55-autopilot/agentrun-grants/`, with the confinement policy (D15) |

This repo keeps byte-identical copies of the two files it uses: `src/autopilot/vendor/agentrun_compose.py`
(`tools/netpol_canary.py` renders with it) and `schemas/agentrun.xrd.yaml` (Clearance's manifests are validated
against it). `tests/test_agentrun_vendor.py` fails when they drift from airframe; copy them over, never edit here.
