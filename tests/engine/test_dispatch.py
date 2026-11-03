import pytest
from datetime import timedelta

from clearance.audit import verify

PR = {"repo": "jfillman/flight-api", "branch": "agent/t-1", "title": "fix probe",
      "files": [{"path": "charts/values.yaml", "content": "x"}]}


def open_(gw, token="alice", agent="coding-agent", req=None):
    r = gw.open_session(token, agent, req)
    assert r.ok, r.as_dict()
    return r.data["session"]


def test_allowed_call_reaches_the_backend_exactly_once_and_is_audited(gw, backends):
    sid = open_(gw)
    r = gw.call("alice", sid, "repo.pr.open", PR)
    assert r.ok and r.data["pr"] == 1
    assert backends.git.names() == ["git.open_pr"]
    last = gw.audit.records[-1]
    assert last["tool"] == "repo.pr.open" and last["decision"] == "allow" and last["session"] == sid
    assert "files" not in str(last) and len(last["args_sha256"]) == 64


def test_denied_call_never_touches_the_backend_and_returns_a_fix_hint(gw, backends):
    sid = open_(gw)
    bad = {**PR, "files": [{"path": ".tekton/pr.yaml", "content": "x"}]}
    r = gw.call("alice", sid, "repo.pr.open", bad)
    assert not r.ok and r.decision["rule"] == "R015" and r.decision["hint"]
    assert backends.git.calls == []
    assert gw.audit.records[-1]["decision"] == "deny" and gw.audit.records[-1]["rule"] == "R015"


def test_budget_is_charged_only_on_allow(gw):
    sid = open_(gw)
    s = gw.store.get(sid)
    gw.call("alice", sid, "repo.pr.open", {**PR, "files": [{"path": ".tekton/pr.yaml", "content": "x"}]})
    assert s.used["tool_calls"] == 0
    gw.call("alice", sid, "repo.pr.open", PR)
    assert s.used["tool_calls"] == 1 and s.used["github_calls"] == 4 and s.used["open_prs"] == 1


def test_tripwire_denies_audits_and_trips_the_breaker(gw, backends):
    sid = open_(gw)
    r = gw.call("alice", sid, "secrets.read", {"name": "x"})
    assert r.decision["rule"] == "R002"
    r2 = gw.call("alice", sid, "catalog.read", {"query": "x"})
    assert r2.decision["rule"] == "R005", "one tripwire attempt ends the session's ability to act"
    assert backends.read.calls == []


def test_repeated_denials_trip_the_breaker(gw):
    sid = open_(gw)
    for _ in range(5):
        gw.call("alice", sid, "no.such.tool", {})
    assert gw.call("alice", sid, "catalog.read", {"query": "x"}).decision["rule"] == "R005"


def test_a_different_principal_cannot_use_my_session(gw, backends):
    sid = open_(gw)
    r = gw.call("bob", sid, "catalog.read", {"query": "x"})
    assert r.decision["rule"] == "R001" and backends.read.calls == []


def test_unknown_session_is_denied_and_audited(gw):
    r = gw.call("alice", "s-nope", "catalog.read", {"query": "x"})
    assert not r.ok and gw.audit.records[-1]["decision"] == "deny"


def test_session_expires(gw, clock):
    sid = open_(gw)
    clock.advance(minutes=31)
    assert gw.call("alice", sid, "catalog.read", {"query": "x"}).decision["rule"] == "R004"


def test_backend_failure_is_reported_without_leaking_internals(gw, backends):
    def boom(*a, **k):
        raise RuntimeError("token ghp_SECRET leaked in message")
    backends.git.open_pr = boom
    sid = open_(gw)
    r = gw.call("alice", sid, "repo.pr.open", PR)
    assert not r.ok and r.error == "backend error" and "ghp_SECRET" not in str(r.as_dict())
    assert gw.audit.records[-1]["result"] == "error:RuntimeError"


def test_workload_identity_must_match_the_definition(gw):
    assert gw.open_session("holmes", "triage-agent").ok
    assert not gw.open_session("alice", "triage-agent").ok
    assert not gw.open_session("holmes", "coding-agent").ok       # delegated agents need a user
    assert not gw.open_session("alice", "no-such-agent").ok


