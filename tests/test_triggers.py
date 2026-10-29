from datetime import timedelta

import pytest

from autopilot import triggers

from conftest import T0


def test_interval_parsing():
    assert triggers.parse_interval("15m") == timedelta(minutes=15)
    assert triggers.parse_interval("24h") == timedelta(hours=24)
    for bad in ("0m", "5s", "h", "15"):
        with pytest.raises(ValueError):
            triggers.parse_interval(bad)


def test_scheduled_agents_are_due_when_never_run_or_elapsed(defs):
    assert triggers.due(defs, {}, T0) == ["nightly-reviewer"]
    assert triggers.due(defs, {"nightly-reviewer": T0 - timedelta(hours=23)}, T0) == []
    assert triggers.due(defs, {"nightly-reviewer": T0 - timedelta(hours=24)}, T0) == ["nightly-reviewer"]


def test_alert_triggers_match_on_all_labels(defs):
    import dataclasses
    defs = {**defs, "triage-agent": dataclasses.replace(
        defs["triage-agent"], triggers=({"type": "alert", "match": {"alertname": "RolloutDegraded"}},))}
    assert triggers.match_alert(defs, {"alertname": "RolloutDegraded", "namespace": "x"}) == ["triage-agent"]
    assert triggers.match_alert(defs, {"alertname": "Other"}) == []
    assert triggers.match_alert(defs, {}) == []


def test_only_agents_that_declare_a_trigger_fire(defs):
    assert "coding-agent" not in triggers.due(defs, {}, T0)


FAILED = {"id": "1-a", "app": "gate-api", "routingKey": "gate-api.pipelinerun.finished",
          "event": {"context": {"type": "dev.cdevents.pipelinerun.finished.0.2.0"},
                    "subject": {"id": "gate-api-push-x", "content": {"outcome": "failure", "pipelineName": "build"}}}}


def test_glidepath_events_match_binding_key_and_cdevent_paths(defs):
    assert [a for a, _ in triggers.match_glidepath_event(defs, FAILED)] == ["triage-agent"]
    ok = {**FAILED, "event": {**FAILED["event"], "subject": {"content": {"outcome": "success"}}}}
    assert triggers.match_glidepath_event(defs, ok) == []
    other = {**FAILED, "routingKey": "boarding-api.pipelinerun.finished"}
    assert triggers.match_glidepath_event(defs, other) == []
    started = {**FAILED, "routingKey": "gate-api.pipelinerun.started"}
    assert triggers.match_glidepath_event(defs, started) == []
    assert triggers.match_glidepath_event(defs, {"routingKey": "gate-api.pipelinerun.finished"}) == []


def test_rabbitmq_matching_ignores_glidepath_triggers(defs):
    assert triggers.match_event(defs, "glidepath", "gate-api.pipelinerun.finished") == []
