# Preflight

Preflight scores an agent definition against cases with a known cause and a known-good outcome, with no judge model.
A case (`preflight/cases/<id>.yaml`) names the definition it scores (`agent`), the task it runs (`input`), how many
runs its verifiers read (`runs`), and for each verifier the deterministic checks over the run's result (`checks`).
Scoring is `src/autopilot/preflight.py`, a pure function of a `RunRecord`.

## A RunRecord from a real run

`record_from_run(case, results, evidence)` builds the record from what a run leaves behind:

| From | Becomes |
|---|---|
| the run's **result** (`GET /v1/sessions/{id}/result`: `status`, `final_text`, `tool_calls [{tool, ok, rule}]`) | each verifier, by the case's checks |
| the run's **evidence** (Clearance's records for the session: `decision`, `tool`, `usage.charged_tokens`) | denials, tripwire attempts (`engine/tiers.py` TRIPWIRES), tokens |

A verifier with no check, or one that reads a run that was not made, is a failure. Checks: `text_contains`,
`text_excludes`, `status`, `tool_called`, `tool_not_called`, `tool_count {tool, eq|min|max}`, `denied_tool`,
`no_tripwire`; `run: 2` reads the second run (the idempotence check of `skyport-delay-digest`).

## The local harness

```
autopilot preflight list
autopilot preflight run --case skyport-delay-digest               # the scripted good run: exit 0
autopilot preflight run --case skyport-delay-digest --seeded-bad  # the seeded bad run: exit 1
autopilot preflight run --case skyport-delay-digest --run run.json # score a real run ({results: [...], evidence: [...]})
autopilot preflight run --all     # every case with scripted runs: exit 0 only if each good run passes AND each bad run fails
```

`preflight/runs/<case id>.yaml` holds the scripted `good` run and the **seeded bad run** for a case. The six Skyport
cases have both; `--all` runs in CI (`.github/workflows/test.yaml`) and in `tests/test_preflight.py`. The two
platform cases (`liveness-probe-never-ready`, `auto-ready-no-ready-condition`) have no scripted runs yet and are skipped.

## On a definition PR

Glidepath's `preflight-` check (glidepath `docs/admin/agent-definitions.md`) does, per changed definition that has
cases (`agent: <name>`):

1. `POST /v1/agents/{name}/runs` with `{"input": <case input>, "document": <the candidate>, "budget": {...}}`, once
   per run the case reads. The candidate must load like a pin, name `{name}` and pin its image by digest. It runs as
   the caller (who must manage the live agent, or be a platform admin for a new one) as a **clamped task** named
   `pf-<agent>-<fingerprint>`: tier at most T1, tools above T1 and tools that open PRs or spend GitHub calls dropped,
   no GitHub calls or PRs, at most 10 minutes, 60 tool calls, 150000 model tokens and 3 children (`budget` can only
   lower these), no triggers. It never enters the live set, so no trigger or pin sees it.
2. Poll `POST /v1/preflight/score` with `{"agent": name, "cases": [{"case": id, "sessions": [sid, ...]}]}` until
   `finished`. The gateway scores each case with the code above, using the session's model usage as the evidence.
3. When every case has finished, the definition's result lands in the **ledger**: `pass` only if every case passed.
   A run record of that definition (`run.launch`, `run.finish`, ...) then carries `preflight {result, definition,
   cases}` (what Clearance accepts) while the definition is unchanged; an edited definition is `not-run` again.

## What it does not do

- The ledger is in memory: after a gateway restart a definition is `not-run` until the next PR check scores it.
- Clearance's `preflight` record field holds `result`, `definition` and `cases` only. The per-case score, reasons and
  token counts are in the score response and the PR check, not the record; recording them needs a Clearance contract
  change (jfillman/clearance).
- Evidence comes from the session's own usage and the result's refused calls, not from reading Clearance's chain back.
- Running it against real models is the hangar-dev run (the managing agent's), not this repo's CI.
