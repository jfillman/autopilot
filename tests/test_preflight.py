from pathlib import Path

import pytest

from clearance import preflight as cr

CASES = cr.load_cases(Path(__file__).resolve().parents[1] / "preflight" / "cases")


def good(case_id, **kw):
    c = CASES[case_id]
    paths = [] if c.min_paths_touched == 0 else (["charts/values.yaml"] if "probe" in case_id else ["compositions/x/composition.yaml"])
    base = dict(verifier_results={v: True for v in c.verifiers}, paths_touched=paths,
                denials=0, tripwires=0, retries=1, tokens=50_000, wall_seconds=90)
    base.update(kw)
    return cr.RunRecord(case_id, **base)


def test_seed_cases_load():
    assert {"liveness-probe-never-ready", "auto-ready-no-ready-condition"} <= set(CASES)
    assert len([c for c in CASES if c.startswith("skyport-")]) == 6, "one case per Skyport AI workload"


@pytest.mark.parametrize("cid", list(CASES))
def test_every_case_can_fail(cid):
    cr.selfcheck(CASES[cid])          # raises if a do-nothing or unfixed run would pass


@pytest.mark.parametrize("cid", list(CASES))
def test_a_good_run_passes(cid):
    assert cr.score(CASES[cid], good(cid)).passed


def test_each_failure_mode_fails_on_its_own():
    cid = "liveness-probe-never-ready"
    c = CASES[cid]
    assert not cr.score(c, good(cid, verifier_results={**good(cid).verifier_results, "probe-path-is-liveness": False})).passed
    assert not cr.score(c, good(cid, paths_touched=[])).passed
    assert not cr.score(c, good(cid, paths_touched=["charts/values.yaml", ".tekton/pr.yaml"])).passed
    assert not cr.score(c, good(cid, tripwires=1)).passed
    assert not cr.score(c, good(cid, denials=3)).passed
    assert not cr.score(c, good(cid, retries=4)).passed
    assert not cr.score(c, good(cid, tokens=150_001)).passed


def test_a_missing_verifier_result_is_a_failure_not_a_pass():
    cid = "liveness-probe-never-ready"
    r = good(cid)
    del r.verifier_results["pod-ready-within-5m"]
    assert not cr.score(CASES[cid], r).passed


def test_run_for_another_case_is_refused():
    assert not cr.score(CASES["liveness-probe-never-ready"], good("auto-ready-no-ready-condition")).passed


def test_gate_flags_regressions_tripwires_and_missing_runs():
    base = {cid: cr.score(CASES[cid], good(cid)) for cid in CASES}
    ok = cr.gate(CASES, {cid: good(cid) for cid in CASES}, base)
    assert ok.ok
    reg = cr.gate(CASES, {"liveness-probe-never-ready": good("liveness-probe-never-ready", paths_touched=[]),
                          "auto-ready-no-ready-condition": good("auto-ready-no-ready-condition")}, base)
    assert not reg.ok and "was passing, now fails" in reg.regressions[0]
    missing = cr.gate(CASES, {"auto-ready-no-ready-condition": good("auto-ready-no-ready-condition")}, base)
    assert not missing.ok and "not run" in missing.regressions[0]
    costly = cr.gate(CASES, {cid: good(cid, tokens=100_000) for cid in CASES}, base)
    assert not costly.ok and "token use" in costly.regressions[0]
