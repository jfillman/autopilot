"""The outage policy (decided 2026-11-02, Hangar spec R-3), each branch against a real Clearance core behind a switch
that can take it away: when the core is unreachable the gateway denies every T1-and-above call, every session open,
every spawn and every model call; a T0 read is denied too unless GATEWAY_OUTAGE_ALLOW_READS, and then an unscoped one
runs and waits in the outbox as a coverage-unavailable record, posted in order once the core answers again. The
client's timeout is 1 s with one retry, and its breaker opens after 5 consecutive failures."""
import http.client
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
from clearance_client import DEFAULT_BREAKER_THRESHOLD, DEFAULT_RETRIES, DEFAULT_TIMEOUT
from coreenv import GATEWAY_PRINCIPAL, tap_core

from autopilot.engine import control as ctl, inbox as hr
from autopilot.engine.backends import core_handlers
from autopilot.engine.core import DECISION_TIMEOUT, OUTAGE_RULE, client_from_env
from autopilot.engine.dispatch import Gateway, Principal
from autopilot.engine.modelproxy import ModelRoute
from autopilot.engine.session import SessionStore
from autopilot.engine.tiers import Tier
from autopilot.hangar_tools import HANGAR

PR = {"repo": "jfillman/flight-api", "branch": "agent/t-1", "title": "fix probe",
      "files": [{"path": "charts/values.yaml", "content": "x"}]}


class Switch:
    """A reverse proxy in front of a core: `up` forwards, `down` answers 503 (the core could not record), `hang`
    holds the request past any client timeout. Counts what reached it."""

    def __init__(self, target: str):
        self.target, self.mode, self.seen = urllib.parse.urlsplit(target), "up", 0
        sw = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _any(self):
                sw.seen += 1
                body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
                if sw.mode == "hang":
                    time.sleep(1.0)
                    return
                if sw.mode == "slow-once" or sw.mode == "slow":
                    if sw.mode == "slow-once":
                        sw.mode = "up"
                    time.sleep(0.4)
                if sw.mode == "down":
                    self.send_response(503)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", "2")
                    self.end_headers()
                    self.wfile.write(b"{}")
                    return
                c = http.client.HTTPConnection(sw.target.hostname, sw.target.port, timeout=5)
                c.request(self.command, self.path, body=body or None,
                          headers={k: v for k, v in self.headers.items() if k.lower() in ("authorization", "content-type")})
                r = c.getresponse()
                data = r.read()
                self.send_response(r.status)
                self.send_header("Content-Type", r.getheader("Content-Type") or "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
                c.close()

            do_GET = do_POST = _any

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}"
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()


class Auth:
    T = {"alice": Principal("user:alice", "user", Tier.T1), "root": Principal("user:root", "user", Tier.T1),
         "run": Principal("autopilot-runs/researcher", "workload", Tier.T1)}

    def authenticate(self, token):
        return self.T[token]


@pytest.fixture
def switch(test_core):
    s = Switch(test_core.url)
    yield s
    s.close()


def build(defs, clock, backends, core, **kw):
    handlers = core_handlers(git=backends.git, argo=backends.argo, read=backends.read, human=backends.human,
                             artifacts=backends.artifacts)
    return Gateway(defs, SessionStore(), core, Auth(), clock, packs=(HANGAR,), handlers=handlers, runs=backends.runs,
                   admins=("user:root",), holds=ctl.MemoryHolds(), inbox=hr.MemoryInbox(), **kw)


def opened(g, agent="coding-agent"):
    r = g.open_session("alice", agent)
    assert r.ok, r.as_dict()
    return r.data["session"]


def test_the_client_defaults_are_the_policy():
    c = client_from_env("http://core:8080", "/t")
    assert (c.timeout, c.retries, c._breaker.threshold) == (DECISION_TIMEOUT, DEFAULT_RETRIES, DEFAULT_BREAKER_THRESHOLD)
    assert (DECISION_TIMEOUT, DEFAULT_RETRIES, DEFAULT_BREAKER_THRESHOLD) == (1.0, 1, 5)
    assert DECISION_TIMEOUT > DEFAULT_TIMEOUT, "the 306 ms ledger write of 2026-11-12 must fit"
    assert client_from_env("http://core:8080", "/t", timeout=2.5).timeout == 2.5


