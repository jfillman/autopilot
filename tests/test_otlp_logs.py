"""otlp_logs: a copy of the gateway's log lines to the OpenTelemetry collector (OTLP/HTTP JSON), so they reach Loki.
Never blocks a request, never raises into logging, drops (and counts) what it cannot send."""
import json
import logging

import pytest

from autopilot import otlp_logs


class Collector:
    def __init__(self, status=200, fail=False):
        self.status, self.fail, self.posts = status, fail, []

    def __call__(self, url, body, timeout):
        if self.fail:
            raise OSError("connection refused")
        self.posts.append((url, json.loads(body), timeout))
        return self.status


def handler(post, env=None):
    h = otlp_logs.OTLPLogHandler("http://collector:4318/v1/logs",
                                 otlp_logs.resource_attributes(env or {}, "autopilot-gateway"), post=post, start=False)
    log = logging.getLogger(f"t.{id(h)}")
    log.propagate, log.level = False, logging.INFO
    log.addHandler(h)
    return h, log


def test_records_are_posted_as_otlp_json_with_the_resource():
    c = Collector()
    env = {"OTEL_RESOURCE_ATTRIBUTES": "k8s.namespace.name=autopilot-system,k8s.pod.name=clearance-abc"}
    h, log = handler(c, env)
    log.warning("expired %d session(s)", 2)
    assert h.flush_once() == 1 and h.sent == 1
    url, body, timeout = c.posts[0]
    assert url == "http://collector:4318/v1/logs" and timeout == otlp_logs.TIMEOUT
    [rl] = body["resourceLogs"]
    attrs = {a["key"]: a["value"]["stringValue"] for a in rl["resource"]["attributes"]}
    assert attrs == {"service.name": "autopilot-gateway", "k8s.namespace.name": "autopilot-system",
                     "k8s.pod.name": "clearance-abc"}
    [rec] = rl["scopeLogs"][0]["logRecords"]
    assert rec["body"]["stringValue"] == "expired 2 session(s)"
    assert rec["severityText"] == "WARNING" and rec["severityNumber"] == 13 and int(rec["timeUnixNano"]) > 0


def test_service_name_comes_from_otel_service_name():
    assert otlp_logs.resource_attributes({"OTEL_SERVICE_NAME": "autopilot-api"}, "autopilot-gateway")["service.name"] == "autopilot-api"


def test_batches_are_bounded():
    c = Collector()
    h, log = handler(c)
    for i in range(otlp_logs.BATCH_MAX + 5):
        log.info("line %d", i)
    assert h.flush_once() == otlp_logs.BATCH_MAX + 5
    assert [len(p[1]["resourceLogs"][0]["scopeLogs"][0]["logRecords"]) for p in c.posts] == [otlp_logs.BATCH_MAX, 5]


def test_a_collector_outage_drops_lines_and_never_raises():
    h, log = handler(Collector(fail=True))
    log.error("one")
    assert h.flush_once() == 0 and h.dropped == 1
    h2, log2 = handler(Collector(status=503))
    log2.error("two")
    assert h2.flush_once() == 0 and h2.dropped == 1


def test_a_full_queue_drops_instead_of_blocking(monkeypatch):
    monkeypatch.setattr(otlp_logs, "QUEUE_MAX", 2)
    h, log = handler(Collector())
    for i in range(5):
        log.info("x")
    assert h.dropped == 3 and h.flush_once() == 2


def test_long_lines_are_cut():
    c = Collector()
    h, log = handler(c)
    log.info("y" * (otlp_logs.MAX_BODY + 100))
    h.flush_once()
    assert len(c.posts[0][1]["resourceLogs"][0]["scopeLogs"][0]["logRecords"][0]["body"]["stringValue"]) == otlp_logs.MAX_BODY


def test_install_is_off_without_an_endpoint_and_refuses_a_bad_one():
    assert otlp_logs.install({}) is None
    with pytest.raises(ValueError):
        otlp_logs.install({"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "collector:4318"})
    root = logging.getLogger("t.install")
    h = otlp_logs.install({"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://c:4318/v1/logs"}, logger=root)
    try:
        assert h in root.handlers and h.endpoint == "http://c:4318/v1/logs"
    finally:
        root.removeHandler(h)
        h._post = Collector()
        h.close()


def test_debug_lines_stay_on_stderr():
    root = logging.getLogger("t.level")
    h = otlp_logs.install({"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://c:4318/v1/logs"}, logger=root)
    try:
        root.setLevel(logging.DEBUG)
        root.debug("noisy")
        root.info("kept")
        assert h.q.qsize() == 1
    finally:
        root.removeHandler(h)
        h._post = Collector()
        h.close()
