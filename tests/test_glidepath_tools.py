"""glidepath.* tools: Clearance's T0 reads of glidepath-api (spec phase 2), run control (phase 3) and release
proposals (phase 4)."""
import asyncio
import dataclasses
import json
import re
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from autopilot.backends import Backends, FakeArtifacts, FakeArgo, FakeGit, FakeHuman, FakeRead, FakeRuns
from autopilot.gateway import TOOLS, hangar_gateway, release_key
from autopilot.glidepath_api import GlidepathAPI, GlidepathError
from clearance.dispatch import Denied
from clearance.server import create_server
from clearance.tiers import TRIPWIRES, Tier

RESULT = "0b9d1a2e-1111-4c4c-9999-aaaaaaaaaaaa"


def open_(gw, agent="cicd-explainer", token="alice"):
    r = gw.open_session(token, agent)
    assert r.ok, r.as_dict()
    return r.data["session"]


WRITES = {"glidepath.run.rerun", "glidepath.run.cancel"}
PROPOSALS = {"glidepath.promote", "glidepath.rollback"}


def test_glidepath_tools_are_reads_except_run_control_and_release_proposals():
    gp = {t.name: t for t in TOOLS.values() if t.name.startswith("glidepath.")}
    assert len(gp) == 12 and "pipeline.rerun" not in TOOLS, "pipeline.rerun is retired for glidepath.run.rerun"
    assert {"glidepath.environments.list", "glidepath.environments.rollbackPlan"} <= set(gp)
    assert all(t.tier == Tier.T0 for n, t in gp.items() if n not in WRITES | PROPOSALS)
    assert all(gp[n].tier == Tier.T1 and gp[n].target == "lower" and not gp[n].writes_files for n in WRITES)
    for n in PROPOSALS:  # T2 is propose only: a human merge applies it, and Clearance spends no GitHub call on it
        t = gp[n]
        assert t.tier == Tier.T2 and t.propose_only and t.target == "upper", n
        assert not t.writes_files and t.github_cost == 0, n


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


# --- phase 4: environments, promote and roll back (T2, propose only) ------------------------------

PROMOTE = {"app": "gate-api", "env": "staging", "cluster": "hangar-prod", "sourceEnv": "dev", "sourceCluster": "hangar-dev"}
ROLLBACK = {"app": "gate-api", "env": "staging", "cluster": "hangar-prod", "targetReleaseId": "rel-2",
            "reason": "5xx after rel-3"}


def test_the_release_reads_reach_glidepath_as_the_session_principal(gw, backends):
    sid = open_(gw, agent="release-proposer")
    plan = gw.call("alice", sid, "glidepath.environments.list", {"app": "gate-api"})
    assert plan.ok and plan.data["upper"][0] == {"name": "staging", "cluster": "hangar-prod"}
    rb = gw.call("alice", sid, "glidepath.environments.rollbackPlan", {"app": "gate-api", "env": "staging", "cluster": "hangar-prod"})
    assert rb.ok and rb.data["targets"][0]["releaseId"] == "rel-2"
    assert gw.call("alice", sid, "glidepath.environments.rollbackPlan", {"app": "gate-api", "env": "staging"}).decision["rule"] == "R008"
    assert backends.glidepath.calls == [
        ("glidepath.environments", ("gate-api",), {"on_behalf_of": "user:alice"}),
        ("glidepath.rollback_plan", ("gate-api", "staging", "hangar-prod"), {"on_behalf_of": "user:alice"})]


def test_a_proposal_needs_the_definition_to_name_it(gw, backends):
    # cicd-explainer (T0, reads only) and coding-agent (T1, no proposal tools) never reach glidepath-api.
    for agent in ("cicd-explainer", "coding-agent"):
        assert gw.call("alice", open_(gw, agent=agent), "glidepath.promote", PROMOTE).decision["rule"] == "R006", agent
    # The T2 tools are not capped by the ceiling (R007 lets propose-only through at T1), but a T0 principal is.
    ro = gw.open_session("readonly", "release-proposer").data["session"]
    assert gw.call("readonly", ro, "glidepath.promote", PROMOTE).decision["rule"] == "R007"
    sid = open_(gw, agent="release-proposer")
    assert gw.call("alice", sid, "glidepath.promote", PROMOTE).ok
    assert gw.call("alice", sid, "glidepath.rollback", ROLLBACK).ok
    assert [c[0] for c in backends.glidepath.calls] == ["glidepath.promote", "glidepath.rollback"]
    assert all(c[2]["on_behalf_of"] == "user:alice" for c in backends.glidepath.calls)