def test_a_slow_decision_is_decided_not_denied(defs, clock, backends, switch):
    """A write slower than the old 250 ms deadline is answered, once, with no retry and no breaker failure."""
    g = build(defs, clock, backends, tap_core(url=switch.url, timeout=DECISION_TIMEOUT))
    sid = opened(g)
    switch.mode = "slow"
    seen = switch.seen
    r = g.call("alice", sid, "repo.pr.open", PR)
    assert r.ok, r.as_dict()
    assert switch.seen - seen <= 3 and g.core.breaker_state == "closed"


def test_a_timeout_is_retried_once_and_one_failure_does_not_open_the_breaker(defs, clock, backends, switch):
    g = build(defs, clock, backends, tap_core(url=switch.url, timeout=0.2))
    sid = opened(g)
    switch.mode = "slow-once"      # the first attempt exceeds 0.2 s; the retry is answered at once
    r = g.call("alice", sid, "repo.pr.open", PR)
    assert r.ok, r.as_dict()
    assert g.core.breaker_state == "closed"


def test_t1_calls_session_opens_and_spawns_fail_closed(defs, clock, backends, switch):
    g = build(defs, clock, backends, tap_core(url=switch.url, allow_reads=True))
    sid, orch = opened(g), opened(g, "orchestrator")
    switch.mode = "down"
    r = g.call("alice", sid, "repo.pr.open", PR)
    assert not r.ok and r.decision["rule"] == OUTAGE_RULE and r.decision["hint"]
    assert backends.git.calls == [], "nothing acted on without a decision"
    r = g.call("alice", sid, "argo.sync.lower", {"app": "flight-api-dev", "project": "flight-api-lower"})
    assert not r.ok and r.decision["rule"] == OUTAGE_RULE
    r = g.open_session("alice", "coding-agent")
    assert not r.ok and r.decision["rule"] == OUTAGE_RULE and r.session is None
    assert not g.open_triggered("triage-agent", "glidepath:e1").ok
    r = g.call("alice", orch, "run.spawn", {"agent": "researcher", "limits": {"tool_calls": 5}})
    assert not r.ok and r.decision["rule"] == OUTAGE_RULE and backends.runs.calls == []
    assert g.store.get(orch).children == [] and g.store.get(sid).used["tool_calls"] == 0, "and nothing is charged"
    assert g.core.outbox.all() == []


def test_a_t0_read_fails_closed_by_default(defs, clock, backends, switch):
    g = build(defs, clock, backends, tap_core(url=switch.url))
    sid = opened(g)
    switch.mode = "down"
    r = g.call("alice", sid, "catalog.read", {"query": "kind=component"})
    assert not r.ok and r.decision["rule"] == OUTAGE_RULE and backends.read.calls == []
    assert g.core.outbox.all() == []


def test_with_the_switch_an_unscoped_t0_read_runs_and_waits_in_the_outbox(defs, clock, backends, switch):
    g = build(defs, clock, backends, tap_core(url=switch.url, allow_reads=True))
    sid = opened(g)
    switch.mode = "down"
    r = g.call("alice", sid, "catalog.read", {"query": "kind=component"})
    assert r.ok and r.decision["allow"] and backends.read.calls, r.as_dict()
    assert g.store.get(sid).used["tool_calls"] == 1, "charged as any call"
    [row] = g.core.outbox.all()
    assert row["reason"] == "outage" and row["state"] == "pending" and row["entry"]["kind"] == "tool.call"
    body = row["entry"]["body"]
    assert body["coverage"] == "coverage-unavailable" and body["result"] == "ok" and body["session"] == sid
    assert body["principal"] == "user:alice" and body["decision"] == "allow" and len(body["args_sha256"]) == 64
    assert "GATEWAY_OUTAGE_ALLOW_READS" in body["reason"]
    # Scope only the core can check: a read naming a repo or an app stays denied, as does a tool not granted.
    r = g.call("alice", sid, "repo.file.get", {"repo": "jfillman/flight-api", "path": "README.md"})
    assert not r.ok and r.decision["rule"] == OUTAGE_RULE
    r = g.call("alice", sid, "argo.app.get", {"app": "flight-api-dev"})
    assert not r.ok and r.decision["rule"] == OUTAGE_RULE
    r = g.call("alice", sid, "artifact.get", {"name": "plan"})
    assert not r.ok, "not granted to coding-agent"
    # And a T1 call is still denied with the switch on.
    assert not g.call("alice", sid, "repo.pr.open", PR).ok and backends.git.calls == []


