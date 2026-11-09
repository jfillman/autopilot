"""Preflight: score an agent run against a case with a known cause and a known-good fix.

No judge model. A case names deterministic verifiers, a path scope the fix must stay inside,
and a denial budget. Scoring is a pure function of a RunRecord.

Two guards against a harness that always says yes:
  * `min_paths_touched`: a "fix" that changed nothing cannot pass.
  * `selfcheck()`: every case must fail a synthetic run in which nothing was fixed.
"""
from __future__ import annotations

import dataclasses
import enum
import hashlib
import json
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Mapping

import yaml

from .engine.scope import glob_to_regex, normalize, BadPath
from .engine.tiers import TRIPWIRES


@dataclass(frozen=True)
class Case:
    id: str
    title: str
    root_cause: str
    verifiers: tuple[str, ...]
    allowed_paths: tuple[str, ...]
    min_paths_touched: int = 1
    max_denials: int = 2
    max_retries: int = 3
    token_budget: int = 200_000
    # A-6: what a run of the case needs. `agent` is the definition the case scores (a PR's changed definition picks
    # its cases by this), `input` the run's task, `runs` how many runs the verifiers read (2: an idempotence check
    # reads a second run), `checks` the deterministic predicates behind each verifier (CHECKS).
    agent: str | None = None
    input: str = ""
    runs: int = 1
    checks: Mapping[str, tuple[Mapping[str, Any], ...]] = field(default_factory=dict)


@dataclass
class RunRecord:
    case_id: str
    verifier_results: dict[str, bool] = field(default_factory=dict)
    paths_touched: list[str] = field(default_factory=list)
    denials: int = 0
    tripwires: int = 0
    retries: int = 0
    tokens: int = 0
    wall_seconds: float = 0.0


@dataclass
class Score:
    case_id: str
    passed: bool
    reasons: list[str]
    retries: int
    tokens: int


def load_cases(directory: str | Path) -> dict[str, Case]:
    out: dict[str, Case] = {}
    for f in sorted(Path(directory).glob("*.yaml")):
        d = yaml.safe_load(f.read_text())
        c = Case(id=d["id"], title=d["title"], root_cause=d["rootCause"], verifiers=tuple(d["verifiers"]),
                 allowed_paths=tuple(d["allowedPaths"]), min_paths_touched=d.get("minPathsTouched", 1),
                 max_denials=d.get("maxDenials", 2), max_retries=d.get("maxRetries", 3),
                 token_budget=d.get("tokenBudget", 200_000), agent=d.get("agent"), input=d.get("input", ""),
                 runs=d.get("runs", 1), checks={k: tuple(v) for k, v in (d.get("checks") or {}).items()})
        unknown = [v for v in c.checks if v not in c.verifiers]
        if unknown:
            raise ValueError(f"case {c.id}: checks for verifiers the case does not name: {', '.join(unknown)}")
        for v, preds in c.checks.items():
            for pr in preds:
                bad = sorted(set(pr) - {"run"} - set(CHECKS))
                if bad or len(set(pr) - {"run"}) != 1:
                    raise ValueError(f"case {c.id}: verifier {v}: each check is one of {sorted(CHECKS)} (got {sorted(pr)})")
        if c.id in out:
            raise ValueError(f"duplicate case id {c.id}")
        out[c.id] = c
    return out


def _out_of_scope(case: Case, paths: list[str]) -> list[str]:
    bad = []
    for p in paths:
        try:
            n = normalize(p)
        except BadPath:
            bad.append(p)
            continue
        if not any(glob_to_regex(g).match(n) for g in case.allowed_paths):
            bad.append(p)
    return bad


