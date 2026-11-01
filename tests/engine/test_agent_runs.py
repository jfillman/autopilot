"""v0.23.0: every definition says what it is for (description) and where its loop runs (runs_in: cluster with a
runtime image, client without one); placeholder digests are refused; a person starts an in-cluster run of a task
agent (Gateway.run_agent, POST /v1/agents/{name}/runs); an MCP client can list the agents (agents_list)."""
import copy

import pytest
import yaml
from starlette.testclient import TestClient

from clearance import control as ctl
from clearance import inbox as hr
from clearance import profile, review
from clearance.audit import AuditLog
from clearance.dispatch import AuthError, Gateway, Principal
from clearance.packs.hangar import HANGAR
from clearance.server import create_http_app
from clearance.session import SessionStore
from clearance.tiers import Tier

from conftest import AGENTS

TOWER = "hangar-prod/app-backstage-prod/backstage"


class Auth:
    table = {"tower": Principal(TOWER, "workload", Tier.T1), "root": Principal("user:root", "user", Tier.T1),
             "sam": Principal("user:sam", "user", Tier.T1)}

    def authenticate(self, token):
        if token not in self.table:
            raise AuthError("no")
        return self.table[token]


class Groups:
    def groups(self, principal):
        return {"user:root": {"group:default/admins"}}.get(principal, set())


def doc(name="researcher"):
    return yaml.safe_load((AGENTS / f"{name}.yaml").read_text())


@pytest.fixture
def g(defs, backends, clock):
    return Gateway(defs, SessionStore(), AuditLog(), Auth(), clock, packs=(HANGAR,), runs=backends.runs,
                   inbox=hr.MemoryInbox(), directory=Groups(), delegates=(TOWER,), admins=("user:root",),
                   holds=ctl.MemoryHolds())


def test_a_definition_needs_a_description():
    d = doc()
    del d["description"]
    with pytest.raises(profile.DefinitionError):
        profile.parse(d)
    assert [p.rule for p in review.validate(d, {})] == ["DF-DESCRIPTION"]
    assert profile.parse(doc()).description.startswith("Investigates")


def test_a_placeholder_digest_is_refused_and_no_image_means_client():
    d = doc()
    d["runtime"]["image"] = "ghcr.io/x/agent@sha256:" + "0" * 64
    with pytest.raises(profile.DefinitionError, match="placeholder"):
        profile.parse(d)
    assert review.validate(d, {})[0].rule == "DF-IMAGE"
    assert profile.parse(doc()).runs_in == "cluster"
    client = doc()
    del client["runtime"]
    assert profile.parse(client).runs_in == "client"


def test_the_read_api_and_mcp_say_what_each_agent_is_for(g):
    from clearance.api import _definition_view
    v = _definition_view(g.defs["researcher"], full=False)
    assert v["description"].startswith("Investigates") and v["runsIn"] == "cluster"
    listed = {a["name"]: a for a in g.agents_for("sam")}
    assert listed["researcher"]["can_open_session"] and not listed["triage-agent"]["can_open_session"]
    assert "instructions" not in listed["triage-agent"]


def test_an_admin_runs_a_task_agent_through_the_delegate(g, backends):
    out = g.run_agent("tower", "researcher", "why is gate-api slow?", on_behalf_of="user:root")
    assert out["by"] == "user:root" and out["via"] == TOWER and out["run"]
    s = g.store.get(out["session"])
    assert s.principal == "user:root" and s.run == out["run"]
    tools = [r["tool"] for r in g.audit.records]
    assert tools[-2:] == ["session.open", "run.launch"]


def test_who_may_run_and_what_may_be_run(g):
    with pytest.raises(ctl.ControlError) as e:
        g.run_agent("tower", "researcher", on_behalf_of="user:sam")          # not the owner group, not an admin
    assert e.value.rule == "CT-FORBIDDEN"
    with pytest.raises(ctl.ControlError) as e:
        g.run_agent("root", "triage-agent")                                    # an event agent
    assert e.value.rule == "R001"
    with pytest.raises(ctl.ControlError) as e:
        g.run_agent("root", "nope")
    assert e.value.rule == "R003"
    with pytest.raises(ctl.ControlError) as e:
        g.run_agent("root", "researcher", "x" * 2001)
    assert e.value.rule == "R008"
    g.hold_triggers("root", "researcher", "maintenance", 1)
    with pytest.raises(ctl.ControlError) as e:
        g.run_agent("root", "researcher")
    assert e.value.rule == "R024"


def test_a_client_only_agent_is_not_run(defs, backends, clock):
    d = copy.deepcopy(doc())
    del d["runtime"]
    defs = {**defs, "researcher": profile.parse(d)}
    g = Gateway(defs, SessionStore(), AuditLog(), Auth(), clock, packs=(HANGAR,), runs=backends.runs,
                directory=Groups(), delegates=(TOWER,), admins=("user:root",))
    with pytest.raises(ctl.ControlError) as e:
        g.run_agent("root", "researcher")
    assert e.value.rule == "R001" and "MCP client" in e.value.message


def test_the_route(g):
    app = create_http_app(g, resource_url="http://x/mcp", issuer_url="http://x")
    c = TestClient(app)
    r = c.post("/v1/agents/researcher/runs", json={"input": "look"},
               headers={"Authorization": "Bearer tower", "X-Hangar-On-Behalf-Of": "user:root"})
    assert r.status_code == 201 and r.json()["agent"] == "researcher"
    r = c.post("/v1/agents/researcher/runs", json={}, headers={"Authorization": "Bearer sam"})
    assert r.status_code == 403 and r.json()["rule"] == "CT-FORBIDDEN"
