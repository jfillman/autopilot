# Autopilot: user guide

For people who **define and run agents**. The runtime is not deployed yet (see the roadmap in Hangar's
`docs/autopilot/roadmap.md`), so this page describes what you can do today and what is coming.

## Today
- **Write an agent definition** in `agents/` (schema: `schemas/`). It names the agent's tier, the repos and paths it
  may touch, and its budgets. Baseline deny paths (`.tekton/**`, `cicd.yaml`, `CODEOWNERS`, `.github/**`, appproject
  files) apply to every agent and cannot be removed.
- **Score it with Preflight.** Cases in `preflight/cases/` run deterministically against a definition; a case must
  be able to fail, and the harness checks that it can.
- **Plan an application from one statement.** The AppSpec planner (`src/clearance/airframe_plan.py`, example in
  `examples/parachute.appspec.yaml`) produces the ordered change set that creates an app, its environments and
  their configuration, without touching a repo.

## Coming (roadmap M2 to M4)
Running a definition as an `AgentRun` XR, sessions with parent and child budgets, and the Tower Agent tab.

Run the tests: `./.venv/bin/python -m pytest -q` (no cluster needed).
