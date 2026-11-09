"""A-6: a PR's candidate definition runs under Preflight (Gateway.run_agent with `document`), is scored against the
agent's cases (Gateway.preflight_score), and the score lands in the ledger and on the definition's run records."""
import copy

import pytest
from starlette.testclient import TestClient

from autopilot import preflight as pf
from autopilot.engine import control as ctl
from autopilot.engine import profile
from autopilot.engine import results as res
from autopilot.engine.dispatch import Gateway, PREFLIGHT_CAP
from autopilot.engine.server import create_http_app
from autopilot.engine.session import SessionStore
from autopilot.hangar_tools import HANGAR
from coreenv import tap_core

from .test_agent_runs import TOWER, Groups, doc
from .test_run_results import RunAuth, Runs, finish

CASE = pf.Case(id="researcher-answers", title="t", root_cause="r", verifiers=("names-languages", "read-catalog"),
               allowed_paths=(), min_paths_touched=0, agent="researcher", input="what languages can I build apps in?",
               checks={"names-languages": ({"text_contains": ["python"]},),
                       "read-catalog": ({"tool_called": "catalog.read"},)})


@pytest.fixture
def g(defs, clock):
    return Gateway(defs, SessionStore(), tap_core(), RunAuth(), clock, packs=(HANGAR,), runs=Runs(),
                   directory=Groups(), delegates=(TOWER,), admins=("user:root",), results=res.MemoryResults(),
                   preflight_cases={CASE.id: CASE}, preflight_ledger=pf.PreflightLedger())


def candidate(**edit):
    d = copy.deepcopy(doc())
    d["profile"].update(edit.pop("profile", {}))
    d.update(edit)
    return d


def launch(g, document, **kw):
    return g.run_agent("root", "researcher", "what languages can I build apps in?", document=document, **kw)


def test_a_candidate_runs_budgeted_and_never_shadows_the_live_definition(g):
    live = g.defs["researcher"]
    out = launch(g, candidate(), budget={"model_tokens": 50_000, "toolCalls": 5})
    assert out["preflight"] and out["definition"].startswith("pf-researcher-") and out["definition"] not in g.defs
    assert g.defs["researcher"] is live and g.store.get(out["session"]).agent == out["definition"]
    lim = out["limits"]
    assert lim["model_tokens"] == 50_000 and lim["ttl_minutes"] <= PREFLIGHT_CAP["ttl_minutes"]
    assert lim["tool_calls"] <= PREFLIGHT_CAP["tool_calls"] and lim["github_calls"] == 0 and lim["open_prs"] == 0


def test_a_candidate_is_clamped_whatever_it_asks_for(g):
    wide = candidate(profile={"tierCeiling": "T2", "tools": ["catalog.read", "repo.pr.open"],
                              "limits": {"ttlMinutes": 600, "toolCalls": 9999, "githubCalls": 50, "openPrs": 5,
                                         "modelTokens": 9_000_000, "children": 9}})
    lim = launch(g, wide)["limits"]
    assert lim["tier_ceiling"] == "T1" and lim["tools"] == ["catalog.read"], "a tool above T1 is dropped"
    assert (lim["ttl_minutes"], lim["tool_calls"], lim["model_tokens"], lim["github_calls"], lim["open_prs"]) == (
        PREFLIGHT_CAP["ttl_minutes"], PREFLIGHT_CAP["tool_calls"], PREFLIGHT_CAP["model_tokens"], 0, 0)
    assert lim["children"] <= PREFLIGHT_CAP["children"]


def test_a_candidate_must_load_name_the_agent_and_pin_its_image(g):
    for bad, why in [(candidate(name="other-agent"), "names 'other-agent'"),
                     (candidate(description=""), "would not load"),
                     (candidate(runtime={"image": "ghcr.io/x/y:latest", "framework": "generic"}), "sha256")]:
        with pytest.raises(ctl.ControlError) as e:
            launch(g, bad)
        assert e.value.status == 400 and why in e.value.message
    with pytest.raises(ctl.ControlError) as e:
        launch(g, "not an object")
    assert e.value.status == 400


def test_only_a_manager_of_the_agent_launches_a_candidate(g):
    with pytest.raises(ctl.ControlError) as e:
        g.run_agent("sam", "researcher", "x", document=candidate())
    assert e.value.rule == "CT-FORBIDDEN"
    assert not g.candidates