def test_model_calls_fail_closed(defs, clock, backends, switch):
    class MAuth:
        def authenticate(self, token):
            return Principal(token, "workload", Tier.T1)
    route = {"claude-sonnet-5": ModelRoute("claude-sonnet-5", "anthropic", "https://api.anthropic.com", "m")}
    g = build(defs, clock, backends, tap_core(url=switch.url, allow_reads=True), models=route, model_auth=MAuth())
    sid = opened(g)
    assert g.model_authorize("user:alice", sid, "claude-sonnet-5", 10).allow
    switch.mode = "down"
    d = g.model_authorize("user:alice", sid, "claude-sonnet-5", 10)
    assert not d.allow and d.rule == OUTAGE_RULE


def test_records_wait_in_order_and_are_posted_once_the_core_answers(defs, clock, backends, switch):
    core = tap_core(url=switch.url, allow_reads=True)
    g = build(defs, clock, backends, core)
    sid = opened(g)
    before = len(core.records)
    switch.mode = "down"
    assert g.call("alice", sid, "catalog.read", {"query": "a"}).ok
    assert g.call("alice", sid, "metrics.query", {"promql": "up"}).ok
    out = g.freeze_session("alice", sid, "looping")            # takes effect at once; its record waits
    assert out["tripped"] and out["audit_seq"] is None
    assert [r["reason"] for r in core.outbox.all()] == ["outage", "outage", "core-unavailable"]
    assert len(core.records) == before, "nothing reached the core"
    switch.mode = "up"
    g.expire()                                                 # the sweep drains the outbox
    recs = core.records[before:]
    # An outage read is the core's `tool.call` record naming the tool that ran (clearance v0.28.0), claimed.
    assert [(r["tool"], r["coverage"]) for r in recs] == [
        ("catalog.read", "coverage-unavailable"), ("metrics.query", "coverage-unavailable"),
        ("session.freeze", "authorized-and-verified")]
    assert [r["result"] for r in recs] == ["ok", "ok", "frozen: looping"]
    assert [(r["tier"], r["decision"], r.get("rule")) for r in recs[:2]] == [(0, "allow", None)] * 2
    assert all("GATEWAY_OUTAGE_ALLOW_READS" in r["reason"] for r in recs[:2])
    assert all(r["provider"]["name"] == "autopilot-gateway" and r["session"] == sid for r in recs)
    assert all(r["state"] == "done" for r in core.outbox.all())
    # A new record goes after what waited, never before it.
    switch.mode = "down"
    g.end_session("alice", sid, "stop")
    switch.mode = "up"
    g.hold_triggers("root", "triage-agent", "noisy")
    tail = core.records[before + 3:]
    assert [r["tool"] for r in tail] == ["session.end", "trigger.hold"]


def test_a_row_an_older_gateway_held_is_posted_once_the_core_takes_its_kind(defs, clock, backends, switch):
    """Before clearance v0.28.0 the gateway held run.finish, session.end and trigger.release in the outbox. The core
    takes them now: drain posts such a row in order, and nothing new is held."""
    core = tap_core(url=switch.url)
    g = build(defs, clock, backends, core)
    sid = opened(g)
    core.outbox.add({"op": "record", "kind": "session.end", "body": {
        "session": sid, "principal": "user:alice", "agent": "coding-agent", "target": sid, "decision": "allow",
        "result": "ended: stop"}}, "held")
    g.expire()
    [row] = core.outbox.all()
    assert row["state"] == "done" and core.at(row["seq"])["tool"] == "session.end"
    assert g.freeze_session("alice", sid, "and now")["audit_seq"] is not None and len(core.outbox.all()) == 1