def test_proposals_are_scoped_to_the_definitions_apps(gw, backends, defs):
    sid = open_(gw, agent="release-proposer")
    for tool, args in (("glidepath.promote", PROMOTE), ("glidepath.rollback", ROLLBACK),
                       ("glidepath.environments.list", {"app": "gate-api"})):
        r = gw.call("alice", sid, tool, {**args, "app": "flight-api"})
        assert r.decision["rule"] == "R022" and r.decision["hint"], tool
    object.__setattr__(defs["release-proposer"], "raw_profile", {**defs["release-proposer"].raw_profile, "glidepath": {"apps": []}})
    assert gw.call("alice", open_(gw, agent="release-proposer"), "glidepath.promote", PROMOTE).decision["rule"] == "R022"
    assert backends.glidepath.calls == []


def keys(backends, name):
    return [c[1][3] for c in backends.glidepath.calls if c[0] == name]


def test_a_retried_proposal_reuses_its_key_so_glidepath_starts_one_run(gw, backends):
    sid = open_(gw, agent="release-proposer")
    first = gw.call("alice", sid, "glidepath.promote", PROMOTE)
    again = gw.call("alice", sid, "glidepath.promote", dict(reversed(list(PROMOTE.items()))))  # argument order is not the key
    assert first.ok and again.ok and "replayed" not in first.data and again.data["replayed"] is True
    k = keys(backends, "glidepath.promote")
    assert len(set(k)) == 1 and re.fullmatch(r"clearance\.[0-9a-f]{32}", k[0]), "glidepath-api's [A-Za-z0-9._:-]{1,128}"
    assert k[0] == release_key(sid, "promote", "gate-api", "staging", "hangar-prod", "env:dev@hangar-dev"), "stable, not random"
    # Anything that makes it a different promotion is a different key: the source, the cluster, the session.
    gw.call("alice", sid, "glidepath.promote", {**PROMOTE, "sourceCluster": "hangar-dev-2"})
    gw.call("alice", sid, "glidepath.promote", {"app": "gate-api", "env": "staging", "cluster": "hangar-prod",
                                                "sourceImageRepo": "ghcr.io/jfillman/gate-api", "sourceImageTag": "1.0.0-abc1234"})
    gw.call("alice", open_(gw, agent="release-proposer"), "glidepath.promote", PROMOTE)
    assert len(set(keys(backends, "glidepath.promote"))) == 4
    # A rollback's key is its target, not its reason: a retry that rewords the reason still replays.
    gw.call("alice", sid, "glidepath.rollback", ROLLBACK)
    again = gw.call("alice", sid, "glidepath.rollback", {**ROLLBACK, "reason": "  error rate up since rel-3 "})
    assert again.data["replayed"] is True and len(set(keys(backends, "glidepath.rollback"))) == 1
    assert backends.glidepath.calls[-1][1][2]["reason"] == "error rate up since rel-3", "trimmed, as glidepath-api does"
    gw.call("alice", sid, "glidepath.rollback", {**ROLLBACK, "targetReleaseId": "rel-1"})
    assert len(set(keys(backends, "glidepath.rollback"))) == 2


def test_a_dry_run_starts_nothing_and_names_the_run_the_real_call_would_start(gw, backends):
    sid = open_(gw, agent="release-proposer")
    dry = gw.call("alice", sid, "glidepath.promote", {**PROMOTE, "dryRun": True})
    assert dry.ok and dry.data["dryRun"] is True and "object" in dry.data
    real = gw.call("alice", sid, "glidepath.promote", PROMOTE)
    assert real.ok and "replayed" not in real.data, "a dry run stored nothing, so the real call is not a replay"
    assert dry.data["pipelineRun"] == real.data["pipelineRun"]
    assert [c[1][4] for c in backends.glidepath.calls] == [True, False]
    assert len(set(keys(backends, "glidepath.promote"))) == 1
    body = backends.glidepath.calls[-1][1][2]
    assert body == {"cluster": "hangar-prod", "sourceEnv": "dev", "sourceCluster": "hangar-dev"}, "dryRun is a header, not body"
    rb = gw.call("alice", sid, "glidepath.rollback", {**ROLLBACK, "dryRun": True})
    assert rb.ok and rb.data["dryRun"] is True and backends.glidepath.calls[-1][1][4] is True


