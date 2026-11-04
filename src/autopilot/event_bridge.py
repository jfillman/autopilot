"""The event trigger bridge: Glidepath events in, triggered agent runs out (Glidepath API spec P5, Autopilot M4 part 10;
durable since A-5).

It holds one stream of glidepath-api's `GET /v1/events` (server-sent events; glidepath docs/admin/api.md "Events")
as the gateway's own identity, which reads events through glidepath-api's reader role. For each event it finds the
agent definitions with a Glidepath event trigger that matches (triggers.match_glidepath_event: the bindingKey against
the event's routingKey, and each `match` path against the CDEvent), and for each one, under that trigger's storm
brake, opens a triggered session and launches its run with the event as input.

What makes this safe to run unattended:
  - Once per (agent, trigger key), across restarts, replays and the reconciliation sweep: the key
    (engine/trigger_ledger.trigger_key) is claimed in the gateway's database before anything is opened, and the
    decision is written back to the same row; a second claim loses. A live duplicate is also answered by
    Gateway.open_triggered, whose task id is a hash of the agent and the key (triggers.event_task_id).
  - A cursor in the gateway's database (gateway_event_cursors): the newest event id handled, saved after each event.
    On start the bridge first reads `/v1/events/recent?since=<cursor>` and handles what it missed, then holds the
    stream with `Last-Event-ID`. A cursor older than glidepath-api's buffer (24 h) counts as `stale-cursor` in
    autopilot_events_resume_total: events may be gone, and the reconciliation sweep (reconcile.py) is what recovers a
    missed `pipelinerun.finished`.
  - A storm brake per agent: at most `maxPerHour` runs an hour (default 6). An event over the limit is dropped,
    recorded as `braked` in the ledger (so no later path launches it either) and logged.
  - The run's identity is the definition's own workload identity, never the event's sender: the trigger is
    recorded, not trusted (Gateway.open_triggered).
  - What reaches the run is a summary of the event (its id, app, routing key, type and subject), within the
    AgentRun's 4 KiB input cap together with the definition's instructions.
"""
from __future__ import annotations

import json
import logging
import threading
import urllib.error
import urllib.parse
import urllib.request
from collections import OrderedDict
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Callable, Iterable, Iterator

from . import triggers
from .engine import trigger_ledger as tl
from .metrics import TriggerMetrics

log = logging.getLogger("autopilot.events")

DEFAULT_PER_HOUR = 6
SEEN = 5000
MAX_SUBJECT = 1500
SOURCE = "glidepath"                 # the cursor's row in gateway_event_cursors
BUFFER_AGE = timedelta(hours=24)     # glidepath-api's api.events.maxAge: a cursor older than this may have lost events
RECENT_LIMIT = 10000                 # /v1/events/recent's maximum; the buffer holds 2000 by default
MAX_JSON = 8 << 20                   # bytes read from a JSON answer at most


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


def id_time(event_id: str | None) -> datetime | None:
    """The receive time a glidepath-api event id carries (`<unix microseconds, 16 digits>-<hash>`), or None."""
    if not isinstance(event_id, str):
        return None
    head, sep, _ = event_id.partition("-")
    if not sep or len(head) != 16 or not head.isdigit():
        return None
    return datetime.fromtimestamp(int(head) / 1e6, tz=timezone.utc)


