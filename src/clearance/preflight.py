"""Preflight: score an agent run against a case with a known cause and a known-good fix.

No judge model. A case names deterministic verifiers, a path scope the fix must stay inside,
and a denial budget. Scoring is a pure function of a RunRecord.

Two guards against a harness that always says yes:
  * `min_paths_touched`: a "fix" that changed nothing cannot pass.
  * `selfcheck()`: every case must fail a synthetic run in which nothing was fixed.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

from .scope import glob_to_regex, normalize, BadPath


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
                 token_budget=d.get("tokenBudget", 200_000))
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
