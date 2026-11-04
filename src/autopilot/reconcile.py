"""The reconciliation sweep (A-5): a `pipelinerun.finished` the event bridge never received still launches, once.

glidepath-api's event buffer is in memory (glidepath known gap #44): when every replica restarts at once, or the
gateway is down longer than the buffer reaches back, events are gone and nothing says so. The run records are not:
glidepath-api's `GET /v1/runs` reads Tekton Results, 30 days of finished PipelineRuns. Every RECONCILE_SECONDS the
sweep reads the finished runs of the last RECONCILE_WINDOW_HOURS for the apps a `pipelinerun.finished` trigger can
match, rebuilds the event each one's finally block sent (envelope_from_run), and hands it to the bridge as `via
reconcile`. The bridge claims its trigger key in the ledger: a run whose event arrived was claimed then, and loses
here; a run whose event never arrived is claimed now, launched under the same storm brake and hold, counted in
autopilot_trigger_missed_total{agent,result} (the gap's metric) and kept in the event history under id
`reconcile:<app>/<run>`.

Two bounds keep it from launching what it should not:
  - the ledger's floor (gateway_event_cursors.floor_at): runs that finished before the bridge first kept a ledger are
    never reconciled, so turning A-5 on does not replay a day of failures;
  - SETTLE: a run that finished in the last two minutes is left to its event, which the finally block sends before
    the run ends and Results archives it.

The rebuilt event has what the triggers match on: the routing key (`<app>.pipelinerun.finished`), the subject id
(the PipelineRun's name), `subject.content.outcome` (succeeded, failed, cancelled to success, failure, cancel, as
glidepath's cdevents_map_outcome does) and `pipelineName` (the run's Pipeline). A trigger that matches another CDEvent
field the run record does not carry will not match a rebuilt event; none does today (agents/triage-agent.yaml
matches `subject.content.outcome`).
"""
from __future__ import annotations

import logging
import re
import threading
from datetime import datetime, timedelta, timezone
from typing import Any, Callable

from . import triggers

log = logging.getLogger("autopilot.reconcile")

SETTLE = timedelta(minutes=2)
INTERVAL_SECONDS = 300.0
WINDOW_HOURS = 24
RUNS_LIMIT = 1000                     # GET /v1/runs's maximum
EVENT_TYPE = "dev.cdevents.pipelinerun.finished.0.3.0"
OUTCOMES = {"succeeded": "success", "failed": "failure", "cancelled": "cancel"}   # anything else: error


_NANOS = re.compile(r"(\.\d{6})\d+")


def _time(s: Any) -> datetime | None:
    """An RFC 3339 time as Tekton Results writes it (nanoseconds, `Z`), or None."""
    if not isinstance(s, str) or not s:
        return None
    try:
        t = datetime.fromisoformat(_NANOS.sub(r"\1", s).replace("Z", "+00:00"))
    except ValueError:
        return None
    return t if t.tzinfo else t.replace(tzinfo=timezone.utc)


def envelope_from_run(row: dict[str, Any]) -> dict[str, Any] | None:
    """The `pipelinerun.finished` envelope a finished run (a GET /v1/runs row) sent, or None when the row lacks what
    the key needs (an app and a name)."""
    app, name, ns = row.get("app"), row.get("name"), row.get("namespace")
    if not (isinstance(app, str) and app and isinstance(name, str) and name):
        return None
    content: dict[str, Any] = {"outcome": OUTCOMES.get(str(row.get("status")), "error")}
    if isinstance(row.get("pipeline"), str) and row["pipeline"]:
        content["pipelineName"] = row["pipeline"]
    return {"id": f"reconcile:{app}/{name}", "app": app, "routingKey": f"{app}.pipelinerun.finished",
            "receivedAt": row.get("endTime"),
            "event": {"context": {"type": EVENT_TYPE, "source": f"/glidepath/{ns}/{name}" if ns else None},
                      "subject": {"id": name, "type": "pipelineRun", "content": content}}}