def score(case: Case, run: RunRecord) -> Score:
    reasons: list[str] = []
    if run.case_id != case.id:
        return Score(case.id, False, [f"run is for case {run.case_id!r}"], run.retries, run.tokens)
    missing = [v for v in case.verifiers if run.verifier_results.get(v) is not True]
    if missing:
        reasons.append("verifier failed: " + ", ".join(missing))
    if len(run.paths_touched) < case.min_paths_touched:
        reasons.append("nothing was changed")
    oos = _out_of_scope(case, run.paths_touched)
    if oos:
        reasons.append("touched paths outside scope: " + ", ".join(oos))
    if run.tripwires:
        reasons.append(f"{run.tripwires} tripwire attempt(s)")
    if run.denials > case.max_denials:
        reasons.append(f"{run.denials} denied calls (max {case.max_denials})")
    if run.retries > case.max_retries:
        reasons.append(f"{run.retries} retries (max {case.max_retries})")
    if run.tokens > case.token_budget:
        reasons.append(f"{run.tokens} tokens (budget {case.token_budget})")
    return Score(case.id, not reasons, reasons, run.retries, run.tokens)


def selfcheck(case: Case) -> None:
    """A run in which nothing was fixed must fail. If it passes, the case cannot detect failure."""
    negative = RunRecord(case.id, {v: False for v in case.verifiers}, [], 0, 0, 0, 0)
    if score(case, negative).passed:
        raise AssertionError(f"case {case.id} passes an unfixed run; its verifiers cannot fail")
    lazy = RunRecord(case.id, {v: True for v in case.verifiers}, [], 0, 0, 0, 0)
    if case.min_paths_touched > 0 and score(case, lazy).passed:
        raise AssertionError(f"case {case.id} passes a run that changed nothing")


@dataclass
class GateResult:
    ok: bool
    regressions: list[str]


def gate(cases: dict[str, Case], current: dict[str, RunRecord], baseline: dict[str, Score],
         token_tolerance: float = 0.25) -> GateResult:
    """Fail if any case regressed against the baseline: passed before and fails now, a tripwire
    appeared, or token use grew beyond tolerance. A case missing from the current run fails."""
    regs: list[str] = []
    for cid, case in cases.items():
        run = current.get(cid)
        if run is None:
            regs.append(f"{cid}: not run")
            continue
        sc = score(case, run)
        base = baseline.get(cid)
        if run.tripwires:
            regs.append(f"{cid}: tripwire attempt")
        if base is not None and base.passed and not sc.passed:
            regs.append(f"{cid}: was passing, now fails ({'; '.join(sc.reasons)})")
        elif base is not None and base.passed and sc.tokens > base.tokens * (1 + token_tolerance):
            regs.append(f"{cid}: token use {sc.tokens} vs baseline {base.tokens}")
    return GateResult(not regs, regs)


# -- RunRecords from real runs (A-6) ---------------------------------------------------------------------------------
#
# A run's RESULT (engine/results.py: status, final_text, turns, tool_calls [{tool, ok, rule}]) plus its EVIDENCE
# (Clearance's records for the session: decision, tool, usage.charged_tokens on a model call) become a RunRecord. The
# verifiers are the case's `checks`: deterministic predicates over the results, never a judge model. A verifier with
# no check cannot be scored from a run (it is a failure, not a pass).

def _calls(r: Mapping[str, Any]) -> list[Mapping[str, Any]]:
    return [c for c in (r.get("tool_calls") or []) if isinstance(c, Mapping)]


def _text(r: Mapping[str, Any]) -> str:
    t = r.get("final_text")
    return t.lower() if isinstance(t, str) else ""


def _count(r: Mapping[str, Any], tool: str) -> int:
    return sum(1 for c in _calls(r) if c.get("tool") == tool and c.get("ok"))


def _tool_count(r: Mapping[str, Any], spec: Mapping[str, Any]) -> bool:
    n = _count(r, spec["tool"])
    return (("eq" not in spec or n == spec["eq"]) and ("min" not in spec or n >= spec["min"])
            and ("max" not in spec or n <= spec["max"]))


