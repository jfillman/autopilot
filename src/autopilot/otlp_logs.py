"""The gateway's log lines to an OpenTelemetry collector (OTLP/HTTP, JSON), so they reach Loki.

Hangar ships logs to Loki only over OTLP: apps push them to the cluster's collector (apron 40-observability
otel-collector, its logs pipeline exporting to Loki's /otlp endpoint); nothing tails pod logs. This handler is the
gateway's push, with the standard library only (no OpenTelemetry SDK in the gateway's dependency set, and no injected
agent in the process that holds the model and GitHub credentials):

  OTEL_EXPORTER_OTLP_LOGS_ENDPOINT   the collector's logs URL, e.g. http://otel-collector...:4318/v1/logs (unset: off)
  OTEL_SERVICE_NAME                   service.name (default autopilot-gateway)
  OTEL_RESOURCE_ATTRIBUTES            more resource attributes, k=v,k=v (the pod's namespace and name, from the
                                      downward API), the standard variable's format

Every record still goes to stderr as before; this is a copy. It never blocks a request: emit() puts the record on a
bounded queue (QUEUE_MAX) and a daemon thread posts batches (BATCH_MAX records, or every FLUSH_SECONDS) with a
deadline; a full queue drops the record and counts it, and a failed post drops the batch and counts it, so a
collector outage costs log lines, never the gateway. The lines are the ones the gateway already writes to stderr,
which by rule carry no token, key or file content.
"""
from __future__ import annotations

import json
import logging
import os
import queue
import threading
import urllib.request
from typing import Callable, Mapping

QUEUE_MAX = 2000
BATCH_MAX = 200
FLUSH_SECONDS = 2.0
TIMEOUT = 5.0
MAX_BODY = 4096
SEVERITY = {logging.DEBUG: 5, logging.INFO: 9, logging.WARNING: 13, logging.ERROR: 17, logging.CRITICAL: 21}

Post = Callable[[str, bytes, float], int]


def _post(url: str, body: bytes, timeout: float) -> int:
    req = urllib.request.Request(url, data=body, method="POST", headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:  # noqa: S310 - the URL is configuration
        return r.status


def resource_attributes(env: Mapping[str, str], default_service: str) -> dict[str, str]:
    attrs = {}
    for pair in (env.get("OTEL_RESOURCE_ATTRIBUTES") or "").split(","):
        k, sep, v = pair.partition("=")
        if sep and k.strip() and v.strip():
            attrs[k.strip()] = v.strip()
    attrs["service.name"] = env.get("OTEL_SERVICE_NAME") or attrs.get("service.name") or default_service
    return attrs


def _attr(k: str, v: str) -> dict:
    return {"key": k, "value": {"stringValue": v}}


class OTLPLogHandler(logging.Handler):
    def __init__(self, endpoint: str, resource: dict[str, str], *, post: Post = _post,
                 flush_seconds: float = FLUSH_SECONDS, start: bool = True):
        super().__init__()
        self.endpoint, self.resource, self._post = endpoint, resource, post
        self.flush_seconds = flush_seconds
        self.q: queue.Queue[dict] = queue.Queue(QUEUE_MAX)
        self.dropped = 0          # records not sent: the queue was full, or their batch's post failed
        self.sent = 0
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._loop, name="otlp-logs", daemon=True)
        if start:
            self._thread.start()

    def emit(self, record: logging.LogRecord) -> None:
        try:
            body = record.getMessage()
        except Exception:  # noqa: BLE001 - a bad format string must not break logging
            body = str(record.msg)
        item = {"timeUnixNano": str(int(record.created * 1e9)),
                "severityNumber": SEVERITY.get(record.levelno, 9), "severityText": record.levelname,
                "body": {"stringValue": body[:MAX_BODY]},
                "attributes": [_attr("logger", record.name), _attr("thread", record.threadName or "")]}
        try:
            self.q.put_nowait(item)
        except queue.Full:
            self.dropped += 1

    def payload(self, records: list[dict]) -> bytes:
        return json.dumps({"resourceLogs": [{
            "resource": {"attributes": [_attr(k, v) for k, v in sorted(self.resource.items())]},
            "scopeLogs": [{"scope": {"name": "autopilot"}, "logRecords": records}]}]},
            separators=(",", ":")).encode()

    def flush_once(self) -> int:
        """Post what is queued now, in batches. Returns how many records were sent."""
        n = 0
        while True:
            batch = []
            while len(batch) < BATCH_MAX:
                try:
                    batch.append(self.q.get_nowait())
                except queue.Empty:
                    break
            if not batch:
                return n
            try:
                status = self._post(self.endpoint, self.payload(batch), TIMEOUT)
            except Exception:  # noqa: BLE001 - the collector being down costs these lines, never the gateway
                status = 0
            if 200 <= status < 300:
                self.sent += len(batch)
                n += len(batch)
            else:
                self.dropped += len(batch)

    def _loop(self) -> None:
        while not self._stop.wait(self.flush_seconds):
            self.flush_once()

    def close(self) -> None:
        self._stop.set()
        if self._thread.is_alive():
            self._thread.join(timeout=self.flush_seconds + TIMEOUT)
        self.flush_once()
        super().close()


def install(env: Mapping[str, str] | None = None, *, default_service: str = "autopilot-gateway",
            logger: logging.Logger | None = None) -> OTLPLogHandler | None:
    """Add the handler to the root logger when OTEL_EXPORTER_OTLP_LOGS_ENDPOINT is set; None when it is not."""
    env = os.environ if env is None else env
    endpoint = (env.get("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") or "").strip()
    if not endpoint:
        return None
    if not endpoint.startswith(("http://", "https://")):
        raise ValueError("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT is an http(s) URL")
    h = OTLPLogHandler(endpoint, resource_attributes(env, default_service))
    h.setLevel(logging.INFO)
    (logger or logging.getLogger()).addHandler(h)
    return h