def finished(g, out, text, calls=()):
    s = g.store.get(out["session"])
    s.add_usage({"charged_tokens": 1000, "input_tokens": 600, "output_tokens": 400})
    return finish(g, s, final_text=text, tool_calls=list(calls))


def test_the_score_lands_in_the_ledger_and_on_the_definition_records(g):
    pr = candidate(description="Investigates a service read-only, now with a better description.")
    out = launch(g, pr)
    ask = {"agent": "researcher", "cases": [{"case": CASE.id, "sessions": [out["session"]]}]}
    waiting = g.preflight_score("root", ask)
    assert waiting["finished"] is False and "passed" not in waiting
    finished(g, out, "Python and Go.", [{"tool": "catalog.read", "ok": True}])
    done = g.preflight_score("root", ask)
    assert done["finished"] and done["passed"] and done["scores"][0]["tokens"] == 1000
    assert done["record"] == {"fingerprint": done["record"]["fingerprint"], "result": "pass", "cases": 1}
    # the candidate is not the live definition (the live one is unchanged: its fingerprint differs), so the live
    # definition's run records still say not-run until a definition that was scored is the one that runs
    assert g.preflight_of(g.defs["researcher"], "researcher")["result"] == "not-run"
    g.defs = {**g.defs, "researcher": profile.parse(pr)}      # the PR merged and was pinned
    assert g.preflight_of(g.defs["researcher"], "researcher") == {"result": "pass", "definition": "researcher", "cases": 1}
    g.defs = {**g.defs, "researcher": profile.parse(candidate(description="Changed after the score."))}
    assert g.preflight_of(g.defs["researcher"], "researcher")["result"] == "not-run", "an edited definition is unscored"


def test_the_seeded_bad_run_fails_and_is_recorded_as_fail(g):
    out = launch(g, candidate())
    finished(g, out, "I do not know.")
    done = g.preflight_score("root", {"agent": "researcher", "cases": [{"case": CASE.id, "sessions": [out["session"]]}]})
    assert done["finished"] and done["passed"] is False and done["record"]["result"] == "fail"
    assert "names-languages" in done["scores"][0]["reasons"][0]


def test_scoring_refuses_what_is_not_a_preflight_run_of_the_agent(g):
    live = g.run_agent("root", "researcher", "x")
    for body in [{"agent": "researcher", "cases": [{"case": CASE.id, "sessions": [live["session"]]}]},
                 {"agent": "researcher", "cases": [{"case": "nope", "sessions": ["s"]}]},
                 {"agent": "researcher", "cases": []}, "x"]:
        with pytest.raises(ctl.ControlError):
            g.preflight_score("root", body)
    with pytest.raises(ctl.ControlError) as e:
        g.preflight_score("sam", {"agent": "researcher", "cases": [{"case": CASE.id, "sessions": ["s"]}]})
    assert e.value.rule == "CT-FORBIDDEN"


def test_the_routes(g):
    c = TestClient(create_http_app(g, resource_url="http://x/mcp", issuer_url="http://x"))
    h = {"Authorization": "Bearer root"}
    r = c.post("/v1/agents/researcher/runs", json={"input": "look", "document": candidate(), "budget": {"model_tokens": 10_000}}, headers=h)
    assert r.status_code == 201 and r.json()["limits"]["model_tokens"] == 10_000
    r2 = c.post("/v1/agents/researcher/runs", json={"input": "look", "document": candidate(name="zzz")}, headers=h)
    assert r2.status_code == 400
    s = c.post("/v1/preflight/score", json={"agent": "researcher", "cases": [{"case": CASE.id, "sessions": [r.json()["session"]]}]}, headers=h)
    assert s.status_code == 200 and s.json()["finished"] is False
    assert c.post("/v1/preflight/score", json={}, headers=h).status_code == 400


def test_the_case_list_route(g):
    c = TestClient(create_http_app(g, resource_url="http://x/mcp", issuer_url="http://x"))
    r = c.get("/v1/preflight/cases?agent=researcher", headers={"Authorization": "Bearer sam"})
    assert r.status_code == 200 and r.json()["cases"] == [
        {"id": CASE.id, "title": "t", "input": CASE.input, "runs": 1}]
    assert c.get("/v1/preflight/cases?agent=other", headers={"Authorization": "Bearer sam"}).json()["cases"] == []
    assert c.get("/v1/preflight/cases", headers={"Authorization": "Bearer sam"}).status_code == 400
    assert c.get("/v1/preflight/cases?agent=x").status_code == 401
