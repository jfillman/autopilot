"""human.request's inbox (inbox.py): non-blocking ask, read with human.request.get; answered by the session's owner
or the definition's owner group (from a Directory), directly or through a delegate; one answer, recorded."""
import json

import pytest
from starlette.testclient import TestClient

from clearance import inbox as hr
from clearance.audit import AuditLog
from clearance.dispatch import AuthError, Gateway, Principal
from clearance.packs.hangar import HANGAR
from clearance.server import create_http_app
from clearance.session import SessionStore
from clearance.tiers import Tier


class Auth:
    table = {"alice": Principal("user:alice", "user", Tier.T1), "bob": Principal("user:bob", "user", Tier.T1),
             "carol": Principal("user:carol", "user", Tier.T1),
             "tower": Principal("hangar-prod/app-backstage-prod/backstage", "workload", Tier.T1),
             "other-svc": Principal("ns/other", "workload", Tier.T1)}

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
                   packs=(HANGAR,), inbox=hr.MemoryInbox(), directory=Groups(),
                   delegates=("hangar-prod/app-backstage-prod/backstage",))


def ask(g, sid, **a):
    return g.call("alice", sid, "human.request", {"question": "Roll gate-api back?", **a})


def test_ask_returns_at_once_and_the_answer_is_read_later(g, defs):
    assert "human.request.get" in defs["orchestrator"].limits.tools, "granted with human.request"
    assert defs["orchestrator"].owner == "group:default/platform"
    sid = g.open_session("alice", "orchestrator").data["session"]
    r = ask(g, sid, options=["yes", "no"])
    assert r.ok and r.data["status"] == "pending" and "does not wait" in r.data["hint"]
    rid = r.data["request"]
    assert g.call("alice", sid, "human.request.get", {"request": rid}).data["status"] == "pending"
    [listed] = g.human_requests("alice")
    assert listed["id"] == rid and listed["options"] == ["yes", "no"] and listed["group"] == "group:default/platform"
    out = g.human_answer("alice", rid, "no")
    assert out["status"] == "answered" and out["answered_by"] == "user:alice" and out["answered_via"] is None
    got = g.call("alice", sid, "human.request.get", {"request": rid}).data
    assert got["answer"] == "no" and got["status"] == "answered"
    rec = g.audit.records[-2]
    assert rec["tool"] == "human.answer" and rec["principal"] == "user:alice" and rec["target"] == rid
    assert g.human_requests("alice") == []


def test_the_owner_group_answers_others_cannot(g):
    sid = g.open_session("alice", "orchestrator").data["session"]
    rid = ask(g, sid).data["request"]
    assert g.human_requests("carol") == []
    with pytest.raises(hr.InboxError) as e:
        g.human_answer("carol", rid, "go")
    assert e.value.status == 404
    assert [r["id"] for r in g.human_requests("bob")] == [rid]
    assert g.human_answer("bob", rid, "go")["answered_by"] == "user:bob"
    with pytest.raises(hr.InboxError) as e:
        g.human_answer("alice", rid, "again")
    assert e.value.rule == "HR-ANSWERED" and e.value.status == 409


def test_a_delegate_answers_for_the_person_it_names_and_only_a_delegate(g):
    sid = g.open_session("alice", "orchestrator").data["session"]
    rid = ask(g, sid).data["request"]
    for token, who, status in (("other-svc", "user:alice", 403), ("other-svc", None, 403), ("tower", "alice", 400)):
        with pytest.raises(hr.InboxError) as e:
            g.human_requests(token, on_behalf_of=who)
        assert e.value.status == status
    assert [r["id"] for r in g.human_requests("tower", on_behalf_of="user:alice")] == [rid]
    out = g.human_answer("tower", rid, "go", on_behalf_of="user:alice")
    assert out["answered_by"] == "user:alice" and out["answered_via"] == "hangar-prod/app-backstage-prod/backstage"
    assert g.audit.records[-1]["result"] == "answered via hangar-prod/app-backstage-prod/backstage"


def test_refusals(g, clock):
    sid = g.open_session("alice", "orchestrator").data["session"]
    for bad in ({"question": ""}, {"question": "x" * 2001}, {"question": "q", "options": ["a", "a"]},
                {"question": "q", "options": "yes"}):
        r = g.call("alice", sid, "human.request", bad)
        assert not r.ok and r.decision["rule"] in ("HR-QUESTION", "HR-OPTIONS", "R008")
    rid = ask(g, sid, options=["yes", "no"]).data["request"]
    with pytest.raises(hr.InboxError) as e:
        g.human_answer("alice", rid, "maybe")
    assert e.value.rule == "HR-ANSWER"
    for _ in range(hr.MAX_PENDING - 1):
        assert ask(g, sid).ok
    r = ask(g, sid)
    assert not r.ok and r.decision["rule"] == "HR-FLOOD"
    other = g.open_session("bob", "orchestrator").data["session"]
    r = g.call("bob", other, "human.request.get", {"request": rid})
    assert not r.ok and r.decision["rule"] == "HR-UNKNOWN", "another task's request is not visible"
    clock.advance(minutes=61)
    assert g.human_requests("alice") == []
    with pytest.raises(hr.InboxError) as e:
        g.human_answer("alice", rid, "yes")
    assert e.value.rule == "HR-EXPIRED" and e.value.status == 410


def test_http_routes(g):
    sid = g.open_session("alice", "orchestrator").data["session"]
    rid = ask(g, sid).data["request"]
    app = create_http_app(g, resource_url="https://clearance.example/mcp", issuer_url="https://dex.example/dex")
    with TestClient(app) as c:
        assert c.get("/v1/human/requests").status_code == 401
        assert c.get("/v1/human/requests", headers={"Authorization": "Bearer nope"}).status_code == 401
        r = c.get("/v1/human/requests", headers={"Authorization": "Bearer tower", "X-Hangar-On-Behalf-Of": "user:alice"})
        assert r.status_code == 200 and [x["id"] for x in r.json()] == [rid]
        assert c.get("/v1/human/requests", headers={"Authorization": "Bearer other-svc"}).status_code == 403
        r = c.post(f"/v1/human/requests/{rid}/answer", headers={"Authorization": "Bearer alice"},
                   content=json.dumps({"answer": "ship it"}))
        assert r.status_code == 200 and r.json()["answer"] == "ship it"
        r = c.post(f"/v1/human/requests/{rid}/answer", headers={"Authorization": "Bearer alice"},
                   content=json.dumps({"answer": "again"}))
        assert r.status_code == 409 and r.json()["rule"] == "HR-ANSWERED"
        assert c.post("/v1/human/requests/h-000000000000/answer", headers={"Authorization": "Bearer alice"},
                      content="{}").status_code == 404
