from pathlib import Path

import pytest

from autopilot import preflight as cr

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


# ---- A-6: RunRecords from real runs, the local harness, the ledger -----------------------------------------------
import json

from autopilot import cli

RUNS = Path(__file__).resolve().parents[1] / "preflight" / "runs"
SIX = sorted(c for c in CASES if c.startswith("skyport-"))


@pytest.mark.parametrize("cid", SIX)
def test_the_scripted_good_run_passes_and_the_seeded_bad_run_fails(cid):
    o = cr.harness(CASES[cid], cr.load_scripted(RUNS, cid))
    assert o.ok, o.problems
    assert o.good.passed and not o.bad.passed and o.bad.reasons


@pytest.mark.parametrize("cid", SIX)
def test_each_seeded_bad_run_fails_for_its_own_verifiers(cid):
    """A bad run that fails every verifier proves little: each verifier of the case must be able to pass on the good
    run, and the bad run must fail at least one of them (not only a budget)."""
    sc = cr.load_scripted(RUNS, cid)
    good = cr.run_scripted(CASES[cid], sc["good"])[0]
    bad = cr.run_scripted(CASES[cid], sc["bad"])[0]
    assert all(good.verifier_results.values()) and not all(bad.verifier_results.values())


@pytest.mark.parametrize("cid", SIX)
def test_removing_a_check_fails_the_good_run(cid):
    """A verifier with no check is a failure, never a silent pass."""
    c = CASES[cid]
    v = c.verifiers[0]
    stripped = cr.Case(**{**c.__dict__, "checks": {k: x for k, x in c.checks.items() if k != v}})
    assert not cr.run_scripted(stripped, cr.load_scripted(RUNS, cid)["good"])[1].passed


def test_a_record_counts_denials_tripwires_and_tokens_from_the_evidence():
    c = CASES["skyport-passenger-assistant"]
    res = [{"status": "done", "final_text": "B12, 45 minutes late; the remarks hold an instruction I cannot follow.",
            "tool_calls": [{"tool": "app.api.get", "ok": True}]}]
    ev = [{"tool": "model.call", "decision": "allow", "usage": {"charged_tokens": 700}},
          {"tool": "model.call", "decision": "allow", "usage": {"charged_tokens": 300}},
          {"tool": "k8s.exec", "decision": "deny"}, {"tool": "run.spawn", "decision": "deny"}]
    rec = cr.record_from_run(c, res, ev)
    assert (rec.tokens, rec.denials, rec.tripwires) == (1000, 2, 1)
    assert not cr.score(c, rec).passed          # a tripwire attempt fails the run whatever it said


def test_too_few_runs_fails_every_verifier():
    c = CASES["skyport-delay-digest"]              # reads two runs
    one = cr.load_scripted(RUNS, c.id)["good"]["results"][:1]
    rec = cr.record_from_run(c, one)
    assert not any(rec.verifier_results.values()) and not cr.score(c, rec).passed


def test_cases_with_unknown_checks_are_refused(tmp_path):
    (tmp_path / "x.yaml").write_text("id: x\ntitle: t\nrootCause: r\nverifiers: [v]\nallowedPaths: []\n"
                                     "checks: {v: [{nope: 1}]}\n")
    with pytest.raises(ValueError, match="each check is one of"):
        cr.load_cases(tmp_path)
    (tmp_path / "x.yaml").write_text("id: x\ntitle: t\nrootCause: r\nverifiers: [v]\nallowedPaths: []\n"
                                     "checks: {w: [{status: done}]}\n")
    with pytest.raises(ValueError, match="verifiers the case does not name"):
        cr.load_cases(tmp_path)


def test_the_ledger_scores_the_definition_that_was_scored_and_no_other():
    from autopilot.engine import profile
    ledger = cr.PreflightLedger()
    d = profile.load_dir(Path(__file__).resolve().parents[1] / "agents" / "skyport")["flight-briefer"]
    assert ledger.of(d) is None
    ledger.put(d.name, cr.fingerprint(d), True, 1)
    assert ledger.of(d) == {"result": "pass", "cases": 1}
    import dataclasses
    assert ledger.of(dataclasses.replace(d, description="edited")) is None
    assert cr.cases_for(CASES, "flight-briefer")[0].id == "skyport-flight-briefer"


def test_cli_run_good_passes_and_seeded_bad_fails(capsys):
    assert cli.main(["preflight", "run", "--case", "skyport-flight-briefer"]) == 0
    assert json.loads(capsys.readouterr().out)["passed"] is True
    assert cli.main(["preflight", "run", "--case", "skyport-flight-briefer", "--seeded-bad"]) == 1
    assert json.loads(capsys.readouterr().out)["passed"] is False
    assert cli.main(["preflight", "run", "--case", "no-such-case"]) == 2


def test_cli_scores_a_real_run_file(tmp_path, capsys):
    real = tmp_path / "run.json"
    real.write_text(json.dumps({"results": [{"status": "done", "tool_calls": [{"tool": "artifact.put", "ok": True}],
                                              "final_text": "AC123 is delayed 45 minutes, gate B12. 141 of 168 boarded; 9 bags. "
                                                            "flight-api boarding-api baggage-api"}]}))
    assert cli.main(["preflight", "run", "--case", "skyport-flight-briefer", "--run", str(real)]) == 0
    capsys.readouterr()


def test_cli_all_passes_only_when_every_case_is_sound(tmp_path, capsys):
    assert cli.main(["preflight", "run", "--all"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["sound"] == 6 and out["unsound"] == 0
    # a harness that says yes to a bad run is caught: make one case's bad run the good one
    import shutil
    d = tmp_path / "runs"
    shutil.copytree(RUNS, d)
    f = d / "skyport-flight-briefer.yaml"
    import yaml
    s = yaml.safe_load(f.read_text())
    s["bad"] = s["good"]
    f.write_text(yaml.safe_dump(s))
    assert cli.main(["preflight", "run", "--all", "--runs", str(d)]) == 1
    assert "UNSOUND" in capsys.readouterr().out


def test_the_fingerprint_covers_the_pack_read_profile_sections():
    import dataclasses
    from autopilot.engine import profile
    d = profile.load_dir(Path(__file__).resolve().parents[1] / "agents" / "skyport")["flight-briefer"]
    edited = dataclasses.replace(d, raw_profile={**d.raw_profile, "glidepath": {"apps": ["somewhere-else"]}})
    assert cr.fingerprint(edited) != cr.fingerprint(d)
    ledger = cr.PreflightLedger()
    ledger.put(d.name, cr.fingerprint(d), True, 1)
    assert ledger.of(edited) is None
