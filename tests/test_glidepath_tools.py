"""glidepath.* tools: Clearance's T0 reads of glidepath-api (spec phase 2)."""
import asyncio
import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from autopilot.backends import Backends, FakeArtifacts, FakeArgo, FakeGit, FakeHuman, FakeRead, FakeRuns
from autopilot.gateway import TOOLS, hangar_gateway
from autopilot.glidepath_api import GlidepathAPI, GlidepathError
from clearance.dispatch import Denied
from clearance.server import create_server
from clearance.tiers import Tier

RESULT = "0b9d1a2e-1111-4c4c-9999-aaaaaaaaaaaa"


def open_(gw, agent="cicd-explainer", token="alice"):
    r = gw.open_session(token, agent)
    assert r.ok, r.as_dict()
    return r.data["session"]


WRITES = {"glidepath.run.rerun", "glidepath.run.cancel"}


def test_glidepath_tools_are_reads_except_run_control():
    gp = {t.name: t for t in TOOLS.values() if t.name.startswith("glidepath.")}
    assert len(gp) == 8 and "pipeline.rerun" not in TOOLS, "pipeline.rerun is retired for glidepath.run.rerun"
    assert all(t.tier == Tier.T0 for n, t in gp.items() if n not in WRITES)
    assert all(gp[n].tier == Tier.T1 and gp[n].target == "lower" and not gp[n].writes_files for n in WRITES)


def test_a_retried_rerun_reuses_its_key_so_glidepath_starts_one_run(gw, backends):
    sid = open_(gw, agent="coding-agent")
    args = {"app": "gate-api", "pipelinerun": "ci-0-build-abcde"}
    first = gw.call("alice", sid, "glidepath.run.rerun", args)
    again = gw.call("alice", sid, "glidepath.run.rerun", args)
    assert first.ok and again.ok and not first.data["replayed"] and again.data["replayed"]
    keys = {c[1][2] for c in backends.glidepath.calls if c[0] == "glidepath.rerun"}
    assert len(keys) == 1 and next(iter(keys)).startswith("clearance.")
    other = open_(gw, agent="coding-agent")
    gw.call("alice", other, "glidepath.run.rerun", args)
    assert len({c[1][2] for c in backends.glidepath.calls if c[0] == "glidepath.rerun"}) == 2, "another session is another re-run"
    assert all(c[2]["on_behalf_of"] == "user:alice" for c in backends.glidepath.calls)


def test_run_control_is_t1_scoped_and_validated(gw, backends):
    sid = open_(gw)  # cicd-explainer: T0, no write tools
    assert gw.call("alice", sid, "glidepath.run.cancel", {"app": "gate-api", "pipelinerun": "x"}).decision["rule"] in ("R006", "R007")
    sid = open_(gw, agent="coding-agent")
    assert gw.call("alice", sid, "glidepath.run.cancel", {"app": "gate-api", "pipelinerun": "../x"}).decision["rule"] == "R008"
    assert gw.call("alice", sid, "glidepath.run.cancel", {"app": "gate-api", "pipelinerun": "ci-0-build-abcde"}).ok
    ro = gw.open_session("readonly", "coding-agent")
    assert gw.call("readonly", ro.data["session"], "glidepath.run.rerun",
                   {"app": "gate-api", "pipelinerun": "ci-0-build-abcde"}).decision["rule"] == "R007"
    assert [c[0] for c in backends.glidepath.calls] == ["glidepath.cancel"]


def test_adapter_posts_writes_with_the_key_and_reads_409_as_an_answer(token):
    o = Opener(201, b'{"name":"r-rerun-abcde","replayed":false}')
    api = GlidepathAPI("http://gp", token, on_behalf_of=True, opener=o)
    assert api.rerun("gate-api", "r", "clearance.k", "user:alice") == {"name": "r-rerun-abcde", "replayed": False}
    req = o.reqs[0]
    assert req.get_method() == "POST" and req.full_url == "http://gp/v1/apps/gate-api/pipelineruns/r/rerun"
    assert req.get_header("Idempotency-key") == "clearance.k" and req.get_header("X-glidepath-on-behalf-of") == "user:alice"
    conflict = GlidepathAPI("http://gp", token, opener=Opener(409, b'{"error":"exists","rule":"GP-API-409"}'))
    assert conflict.rerun("a", "r", "k", "p")["error"]["rule"] == "GP-API-409"
    with pytest.raises(Denied):
        GlidepathAPI("http://gp", token, opener=Opener(403, b'{"error":"no","rule":"GP-AUTHZ-001"}')).cancel("a", "r", "p")
    with pytest.raises(Denied) as e:
        GlidepathAPI("http://gp", token, opener=Opener(422, b'{"error":"denied by tower-pipelinerun-shape","rule":"GP-API-422"}')).rerun("a", "r", "k", "p")
    assert e.value.rule == "GP-API-422"