def test_open_session_limits_cannot_widen(gw):
    r = gw.open_session("alice", "coding-agent", {"tier_ceiling": "T2"})
    assert not r.ok and r.decision["rule"] == "R007"
    r = gw.open_session("alice", "coding-agent", {"ttl_minutes": 31})
    assert not r.ok


def test_every_call_leaves_exactly_one_audit_record_and_the_chain_holds(gw):
    sid = open_(gw)
    n = len(gw.audit.records)
    gw.call("alice", sid, "catalog.read", {"query": "x"})
    gw.call("alice", sid, "k8s.exec", {})
    gw.call("alice", sid, "catalog.read", {"query": "y"})   # denied by the tripped breaker
    assert len(gw.audit.records) == n + 3
    assert verify(gw.audit.records) == (True, None)


# ---- teams: run.spawn / run.close --------------------------------------------------
def test_spawn_creates_a_narrower_child_and_an_agentrun_xr(gw, backends):
    sid = open_(gw, agent="orchestrator")
    r = gw.call("alice", sid, "run.spawn", {"agent": "researcher", "limits": {"tool_calls": 30}})
    assert r.ok, r.as_dict()
    child = gw.store.get(r.data["session"])
    assert child.limits.tool_calls == 30 and child.parent_id == sid and child.task_id == gw.store.get(sid).task_id
    assert backends.runs.live[child.id] == r.data["run"] and backends.runs.names() == ["runs.create"]


def test_spawn_that_widens_is_denied_with_r007_and_creates_nothing(gw, backends):
    sid = open_(gw, agent="orchestrator")
    r = gw.call("alice", sid, "run.spawn", {"agent": "researcher", "limits": {"tier_ceiling": "T1"}})
    assert not r.ok and r.decision["rule"] == "R007"
    assert backends.runs.live == {}


def test_spawn_child_cannot_exceed_what_the_parent_has_left(gw):
    sid = open_(gw, agent="orchestrator", req={"tool_calls": 50})
    r = gw.call("alice", sid, "run.spawn", {"agent": "researcher", "limits": {"tool_calls": 90}})
    assert not r.ok and r.decision["rule"] == "R007"


def test_spawn_limit_is_enforced(gw):
    sid = open_(gw, agent="orchestrator")          # children: 3
    for _ in range(3):
        assert gw.call("alice", sid, "run.spawn", {"agent": "researcher", "limits": {"tool_calls": 10}}).ok
    r = gw.call("alice", sid, "run.spawn", {"agent": "researcher", "limits": {"tool_calls": 10}})
    assert not r.ok and r.decision["rule"] == "R016"


def test_close_deletes_the_run_and_only_my_own(gw, backends):
    sid = open_(gw, agent="orchestrator")
    r = gw.call("alice", sid, "run.spawn", {"agent": "researcher", "limits": {"tool_calls": 10}})
    other = open_(gw, token="bob", agent="orchestrator")
    assert not gw.call("bob", other, "run.close", {"run_id": r.data["session"]}).ok
    c = gw.call("alice", sid, "run.close", {"run_id": r.data["session"]})
    assert c.ok and backends.runs.live == {}


# ---- scheduled and event-driven agents ----------------------------------------------
def test_triggered_workload_agents_run_as_their_own_identity(gw):
    r = gw.open_triggered("triage-agent", "alert:RolloutDegraded")
    assert r.ok
    s = gw.store.get(r.data["session"])
    assert s.principal == "holmesgpt/holmes"
    assert gw.audit.records[-1]["principal"] == "holmesgpt/holmes"


def test_delegated_and_task_agents_cannot_be_triggered(gw):
    assert not gw.open_triggered("coding-agent", "cron").ok
    assert not gw.open_triggered("nope", "cron").ok


