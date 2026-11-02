"""Interval triggers (`{type: interval, every: 24h}`): the scheduled agents' clock.

Each interval is a slot aligned to the Unix epoch in UTC (every 24h: 00:00 UTC; every 6h: 00, 06, 12, 18). An agent
fires once per slot, within GRACE of the slot's start, as `Gateway.open_triggered(agent, "interval:<every>",
task_id=slot_task_id(agent, slot))` then `launch` with its instructions. The slot's task id makes a second firing of the
same slot a dedupe while the first run is live, and the grace window keeps a restart from firing a slot long after it
began; a restart inside the window, after a slot's run has already finished, can run it twice (accepted: the runs are
read-only reviewers and each is evidence). A held agent (R024) is skipped. Nothing here knows Glidepath.
"""
from __future__ import annotations

import hashlib
import logging
import threading
from datetime import datetime, timedelta, timezone
from typing import Any, Callable

from . import triggers

log = logging.getLogger("autopilot.scheduler")

GRACE = timedelta(minutes=10)
TICK_SECONDS = 30.0


def slot_task_id(agent: str, slot: int) -> str:
    """A stable task id per (agent, slot), in the AgentRun's `t-<hex>` form (schemas/agentrun.xrd.yaml spec.taskId; the
    API server refuses anything else), as triggers.event_task_id is for an event."""
    return "t-" + hashlib.sha256(f"sched:{agent}:{slot}".encode()).hexdigest()[:12]


def slot_of(now: datetime, every: timedelta) -> tuple[int, datetime]:
    """The slot `now` falls in and when it started."""
    secs = int(every.total_seconds())
    n = int(now.timestamp()) // secs
    return n, datetime.fromtimestamp(n * secs, tz=timezone.utc)


class Scheduler:
    def __init__(self, gw: Any, *, clock: Callable[[], datetime] = lambda: datetime.now(timezone.utc),
                 tick: float = TICK_SECONDS):
        self.gw, self.clock, self.tick_seconds = gw, clock, tick
        self.fired: set[tuple[str, int]] = set()
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._loop, name="autopilot-scheduler", daemon=True)

    def schedules(self) -> list[tuple[str, str, timedelta]]:
        out = []
        for name, d in sorted(self.gw.defs.items()):
            if d.kind != "scheduled" or d.identity_type != "workload":
                continue
            for t in d.triggers:
                if t.get("type") == "interval":
                    out.append((name, t["every"], triggers.parse_interval(t["every"])))
        return out

    def tick(self) -> list[dict[str, Any]]:
        now, done = self.clock(), []
        for agent, every_s, every in self.schedules():
            slot, start = slot_of(now, every)
            if (agent, slot) in self.fired or now - start > GRACE:
                continue
            self.fired.add((agent, slot))
            r = self.gw.open_triggered(agent, f"interval:{every_s}", task_id=slot_task_id(agent, slot))
            if not r.ok:
                rule = r.decision.get("rule")
                done.append({"agent": agent, "slot": slot, "result": "held" if rule == "R024" else "refused", "rule": rule})
                continue
            if r.data.get("deduped"):
                done.append({"agent": agent, "slot": slot, "result": "deduped", "session": r.session})
                continue
            d = self.gw.defs[agent]
            run_input: dict[str, Any] = {"prompt": f"Scheduled run ({every_s}, slot starting {start.isoformat()}). "
                                                   "Carry out your instructions."}
            if d.instructions:
                run_input["instructions"] = d.instructions
            try:
                launched = self.gw.launch(r.session, run_input)
            except Exception as e:  # noqa: BLE001 - one agent's failed launch must not skip the others in this tick
                log.exception("schedule %s slot %s: launch raised", agent, slot)
                done.append({"agent": agent, "slot": slot, "result": "launch-failed", "session": r.session,
                             "error": f"{type(e).__name__}: {e}"})
                continue
            done.append({"agent": agent, "slot": slot, "result": "launched" if launched.ok else "launch-failed",
                         "session": r.session, "run": (launched.data or {}).get("run"),
                         **({} if launched.ok else {"error": launched.error})})
        for d in done:
            log.info("schedule %s slot %s: %s", d["agent"], d["slot"], d["result"])
        return done

    def start(self) -> "Scheduler":
        self._thread.start()
        return self

    def stop(self) -> None:
        self._stop.set()
        self._thread.join(timeout=self.tick_seconds + 5)

    def _loop(self) -> None:
        while True:
            try:
                self.tick()
            except Exception as e:  # noqa: BLE001 - one bad tick must not end the clock
                log.error("schedule tick failed: %s", e)
            if self._stop.wait(self.tick_seconds):
                return