def test_explain_reaches_glidepath_as_the_session_principal(gw, backends):
    sid = open_(gw)
    r = gw.call("alice", sid, "glidepath.explain", {"app": "gate-api", "result": RESULT})
    assert r.ok and r.data["failures"][0]["knownFailure"]["id"] == "SourceVolumeFull"
    assert backends.glidepath.calls == [("glidepath.explain", ("gate-api", RESULT), {"on_behalf_of": "user:alice"})]
    assert gw.audit.records[-1]["tool"] == "glidepath.explain" and gw.audit.records[-1]["decision"] == "allow"


def test_an_app_off_the_allowlist_is_refused_before_any_call(gw, backends, defs):
    sid = open_(gw, agent="coding-agent")  # no glidepath tools at all
    assert gw.call("alice", sid, "glidepath.explain", {"app": "gate-api", "result": RESULT}).decision["rule"] == "R006"
    d = defs["cicd-explainer"]
    object.__setattr__(d, "raw_profile", {**d.raw_profile, "glidepath": {"apps": ["gate-*"]}})
    sid = open_(gw)
    assert gw.call("alice", sid, "glidepath.runs.list", {"app": "gate-api"}).ok
    r = gw.call("alice", sid, "glidepath.runs.list", {"app": "flight-api"})
    assert r.decision["rule"] == "R022" and r.decision["hint"]
    assert [c[0] for c in backends.glidepath.calls] == ["glidepath.runs"]


def test_no_apps_listed_means_none(gw, backends, defs):
    object.__setattr__(defs["cicd-explainer"], "raw_profile", {**defs["cicd-explainer"].raw_profile, "glidepath": {"apps": []}})
    sid = open_(gw)
    assert gw.call("alice", sid, "glidepath.runs.list", {"app": "gate-api"}).decision["rule"] == "R022"
    assert backends.glidepath.calls == []


@pytest.mark.parametrize("args", [
    {"app": "../etc", "result": RESULT},
    {"app": "gate-api", "result": "not-a-uid"},
    {"app": "gate-api", "result": RESULT + "/../x"},
    {"app": "Gate", "result": RESULT},
])
def test_arguments_that_are_not_path_segments_are_bad_arguments(gw, backends, args):
    sid = open_(gw)
    assert gw.call("alice", sid, "glidepath.explain", args).decision["rule"] == "R008"
    assert backends.glidepath.calls == []


def test_window_arguments_are_bounded(gw):
    sid = open_(gw)
    for bad in ({"sinceHours": 0}, {"sinceHours": 745}, {"limit": "10"}, {"limit": True}):
        assert gw.call("alice", sid, "glidepath.runs.list", {"app": "gate-api", **bad}).decision["rule"] == "R008", bad
    assert gw.call("alice", sid, "glidepath.runs.list", {"app": "gate-api", "sinceHours": 168, "limit": 20}).ok


# --- the HTTP adapter --------------------------------------------------------------------------

class Opener:
    def __init__(self, status=200, body=b"{}"):
        self.status, self.body, self.reqs = status, body, []

    def __call__(self, req, timeout):
        self.reqs.append(req)
        return self.status, self.body


@pytest.fixture
def token(tmp_path):
    p = tmp_path / "token"
    p.write_text("tok-1\n")
    return p


def test_adapter_sends_the_token_both_ways_and_rereads_it(token):
    o = Opener(body=b'{"runs": []}')
    api = GlidepathAPI("https://apiserver/api/v1/namespaces/platform-system/services/glidepath-api:8080/proxy/", token, opener=o)
    api.runs("gate-api", 24, None, "user:alice")
    token.write_text("tok-2")
    api.contract("user:alice")
    first, second = o.reqs
    assert first.full_url == "https://apiserver/api/v1/namespaces/platform-system/services/glidepath-api:8080/proxy/v1/apps/gate-api/runs?sinceHours=24"
    assert first.get_header("Authorization") == "Bearer tok-1" and first.get_header("X-glidepath-authorization") == "Bearer tok-1"
    assert second.get_header("Authorization") == "Bearer tok-2", "a rotated token is picked up"
    assert first.get_header("X-glidepath-on-behalf-of") is None, "off unless Clearance is a delegate"


