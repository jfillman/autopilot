"""glidepath.live.list and glidepath.live.watch: what an agent reads of runs in flight.

glidepath-api's live reads (GET /v1/apps/{app}/live, .../live/stream) return PipelineRuns and TaskRuns as the cluster
holds them, whole Kubernetes objects of tens of kilobytes each. An agent needs much less: which runs there are, where
each is, which task failed, and the step containers whose live logs it may read. `summarize` is that view.

`watch` is the stream as one bounded tool call. MCP answers a tool call once, so an agent that started a run cannot
subscribe to it; instead it asks to be told when the run finishes. The call reads the stream's snapshot, then its
change events, until the named PipelineRun finishes or the deadline passes, and returns the run as it is then and every
task change it saw. When the stream cannot be opened (a replica at its stream limit answers 503) it polls `live` every
few seconds until the same deadline, so the agent gets the same answer either way.
"""
from __future__ import annotations

import json
import time
from typing import Any, Callable, Iterable, Iterator

LABEL_PIPELINE = "tekton.dev/pipeline"
LABEL_PIPELINE_RUN = "tekton.dev/pipelineRun"
LABEL_PIPELINE_TASK = "tekton.dev/pipelineTask"
MAX_CHANGES = 100
POLL_SECONDS = 3.0


def status_of(obj: dict) -> str:
    """running, succeeded, failed or cancelled, from the object's Succeeded condition (pending before it has one)."""
    conds = (obj.get("status") or {}).get("conditions") or []
    c = next((c for c in conds if c.get("type") == "Succeeded"), None)
    if not c:
        return "pending"
    if c.get("status") == "True":
        return "succeeded"
    if c.get("status") == "False":
        return "cancelled" if "Cancel" in (c.get("reason") or "") else "failed"
    return "running"


def _meta(obj: dict) -> dict:
    return obj.get("metadata") or {}


def _task(tr: dict) -> dict:
    st = tr.get("status") or {}
    steps = []
    for s in st.get("steps") or []:
        state = "running" if "running" in s else "terminated" if "terminated" in s else "waiting"
        step = {"name": s.get("name"), "container": s.get("container"), "state": state}
        if "terminated" in s:
            step["exitCode"] = s["terminated"].get("exitCode")
        steps.append(step)
    cond = next((c for c in st.get("conditions") or [] if c.get("type") == "Succeeded"), {})
    out = {"name": _meta(tr).get("labels", {}).get(LABEL_PIPELINE_TASK) or _meta(tr).get("name"),
           "taskRun": _meta(tr).get("name"), "status": status_of(tr), "steps": steps}
    for k in ("startTime", "completionTime"):
        if st.get(k):
            out[k] = st[k]
    if out["status"] in ("failed", "cancelled") and cond.get("message"):
        out["message"] = cond["message"][:500]
    return out


def summarize_run(pr: dict, task_runs: Iterable[dict]) -> dict:
    md, st = _meta(pr), pr.get("status") or {}
    labels = md.get("labels") or {}
    cond = next((c for c in st.get("conditions") or [] if c.get("type") == "Succeeded"), {})
    out = {"name": md.get("name"), "pipeline": labels.get(LABEL_PIPELINE), "status": status_of(pr),
           "reason": cond.get("reason"), "tasks": sorted((_task(t) for t in task_runs),
                                                          key=lambda t: t.get("startTime") or "~")}
    for k in ("startTime", "completionTime"):
        if st.get(k):
            out[k] = st[k]
    for k in ("pipelinesascode.tekton.dev/event-type", "pipelinesascode.tekton.dev/sha"):
        if labels.get(k):
            out[k.split("/")[1]] = labels[k]
    if out["status"] in ("failed", "cancelled") and cond.get("message"):
        out["message"] = cond["message"][:500]
    return out


def summarize(live: dict) -> dict:
    """GET /v1/apps/{app}/live as an agent reads it: each PipelineRun with its TaskRuns, running first, newest first."""
    by_run: dict[str, list[dict]] = {}
    for tr in live.get("taskRuns") or []:
        by_run.setdefault((_meta(tr).get("labels") or {}).get(LABEL_PIPELINE_RUN, ""), []).append(tr)
    runs = [summarize_run(pr, by_run.get(_meta(pr).get("name"), [])) for pr in live.get("pipelineRuns") or []]
    runs.sort(key=lambda r: r.get("startTime") or "", reverse=True)
    runs.sort(key=lambda r: r["status"] not in ("running", "pending"))
    return {"pipelineRuns": runs}


