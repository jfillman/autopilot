"""The gateway's Prometheus metrics (A-5): the trigger bridge, the reconciliation sweep and the scheduler.

A few counters and gauges in Prometheus's text format (0.0.4), with no client library: the gateway takes no
dependency for a dozen series. They are served on their own port (METRICS_PORT, default 9090; 0 turns it off), not on
the MCP port, so the cluster Gateway's route to /mcp never exposes them, and NetworkPolicy can admit Prometheus to
this port alone. The server answers GET /metrics only, with a socket timeout on every connection.

What each series means, and the alerts on them, are in docs/admin/trigger-durability.md.
"""
from __future__ import annotations

import logging
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Iterable

log = logging.getLogger("autopilot.metrics")

LABEL_VALUE_MAX = 128


def _escape(v: str) -> str:
    v = v[:LABEL_VALUE_MAX]
    return v.replace("\\", "\\\\").replace("\n", "\\n").replace('"', '\\"')


class _Metric:
    kind = ""

    def __init__(self, registry: "Registry", name: str, help_: str, labels: Iterable[str] = ()):
        self.name, self.help, self.labels = name, help_, tuple(labels)
        self._values: dict[tuple[str, ...], float] = {}
        self._lock = threading.Lock()
        registry.add(self)

    def _key(self, labels: dict[str, str]) -> tuple[str, ...]:
        if set(labels) != set(self.labels):
            raise ValueError(f"{self.name} takes labels {self.labels}, not {tuple(labels)}")
        return tuple(str(labels[k]) for k in self.labels)

    def value(self, **labels: str) -> float:
        return self._values.get(self._key(labels), 0.0)

    def render(self) -> list[str]:
        out = [f"# HELP {self.name} {self.help}", f"# TYPE {self.name} {self.kind}"]
        with self._lock:
            items = sorted(self._values.items())
        # An unlabelled counter starts at 0; a gauge has no sample until it is set, so an alert on a timestamp gauge
        # never fires for a feature that is off or has not run yet.
        if not items and not self.labels and self.kind == "counter":
            items = [((), 0.0)]
        for key, v in items:
            ls = ",".join(f'{k}="{_escape(x)}"' for k, x in zip(self.labels, key))
            out.append(f"{self.name}{{{ls}}} {v:g}" if ls else f"{self.name} {v:g}")
        return out


class Counter(_Metric):
    kind = "counter"

    def inc(self, n: float = 1.0, **labels: str) -> None:
        k = self._key(labels)
        with self._lock:
            self._values[k] = self._values.get(k, 0.0) + n


class Gauge(_Metric):
    kind = "gauge"

    def set(self, v: float, **labels: str) -> None:
        k = self._key(labels)
        with self._lock:
            self._values[k] = float(v)


class Registry:
    def __init__(self):
        self.metrics: list[_Metric] = []

    def add(self, m: _Metric) -> None:
        if any(x.name == m.name for x in self.metrics):
            raise ValueError(f"metric {m.name} registered twice")
        self.metrics.append(m)

    def render(self) -> str:
        return "\n".join(line for m in self.metrics for line in m.render()) + "\n"


class TriggerMetrics:
    """The series the bridge, the sweep and the scheduler write. One instance per process (serve.main); tests make
    their own."""

    def __init__(self, registry: Registry | None = None):
        r = self.registry = registry or Registry()
        self.events_received = Counter(r, "autopilot_events_received_total",
                                       "Glidepath events the trigger bridge handled, by how they arrived.", ["via"])
        self.stream_connected = Gauge(r, "autopilot_events_stream_connected",
                                      "1 while the bridge holds glidepath-api's event stream.")
        self.last_event = Gauge(r, "autopilot_events_last_received_timestamp_seconds",
                                "Unix time the bridge last handled an event.")
        self.cursor_time = Gauge(r, "autopilot_events_cursor_timestamp_seconds",
                                 "Receive time carried by the bridge's saved cursor (the newest event id handled).")
        self.resumes = Counter(r, "autopilot_events_resume_total",
                               "Catch-ups from the saved cursor at (re)connect, by result: caught-up, "
                               "stale-cursor (older than glidepath-api's buffer: events may be gone), failed.",
                               ["result"])
        self.outcomes = Counter(r, "autopilot_trigger_outcomes_total",
                                "What a trigger firing became, per agent, by path and result.",
                                ["agent", "via", "result"])
        self.missed = Counter(r, "autopilot_trigger_missed_total",
                              "Trigger firings no event delivered, found by the reconciliation sweep (the gap), by "
                              "what the sweep decided: launched, or held, braked, refused, launch-failed.",
                              ["agent", "result"])
        self.sweeps = Counter(r, "autopilot_reconcile_sweeps_total",
                              "Reconciliation sweeps, by result (ok, failed).", ["result"])
        self.sweep_ok = Gauge(r, "autopilot_reconcile_last_success_timestamp_seconds",
                              "Unix time of the last reconciliation sweep that read glidepath-api's run records.")
        self.slot_launches = Counter(r, "autopilot_schedule_launches_total",
                                     "Interval slots fired, per agent, by result.", ["agent", "result"])
        self.slot_missed = Counter(r, "autopilot_schedule_missed_total",
                                   "Interval slots whose grace window passed with no firing (the gateway was down).",
                                   ["agent"])
        self.slot_lag = Gauge(r, "autopilot_schedule_lag_seconds",
                              "Seconds between the start of an agent's last fired slot and its firing.", ["agent"])


class _Handler(BaseHTTPRequestHandler):
    registry: Registry
    timeout = 10   # seconds per socket operation: a slow client cannot hold a thread

    def do_GET(self):  # noqa: N802 - the stdlib's name
        if self.path.split("?", 1)[0] != "/metrics":
            self.send_error(404)
            return
        body = self.registry.render().encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):   # no access log per scrape
        pass


def serve(registry: Registry, port: int, host: str = "0.0.0.0") -> ThreadingHTTPServer:  # noqa: S104 - in a pod
    """Serve GET /metrics on a daemon thread; returns the server (shutdown() stops it)."""
    handler = type("MetricsHandler", (_Handler,), {"registry": registry})
    srv = ThreadingHTTPServer((host, port), handler)
    srv.daemon_threads = True
    threading.Thread(target=srv.serve_forever, name="autopilot-metrics", daemon=True).start()
    log.info("metrics on :%d/metrics", srv.server_address[1])
    return srv
