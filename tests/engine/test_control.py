"""Person controls (control.py, Gateway.freeze_session / end_session / hold_triggers / release_triggers, the gateway's
routes, the API's `hold` field): who may act, what each does, and that each is an evidence record."""
import json
from datetime import timedelta

import pytest
from starlette.testclient import TestClient

from clearance import api, control as ctl
from clearance import inbox as hr
from clearance.audit import AuditLog
from clearance.dispatch import AuthError, Gateway, Principal
from clearance.packs.hangar import HANGAR
from clearance.server import create_http_app
from clearance.session import SessionStore
from clearance.tiers import Tier

TOWER = "hangar-prod/app-backstage-prod/backstage"


class Auth:
    table = {"alice": Principal("user:alice", "user", Tier.T1), "bob": Principal("user:bob", "user", Tier.T1),
             "carol": Principal("user:carol", "user", Tier.T1), "root": Principal("user:root", "user", Tier.T1),
             "tower": Principal(TOWER, "workload", Tier.T1), "other-svc": Principal("ns/other", "workload", Tier.T1)}

    def authenticate(self, token):
        if token not in self.table:
            raise AuthError("no")
        return self.table[token]


class Groups:
    def groups(self, principal):
        return {"group:default/platform"} if principal == "user:bob" else set()


@pytest.fixture
def g(defs, clock):
    counter = iter(range(1, 1000))
    return Gateway(defs, SessionStore(id_gen=lambda: f"s-{next(counter):06x}"), AuditLog(), Auth(), clock,
                   packs=(HANGAR,), inbox=hr.MemoryInbox(), directory=Groups(), delegates=(TOWER,),
                   holds=ctl.MemoryHolds(), admins=("user:root",))


def tools(g):
    return [r["tool"] for r in g.audit.records]


def test_who_may_freeze_a_session(g):
    sid = g.open_session("alice", "coding-agent").data["session"]      # coding-agent has no owner group
    with pytest.raises(ctl.ControlError) as e:
        g.freeze_session("carol", sid, "looks wrong")
    assert e.value.rule == "CT-FORBIDDEN" and e.value.status == 403
    with pytest.raises(ctl.ControlError) as e:
        g.freeze_session("alice", sid, "")
    assert e.value.rule == "CT-REASON"
    out = g.freeze_session("alice", sid, "it is looping")                # her own session
    assert out["tripped"] is True and out["trip_reason"] == "frozen by user:alice: it is looping"
    rec = g.audit.records[out["audit_seq"]]
    assert (rec["tool"], rec["principal"], rec["session"], rec["decision"]) == ("session.freeze", "user:alice", sid, "allow")
    assert not g.call("alice", sid, "metrics.query", {"promql": "up"}).ok, "a frozen session refuses every call"
    with pytest.raises(ctl.ControlError) as e:
        g.freeze_session("alice", sid, "again")
    assert e.value.rule == "CT-TRIPPED" and e.value.status == 409
    with pytest.raises(ctl.ControlError) as e:
        g.freeze_session("alice", "s-nope", "x")
    assert e.value.status == 404


def test_the_owner_group_an_admin_or_a_delegate_ends_a_session(g):
    sid = g.open_session("alice", "orchestrator").data["session"]       # owner group:default/platform (bob)
    with pytest.raises(ctl.ControlError):
        g.end_session("carol", sid, "stop")
    out = g.end_session("tower", sid, "stop it", on_behalf_of="user:bob")
    assert out["closed"] is True
    rec = g.audit.records[out["audit_seq"]]
    assert rec["tool"] == "session.end" and rec["principal"] == "user:bob" and rec["result"] == f"ended: stop it via {TOWER}"
    with pytest.raises(ctl.ControlError) as e:
        g.end_session("root", sid, "twice")
    assert e.value.rule == "CT-CLOSED"
    # A delegate must be listed, and must name a person.
    sid2 = g.open_session("alice", "orchestrator").data["session"]
    with pytest.raises(ctl.ControlError) as e:
        g.end_session("other-svc", sid2, "x", on_behalf_of="user:bob")
    assert e.value.rule == "CT-DELEGATE" and e.value.status == 403
    assert g.end_session("root", sid2, "admin ends it")["closed"] is True