@pytest.mark.parametrize("tool,args", [
    ("glidepath.promote", {k: v for k, v in PROMOTE.items() if k not in ("sourceEnv", "sourceCluster")}),  # no source
    ("glidepath.promote", {**PROMOTE, "sourceImageRepo": "ghcr.io/o/a", "sourceImageTag": "1.0.0-abc1234"}),  # both
    ("glidepath.promote", {k: v for k, v in PROMOTE.items() if k != "sourceCluster"}),  # half a pair
    ("glidepath.promote", {**PROMOTE, "sourceImageTag": "1.0.0-abc1234"}),  # a pair and a half
    ("glidepath.promote", {**PROMOTE, "env": "../prod"}),
    ("glidepath.promote", {**PROMOTE, "cluster": "hangar prod"}),
    ("glidepath.promote", {**PROMOTE, "env": "staging\n"}),
    ("glidepath.promote", {**PROMOTE, "dryRun": "true"}),
    ("glidepath.rollback", {**ROLLBACK, "reason": "   "}),
    ("glidepath.rollback", {**ROLLBACK, "reason": "x" * 1025}),
    ("glidepath.rollback", {**ROLLBACK, "targetReleaseId": "rel 2"}),
    ("glidepath.rollback", {k: v for k, v in ROLLBACK.items() if k != "reason"}),
])
def test_proposal_arguments_are_checked_before_any_call(gw, backends, tool, args):
    assert gw.call("alice", open_(gw, agent="release-proposer"), tool, args).decision["rule"] == "R008"
    assert backends.glidepath.calls == []


def test_a_refused_proposal_is_a_denial_with_glidepaths_hint(defs, clock, token):
    """422 (out of order, a Ground target, an ineligible rollback target, or admission) is a policy answer: Denied,
    with glidepath-api's rule and hint, recorded as a denial; the agent should not retry it."""
    from conftest import StaticAuth
    from clearance.audit import AuditLog
    from clearance.dispatch import Principal
    from clearance.session import SessionStore
    refusal = {"error": "dev is a Ground environment", "rule": "GP-API-422",
               "hint": "Promote to a Flight environment; Ground promotions are a commit to the app repo."}
    api = GlidepathAPI("http://gp", token, on_behalf_of=True, opener=Opener(422, json.dumps(refusal).encode()))
    b = Backends(FakeGit(), FakeArgo(), FakeRuns(), FakeRead(), FakeHuman(), FakeArtifacts(), api)
    gw = hangar_gateway(defs, SessionStore(), AuditLog(), b, StaticAuth({"alice": Principal("user:alice", "user", Tier.T1)}), clock)
    sid = gw.open_session("alice", "release-proposer").data["session"]
    r = gw.call("alice", sid, "glidepath.promote", {**PROMOTE, "env": "dev"})
    assert not r.ok and r.decision["rule"] == "GP-API-422" and r.decision["hint"] == refusal["hint"]
    assert "Ground" in r.decision["message"] and gw.audit.records[-1]["decision"] == "deny"
    # With no hint from the API, the agent is pointed at the two plans.
    bare = GlidepathAPI("http://gp", token, opener=Opener(422, b'{"error":"not a target","rule":"GP-API-422"}'))
    with pytest.raises(Denied) as e:
        bare.rollback("gate-api", "staging", {}, "k", False, "p")
    assert "rollbackPlan" in e.value.hint


def test_adapter_posts_a_proposal_with_its_body_key_and_dry_run_header(token):
    o = Opener(201, b'{"mode":"pipeline","pipelineRun":"promote-staging-abc1234d-3f9a1c"}')
    api = GlidepathAPI("http://gp", token, on_behalf_of=True, opener=o)
    body = {"cluster": "hangar-prod", "sourceEnv": "dev", "sourceCluster": "hangar-dev"}
    assert api.promote("gate-api", "staging", body, "clearance.k", False, "user:alice")["mode"] == "pipeline"
    api.promote("gate-api", "staging", body, "clearance.k", True, "user:alice")
    api.rollback("gate-api", "prod", {"cluster": "hangar-prod", "targetReleaseId": "rel-2", "reason": "r"}, "clearance.r", False, "user:alice")
    o.status = 200
    api.environments("gate-api", "user:alice")
    api.rollback_plan("gate-api", "staging", "hangar-prod", "user:alice")
    real, dry, rb, envs, plan = o.reqs
    assert real.get_method() == "POST" and real.full_url == "http://gp/v1/apps/gate-api/environments/staging/promote"
    assert json.loads(real.data) == body and real.get_header("Content-type") == "application/json"
    assert real.get_header("Idempotency-key") == "clearance.k" and real.get_header("X-glidepath-on-behalf-of") == "user:alice"
    assert real.get_header("X-glidepath-dry-run") == "false" and dry.get_header("X-glidepath-dry-run") == "true"
    assert "dryRun" not in dry.full_url, "the API server's service proxy refuses ?dryRun; the header works both ways"
    assert rb.full_url == "http://gp/v1/apps/gate-api/environments/prod/rollback" and json.loads(rb.data)["targetReleaseId"] == "rel-2"
    assert envs.get_method() == "GET" and envs.full_url == "http://gp/v1/apps/gate-api/environments"
    assert plan.full_url == "http://gp/v1/apps/gate-api/environments/staging/rollback-plan?cluster=hangar-prod"
    pr = GlidepathAPI("http://gp", token, opener=Opener(200, b'{"mode":"pr","prUrl":"https://github.com/o/gitops-gate-api/pull/7","alreadyOpen":true}'))
    assert pr.promote("gate-api", "staging", body, "k", False, "p")["alreadyOpen"] is True
    assert GlidepathAPI("http://gp", token, opener=Opener(409, b'{"error":"taken","rule":"GP-API-409"}')).promote(
        "gate-api", "staging", body, "k", False, "p")["error"]["rule"] == "GP-API-409"