def sse_events(lines: Iterator[bytes]) -> Iterator[tuple[str, str]]:
    """(event, data) for each server-sent event, and ("comment", text) for each comment line: glidepath-api's 15 s
    heartbeat is how a quiet stream still lets the caller look at its clock."""
    event, data = "message", []
    for raw in lines:
        line = raw.decode("utf-8", "replace").rstrip("\r\n")
        if not line:
            if data:
                yield event, "\n".join(data)
            event, data = "message", []
        elif line.startswith(":"):
            yield "comment", line[1:].strip()
        elif line.startswith("event:"):
            event = line[6:].strip()
        elif line.startswith("data:"):
            data.append(line[5:].lstrip())


class Watch:
    """The state of one PipelineRun and its TaskRuns as a stream (or polling) reports them."""

    def __init__(self, name: str):
        self.name, self.run, self.task_runs, self.changes = name, None, {}, []

    def _mine(self, tr: dict) -> bool:
        return (_meta(tr).get("labels") or {}).get(LABEL_PIPELINE_RUN) == self.name

    def snapshot(self, live: dict, record: bool) -> None:
        self.run = next((p for p in live.get("pipelineRuns") or [] if _meta(p).get("name") == self.name), None)
        for tr in live.get("taskRuns") or []:
            if self._mine(tr):
                self.upsert_task(tr, record)

    def upsert_task(self, tr: dict, record: bool = True) -> None:
        name = _meta(tr).get("name")
        before = status_of(self.task_runs[name]) if name in self.task_runs else None
        self.task_runs[name] = tr
        if record and status_of(tr) != before and len(self.changes) < MAX_CHANGES:
            t = _task(tr)
            self.changes.append({"at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "task": t["name"],
                                 "taskRun": name, "status": t["status"]})

    def change(self, ev: dict) -> None:
        obj, kind = ev.get("object") or {}, ev.get("kind")
        if kind == "PipelineRun" and ev.get("name") == self.name:
            self.run = None if ev.get("type") == "delete" else obj
        elif kind == "TaskRun" and ev.get("type") == "upsert" and self._mine(obj):
            self.upsert_task(obj)

    def finished(self) -> bool:
        return self.run is not None and status_of(self.run) not in ("running", "pending")

    def answer(self, waited: float, finished: bool, via: str) -> dict:
        out = {"finished": finished, "waitedSeconds": round(waited, 1), "via": via, "changes": self.changes}
        out["pipelineRun"] = summarize_run(self.run, self.task_runs.values()) if self.run else None
        return out


def watch(name: str, timeout: float, open_stream: Callable[[float], tuple[int, Any]],
          poll: Callable[[], dict], clock: Callable[[], float] = time.monotonic,
          sleep: Callable[[float], None] = time.sleep) -> dict:
    """Follow PipelineRun `name` until it finishes or `timeout` seconds pass.

    open_stream(timeout) answers (status, an iterator of the stream's lines) or (status, body bytes) when it is not
    200; poll() answers GET .../live. A run that is not in the cluster is an answer (`pipelineRun: null`), not an error:
    it may have finished and been deleted, which glidepath.runs.list then shows.
    """
    start, w = clock(), Watch(name)
    status, stream = open_stream(timeout)
    via = "stream"
    if status == 200:
        try:
            for event, data in sse_events(stream):
                if event == "comment":
                    if data == "lagged":
                        break  # glidepath-api closed a viewer that fell behind: poll for the rest
                    if clock() - start >= timeout:
                        break
                    continue
                body = json.loads(data)
                if event == "snapshot":
                    w.snapshot(body, record=False)
                    if w.run is None or w.finished():
                        break
                elif event == "change":
                    w.change(body)
                    if w.run is None or w.finished():
                        break
                if clock() - start >= timeout:
                    break
        except (OSError, ValueError):
            pass  # a stream that drops or times out mid-read: fall through to polling for the rest of the deadline
        if w.run is None or w.finished() or clock() - start >= timeout:
            return w.answer(clock() - start, w.finished(), via)
    via = "poll" if status != 200 else "stream+poll"
    first = True
    while True:
        live = poll()
        if isinstance(live, dict) and "error" in live:
            return live
        w.snapshot(live, record=not first)
        first = False
        if w.run is None or w.finished() or clock() - start >= timeout:
            return w.answer(clock() - start, w.finished(), via)
        sleep(min(POLL_SECONDS, max(0.0, timeout - (clock() - start))))
