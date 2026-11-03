"""v0.25.0: an in-cluster run hands back its result as it exits (run_finish, Gateway.finish_run). The answer is kept
30 days by session, the session keeps what it was asked and how it ended, and a run that ends without a result is
closed by the sweep from what the run backend says."""
from datetime import datetime, timedelta

import pytest

from clearance import control as ctl
from clearance import results as res
from clearance.audit import AuditLog
from clearance.backends import FakeRunBackend
from clearance.dispatch import Gateway, Principal
from clearance.packs.hangar import HANGAR
from clearance.server import create_server
from clearance.session import SessionStore
from clearance.tiers import Tier

from test_agent_runs import TOWER, Auth, Groups
from test_server import payload, run


class Runs(FakeRunBackend):
    """The run pod authenticates as agent-<run>/run (autopilot's AgentRunBackend.principal), and says how it ended."""
    def __init__(self):
        super().__init__()
        self.phases = {}

    def principal(self, s):
        return f"agent-{s.run}/run" if s.run else None

    def ended(self, s):
        return self.phases.get(s.run)


class RunAuth(Auth):
    def authenticate(self, token):
        if token.startswith("agent-"):
            return Principal(token, "workload", Tier.T1)
        return super().authenticate(token)


@pytest.fixture
def g(defs, clock):
    return Gateway(defs, SessionStore(), AuditLog(), RunAuth(), clock, packs=(HANGAR,), runs=Runs(),
                   directory=Groups(), delegates=(TOWER,), admins=("user:root",), results=res.MemoryResults())


def started(g, task="what languages can I build apps in?"):
    out = g.run_agent("root", "researcher", task)
    return g.store.get(out["session"])


def finish(g, s, **body):
    return g.finish_run(f"agent-{s.run}/run", s.id, {"status": "done", "final_text": "Python and Go.", "turns": 2,
                                                       "tool_calls": [{"tool": "catalog.read", "ok": True, "rule": None}],
                                                       **body})


def test_the_session_keeps_what_the_run_was_asked(g):
    s = started(g)
    assert s.input == {"prompt": "what languages can I build apps in?", "started_by": "user:root"}
    assert s.to_record()["input"] == s.input and "instructions" not in s.input


def test_the_run_finishes_its_session_and_its_answer_is_kept_30_days(g, clock):
    s = started(g)
    out = finish(g, s)
    assert out["closed"] and g.store.get(s.id).closed
    o = s.outcome
    assert o["status"] == "done" and o["turns"] == 2 and o["tool_calls"] == 1 and o["not_ok"] == 0 and o["via"] == "runtime"
    assert o["result"]["bytes"] == len("Python and Go.") and len(o["result"]["sha256"]) == 64
    rec = g.audit.records[-1]
    assert rec["tool"] == "run.finish" and rec["result"] == "done" and "Python" not in str(rec)
    kept = g.results.get(s.id, clock())
    assert kept["final_text"] == "Python and Go." and kept["tool_calls"] == [{"tool": "catalog.read", "ok": True, "rule": None}]
    clock.advance(days=29)
    assert g.results.get(s.id, clock())
    clock.advance(days=2)
    assert g.results.get(s.id, clock()) is None


def test_the_sweep_then_deletes_the_finished_run(g):
    s = started(g)
    finish(g, s)
    g.expire()
    assert [r for r in g.audit.records if r["tool"] == "run.delete"][-1]["result"] == "deleted"
    assert g.store.get(s.id).run is None


def test_only_the_run_or_its_principal_finishes_once(g):
    s = started(g)
    with pytest.raises(ctl.ControlError) as e:
        g.finish_run("sam", s.id, {"status": "done"})
    assert e.value.rule == "R001"
    with pytest.raises(ctl.ControlError) as e:
        g.finish_run("agent-r-other/run", s.id, {"status": "done"})
    assert e.value.rule == "R001"
    finish(g, s)
    with pytest.raises(ctl.ControlError) as e:
        finish(g, s)
    assert e.value.rule == "R004"


def test_a_bad_result_is_refused_and_a_long_answer_cut(g, clock):
    s = started(g)
    with pytest.raises(ctl.ControlError) as e:
        finish(g, s, status="great")
    assert e.value.rule == "R008"
    with pytest.raises(ctl.ControlError):
        finish(g, s, turns="2")
    assert not g.store.get(s.id).closed
    finish(g, s, final_text="é" * 20000)
    kept = g.results.get(s.id, clock())
    assert kept["truncated"] and len(kept["final_text"].encode()) <= res.MAX_TEXT_BYTES
    assert s.outcome["result"]["truncated"]


def test_a_run_that_ends_without_a_result_is_closed_by_the_sweep(g):
    s = started(g)
    g.expire()
    assert not g.store.get(s.id).closed                       # still running: the backend has no phase yet
    g.runs.phases[s.run] = "Succeeded"
    g.expire()
    assert s.closed and s.outcome["status"] == "no-result" and s.outcome["run_phase"] == "Succeeded"
    assert [r["result"] for r in g.audit.records if r["tool"] == "run.finish"] == ["no-result"]


def test_a_session_past_its_deadline_has_the_outcome_expired(g, clock):
    s = started(g)
    clock.advance(hours=2)
    g.expire()
    assert s.closed and s.outcome["status"] == "expired" and s.outcome["via"] == "deadline"


def test_run_finish_through_mcp(g):
    s = started(g)
    srv = create_server(g, lambda: f"agent-{s.run}/run")
    out = payload(run(srv.call_tool("run_finish", {"session": s.id, "status": "done", "final_text": "ok", "turns": 1})))
    assert out["ok"] and out["closed"]
    again = payload(run(srv.call_tool("run_finish", {"session": s.id, "status": "done"})))
    assert not again["ok"] and again["rule"] == "R004"