# --- the phase-4 done-when: the agent had no way to merge -----------------------------------------

# Every tool that reaches GitHub with Clearance's own identity: it pushes a branch, commits, or opens a PR.
GITHUB_WRITERS = {n for n, t in TOOLS.items() if t.github_cost > 0 or t.writes_files}


def test_the_registry_has_no_tool_that_merges_or_approves():
    assert GITHUB_WRITERS == {"repo.pr.open", "repo.pr.open_upper", "xr.request"}, \
        "a new GitHub-writing tool: decide whether release-proposer may ever see it"
    for name in TOOLS:
        assert not re.search(r"merge|approv|push|review|branch", name), f"{name}: a tool that can merge or approve needs review here"
    assert "branch_protection.update" in TRIPWIRES


def test_release_proposer_cannot_reach_any_github_write(gw, backends, defs):
    d = defs["release-proposer"]
    assert d.limits.tools == {"glidepath.environments.list", "glidepath.environments.rollbackPlan", "glidepath.releases.get",
                              "glidepath.runs.list", "glidepath.run.get", "glidepath.explain", "glidepath.contract",
                              "glidepath.promote", "glidepath.rollback"}
    assert all(t.startswith("glidepath.") for t in d.limits.tools) and not d.limits.tools & GITHUB_WRITERS
    assert d.limits.github_calls == 0 and d.limits.open_prs == 0 and d.limits.children == 0 and d.repos_allow == ()
    assert d.raw_profile["glidepath"]["apps"] == ["gate-api"] and d.identity_type == "delegated"
    sid = open_(gw, agent="release-proposer")
    pr = {"repo": "jfillman/gitops-gate-api", "branch": "x", "title": "t", "files": [{"path": "a.yaml", "content": "a: 1"}]}
    for tool, args in (("repo.pr.open_upper", pr), ("repo.pr.open", pr),
                       ("xr.request", {"app": "gate-api", "env": "dev", "kind": "K", "spec": {}}),
                       ("run.spawn", {"agent": "coding-agent", "limits": {}})):
        assert gw.call("alice", sid, tool, args).decision["rule"] == "R006", tool
    assert backends.git.calls == [] and backends.runs.calls == []
    # Even a definition that wrongly listed a GitHub tool has no GitHub budget to spend on it.
    wider = dataclasses.replace(d.limits, tools=d.limits.tools | {"repo.pr.open_upper", "repo.pr.open"})
    object.__setattr__(d, "limits", wider)
    sid = open_(gw, agent="release-proposer")
    assert gw.call("alice", sid, "repo.pr.open_upper", pr).decision["rule"] == "R010"
    assert gw.call("alice", sid, "repo.pr.open", pr).decision["rule"] == "R010"
    assert backends.git.calls == []


# --- through MCP, against an HTTP glidepath-api ---------------------------------------------------