# check name -> predicate(result, argument) -> bool
CHECKS: dict[str, Any] = {
    "text_contains": lambda r, a: all(x.lower() in _text(r) for x in a),
    "text_excludes": lambda r, a: not any(x.lower() in _text(r) for x in a),
    "status": lambda r, a: r.get("status") in (a if isinstance(a, list) else [a]),
    "tool_called": lambda r, a: _count(r, a) > 0,
    "tool_not_called": lambda r, a: not any(c.get("tool") == a for c in _calls(r)),
    "tool_count": _tool_count,
    # the gateway refused a call to this tool (a refused call carries the refusing rule)
    "denied_tool": lambda r, a: any(c.get("tool") == a and not c.get("ok") and c.get("rule") for c in _calls(r)),
    "no_tripwire": lambda r, a: (not a) or not any(c.get("tool") in TRIPWIRES for c in _calls(r)),
}


def verifier_results(case: Case, results: list[Mapping[str, Any]]) -> dict[str, bool]:
    """Each verifier of the case: True only when it has checks and every one holds on the run it reads (`run: N`,
    1-based, default 1). A verifier without checks, or reading a run that was not made, is False."""
    out: dict[str, bool] = {}
    for v in case.verifiers:
        preds = case.checks.get(v, ())
        ok = bool(preds)
        for pr in preds:
            n = int(pr.get("run", 1))
            op = next(k for k in pr if k != "run")
            ok = ok and 1 <= n <= len(results) and bool(CHECKS[op](results[n - 1], pr[op]))
        out[v] = ok
    return out


def record_from_run(case: Case, results: list[Mapping[str, Any]], evidence: list[Mapping[str, Any]] | None = None,
                    paths_touched: list[str] | None = None, wall_seconds: float = 0.0) -> RunRecord:
    """The RunRecord of a case's run(s): `results` the runs' results in order (one per `case.runs`), `evidence` the
    session records Clearance kept for them (decision/tool, and usage.charged_tokens on a model call). Denials and
    tripwires count from the evidence when there is any, else from the results' refused calls; tokens from the model
    calls' charge. Fewer results than `case.runs` scores every verifier False."""
    ev = list(evidence or [])
    calls = [c for r in results for c in _calls(r)]
    if ev:
        denied = [e for e in ev if e.get("decision") == "deny"]
        denials = len(denied)
        trips = sum(1 for e in denied if e.get("tool") in TRIPWIRES)
    else:
        denials = sum(1 for c in calls if not c.get("ok") and c.get("rule"))
        trips = sum(1 for c in calls if c.get("tool") in TRIPWIRES)
    tokens = 0
    for e in ev:
        u = e.get("usage")
        if isinstance(u, Mapping) and isinstance(u.get("charged_tokens"), int):
            tokens += u["charged_tokens"]
    # a run that ended in error and was run again is a retry: the runs beyond the case's count
    retries = max(0, len(results) - case.runs)
    vr = verifier_results(case, list(results)) if len(results) >= case.runs else {v: False for v in case.verifiers}
    return RunRecord(case.id, vr, list(paths_touched or []), denials, trips, retries, tokens, wall_seconds)


def score_runs(case: Case, results: list[Mapping[str, Any]], evidence: list[Mapping[str, Any]] | None = None,
               paths_touched: list[str] | None = None) -> tuple[RunRecord, Score]:
    rec = record_from_run(case, results, evidence, paths_touched)
    return rec, score(case, rec)


def score_view(sc: Score, rec: RunRecord | None = None) -> dict[str, Any]:
    """The score as the gateway's API and the `preflight-` check show it."""
    out: dict[str, Any] = {"case": sc.case_id, "passed": sc.passed, "reasons": sc.reasons, "retries": sc.retries,
                           "tokens": sc.tokens}
    if rec is not None:
        out["verifiers"] = dict(rec.verifier_results)
    return out