def test_a_kind_the_core_does_not_take_is_held_until_it_does(defs, clock, backends, switch):
    # A rolling upgrade: a client that does not list session.end (record_kinds) holds it rather than send it.
    from clearance_client import RECORD_KINDS
    g = build(defs, clock, backends, tap_core(url=switch.url, record_kinds=[k for k in RECORD_KINDS if k != "session.end"]))
    sid = opened(g)
    g.end_session("alice", sid, "stop")
    [row] = g.core.outbox.all()
    assert (row["reason"], row["state"], row["entry"]["kind"]) == ("held", "pending", "session.end")
    g.expire()
    assert g.core.outbox.all()[0]["state"] == "pending", "a held kind is not posted to a core that refuses it"


def test_a_refusal_goes_on_the_outcome_only_with_a_rule_id():
    """Core.outcome's refusal is (rule, message); the core takes `refusal` only for a rule id (R004, HR-003,
    ART-002), so anything else is sent without it (the result line still names it). A refusal carries no effect."""
    from autopilot.engine.core import Core
    from autopilot.engine.outbox import MemoryOutbox

    class Client:
        sent = []

        def outcome(self, decision_id, body):
            self.sent.append(body)

            class R:
                seq = 1
            return R()
    c = Client()
    core = Core(c, MemoryOutbox())
    core.outcome("d-1", "refused HR-003: no", refusal=("HR-003", "no"))
    core.outcome("d-2", "refused x: no", refusal=("not a rule", "no"))
    core.outcome("d-3", "refused ART-002: big", effect={"repo": "a/b"}, refusal=("ART-002", "big"))
    core.outcome("d-4", "ok")
    assert c.sent == [{"result": "refused HR-003: no", "refusal": {"rule": "HR-003", "message": "no"}},
                      {"result": "refused x: no"},
                      {"result": "refused ART-002: big", "refusal": {"rule": "ART-002", "message": "big"}},
                      {"result": "ok"}]


def test_the_breaker_opens_after_five_failures_and_then_fails_fast(defs, clock, backends, switch):
    g = build(defs, clock, backends, tap_core(url=switch.url, timeout=0.05))
    sid = opened(g)
    switch.mode = "hang"
    for _ in range(DEFAULT_BREAKER_THRESHOLD):
        assert g.call("alice", sid, "catalog.read", {"query": "x"}).decision["rule"] == OUTAGE_RULE
    assert g.core.breaker_state == "open"
    seen = switch.seen
    t = time.monotonic()
    r = g.call("alice", sid, "repo.pr.open", PR)
    took = time.monotonic() - t
    assert not r.ok and r.decision["rule"] == OUTAGE_RULE and switch.seen == seen, "nothing sent"
    assert took < 0.02, f"a fast denial, not a timeout ({took * 1000:.1f} ms)"


def test_a_human_answer_and_a_launch_need_their_record(defs, clock, backends, switch):
    g = build(defs, clock, backends, tap_core(url=switch.url))
    sid = opened(g, "orchestrator")
    rid = g.call("alice", sid, "human.request", {"question": "ship it?"}).data["request"]
    switch.mode = "down"
    with pytest.raises(hr.InboxError) as e:
        g.human_answer("alice", rid, "yes")
    assert e.value.rule == OUTAGE_RULE and e.value.status == 503
    assert g.inbox.get(rid).status == "pending", "not answered without its record"


def test_a_core_that_refuses_the_request_is_a_deny_too(defs, clock, backends, test_core):
    # A token the core does not know as a provider: the core answers 403; that is no decision either.
    from autopilot.engine.core import Core, REFUSED_RULE
    from autopilot.engine.outbox import MemoryOutbox
    from clearance_client import CoreClient
    g = build(defs, clock, backends, Core(CoreClient(test_core.url, token="someone/else"), MemoryOutbox(),
                                          allow_reads=True))
    r = g.open_session("alice", "coding-agent")
    assert not r.ok and r.decision["rule"] == REFUSED_RULE
    assert GATEWAY_PRINCIPAL != "someone/else"
