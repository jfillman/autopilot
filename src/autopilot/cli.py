"""`autopilot`: the local tools. Today: `autopilot preflight` (the Preflight harness, A-6).

    autopilot preflight list
    autopilot preflight run --case skyport-delay-digest              # the scripted good run: must pass (exit 0)
    autopilot preflight run --case skyport-delay-digest --seeded-bad # the seeded bad run: must fail (exit 1)
    autopilot preflight run --case X --run results.json              # score a real run's result(s) (+ evidence)
    autopilot preflight run --all                                    # every case with scripted runs: good passes AND bad fails

Exit codes: 0 pass (for --all: every case sound), 1 fail (for --all: a case is unsound), 2 usage error.
Scoring is src/autopilot/preflight.py, the code the gateway scores a PR's candidate run with.
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Sequence

from . import preflight as pf

ROOT = Path(__file__).resolve().parents[2]


def _out(x: Any) -> None:
    print(json.dumps(x, indent=2, sort_keys=True))


def _run(a: argparse.Namespace) -> int:
    cases = pf.load_cases(a.cases)
    if a.all:
        rows, bad = [], 0
        for cid, case in cases.items():
            scripted = pf.load_scripted(a.runs, cid)
            if scripted is None:
                rows.append({"case": cid, "status": "skipped", "why": "no scripted runs"})
                continue
            o = pf.harness(case, scripted)
            bad += 0 if o.ok else 1
            rows.append({"case": cid, "status": "ok" if o.ok else "UNSOUND", "good": o.good.passed if o.good else None,
                         "bad_failed": (not o.bad.passed) if o.bad else None, "problems": o.problems,
                         "bad_reasons": o.bad.reasons if o.bad else []})
        scored = [r for r in rows if r["status"] != "skipped"]
        if not scored:
            print("no case has scripted runs", file=sys.stderr)
            return 2
        _out({"cases": rows, "sound": len(scored) - bad, "unsound": bad})
        return 1 if bad else 0
    if not a.case:
        print("--case or --all is required", file=sys.stderr)
        return 2
    if a.case not in cases:
        print(f"no case {a.case!r}", file=sys.stderr)
        return 2
    case = cases[a.case]
    if a.run:
        doc = json.loads(Path(a.run).read_text())
        bundle = doc if "results" in doc else {"results": [doc]}
    else:
        scripted = pf.load_scripted(a.runs, a.case)
        if scripted is None:
            print(f"case {a.case} has no scripted runs in {a.runs}", file=sys.stderr)
            return 2
        bundle = scripted["bad" if a.seeded_bad else "good"]
    rec, sc = pf.run_scripted(case, bundle)
    _out(pf.score_view(sc, rec))
    return 0 if sc.passed else 1


def main(argv: Sequence[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="autopilot")
    sub = p.add_subparsers(dest="cmd", required=True)
    pre = sub.add_parser("preflight", help="the Preflight harness").add_subparsers(dest="sub", required=True)
    ls = pre.add_parser("list", help="the cases")
    run = pre.add_parser("run", help="score a case")
    for sp in (ls, run):
        sp.add_argument("--cases", default=str(ROOT / "preflight" / "cases"))
    run.add_argument("--runs", default=str(ROOT / "preflight" / "runs"))
    run.add_argument("--case")
    run.add_argument("--all", action="store_true", help="every case with scripted runs: good must pass, bad must fail")
    run.add_argument("--seeded-bad", action="store_true", help="score the case's seeded bad run")
    run.add_argument("--run", help="a JSON file: a run's result, or {results: [...], evidence: [...]}")
    a = p.parse_args(argv)
    if a.sub == "list":
        _out([{"id": c.id, "agent": c.agent, "title": c.title} for c in pf.load_cases(a.cases).values()])
        return 0
    return _run(a)


if __name__ == "__main__":
    raise SystemExit(main())