class EventBridge:
    def __init__(self, gw: Any, base_url: str, token_file: str | Path, *, clock: Callable[[], datetime] | None = None,
                 timeout: float = 60.0, backoff: float = 5.0, opener: Callable[..., Any] | None = None,
                 ledger: Any = None, metrics: TriggerMetrics | None = None):
        self.gw, self.base, self.token_file = gw, base_url.rstrip("/"), Path(token_file)
        self.now = clock or (lambda: datetime.now(timezone.utc))
        self.timeout, self.backoff = timeout, backoff
        self._open = opener or (lambda req, t: urllib.request.urlopen(req, timeout=t))  # noqa: S310 - configuration
        self.ledger = ledger if ledger is not None else (getattr(gw, "triggers", None) or tl.MemoryTriggers())
        self.metrics = metrics or TriggerMetrics()
        self.last_id: str | None = None
        self.started = self.now()
        self._resumed = False
        self._seen: OrderedDict[str, None] = OrderedDict()
        self._brakes: dict[str, triggers.RateLimiter] = {}
        self._handling = threading.Lock()   # the stream and the sweep hand events in from two threads
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._loop, name="autopilot-events", daemon=True)

    def _db(self, fn: Callable[..., Any], *a: Any) -> Any:
        """A ledger call under the gateway's lock (serve.SerializedGateway.guarded), so it never interleaves with a
        gateway operation on the one connection."""
        guarded = getattr(self.gw, "guarded", None)
        return guarded(fn, *a) if guarded is not None else fn(*a)

    # -- one event ----------------------------------------------------------------------------------------------
    def handle(self, envelope: dict[str, Any], via: str = "stream") -> list[dict[str, Any]]:
        """Start every run this event triggers; returns what was done per agent (for logs and tests). Each outcome
        says `new` when this call made the decision, false when the ledger already had one."""
        with self._handling:
            return self._handle(envelope, via)

    def _handle(self, envelope: dict[str, Any], via: str) -> list[dict[str, Any]]:
        eid = envelope.get("id")
        if not isinstance(eid, str) or not eid:
            return []
        if eid in self._seen:
            return []
        key = tl.trigger_key(envelope)
        done: list[dict[str, Any]] = []
        for agent, trig in triggers.match_glidepath_event(self.gw.defs, envelope):
            now = self.now()
            if not self._db(self.ledger.claim, agent, key, via, eid, now, tl.stale_before(self.started, now)):
                row = self._db(self.ledger.get, agent, key) or {}
                done.append({"agent": agent, "result": "deduped", "via": via, "new": False,
                             "replay": row.get("event_id") == eid, "session": row.get("session"), "run": row.get("run")})
                continue
            out = self._fire(agent, trig, envelope, key, via)
            self._db(self.ledger.decide, agent, key, out["result"], out.get("session"), out.get("run"))
            done.append(out)
        self._seen[eid] = None
        while len(self._seen) > SEEN:
            self._seen.popitem(last=False)
        for d in done:
            self.metrics.outcomes.inc(agent=d["agent"], via=via, result=d["result"])
            log.info("event %s %s via %s: %s %s%s", eid, envelope.get("routingKey"), via, d["agent"], d["result"],
                     "" if d["new"] else " (already decided)")
        if via == "reconcile":
            if any(d["new"] for d in done):
                for d in done:
                    if d["new"]:   # the event path lost it, whatever the sweep then decided (launched, held, ...)
                        self.metrics.missed.inc(agent=d["agent"], result=d["result"])
                self._record(envelope, done)
        elif not (done and all(d.get("replay") for d in done)):   # a replay of an event already kept is not new history
            self._record(envelope, done)
        if via in ("stream", "recent"):
            self.metrics.events_received.inc(via=via)
            self.metrics.last_event.set(self.now().timestamp())
        return done

    def _fire(self, agent: str, trig: dict[str, Any], envelope: dict[str, Any], key: str, via: str) -> dict[str, Any]:
        """Brake, open and launch one claimed trigger; the decision for its ledger row."""
        eid = envelope["id"]
        brake = self._brakes.setdefault(agent, triggers.RateLimiter(int(trig.get("maxPerHour", DEFAULT_PER_HOUR))))
        if not brake.allow(self.now()):
            log.warning("event %s: %s over its storm brake (%d/h); dropped", eid, agent, brake.per_hour)
            return {"agent": agent, "result": "braked", "via": via, "new": True}
        try:
            r = self.gw.open_triggered(agent, f"glidepath:{eid}", task_id=triggers.event_task_id(agent, key))
            if not r.ok:
                # R024: a person holds this agent's triggers (control.py); the event is history, not a run.
                rule = r.decision.get("rule")
                return {"agent": agent, "result": "held" if rule == "R024" else "refused", "rule": rule, "via": via,
                        "new": True}
            if r.data.get("deduped"):
                return {"agent": agent, "result": "deduped", "session": r.session, "via": via, "new": True}
            d = self.gw.defs[agent]
            run_input = {"event": summarize(envelope)}
            if d.instructions:
                run_input["instructions"] = d.instructions
            launched = self.gw.launch(r.session, run_input)
        except Exception as e:  # noqa: BLE001 - one agent's failure must not skip the others for this event
            log.error("event %s: %s: %s", eid, agent, type(e).__name__)
            return {"agent": agent, "result": "launch-failed", "via": via, "new": True}
        return {"agent": agent, "result": "launched" if launched.ok else "launch-failed", "session": r.session,
                "run": (launched.data or {}).get("run"), "via": via, "new": True}

    def _record(self, envelope: dict[str, Any], done: list[dict[str, Any]]) -> None:
        """Keep the event and what was done with it (engine/events.py, Gateway.record_event: history, not evidence),
        every event, matched or not, so a trigger dry run can replay them. Best effort: history never stops a run."""
        record = getattr(self.gw, "record_event", None)
        if record is None:
            return
        try:
            record(summarize(envelope), done)
        except Exception as e:  # noqa: BLE001 - history is best effort
            log.warning("event %s: history not recorded: %s", envelope.get("id"), e)

    def _advance(self, eid: str | None) -> None:
        """Move the saved cursor to `eid` if it is newer (ids order by receive time)."""
        if not eid:
            return
        if self.last_id is None or eid > self.last_id:
            self.last_id = eid
        self._db(self.ledger.advance, SOURCE, eid)
        t = id_time(self.last_id)
        if t is not None:
            self.metrics.cursor_time.set(t.timestamp())

    def _handle_one(self, envelope: dict[str, Any], via: str) -> None:
        eid = envelope.get("id")
        try:
            self.handle(envelope, via)
        except Exception as e:  # noqa: BLE001 - one bad event must not end the stream
            log.error("event %s: %s", eid, type(e).__name__)
        self._advance(eid if isinstance(eid, str) else None)

    # -- glidepath-api ------------------------------------------------------------------------------------------
    def _headers(self) -> dict[str, str]:
        token = self.token_file.read_text().strip()
        return {"Authorization": f"Bearer {token}", "X-Glidepath-Authorization": f"Bearer {token}"}

    def get_json(self, path: str, params: dict[str, Any]) -> Any:
        """One JSON read from glidepath-api, as the gateway, under the bridge's timeout."""
        q = urllib.parse.urlencode({k: v for k, v in params.items() if v is not None})
        req = urllib.request.Request(self.base + path + ("?" + q if q else ""),
                                     headers={"Accept": "application/json", **self._headers()}, method="GET")
        with self._open(req, self.timeout) as res:
            body = res.read(MAX_JSON + 1)
        if len(body) > MAX_JSON:
            raise ValueError(f"{path}: answer over {MAX_JSON} bytes")
        return json.loads(body)

    def _request(self) -> urllib.request.Request:
        headers = {"Accept": "text/event-stream", **self._headers()}
        if self.last_id:
            headers["Last-Event-ID"] = self.last_id
        return urllib.request.Request(self.base + "/v1/events", headers=headers, method="GET")

    def resume(self) -> str:
        """At start: load the saved cursor and handle what `/v1/events/recent?since=` holds after it. Returns the
        result counted in autopilot_events_resume_total (`none` when there is no cursor yet: the first start)."""
        cur = self._db(self.ledger.cursor, SOURCE, self.now())
        self._resumed = True
        if cur.last_id is None:
            return "none"
        self.last_id = cur.last_id
        t = id_time(cur.last_id)
        if t is not None:
            self.metrics.cursor_time.set(t.timestamp())
        try:
            page = self.get_json("/v1/events/recent", {"since": cur.last_id, "limit": RECENT_LIMIT})
        except (OSError, ValueError) as e:
            log.warning("event catch-up from %s failed: %s; the stream resumes with Last-Event-ID", cur.last_id,
                        type(e).__name__)
            self.metrics.resumes.inc(result="failed")
            return "failed"
        evs = [e for e in (page.get("events") or []) if isinstance(e, dict)] if isinstance(page, dict) else []
        for env in evs:
            if self._stop.is_set():
                break
            self._handle_one(env, "recent")
        stale = t is not None and self.now() - t > BUFFER_AGE
        result = "stale-cursor" if stale else "caught-up"
        if stale:
            log.warning("event cursor %s is older than glidepath-api's %s buffer: events may be gone; the "
                        "reconciliation sweep recovers missed pipelinerun.finished", cur.last_id, BUFFER_AGE)
        log.info("event catch-up from %s: %d event(s) (%s)", cur.last_id, len(evs), result)
        self.metrics.resumes.inc(result=result)
        return result

    def run_once(self) -> None:
        """Hold one stream until it ends or fails. Every event is handled; a failure handling one is logged."""
        if not self._resumed:
            self.resume()
        with self._open(self._request(), self.timeout) as res:
            self.metrics.stream_connected.set(1)
            try:
                for eid, data in sse_events(res):
                    if self._stop.is_set():
                        return
                    try:
                        envelope = json.loads(data)
                    except ValueError:
                        continue
                    if not isinstance(envelope, dict):
                        continue
                    if eid and not envelope.get("id"):
                        envelope["id"] = eid
                    self._handle_one(envelope, "stream")
            finally:
                self.metrics.stream_connected.set(0)

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
