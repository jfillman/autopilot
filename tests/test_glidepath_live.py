"""glidepath.live.list / .live.watch / .live.logs.get and the other reads added with Clearance v0.24.0, through the real
GlidepathAPI client against a stub glidepath-api (server-sent events included)."""
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

import pytest

from autopilot import glidepath_live
from autopilot.glidepath_api import LOG_TAIL, GlidepathAPI
from clearance.dispatch import Denied

RUN = "gate-api-ci-0-build-abcde"


def pr(status="Unknown", reason="Running"):
    return {"metadata": {"name": RUN, "labels": {"tekton.dev/pipeline": "build",
                                                 "pipelinesascode.tekton.dev/event-type": "push",
                                                 "pipelinesascode.tekton.dev/sha": "abc1234"}},
            "status": {"startTime": "2026-11-01T00:00:00Z",
                       "conditions": [{"type": "Succeeded", "status": status, "reason": reason, "message": "Tasks Completed: 1 (Failed: 1)"}]}}


def tr(name, task, status="Unknown", reason="Running", steps=None):
    return {"metadata": {"name": name, "labels": {"tekton.dev/pipelineRun": RUN, "tekton.dev/pipelineTask": task}},
            "status": {"startTime": "2026-11-01T00:00:01Z", "steps": steps or [{"name": "build", "container": "step-build", "running": {}}],
                       "conditions": [{"type": "Succeeded", "status": status, "reason": reason, "message": "exit 1"}]}}


def test_summarize_keeps_what_an_agent_needs():
    other = {"metadata": {"name": "old", "labels": {}}, "status": {"startTime": "2026-10-31T00:00:00Z",
                                                                   "conditions": [{"type": "Succeeded", "status": "True"}]}}
    out = glidepath_live.summarize({"pipelineRuns": [other, pr()], "taskRuns": [tr(f"{RUN}-build", "build")]})
    runs = out["pipelineRuns"]
    assert [r["name"] for r in runs] == [RUN, "old"], "running first"
    r = runs[0]
    assert r == {"name": RUN, "pipeline": "build", "status": "running", "reason": "Running", "startTime": "2026-11-01T00:00:00Z",
                 "event-type": "push", "sha": "abc1234",
                 "tasks": [{"name": "build", "taskRun": f"{RUN}-build", "status": "running", "startTime": "2026-11-01T00:00:01Z",
                            "steps": [{"name": "build", "container": "step-build", "state": "running"}]}]}
    assert glidepath_live.status_of({"status": {"conditions": [{"type": "Succeeded", "status": "False", "reason": "Cancelled"}]}}) == "cancelled"
    assert glidepath_live.status_of({}) == "pending"


class Clock:
    def __init__(self):
        self.t = 0.0

    def __call__(self):
        return self.t

    def sleep(self, s):
        self.t += s


def sse(*events):
    out = []
    for ev, data in events:
        if ev == "comment":
            out.append(b": ping\n")
        else:
            out += [f"event: {ev}\n".encode(), f"data: {json.dumps(data)}\n".encode(), b"\n"]
    return out


def test_watch_follows_the_stream_until_the_run_finishes():
    lines = sse(("snapshot", {"pipelineRuns": [pr()], "taskRuns": [tr(f"{RUN}-build", "build")]}),
                ("comment", None),
                ("change", {"type": "upsert", "kind": "TaskRun", "name": f"{RUN}-build",
                            "object": tr(f"{RUN}-build", "build", "False", "Failed", [{"name": "build", "container": "step-build", "terminated": {"exitCode": 1}}])}),
                ("change", {"type": "upsert", "kind": "PipelineRun", "name": RUN, "object": pr("False", "Failed")}),
                ("change", {"type": "upsert", "kind": "PipelineRun", "name": "never-read", "object": {}}))
    out = glidepath_live.watch(RUN, 60, lambda t: (200, iter(lines)), poll=lambda: pytest.fail("no poll when the stream works"))
    assert out["finished"] and out["via"] == "stream"
    assert out["pipelineRun"]["status"] == "failed" and out["pipelineRun"]["message"].startswith("Tasks Completed")
    assert [(c["task"], c["status"]) for c in out["changes"]] == [("build", "failed")], "the snapshot itself is no change"
    assert out["pipelineRun"]["tasks"][0]["steps"][0] == {"name": "build", "container": "step-build", "state": "terminated", "exitCode": 1}


def test_watch_returns_at_its_deadline_on_a_quiet_stream():
    clock = Clock()

    def lines():
        yield from sse(("snapshot", {"pipelineRuns": [pr()], "taskRuns": []}))
        while True:  # heartbeats only, 15 s apart
            clock.t += 15
            yield b": ping\n"

    out = glidepath_live.watch(RUN, 30, lambda t: (200, lines()), poll=lambda: pytest.fail("deadline passed"), clock=clock)
    assert not out["finished"] and out["pipelineRun"]["status"] == "running" and out["waitedSeconds"] == 30


def test_watch_polls_when_the_stream_is_refused():
    clock, states = Clock(), iter([pr(), pr(), pr("True", "Succeeded")])
    polls = []

    def poll():
        polls.append(clock.t)
        return {"pipelineRuns": [next(states)], "taskRuns": []}

    out = glidepath_live.watch(RUN, 60, lambda t: (503, b'{"rule":"GP-API-503"}'), poll, clock=clock, sleep=clock.sleep)
    assert out["finished"] and out["via"] == "poll" and out["pipelineRun"]["status"] == "succeeded"
    assert polls == [0.0, 3.0, 6.0]


