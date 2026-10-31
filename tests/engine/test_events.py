"""Event history (events.py, Gateway.record_event, GET /v1/events): what the trigger bridge received and did, kept as
history outside the chain and served scoped like everything else."""
import json
from datetime import datetime, timezone

import pytest
from starlette.testclient import TestClient

from clearance import api, events
from clearance.packs.hangar import HANGAR

from test_api import Auth, Groups, MemStore, Owns, TOWER, get

NOW = datetime(2026, 10, 31, 6, 0, tzinfo=timezone.utc)


def summary(eid="ev-1", app="gate-api", key="gate-api.pipelinerun.finished", **over):
    s = {"id": eid, "app": app, "routingKey": key, "receivedAt": "2026-10-31T05:59:00Z",
         "type": "dev.cdevents.pipelinerun.finished.0.2.0",
         "subject": {"id": "gate-api-push-abc", "content": {"outcome": "failure", "pipelineName": "build"}}}
    s.update(over)
    return s


def test_a_summary_becomes_a_bounded_record():
    r = events.from_summary(summary(), [{"agent": "triage-agent", "result": "launched", "session": "s-1", "run": "r-1", "extra": "x"},
                                        {"result": "no agent"}, {"agent": "other", "result": "braked"}], NOW)
    assert r.id == "ev-1" and r.app == "gate-api" and r.routing_key == "gate-api.pipelinerun.finished"
    assert r.received_at == datetime(2026, 10, 31, 5, 59, tzinfo=timezone.utc), "the bridge's receivedAt, not now"
    assert r.outcomes == [{"agent": "triage-agent", "result": "launched", "session": "s-1", "run": "r-1"},
                          {"agent": "other", "result": "braked"}], "unknown keys and agentless outcomes are dropped"
    assert r.view()["received_at"] == "2026-10-31T05:59:00+00:00"
    # A bad or missing time falls back to the clock; a huge subject is trimmed; no id is refused.
    assert events.from_summary(summary(receivedAt="soon"), [], NOW).received_at == NOW
    big = events.from_summary(summary(subject={"id": "x", "content": {f"k{i}": "v" * 100 for i in range(60)}}), [], NOW)
    assert len(json.dumps(big.subject, separators=(",", ":"))) <= events.MAX_SUBJECT_BYTES
    with pytest.raises(ValueError):
        events.from_summary(summary(id=""), [], NOW)


def test_the_memory_store_keeps_the_newest_and_dedupes_by_id():
    st = events.MemoryEvents(keep=3)
    for i in range(5):
        st.record(events.from_summary(summary(eid=f"ev-{i}", receivedAt=f"2026-10-31T05:0{i}:00Z"), [], NOW))
    st.record(events.from_summary(summary(eid="ev-4", receivedAt="2026-10-31T05:04:00Z"), [{"agent": "a", "result": "launched"}], NOW))
    assert [e.id for e in st.recent()] == ["ev-4", "ev-3", "ev-2"]
    assert st.recent()[0].outcomes[0]["result"] == "launched"


def test_the_gateway_records_an_event_without_touching_the_chain(gw, clock):
    assert gw.record_event(summary(), []) is False, "no history store: nothing kept, nothing fails"
    gw.events = events.MemoryEvents()
    head = len(gw.audit.records)
    assert gw.record_event(summary(), [{"agent": "triage-agent", "result": "launched"}]) is True
    assert [e.id for e in gw.events.recent()] == ["ev-1"]
    assert len(gw.audit.records) == head, "history is not evidence"


class EventsOnly(MemStore):
    def __init__(self, ev):
        super().__init__([], [], [], ev)

    def ping(self):
        pass


@pytest.fixture
def client(defs):
    access = api.Access(Auth(), defs, delegates=(TOWER,), notifiers=(TOWER,), auditors=("user:auditor",),
                        directory=Groups(), ownership=Owns())
    ev = [
        events.from_summary(summary(eid="ev-1", app="flight-api", key="flight-api.pipelinerun.finished", receivedAt="2026-10-31T05:01:00Z"),
                            [{"agent": "coding-agent", "result": "launched", "session": "s-9"}], NOW).view(),
        events.from_summary(summary(eid="ev-2", app="gate-api", receivedAt="2026-10-31T05:02:00Z"),
                            [{"agent": "orchestrator", "result": "braked"}], NOW).view(),
        events.from_summary(summary(eid="ev-3", app="baggage-api", key="baggage-api.pipelinerun.finished", receivedAt="2026-10-31T05:03:00Z"),
                            [], NOW).view(),
    ]
    from clearance.policy import Policy
    app = api.create_api_app(EventsOnly(ev), access, policy=Policy((HANGAR,)))
    with TestClient(app) as c:
        yield c


def test_events_are_scoped_filtered_and_paged(client):
    assert get(client, "/v1/events").status_code == 401
    assert get(client, "/v1/events", "tower").status_code == 403, "a notifier reads the inbox feed only"
    assert [e["id"] for e in get(client, "/v1/events", "audit").json()["items"]] == ["ev-3", "ev-2", "ev-1"]
    assert [e["id"] for e in get(client, "/v1/events", "carol").json()["items"]] == ["ev-1"], "an app owner sees her app's events"
    assert [e["id"] for e in get(client, "/v1/events", "bob").json()["items"]] == ["ev-2"], "a team sees its agent's outcomes"
    assert get(client, "/v1/events", "alice").json()["items"] == []
    assert [e["id"] for e in get(client, "/v1/events?app=GATE-API", "audit").json()["items"]] == ["ev-2"]
    assert [e["id"] for e in get(client, "/v1/events?agent=coding-agent", "audit").json()["items"]] == ["ev-1"]
    assert [e["id"] for e in get(client, "/v1/events?key=baggage-api.pipelinerun.finished", "audit").json()["items"]] == ["ev-3"]
    assert [e["id"] for e in get(client, "/v1/events?subject=gate-api-push-abc&since=2026-10-31T05:02:00Z", "audit").json()["items"]] == ["ev-3", "ev-2"]
    page = get(client, "/v1/events?limit=2", "audit").json()
    assert [e["id"] for e in page["items"]] == ["ev-3", "ev-2"] and page["next"] == "2026-10-31T05:02:00+00:00"
    assert [e["id"] for e in get(client, f"/v1/events?limit=2&after={page['next']}", "audit").json()["items"]] == ["ev-1"]
    assert get(client, "/v1/events?since=yesterday", "audit").status_code == 400
    assert any(op["operationId"] == "listEvents" for op in get(client, "/v1/contract").json()["operations"])
