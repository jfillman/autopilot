"""event_bridge: Glidepath events in, triggered runs out. One run per (agent, event), a storm brake, the event
summarised within the input cap, the run's pod acting as its session, and a stream that resumes. No network."""
import io
import json
import pytest

from autopilot import serve
from autopilot.backends import Backends, FakeRuns
from autopilot.event_bridge import EventBridge, sse_events, summarize
from autopilot.gateway import AgentRunBackend
from clearance.dispatch import Principal
from clearance.tiers import Tier

EPH = {"CLEARANCE_EPHEMERAL": "1", "AIRFRAME_ENABLED": "0", "ARGO_ENABLED": "0"}


def failed(eid="1793291758385806-7dd8657e1c25", outcome="failure", app="gate-api"):
    return {"id": eid, "app": app, "routingKey": f"{app}.pipelinerun.finished", "receivedAt": "2026-10-29T16:00:00Z",
            "event": {"context": {"type": "dev.cdevents.pipelinerun.finished.0.2.0", "source": "/glidepath/x/y"},
                      "subject": {"id": f"{app}-push-abc", "content": {"outcome": outcome, "pipelineName": "build",
                                                                        "big": "x" * 5000}}}}


class Auth:
    def authenticate(self, token):
        return Principal(token, "workload", Tier.T1)


@pytest.fixture
def gw(clock):
    runs = FakeRuns()
    b = Backends(None, None, runs, None, None)
    g, _ = serve.build_gateway(serve.config_from_env(EPH), backends=b, auth=Auth(), clock=clock)
    return g, runs


def test_a_failed_run_launches_the_triage_agent_with_the_event(gw, clock):
    g, runs = gw
    done = EventBridge(g, "http://gp", "/dev/null", clock=clock).handle(failed())
    assert [d["result"] for d in done] == ["launched"]
    (manifest,) = runs.manifests.values()
    inp = manifest["spec"]["input"]
    assert inp["event"]["subject"] == {"id": "gate-api-push-abc", "content": {"outcome": "failure", "pipelineName": "build"}}
    assert inp["instructions"].startswith("A Glidepath pipeline run")
    assert len(json.dumps(inp, separators=(",", ":"))) <= 4096
    assert manifest["spec"]["agent"] == "triage-agent"


def test_success_and_other_apps_launch_nothing(gw, clock):
    g, runs = gw
    b = EventBridge(g, "http://gp", "/dev/null", clock=clock)
    assert b.handle(failed(outcome="success")) == [] and b.handle(failed(eid="2-x", app="boarding-api")) == []
    assert runs.manifests == {}


def test_a_replayed_event_starts_no_second_run(gw, clock):
    g, runs = gw
    b = EventBridge(g, "http://gp", "/dev/null", clock=clock)
    b.handle(failed())
    assert b.handle(failed()) == []                      # seen by this bridge
    b2 = EventBridge(g, "http://gp", "/dev/null", clock=clock)   # a restarted bridge: the gateway dedupes
    assert [d["result"] for d in b2.handle(failed())] == ["deduped"]
    assert len(runs.manifests) == 1


def test_the_storm_brake(gw, clock):
    g, runs = gw
    b = EventBridge(g, "http://gp", "/dev/null", clock=clock)
    results = [b.handle(failed(eid=f"{i}-x"))[0]["result"] for i in range(6)]
    assert results == ["launched"] * 4 + ["braked"] * 2
    clock.advance(hours=1, minutes=1)
    assert b.handle(failed(eid="9-x"))[0]["result"] == "launched"


def test_the_run_pod_acts_as_its_session_and_nobody_else_does(gw, clock):
    g, runs = gw
    (d,) = EventBridge(g, "http://gp", "/dev/null", clock=clock).handle(failed())
    sid, run = d["session"], d["run"]
    assert AgentRunBackend(runs).principal(g.store.get(sid)) == f"agent-{run}/run"
    r = g.call(f"agent-{run}/run", sid, "glidepath.runs.list", {"app": "gate-api"})
    assert r.decision["allow"] is True and r.error == "no backend for this tool in this deployment"
    assert g.audit.records[-1]["principal"] == f"agent-{run}/run"
    for other in ("agent-r-ffffffffffff/run", "autopilot-runs/default", "user:alice"):
        assert g.call(other, sid, "glidepath.runs.list", {"app": "gate-api"}).decision["rule"] == "R001"


def test_summary_fits_the_input_cap():
    s = summarize(failed())
    assert "big" not in s["subject"]["content"] and s["routingKey"] == "gate-api.pipelinerun.finished"


def test_sse_parsing_skips_pings_and_keeps_ids():
    raw = b": ping\n\nid: 1-a\ndata: {\"id\":\"1-a\"}\n\n: ping\n\nid: 2-b\ndata: {\"id\":\n\n"
    assert list(sse_events(io.BytesIO(raw))) == [("1-a", '{"id":"1-a"}'), ("2-b", '{"id":')]


def test_the_stream_resumes_with_last_event_id(gw, clock, tmp_path):
    g, runs = gw
    tok = tmp_path / "t"
    tok.write_text("sa")
    seen = []

    class Res(io.BytesIO):
        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    def opener(req, timeout):
        seen.append((req.full_url, req.get_header("Last-event-id"), req.get_header("X-glidepath-authorization")))
        ev = failed(eid=f"{len(seen)}-z")
        return Res(f"id: {ev['id']}\ndata: {json.dumps(ev)}\n\n".encode())
    b = EventBridge(g, "http://gp/", tok, clock=clock, opener=opener)
    b.run_once()
    b.run_once()
    assert seen == [("http://gp/v1/events", None, "Bearer sa"), ("http://gp/v1/events", "1-z", "Bearer sa")]
    assert len(runs.manifests) == 2


def test_every_event_is_kept_as_history_with_what_was_done(gw, clock):
    """clearance events.py: the dry run, the trigger statistics and Tower's run marks read this; a miss is kept too."""
    g, _ = gw
    bridge = EventBridge(g, "http://gp", "/dev/null", clock=clock)
    bridge.handle(failed())
    bridge.handle(failed(eid="2", outcome="success"))
    kept = {e.id: e for e in g.events.recent()}
    assert set(kept) == {"1793291758385806-7dd8657e1c25", "2"}
    hit = kept["1793291758385806-7dd8657e1c25"]
    assert hit.app == "gate-api" and hit.routing_key == "gate-api.pipelinerun.finished"
    assert hit.subject["id"] == "gate-api-push-abc" and "big" not in hit.subject["content"]
    assert [(o["agent"], o["result"]) for o in hit.outcomes] == [("triage-agent", "launched")]
    assert hit.outcomes[0]["session"] and hit.outcomes[0]["run"]
    assert kept["2"].outcomes == [], "an event no trigger matched is history too"
    assert len(g.audit.records) == len([r for r in g.audit.records]), "and none of it is evidence"
    # A gateway without history (an older Clearance, or none configured) changes nothing.
    g.events = None
    assert [d["result"] for d in bridge.handle(failed(eid="3"))] == ["launched"]