# -- the scripted runs of the local harness ---------------------------------------------------------------------------
# preflight/runs/<case id>.yaml: `good` (a run that did the work) and `bad` (the seeded bad run: the failure the case
# exists to catch). Each is {results: [<run result>...], evidence: [...], paths_touched: [...]}. The harness scores
# both through the code the gateway uses; the case is sound when good passes and bad fails.

def load_scripted(directory: str | Path, case_id: str) -> dict[str, Any] | None:
    f = Path(directory) / f"{case_id}.yaml"
    return yaml.safe_load(f.read_text()) if f.is_file() else None


def run_scripted(case: Case, bundle: Mapping[str, Any]) -> tuple[RunRecord, Score]:
    return score_runs(case, list(bundle.get("results") or []), list(bundle.get("evidence") or []),
                      list(bundle.get("paths_touched") or []))


@dataclass
class HarnessOutcome:
    case_id: str
    good: Score | None
    bad: Score | None
    problems: list[str]

    @property
    def ok(self) -> bool:
        return not self.problems


def harness(case: Case, scripted: Mapping[str, Any] | None) -> HarnessOutcome:
    """Score a case's good and seeded-bad scripted runs. Sound = good passes AND bad fails (a harness that says yes to
    everything, or no to everything, is caught here)."""
    if not scripted or "good" not in scripted or "bad" not in scripted:
        return HarnessOutcome(case.id, None, None, ["no scripted good and bad runs (preflight/runs)"])
    problems: list[str] = []
    g = run_scripted(case, scripted["good"])[1]
    b = run_scripted(case, scripted["bad"])[1]
    if not g.passed:
        problems.append("the good run failed: " + "; ".join(g.reasons))
    if b.passed:
        problems.append("the seeded bad run passed")
    return HarnessOutcome(case.id, g, b, problems)


# -- what a definition's Preflight result is, and where it lands in the record ---------------------------------------

def _plain(x: Any) -> Any:
    if dataclasses.is_dataclass(x) and not isinstance(x, type):
        return {f.name: _plain(getattr(x, f.name)) for f in dataclasses.fields(x)}
    if isinstance(x, enum.Enum):
        return x.name
    if isinstance(x, Mapping):
        return {str(k): _plain(v) for k, v in x.items()}
    if isinstance(x, (set, frozenset)):
        return sorted(_plain(i) for i in x)
    if isinstance(x, (list, tuple)):
        return [_plain(i) for i in x]
    return x


def fingerprint(d: Any) -> str:
    """A stable digest of a parsed AgentDefinition (everything the loader reads from the file, the pack-read profile sections included), so a score belongs to
    the definition it scored and to no later edit of it."""
    return hashlib.sha256(json.dumps(_plain(d), sort_keys=True, default=str).encode()).hexdigest()


class PreflightLedger:
    """The latest Preflight result per definition name, kept with the definition's fingerprint. The gateway's
    `Gateway.preflight` callable is `ledger.of`: a run record of a definition carries `preflight {result, definition,
    cases}` (the fields Clearance's record accepts) only while the definition is the one that was scored; an edited
    definition is not-run again. In memory: a restart is not-run until the next PR check scores it."""

    def __init__(self) -> None:
        self._rows: dict[str, dict[str, Any]] = {}
        self._lock = threading.Lock()

    def put(self, name: str, fp: str, passed: bool, cases: int) -> dict[str, Any]:
        row = {"fingerprint": fp, "result": "pass" if passed else "fail", "cases": cases}
        with self._lock:
            self._rows[name] = row
        return row

    def of(self, d: Any) -> dict[str, Any] | None:
        with self._lock:
            row = self._rows.get(getattr(d, "name", ""))
        if row is None or row["fingerprint"] != fingerprint(d):
            return None
        return {"result": row["result"], "cases": row["cases"]}


def cases_for(cases: Mapping[str, Case], agent: str) -> list[Case]:
    return sorted((c for c in cases.values() if c.agent == agent), key=lambda c: c.id)
