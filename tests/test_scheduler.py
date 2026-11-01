"""Interval triggers (scheduler.py): a scheduled workload agent fires once per UTC-aligned slot, within the grace
window, with its instructions; a held agent is skipped; task agents are never scheduled."""
from datetime import datetime, timedelta, timezone

from autopilot import scheduler as sch


class Result:
    def __init__(self, ok=True, data=None, decision=None, session="s-1", error=None):
        self.ok, self.data, self.decision, self.session, self.error = ok, data or {}, decision or {}, session, error


class Def:
    def __init__(self, kind="scheduled", identity="workload", every="24h", instructions="review"):
        self.kind, self.identity_type, self.instructions = kind, identity, instructions
        self.triggers = ({"type": "interval", "every": every},)


class GW:
    def __init__(self, defs, held=()):
        self.defs, self.held, self.opened, self.launched = defs, set(held), [], []

    def open_triggered(self, agent, trigger, task_id=None):
        self.opened.append((agent, trigger, task_id))
        if agent in self.held:
            return Result(False, decision={"rule": "R024"})
        return Result(True, {"deduped": False}, session=f"s-{len(self.opened)}")

    def launch(self, session, run_input):
        self.launched.append((session, run_input))
        return Result(True, {"run": "r-" + session})


def at(h, m=0):
    return datetime(2026, 11, 2, h, m, tzinfo=timezone.utc)


def test_once_per_slot_inside_the_grace_window():
    gw = GW({"nightly-reviewer": Def(), "researcher": Def(kind="task", identity="delegated")})
    now = [at(0, 2)]
    s = sch.Scheduler(gw, clock=lambda: now[0])
    out = s.tick()
    assert [(d["agent"], d["result"]) for d in out] == [("nightly-reviewer", "launched")]
    agent, trigger, task = gw.opened[0]
    assert trigger == "interval:24h" and task.startswith("sched-nightly-reviewer-")
    assert gw.launched[0][1]["instructions"] == "review" and "Scheduled run" in gw.launched[0][1]["prompt"]
    now[0] = at(0, 5)
    assert s.tick() == [] and len(gw.opened) == 1                     # same slot: once
    now[0] = at(1)
    assert s.tick() == []                                              # past the grace window: wait for the next slot
    now[0] = at(0, 3) + timedelta(days=1)
    assert [d["result"] for d in s.tick()] == ["launched"]             # the next day's slot


def test_a_restart_late_in_a_slot_does_not_fire_it():
    gw = GW({"nightly-reviewer": Def()})
    assert sch.Scheduler(gw, clock=lambda: at(9)).tick() == [] and gw.opened == []


def test_a_held_agent_is_skipped():
    gw = GW({"nightly-reviewer": Def()}, held={"nightly-reviewer"})
    out = sch.Scheduler(gw, clock=lambda: at(0, 1)).tick()
    assert out[0]["result"] == "held" and gw.launched == []


def test_slots_align_to_utc():
    n, start = sch.slot_of(at(13, 7), timedelta(hours=6))
    assert start == at(12) and sch.slot_of(at(17, 59), timedelta(hours=6))[0] == n