# ---- expiry deletes runs (Hangar D20) ----------------------------------------------------------
def test_expire_deletes_each_expired_run_with_a_record_and_leaves_live_ones(gw, backends, clock):
    sid = open_(gw, agent="orchestrator")
    child = gw.call("alice", sid, "run.spawn", {"agent": "researcher", "limits": {"tool_calls": 5}}).data
    other = open_(gw, token="bob", agent="orchestrator")
    clock.advance(minutes=24 * 60)
    fresh = open_(gw, token="bob", agent="orchestrator")
    fresh_child = gw.call("bob", fresh, "run.spawn", {"agent": "researcher", "limits": {"tool_calls": 5}}).data
    assert other and fresh_child
    done = gw.expire()
    assert child["session"] in done and fresh_child["session"] not in done
    assert backends.runs.names().count("runs.delete") == 1
    rec = [r for r in gw.audit.records if r["tool"] == "run.delete"]
    assert len(rec) == 1 and rec[0]["session"] == child["session"] and rec[0]["result"] == "deleted"
    assert gw.store.get(child["session"]).run is None
    assert gw.expire() == [], "a deleted run is not deleted again"


def test_a_failed_delete_is_recorded_and_retried(gw, backends, clock):
    sid = open_(gw, agent="orchestrator")
    child = gw.call("alice", sid, "run.spawn", {"agent": "researcher", "limits": {"tool_calls": 5}}).data
    clock.advance(minutes=24 * 60)
    real = backends.runs.delete

    def down(s):
        raise OSError("api server down")
    backends.runs.delete = down
    assert gw.expire() == [child["session"]]
    assert [r["result"] for r in gw.audit.records if r["tool"] == "run.delete"] == ["error:OSError"]
    backends.runs.delete = real
    gw.expire()
    assert [r["result"] for r in gw.audit.records if r["tool"] == "run.delete"] == ["error:OSError", "deleted"]


def test_closing_a_run_deletes_its_childrens_runs_too(gw, backends):
    sid = open_(gw, agent="orchestrator")
    child = gw.call("alice", sid, "run.spawn", {"agent": "orchestrator", "limits": {"tool_calls": 20}}).data
    grand = gw.call("alice", child["session"], "run.spawn", {"agent": "researcher", "limits": {"tool_calls": 5}})
    assert grand.ok, grand.as_dict()
    assert gw.call("alice", sid, "run.close", {"run_id": child["session"]}).ok
    assert backends.runs.names().count("runs.delete") == 2


def _with_file_get(gw):
    import dataclasses
    d = gw.defs["coding-agent"]
    gw.defs["coding-agent"] = dataclasses.replace(d, limits=dataclasses.replace(d.limits, tools=d.limits.tools | {"repo.file.get"}))


def test_repo_file_get_is_scoped_to_the_definitions_repos(gw, backends):
    _with_file_get(gw)
    sid = gw.open_session("alice", "coding-agent").data["session"]
    r = gw.call("alice", sid, "repo.file.get", {"repo": "jfillman/flight-api", "path": "src/app.py"})
    assert r.ok, r.as_dict()
    assert backends.git.calls[-1] == ("git.read_file", ("jfillman/flight-api", "src/app.py", "main"), {})
    r = gw.call("alice", sid, "repo.file.get", {"repo": "someone/else", "path": "a"})
    assert not r.ok and r.decision["rule"] == "R014"


@pytest.mark.parametrize("path", ["/etc/passwd", "../x", "a/../../b", "a\\b", 7])
def test_repo_file_get_refuses_a_path_outside_the_repo(gw, path):
    _with_file_get(gw)
    sid = gw.open_session("alice", "coding-agent").data["session"]
    r = gw.call("alice", sid, "repo.file.get", {"repo": "jfillman/flight-api", "path": path})
    assert not r.ok and r.decision["rule"] == "R008", r.as_dict()


@pytest.mark.parametrize("args, expect", [({}, ""), ({"path": ""}, ""), ({"path": "."}, ""), ({"path": "src"}, "src")])
def test_repo_file_get_lists_the_root_without_a_path(gw, backends, args, expect):
    _with_file_get(gw)
    sid = gw.open_session("alice", "coding-agent").data["session"]
    r = gw.call("alice", sid, "repo.file.get", {"repo": "jfillman/flight-api", **args})
    assert r.ok, r.as_dict()
    assert backends.git.calls[-1][1][1] == expect
