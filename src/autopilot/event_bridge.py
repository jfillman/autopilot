"""The event trigger bridge: Glidepath events in, triggered agent runs out (Glidepath API spec P5, Autopilot M4 part 10).

It holds one stream of glidepath-api's `GET /v1/events` (server-sent events; glidepath docs/admin/api.md "Events")
as Clearance's own identity, which reads events through glidepath-api's reader role. For each event it finds the
agent definitions with a Glidepath event trigger that matches (triggers.match_glidepath_event: the bindingKey against
the event's routingKey, and each `match` path against the CDEvent), and for each one, under that trigger's storm
brake, opens a triggered session and launches its run with the event as input.

What makes this safe to run unattended:
  - Exactly-once per (agent, event) while a run is live: the task id is a hash of the agent and the event id
    (triggers.event_task_id), and Gateway.open_triggered answers a live duplicate with the run already started.
    A resume replays up to 10 s of events (and replicas interleave), so ids seen recently are also skipped here.
  - A storm brake per agent: at most `maxPerHour` runs an hour (default 6). An event over the limit is dropped and
    logged; the stream is for reacting, and the run history is in Glidepath, not here.
  - The run's identity is the definition's own workload identity, never the event's sender: the trigger is
    recorded, not trusted (Gateway.open_triggered).
  - What reaches the run is a summary of the event (its id, app, routing key, type and subject), within the
    AgentRun's 4 KiB input cap together with the definition's instructions.

The stream is best-effort: on any error it reconnects with Last-Event-ID after a backoff. Events received while the
gateway was down are not replayed beyond what glidepath-api's buffer holds.
"""
from __future__ import annotations

import json
import logging
import threading
import urllib.error
import urllib.request
from collections import OrderedDict
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Iterable, Iterator

from . import triggers

log = logging.getLogger("autopilot.events")

DEFAULT_PER_HOUR = 6
SEEN = 5000
MAX_SUBJECT = 1500


def summarize(envelope: dict[str, Any]) -> dict[str, Any]:
    """The part of an event a run gets: enough to find the run in Glidepath, small enough for the input cap."""
    ev = envelope.get("event") or {}
    subject = ev.get("subject") or {}
    content = subject.get("content") or {}
    small = {k: v for k, v in content.items() if isinstance(v, (str, int, float, bool)) and len(str(v)) <= 300}
    out = {"id": envelope.get("id"), "app": envelope.get("app"), "routingKey": envelope.get("routingKey"),
           "receivedAt": envelope.get("receivedAt"), "type": (ev.get("context") or {}).get("type"),
           "subject": {"id": subject.get("id"), "content": small}}
    while len(json.dumps(out["subject"])) > MAX_SUBJECT and out["subject"]["content"]:
        out["subject"]["content"].popitem()
    return out


def sse_events(lines: Iterable[bytes]) -> Iterator[tuple[str | None, str]]:
    """(id, data) per server-sent event; comments (`: ping`) and other fields are skipped."""
    eid, data = None, []
    for raw in lines:
        line = raw.decode("utf-8", "replace").rstrip("\r\n")
        if not line:
            if data:
                yield eid, "\n".join(data)
            eid, data = None, []
        elif line.startswith(":"):
            continue
        elif line.startswith("id:"):
            eid = line[3:].strip()
        elif line.startswith("data:"):
            data.append(line[5:].lstrip(" "))


class EventBridge:
    def __init__(self, gw: Any, base_url: str, token_file: str | Path, *, clock: Callable[[], datetime] | None = None,
                 timeout: float = 60.0, backoff: float = 5.0, opener: Callable[..., Any] | None = None):
        self.gw, self.base, self.token_file = gw, base_url.rstrip("/"), Path(token_file)
        self.now = clock or (lambda: datetime.now(timezone.utc))
        self.timeout, self.backoff = timeout, backoff
        self._open = opener or (lambda req, t: urllib.request.urlopen(req, timeout=t))  # noqa: S310 - configuration
        self.last_id: str | None = None
        self._seen: OrderedDict[str, None] = OrderedDict()
        self._brakes: dict[str, triggers.RateLimiter] = {}
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._loop, name="clearance-events", daemon=True)

    # -- one event ----------------------------------------------------------------------------------------------
    def handle(self, envelope: dict[str, Any]) -> list[dict[str, Any]]:
        """Start every run this event triggers; returns what was done per agent (for logs and tests)."""
        eid = envelope.get("id")
        if not isinstance(eid, str) or not eid:
            return []
        if eid in self._seen:
            return []
        self._seen[eid] = None
        while len(self._seen) > SEEN:
            self._seen.popitem(last=False)
        done = []
        for agent, trig in triggers.match_glidepath_event(self.gw.defs, envelope):
            brake = self._brakes.setdefault(agent, triggers.RateLimiter(int(trig.get("maxPerHour", DEFAULT_PER_HOUR))))
            if not brake.allow(self.now()):
                log.warning("event %s: %s over its storm brake (%d/h); dropped", eid, agent, brake.per_hour)
                done.append({"agent": agent, "result": "braked"})
                continue
            r = self.gw.open_triggered(agent, f"glidepath:{eid}", task_id=triggers.event_task_id(agent, eid))
            if not r.ok:
                done.append({"agent": agent, "result": "refused", "rule": r.decision.get("rule")})
                continue
            if r.data.get("deduped"):
                done.append({"agent": agent, "result": "deduped", "session": r.session})
                continue
            d = self.gw.defs[agent]
            run_input = {"event": summarize(envelope)}
            if d.instructions:
                run_input["instructions"] = d.instructions
            launched = self.gw.launch(r.session, run_input)
            done.append({"agent": agent, "result": "launched" if launched.ok else "launch-failed",
                         "session": r.session, "run": (launched.data or {}).get("run")})
        for d in done:
            log.info("event %s %s: %s %s", eid, envelope.get("routingKey"), d["agent"], d["result"])
        return done

    # -- the stream -----------------------------------------------------------------------------------------------
    def _request(self) -> urllib.request.Request:
        token = self.token_file.read_text().strip()
        headers = {"Accept": "text/event-stream", "Authorization": f"Bearer {token}",
                   "X-Glidepath-Authorization": f"Bearer {token}"}
        if self.last_id:
            headers["Last-Event-ID"] = self.last_id
        return urllib.request.Request(self.base + "/v1/events", headers=headers, method="GET")

    def run_once(self) -> None:
        """Hold one stream until it ends or fails. Every event is handled; a failure handling one is logged."""
        with self._open(self._request(), self.timeout) as res:
            for eid, data in sse_events(res):
                if self._stop.is_set():
                    return
                try:
                    envelope = json.loads(data)
                except ValueError:
                    continue
                if eid:
                    self.last_id = eid
                try:
                    self.handle(envelope)
                except Exception as e:  # noqa: BLE001 - one bad event must not end the stream
                    log.error("event %s: %s", eid, type(e).__name__)

    def start(self) -> "EventBridge":
        self._thread.start()
        return self

    def stop(self) -> None:
        self._stop.set()

    def _loop(self) -> None:
        while not self._stop.is_set():
            try:
                self.run_once()
            except (OSError, urllib.error.URLError) as e:
                log.warning("event stream: %s; reconnecting in %gs", type(e).__name__, self.backoff)
            except Exception as e:  # noqa: BLE001 - the bridge must outlive any one failure
                log.error("event stream: %s; reconnecting in %gs", type(e).__name__, self.backoff)
            self._stop.wait(self.backoff)
