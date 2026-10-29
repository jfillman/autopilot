# Autopilot: user guide

For people who **define and run agents**. The runtime is not deployed yet (see the roadmap in Hangar's
`docs/autopilot/roadmap.md`), so this page describes what you can do today and what is coming.

## Today
- **Write an agent definition** in `agents/` (schema: `schemas/`). It names the agent's tier, the repos and paths it
  may touch, and its budgets. Baseline deny paths (`.tekton/**`, `CODEOWNERS`, `.github/**`, appproject files,
  release files) apply to every agent and cannot be removed.
- **cicd.yaml is scoped by field, not denied whole.** An agent may change a `cicd.yaml` in a pull request, but only
  within Glidepath's contract: fields marked `open` freely, `tighten` fields (sast, imageScan, policyCheck, sbom,
  githubMerge, a step's `required`) only the strict way, and never a `human` field (secrets, allowedCommitSigners,
  deploy target, clusters, gitops repos), a Flight environment, a check-ride step or a required test step. Clearance
  runs Glidepath's own check (`src/clearance/packs/hangar/vendor/cicd_scope.py`, vendored byte for byte) on the file's `before`
  and new content and denies with the Glidepath rule id (`GP-SCOPE-001` human field, `GP-SCOPE-002` gate loosened,
  `GP-SCOPE-003` Flight environment or tier) and its hint; every problem is listed under `problems`. Send the file's
  current content as `before`: without it the file counts as new, and a new cicd.yaml is a human's change.
  `op: merge-patch` content is applied to `before` first. The generated `.tekton/` stays denied whole.
- **Score it with Preflight.** Cases in `preflight/cases/` run deterministically against a definition; a case must
  be able to fail, and the harness checks that it can.
- **Plan an application from one statement.** The AppSpec planner (`src/autopilot/airframe_plan.py`, example in
  `examples/parachute.appspec.yaml`) produces the ordered change set that creates an app, its environments and
  their configuration, without touching a repo.

- **Ask Airframe through Clearance.** An agent with the `airframe.*` tools (`agents/airframe-operator.yaml`) can
  read what Airframe offers (`airframe.capabilities`), check env-file or values content before proposing it
  (`airframe.validate`, airframe's own validator: AF-* rule ids, JSON pointers, hints), look up a rule or a
  condition reason (`airframe.explain`), plan an AppSpec (`airframe.plan`: pass the app's current cicd.yaml as
  `baseCicd`), and read an app's XR conditions, an environment's Argo CD Application and Rollout, and an XR's verify
  checks (`airframe.status`, `airframe.describe`, `airframe.verify`). Those three read the cluster and are limited
  to the apps under `profile.airframe.apps` (none listed means none). `verify` never reads a Secret or opens a
  port-forward: those checks come back `skipped` with the reason. A Flight environment's Application and Rollout
  run on its own cluster; `describe` says so.

## Coming (roadmap M2 to M4)
Running a definition as an `AgentRun` XR, sessions with parent and child budgets, and the Tower Agent tab.

Run the tests: `./.venv/bin/python -m pytest -q` (no cluster needed).
