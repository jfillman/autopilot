from datetime import timedelta

import pytest

from clearance import triggers

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
    assert triggers.match_alert(defs, {"alertname": "RolloutDegraded", "namespace": "x"}) == ["triage-agent"]
    assert triggers.match_alert(defs, {"alertname": "Other"}) == []
    assert triggers.match_alert(defs, {}) == []


def test_only_agents_that_declare_a_trigger_fire(defs):
    assert "coding-agent" not in triggers.due(defs, {}, T0)