def test_adapter_names_the_principal_when_it_is_a_delegate(token):
    o = Opener()
    GlidepathAPI("http://gp:8080", token, on_behalf_of=True, opener=o).explain("gate-api", RESULT, "user:alice")
    assert o.reqs[0].get_header("X-glidepath-on-behalf-of") == "user:alice"
    assert o.reqs[0].full_url == f"http://gp:8080/v1/apps/gate-api/runs/{RESULT}/explain"


def test_not_found_is_an_answer_a_refusal_is_a_denial_and_a_failure_is_hidden(token):
    nolog = json.dumps({"error": "no log", "rule": "GP-LOG-404", "hint": "h"}).encode()
    assert GlidepathAPI("http://gp", token, opener=Opener(404, nolog)).log("a", RESULT, "t", "p") == \
        {"error": {"rule": "GP-LOG-404", "message": "no log", "hint": "h"}}
    with pytest.raises(Denied) as e:
        GlidepathAPI("http://gp", token, opener=Opener(403, b'{"error":"no","rule":"GP-AUTHZ-001"}')).run("a", RESULT, "p")
    assert e.value.rule == "GP-AUTHZ-001"
    with pytest.raises(GlidepathError):
        GlidepathAPI("http://gp", token, opener=Opener(502, b'{"error":"results down","rule":"GP-API-502"}')).run("a", RESULT, "p")


# --- an agent starting cold, through MCP, against an HTTP glidepath-api --------------------------

EXPLANATION = {"result": RESULT, "status": "failed", "contractVersion": 1, "failures": [{
    "taskRun": "itsm-bh5bn-verify-change", "task": "verify-change", "step": "verify", "log": "archived",
    "knownFailure": {"id": "ItsmChangeNotApproved", "hint": "Get the change approved, then /retest itsm-."},
    "evidence": "error: change CHG0010035 ... is in state '-5' with approval 'not requested' - not approved or scheduled."}]}


class StubAPI(BaseHTTPRequestHandler):
    seen: list = []

    def do_GET(self):  # noqa: N802
        StubAPI.seen.append((self.path, self.headers.get("X-Glidepath-Authorization")))
        if self.path.startswith("/v1/apps/gate-api/runs?"):
            body = {"runs": [{"result": RESULT, "name": "itsm-bh5bn", "status": "failed"}]}
        elif self.path == f"/v1/apps/gate-api/runs/{RESULT}/explain":
            body = EXPLANATION
        else:
            self.send_response(404)
            self.end_headers()
            self.wfile.write(b'{"error":"no such run","rule":"GP-API-404"}')
            return
        raw = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *a):
        pass


def test_an_agent_names_the_failure_id_using_only_mcp(defs, clock, token):
    srv = HTTPServer(("127.0.0.1", 0), StubAPI)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        from conftest import StaticAuth
        from clearance.audit import AuditLog
        from clearance.dispatch import Principal
        from clearance.session import SessionStore
        api = GlidepathAPI(f"http://127.0.0.1:{srv.server_address[1]}", token)
        b = Backends(FakeGit(), FakeArgo(), FakeRuns(), FakeRead(), FakeHuman(), FakeArtifacts(), api)
        gw = hangar_gateway(defs, SessionStore(), AuditLog(), b, StaticAuth({"alice": Principal("user:alice", "user", Tier.T1)}), clock)
        mcp = create_server(gw, lambda: "alice")

        def call(name, args):
            r = asyncio.run(mcp.call_tool(name, args))
            sc = getattr(r, "structured_content", None) or getattr(r, "structuredContent", None)
            return sc.get("result", sc) if sc else json.loads(r.content[0].text)

        sid = call("session_open", {"agent": "cicd-explainer"})["data"]["session"]
        runs = call("call", {"session": sid, "tool": "glidepath.runs.list", "args": {"app": "gate-api", "sinceHours": 24}})
        failed = [r for r in runs["data"]["runs"] if r["status"] == "failed"][0]
        ex = call("call", {"session": sid, "tool": "glidepath.explain", "args": {"app": "gate-api", "result": failed["result"]}})
        assert ex["ok"] and ex["data"]["failures"][0]["knownFailure"]["id"] == "ItsmChangeNotApproved"
        assert all(auth == "Bearer tok-1" for _, auth in StubAPI.seen)
        miss = call("call", {"session": sid, "tool": "glidepath.run.get", "args": {"app": "gate-api", "result": "0" * 8 + RESULT[8:]}})
        assert miss["ok"] and miss["data"]["error"]["rule"] == "GP-API-404"
    finally:
        srv.shutdown()
