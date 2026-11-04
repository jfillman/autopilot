# Autopilot: user guide

For people who **define and run agents**. Autopilot runs on hangar-dev (the gateway image gateway-v0.4.0 calling
clearance v0.29.0's core, as of 2026-11-03); Hangar's portal on hangar-prod shows it (the Autopilot and Clearance pages,
and Tower's Agent tab per app). Hangar's `docs/autopilot/roadmap.md` has the
plan.

## Today
- **Write an agent definition** in `agents/` (schema: `schemas/`). It names the agent's tier, the repos and paths it
  may touch, and its budgets. Baseline deny paths (`.tekton/**`, `CODEOWNERS`, `.github/**`, appproject files,
  release files) apply to every agent and cannot be removed.
- **cicd.yaml is scoped by field, not denied whole.** An agent may change a `cicd.yaml` in a pull request, but only
  within Glidepath's contract: fields marked `open` freely, `tighten` fields (sast, imageScan, policyCheck, sbom,
  githubMerge, a step's `required`) only the strict way, and never a `human` field (secrets, allowedCommitSigners,
  deploy target, clusters, gitops repos), a Flight environment, a check-ride step or a required test step. Clearance
  runs Glidepath's own check (clearance's `packs/hangar/vendor/cicd_scope.py`, vendored byte for byte) on the file's `before`
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
  (`airframe.validate`, airframe's own validator: AF-* rule ids, JSON pointers, hints; not in Hangar's gateway image
  yet, so there it answers "no backend"), look up a rule or a
  condition reason (`airframe.explain`), plan an AppSpec (`airframe.plan`: pass the app's current cicd.yaml as
  `baseCicd`), and read an app's XR conditions, an environment's Argo CD Application and Rollout, and an XR's verify
  checks (`airframe.status`, `airframe.describe`, `airframe.verify`). Those three read the cluster and are limited
  to the apps under `profile.airframe.apps` (none listed means none). `verify` never reads a Secret or opens a
  port-forward: those checks come back `skipped` with the reason. A Flight environment's Application and Rollout
  run on its own cluster; `describe` says so.

- **Propose a release through Glidepath.** An agent with `glidepath.promote` or `glidepath.rollback`
  (`agents/release-proposer.yaml`) proposes a promotion to the next Flight environment, or a rollback to one of the
  environment's rollback-plan targets: glidepath-api starts the app's release run, which opens the gitops PR, and a
  person merges it. Read the plan first (`glidepath.environments.list`, `glidepath.environments.rollbackPlan`), pass
  `dryRun: true` to check everything without starting anything, and follow the release with
  `glidepath.releases.get`. Retrying the same call starts no second run. A refusal (out of order, a Ground target,
  a target not in the plan) comes back as a denial with Glidepath's rule (`GP-API-422`) and hint. The definition
  lists no tool that reaches GitHub, so the merge is always a person's. `tools/glidepath_release_smoke.py` runs it
  against a live glidepath-api (dry run unless `--propose`).

- **Change an app's `cicd.yaml` through Glidepath.** `glidepath.cicd.validate` (T0) takes a JSON merge patch and asks
  glidepath-api what it would mean: valid or not, each finding with its `GP-*` rule and fix, every field the change
  touches with its scope (`open`, `tighten`, `human`), and the resulting file. `glidepath.cicd.propose` (T1,
  `agents/coding-agent.yaml`) asks the same and, when the change is clean, opens the pull request: one commit of
  `cicd.yaml` on the app's own repo (`repo: owner/<app>`), as the agents' App, with the verdict and the changed fields
  in the PR body; a person merges it. A change an agent may not make (a `human` field, a gate turned off, a Flight
  environment) is refused with the same `GP-SCOPE` rule the PR's `cicd-validate` check would post, before anything
  exists on GitHub. The PR counts against `openPrs` and carries the agent gates' receipt like `repo.pr.open`. A
  `repo.pr.open` whose files include the app's root `cicd.yaml` (a code change with its pipeline step, say) gets the
  same verdict first, so no `cicd.yaml` reaches GitHub through Clearance without it; R021 still runs on every
  `cicd.yaml`, at any depth.

- **Run an agent.** From the portal's Autopilot pages, a person's Run starts an in-cluster agent as an `AgentRun` with
  your input as its prompt and the definition's `runtime.instructions` beside it; its session has the definition's
  budgets, and a child run's budget comes out of its parent's. A run hands back its answer as it ends
  (`run_finish`), kept 30 days on the run's page. Agents can also start on a schedule (`triggers` interval) or on a
  Glidepath event.
- **Keep a definition in your app's repo.** An `Agent` XR (airframe `xrds/agent.yaml`) pins one commit of a definition
  file in the app's own repository; the gateway loads it beside the bundled `agents/` (Autopilot M4). Its release is
  reviewed through the deployment gate (Clearance `review.py`).
- **Answer an agent.** `human.request` questions arrive in the portal; the session's owner or the definition's `owner`
  group answers. Freeze or end a session, and hold or release an agent's triggers, from the same pages.
- **Use your own MCP client.** Sign in through people-dex (`tools/hangar_login.py`, the README's "Signing in as a
  person") and call Clearance's tools as yourself; Clearance enforces the definition's envelope, but your client is
  the agent. When you are done, `session_close` ends your session at once (it is recorded, and any runs it spawned
  are deleted) instead of leaving it open until its TTL; only you can close your own session this way.

## Not yet
Tools without a backend on Hangar (`chat.*`, `app.api.get`, `xr.request`, `airframe.validate`) and the agents that
need them (the Skyport set); the README's "What is NOT built" has the full list.

Run the tests: `./.venv/bin/python -m pytest -q` (no cluster needed).
