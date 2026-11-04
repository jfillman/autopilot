"""Interval triggers (`{type: interval, every: 24h}`): the scheduled agents' clock.

Each interval is a slot aligned to the Unix epoch in UTC (every 24h: 00:00 UTC; every 6h: 00, 06, 12, 18). An agent
fires once per slot, within GRACE of the slot's start, as `Gateway.open_triggered(agent, "interval:<every>",
task_id=slot_task_id(agent, slot))` then `launch` with its instructions. A held agent (R024) is skipped. Nothing here
knows Glidepath.

Once per slot, across restarts (A-5): the slot's key (`slot:<every>:<n>`) is claimed in the trigger ledger
(engine/trigger_ledger.py; gateway_trigger_launches) before the session is opened, so a gateway restarted inside the
grace window after the slot has fired, its run finished or not, does not fire it again. The grace window keeps a
restart from firing a slot long after it began: a slot whose window passed with no firing (the gateway was down for
all of it) is recorded `missed` in the ledger, once per slot (back to the floor), and counted in autopilot_schedule_missed_total{agent}; it is
not run late. Slots that began before the agent's floor (when the scheduler first saw that agent with a ledger) are never counted
missed.
autopilot_schedule_lag_seconds{agent} is how long after its slot's start the last firing came (one tick, 30 s, at
most when the gateway is up).
"""
from __future__ import annotations

import hashlib
import logging
import threading
from datetime import datetime, timedelta, timezone
from typing import Any, Callable

from . import triggers
from .engine import trigger_ledger as tl
from .metrics import TriggerMetrics

log = logging.getLogger("autopilot.scheduler")

GRACE = timedelta(minutes=10)
TICK_SECONDS = 30.0
MISSED_LOOKBACK = 744   # slots checked back per agent at most (31 days of hourly slots)
SOURCE = "schedule"     # the ledger floors' rows in gateway_event_cursors: `schedule:<agent>` (only the floor is used)


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
                 tick: float = TICK_SECONDS, ledger: Any = None, metrics: TriggerMetrics | None = None):
        self.gw, self.clock, self.tick_seconds = gw, clock, tick
        self.ledger = ledger if ledger is not None else (getattr(gw, "triggers", None) or tl.MemoryTriggers())
        self.metrics = metrics or TriggerMetrics()
        self.fired: set[tuple[str, int]] = set()      # this process's cache of slots already claimed or checked
        self._checked: set[tuple[str, int]] = set()
        self._floors: dict[str, datetime] = {}
        self.started = clock()
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

    def _db(self, fn: Callable[..., Any], *a: Any) -> Any:
        guarded = getattr(self.gw, "guarded", None)
        return guarded(fn, *a) if guarded is not None else fn(*a)

    def floor(self, agent: str, now: datetime) -> datetime:
        """When this agent's schedule was first seen with a ledger: a durable row per agent (gateway_event_cursors
        source `schedule:<agent>`), so an agent added to a running deployment never has earlier slots called missed."""
        f = self._floors.get(agent)
        if f is None:
            f = self._floors[agent] = self._db(self.ledger.cursor, f"{SOURCE}:{agent}", now).floor
        return f

    def _check_missed(self, agent: str, every_s: str, every: timedelta, slot: int, start: datetime,
                      now: datetime) -> list[dict[str, Any]]:
        """Every slot whose grace window has passed, back to the ledger's floor (at most MISSED_LOOKBACK slots):
        each one nothing fired is recorded missed, once. A gateway down for three nights records three."""
        if every <= GRACE:     # windows overlap: a short interval's next slot comes before a miss could be told
            return []
        last, last_start = (slot, start) if now - start > GRACE else (slot - 1, start - every)
        floor, out = self.floor(agent, now), []
        for n in range(MISSED_LOOKBACK):
            s, s_start = last - n, last_start - n * every
            if s_start < floor or (agent, s) in self._checked:
                break           # before the ledger existed, or already checked (and so is everything older)
            self._checked.add((agent, s))
            if (agent, s) in self.fired:
                break
            key = tl.slot_key(every_s, s)
            # Any row means the slot was fired or already called missed, and every slot before it was accounted for
            # then: stop. (A `claimed` orphan a dead process left may have launched its run: not missed either.)
            if self._db(self.ledger.get, agent, key) is not None:
                break
            if not self._db(self.ledger.claim, agent, key, "schedule", None, now):
                continue
            self._db(self.ledger.decide, agent, key, "missed")
            self.metrics.slot_missed.inc(agent=agent)
            log.warning("schedule %s slot %s (%s) passed with no firing: missed", agent, s, s_start.isoformat())
            out.append({"agent": agent, "slot": s, "result": "missed"})
        return out

    def tick(self) -> list[dict[str, Any]]:
        now, done = self.clock(), []
        for agent, every_s, every in self.schedules():
            slot, start = slot_of(now, every)
            done += self._check_missed(agent, every_s, every, slot, start, now)
            if (agent, slot) in self.fired or now - start > GRACE:
                continue
            self.fired.add((agent, slot))
            key = tl.slot_key(every_s, slot)
            if not self._db(self.ledger.claim, agent, key, "schedule", None, now, tl.stale_before(self.started, now)):
                continue          # fired before this process started (a restart inside the window)
            out = self._fire(agent, every_s, slot, start)
            self._db(self.ledger.decide, agent, key, out["result"], out.get("session"), out.get("run"))
            self.metrics.slot_launches.inc(agent=agent, result=out["result"])
            if out["result"] == "launched":
                self.metrics.slot_lag.set((now - start).total_seconds(), agent=agent)
            done.append(out)
        for d in done:
            log.info("schedule %s slot %s: %s", d["agent"], d["slot"], d["result"])
        return done

    def _fire(self, agent: str, every_s: str, slot: int, start: datetime) -> dict[str, Any]:
        """Open and launch one claimed slot; the decision for its ledger row."""
        r = None
        try:
            r = self.gw.open_triggered(agent, f"interval:{every_s}", task_id=slot_task_id(agent, slot))
            if not r.ok:
                rule = r.decision.get("rule")
                return {"agent": agent, "slot": slot, "result": "held" if rule == "R024" else "refused", "rule": rule}
            if r.data.get("deduped"):
                return {"agent": agent, "slot": slot, "result": "deduped", "session": r.session}
            d = self.gw.defs[agent]
            run_input: dict[str, Any] = {"prompt": f"Scheduled run ({every_s}, slot starting {start.isoformat()}). "
                                                   "Carry out your instructions."}
            if d.instructions:
                run_input["instructions"] = d.instructions
            launched = self.gw.launch(r.session, run_input)
        except Exception as e:  # noqa: BLE001 - one agent's failed launch must not skip the others in this tick
            log.exception("schedule %s slot %s: launch raised", agent, slot)
            return {"agent": agent, "slot": slot, "result": "launch-failed", "session": r.session if r is not None else None,
                    "error": f"{type(e).__name__}: {e}"}
        return {"agent": agent, "slot": slot, "result": "launched" if launched.ok else "launch-failed",
                "session": r.session, "run": (launched.data or {}).get("run"),
                **({} if launched.ok else {"error": launched.error})}

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