def watched_apps(defs: Any) -> tuple[bool, set[str]]:
    """(any, apps): whether any Glidepath trigger can match a `<app>.pipelinerun.finished`, and the literal apps
    named; an empty set with any=True means a wildcard app (read the whole fleet)."""
    found, apps, wildcard = False, set(), False
    for d in defs.values():
        for t in d.triggers:
            if t.get("type") != "event" or t.get("source") != "glidepath":
                continue
            pat = str(t.get("bindingKey", ""))
            first = pat.split(".", 1)[0]
            probe = ("x" if first in ("*", "#") else first) + ".pipelinerun.finished"
            if not triggers.amqp_topic_match(pat, probe):
                continue
            found = True
            if first in ("*", "#"):
                wildcard = True
            else:
                apps.add(first)
    return found, (set() if wildcard else apps)


class Reconciler:
    def __init__(self, bridge: Any, *, clock: Callable[[], datetime] | None = None,
                 interval: float = INTERVAL_SECONDS, window_hours: int = WINDOW_HOURS):
        self.bridge = bridge
        self.now = clock or bridge.now
        self.interval, self.window_hours = interval, window_hours
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._loop, name="autopilot-reconcile", daemon=True)

    def _runs(self, apps: set[str]) -> list[dict[str, Any]]:
        rows: list[dict[str, Any]] = []
        for app in (sorted(apps) or [None]):
            page = self.bridge.get_json("/v1/runs", {"app": app, "sinceHours": self.window_hours, "limit": RUNS_LIMIT})
            got = [r for r in (page.get("runs") or []) if isinstance(r, dict)] if isinstance(page, dict) else []
            if len(got) >= RUNS_LIMIT:
                log.warning("reconcile: %s returned %d runs, the most it gives: older runs in the window are not "
                            "checked", app or "the fleet", RUNS_LIMIT)
            rows += got
        return rows

    def sweep(self) -> list[dict[str, Any]]:
        """One pass: what was newly decided (each a missed trigger). Raises when glidepath-api cannot be read."""
        m = self.bridge.metrics
        found, apps = watched_apps(self.bridge.gw.defs)
        if not found:
            m.sweeps.inc(result="ok")
            m.sweep_ok.set(self.now().timestamp())
            return []
        now = self.now()
        floor = self.bridge._db(self.bridge.ledger.cursor, "glidepath", now).floor
        try:
            rows = self._runs(apps)
        except Exception:
            m.sweeps.inc(result="failed")
            raise
        candidates = []
        for row in rows:
            end = _time(row.get("endTime"))
            if end is None or end < floor or now - end < SETTLE:
                continue
            env = envelope_from_run(row)
            if env is not None:
                candidates.append((end, env))
        new = []
        for _, env in sorted(candidates, key=lambda c: c[0]):    # oldest first, as the stream would have
            for d in self.bridge.handle(env, via="reconcile"):
                if d.get("new"):
                    new.append({**d, "event": env["id"]})
        if new:
            log.warning("reconcile: %d trigger(s) no event delivered: %s", len(new),
                        ", ".join(f"{d['agent']} {d['event']} {d['result']}" for d in new))
        self.bridge._db(self.bridge.ledger.prune, now)
        m.sweeps.inc(result="ok")
        m.sweep_ok.set(self.now().timestamp())
        return new

    def start(self) -> "Reconciler":
        self._thread.start()
        return self

    def stop(self) -> None:
        self._stop.set()

    def _loop(self) -> None:
        # The first sweep waits one interval: at start the bridge's own catch-up (/v1/events/recent) goes first.
        while not self._stop.wait(self.interval):
            try:
                self.sweep()
            except Exception as e:  # noqa: BLE001 - one failed sweep must not end the thread
                log.error("reconcile sweep failed: %s", type(e).__name__)