def test_watch_of_a_run_that_is_gone_says_so():
    out = glidepath_live.watch(RUN, 60, lambda t: (200, iter(sse(("snapshot", {"pipelineRuns": [], "taskRuns": []})))), poll=dict)
    assert out["pipelineRun"] is None and not out["finished"]


class Stub(BaseHTTPRequestHandler):
    seen: list = []

    def log_message(self, *a):
        pass

    def do_GET(self):
        u = urlparse(self.path)
        Stub.seen.append((u.path, parse_qs(u.query), self.headers.get("X-Glidepath-Authorization"), self.headers.get("Accept")))
        if u.path == "/v1/apps/gate-api/live/stream":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            for line in sse(("snapshot", {"pipelineRuns": [pr()], "taskRuns": []}),
                            ("change", {"type": "upsert", "kind": "PipelineRun", "name": RUN, "object": pr("True", "Succeeded")})):
                self.wfile.write(line)
                self.wfile.flush()
            return
        if u.path == "/v1/apps/denied/live/stream":
            return self._json(403, {"error": "no", "rule": "GP-AUTHZ-001"})
        if u.path.endswith("/logs/step-build"):
            body = b"x" * (LOG_TAIL + 10)
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self._json(200, {"path": u.path})

    def _json(self, status, v):
        b = json.dumps(v).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)


@pytest.fixture
def api(tmp_path):
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Stub)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    tok = tmp_path / "token"
    tok.write_text("sa-token\n")
    Stub.seen = []
    yield GlidepathAPI(f"http://127.0.0.1:{srv.server_port}", tok)
    srv.shutdown()


def test_client_watch_reads_the_real_stream(api):
    out = api.watch("gate-api", RUN, 30, "user:alice")
    assert out["finished"] and out["pipelineRun"]["status"] == "succeeded" and out["via"] == "stream"
    path, _, auth, accept = Stub.seen[0]
    assert path == "/v1/apps/gate-api/live/stream" and auth == "Bearer sa-token" and accept == "text/event-stream"


def test_client_watch_refusal_is_a_denial(api):
    with pytest.raises(Denied):
        api.watch("denied", RUN, 30, "user:alice")


def test_client_reads_build_the_right_requests(api):
    api.release_run("gate-api", "promote-prod-abcdef01-3f9a1c", "")
    api.evidence("gate-api", "ghcr.io/o/gate-api:2.0.6", "")
    api.tests("gate-api", 20, "")
    api.events("gate-api", "dev.cdevents.pipelinerun.", None, 10, "")
    api.previews("gate-api", "")
    got = [(p, q) for p, q, _, _ in Stub.seen]
    assert got == [("/v1/apps/gate-api/releases/runs/promote-prod-abcdef01-3f9a1c", {}),
                   ("/v1/apps/gate-api/evidence", {"image": ["ghcr.io/o/gate-api:2.0.6"]}),
                   ("/v1/apps/gate-api/tests", {"limit": ["20"]}),
                   ("/v1/events/recent", {"app": ["gate-api"], "type": ["dev.cdevents.pipelinerun."], "limit": ["10"]}),
                   ("/v1/apps/gate-api/previews", {})]


def test_a_long_live_log_keeps_its_end(api):
    out = api.live_log("gate-api", f"{RUN}-build", "step-build", "")
    assert out["truncated"] and out["bytes"] == LOG_TAIL + 10 and len(out["log"]) == LOG_TAIL


# Through the gateway: each new tool reaches its backend call with the session's principal.
CALLS = [
    ("cicd-explainer", "glidepath.live.list", {"app": "gate-api"}, "glidepath.live"),
    ("cicd-explainer", "glidepath.live.logs.get", {"app": "gate-api", "taskRun": f"{RUN}-build", "container": "step-build"}, "glidepath.live_log"),
    ("cicd-explainer", "glidepath.tests.list", {"app": "gate-api", "limit": 5}, "glidepath.tests"),
    ("cicd-explainer", "glidepath.tests.report", {"app": "gate-api", "id": "6902a1b2c3d4"}, "glidepath.test_report"),
    ("cicd-explainer", "glidepath.evidence.get", {"app": "gate-api", "image": "ghcr.io/o/gate-api:2.0.6"}, "glidepath.evidence"),
    ("cicd-explainer", "glidepath.events.recent", {"app": "gate-api", "type": "dev.cdevents."}, "glidepath.events"),
    ("cicd-explainer", "glidepath.previews.list", {"app": "gate-api"}, "glidepath.previews"),
    ("release-proposer", "glidepath.releaseRun.get", {"app": "gate-api", "name": "promote-prod-abcdef01-3f9a1c"}, "glidepath.release_run"),
    ("release-proposer", "glidepath.live.watch", {"app": "gate-api", "pipelinerun": RUN, "timeoutSeconds": 30}, "glidepath.watch"),
]


@pytest.mark.parametrize("agent,tool,args,backend_call", CALLS)
def test_each_new_tool_reaches_glidepath(gw, backends, agent, tool, args, backend_call):
    r = gw.open_session("alice", agent)
    assert r.ok, r.as_dict()
    res = gw.call("alice", r.data["session"], tool, args)
    assert res.ok, res.as_dict()
    call = next(c for c in backends.glidepath.calls if c[0] == backend_call)
    assert call[2]["on_behalf_of"] == "user:alice"


def test_a_bad_timeout_is_refused_before_glidepath(gw, backends):
    sid = gw.open_session("alice", "release-proposer").data["session"]
    res = gw.call("alice", sid, "glidepath.live.watch", {"app": "gate-api", "pipelinerun": RUN, "timeoutSeconds": 600})
    assert not res.ok and not [c for c in backends.glidepath.calls if c[0] == "glidepath.watch"]