class ReleaseStub(BaseHTTPRequestHandler):
    seen: list = []

    def _send(self, status, body):
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):  # noqa: N802
        ReleaseStub.seen.append(("GET", self.path, dict(self.headers), None))
        if self.path == "/v1/apps/gate-api/environments":
            return self._send(200, {"order": ["dev", "staging", "prod"], "orderSource": "promotionOrder", "lower": ["dev"],
                                    "upper": [{"name": "staging", "cluster": "hangar-prod"},
                                              {"name": "prod", "cluster": "hangar-prod", "production": True}]})
        self._send(404, {"error": "no such route", "rule": "GP-API-404"})

    def do_POST(self):  # noqa: N802
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        ReleaseStub.seen.append(("POST", self.path, dict(self.headers), body))
        if self.path != "/v1/apps/gate-api/environments/staging/promote":
            return self._send(422, {"error": "dev is a Ground environment", "rule": "GP-API-422",
                                    "hint": "Promote to a Flight environment."})
        name = "promote-staging-abc1234d-" + self.headers["Idempotency-Key"][-6:]
        if self.headers.get("X-Glidepath-Dry-Run") == "true":
            return self._send(200, {"mode": "pipeline", "pipelineRun": name, "dryRun": True, "object": {"kind": "PipelineRun"}})
        replayed = sum(1 for m, p, h, _ in ReleaseStub.seen if m == "POST" and h.get("X-Glidepath-Dry-Run") == "false"
                       and h["Idempotency-Key"] == self.headers["Idempotency-Key"]) > 1
        self._send(200 if replayed else 201, {"mode": "pipeline", "pipelineRun": name, **({"replayed": True} if replayed else {})})

    def log_message(self, *a):
        pass


def test_an_agent_proposes_a_staging_promotion_using_only_mcp(defs, clock, token):
    ReleaseStub.seen = []
    srv = HTTPServer(("127.0.0.1", 0), ReleaseStub)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        from conftest import StaticAuth
        from clearance.audit import AuditLog
        from clearance.dispatch import Principal
        from clearance.session import SessionStore
        api = GlidepathAPI(f"http://127.0.0.1:{srv.server_address[1]}", token, on_behalf_of=True)
        git = FakeGit()
        b = Backends(git, FakeArgo(), FakeRuns(), FakeRead(), FakeHuman(), FakeArtifacts(), api)
        gw = hangar_gateway(defs, SessionStore(), AuditLog(), b, StaticAuth({"alice": Principal("user:alice", "user", Tier.T1)}), clock)
        mcp = create_server(gw, lambda: "alice")

        def call(name, args):
            r = asyncio.run(mcp.call_tool(name, args))
            sc = getattr(r, "structured_content", None) or getattr(r, "structuredContent", None)
            return sc.get("result", sc) if sc else json.loads(r.content[0].text)

        sid = call("session_open", {"agent": "release-proposer"})["data"]["session"]
        plan = call("call", {"session": sid, "tool": "glidepath.environments.list", "args": {"app": "gate-api"}})["data"]
        nxt = plan["order"][plan["order"].index("dev") + 1]
        staging = next(u for u in plan["upper"] if u["name"] == nxt)
        args = {"app": "gate-api", "env": staging["name"], "cluster": staging["cluster"], "sourceEnv": "dev", "sourceCluster": "hangar-dev"}
        dry = call("call", {"session": sid, "tool": "glidepath.promote", "args": {**args, "dryRun": True}})
        real = call("call", {"session": sid, "tool": "glidepath.promote", "args": args})
        retry = call("call", {"session": sid, "tool": "glidepath.promote", "args": args})
        assert dry["ok"] and dry["data"]["dryRun"] and real["ok"] and real["data"]["pipelineRun"] == dry["data"]["pipelineRun"]
        assert "replayed" not in real["data"] and retry["data"]["replayed"] is True
        ground = call("call", {"session": sid, "tool": "glidepath.promote", "args": {**args, "env": "dev"}})
        assert not ground["ok"] and ground["decision"]["rule"] == "GP-API-422" and ground["decision"]["hint"]
        merge = call("call", {"session": sid, "tool": "repo.pr.open_upper", "args": {
            "repo": "jfillman/gitops-gate-api", "branch": "b", "title": "t", "files": [{"path": "a.yaml", "content": "a: 1"}]}})
        assert merge["decision"]["rule"] == "R006" and git.calls == []
        posts = [(p, h, bd) for m, p, h, bd in ReleaseStub.seen if m == "POST"]
        assert len(posts) == 4
        assert all(h["X-Glidepath-On-Behalf-Of"] == "user:alice" and h["X-Glidepath-Authorization"] == "Bearer tok-1" for _, h, _ in posts)
        assert len({h["Idempotency-Key"] for p, h, _ in posts if p.endswith("/staging/promote")}) == 1
        assert posts[0][2] == {"cluster": "hangar-prod", "sourceEnv": "dev", "sourceCluster": "hangar-dev"}
    finally:
        srv.shutdown()