def test_a_hold_stops_triggers_until_it_expires_or_is_released(g, clock):
    assert g.open_triggered("triage-agent", "glidepath:e1").ok, "no hold yet"
    with pytest.raises(ctl.ControlError) as e:
        g.hold_triggers("bob", "triage-agent", "noisy")          # triage-agent has no owner group: admins only
    assert e.value.rule == "CT-FORBIDDEN"
    with pytest.raises(ctl.ControlError) as e:
        g.hold_triggers("root", "triage-agent", "noisy", hours=0)
    assert e.value.rule == "CT-HOURS"
    with pytest.raises(ctl.ControlError) as e:
        g.hold_triggers("root", "nope", "x")
    assert e.value.status == 404

    hold = g.hold_triggers("tower", "triage-agent", "it opens bad PRs", hours=2, on_behalf_of="user:root")
    assert hold["by"] == "user:root" and hold["via"] == TOWER and hold["until"] == (clock() + timedelta(hours=2)).isoformat()
    r = g.open_triggered("triage-agent", "glidepath:e2")
    assert not r.ok and r.decision["rule"] == "R024" and "held by user:root until" in r.error
    assert tools(g)[-2:] == ["trigger.hold", "session.open"]
    assert g.audit.records[-1]["decision"] == "deny" and g.audit.records[-1]["rule"] == "R024"

    # Renewing replaces; expiry lifts it by itself; releasing lifts it early and is recorded.
    assert g.hold_triggers("root", "triage-agent", "still")["until"] == (clock() + timedelta(hours=24)).isoformat()
    clock.advance(hours=25)
    assert g.hold_for("triage-agent") is None
    assert g.open_triggered("triage-agent", "glidepath:e3").ok
    with pytest.raises(ctl.ControlError) as e:
        g.release_triggers("root", "triage-agent")
    assert e.value.rule == "CT-NOHOLD"
    g.hold_triggers("root", "triage-agent", "once more")
    out = g.release_triggers("root", "triage-agent")
    assert out["released"] is True and g.audit.records[out["audit_seq"]]["tool"] == "trigger.release"
    assert g.open_triggered("triage-agent", "glidepath:e4").ok


def test_without_a_hold_store_nothing_is_held(defs, clock):
    g = Gateway(defs, SessionStore(), AuditLog(), Auth(), clock, packs=(HANGAR,), admins=("user:root",))
    assert g.hold_for("triage-agent") is None
    with pytest.raises(ctl.ControlError) as e:
        g.hold_triggers("root", "triage-agent", "x")
    assert e.value.rule == "CT-OFF"


def test_http_routes(g):
    sid = g.open_session("alice", "orchestrator").data["session"]
    app = create_http_app(g, resource_url="https://clearance.example/mcp", issuer_url="https://dex.example/dex")
    with TestClient(app) as c:
        h = lambda t, who=None: {"Authorization": f"Bearer {t}", **({"X-Hangar-On-Behalf-Of": who} if who else {})}  # noqa: E731
        assert c.post(f"/v1/sessions/{sid}/freeze", content="{}").status_code == 401
        r = c.post(f"/v1/sessions/{sid}/freeze", headers=h("carol"), content=json.dumps({"reason": "x"}))
        assert r.status_code == 403 and r.json()["rule"] == "CT-FORBIDDEN"
        r = c.post(f"/v1/sessions/{sid}/freeze", headers=h("tower", "user:bob"), content=json.dumps({"reason": "looping"}))
        assert r.status_code == 200 and r.json()["tripped"] is True
        r = c.post(f"/v1/sessions/{sid}/end", headers=h("alice"), content=json.dumps({"reason": "done"}))
        assert r.status_code == 200 and r.json()["closed"] is True
        r = c.post("/v1/agents/triage-agent/hold", headers=h("root"), content=json.dumps({"reason": "noisy", "hours": 3}))
        assert r.status_code == 200 and r.json()["agent"] == "triage-agent"
        assert c.post("/v1/agents/triage-agent/hold", headers=h("root"), content="not json").status_code == 400
        assert c.delete("/v1/agents/triage-agent/hold", headers=h("bob")).status_code == 403
        assert c.delete("/v1/agents/triage-agent/hold", headers=h("root")).status_code == 200
        assert c.delete("/v1/agents/triage-agent/hold", headers=h("root")).status_code == 404


def test_the_read_api_shows_the_hold(defs):
    from test_api import Auth as ApiAuth, Groups as ApiGroups, MemStore, Owns, TOWER as API_TOWER, get
    hold = {"agent": "triage-agent", "by": "user:root", "via": None, "reason": "noisy", "since": "2026-10-31T06:00:00+00:00", "until": "2026-10-31T08:00:00+00:00"}
    access = api.Access(ApiAuth(), defs, delegates=(API_TOWER,), auditors=("user:auditor",), directory=ApiGroups(), ownership=Owns())
    app = api.create_api_app(MemStore([], [], [], holds={"triage-agent": hold}), access)
    with TestClient(app) as c:
        items = {a["name"]: a for a in get(c, "/v1/agents", "alice").json()["items"]}
        assert items["triage-agent"]["hold"] == hold
        assert items["coding-agent"]["hold"] is None
        assert get(c, "/v1/agents/triage-agent", "alice").json()["hold"]["by"] == "user:root"
