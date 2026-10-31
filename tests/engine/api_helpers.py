"""clearance-api (api.py): who may call what, the scoped reads, the chain check, and the committed contract. The SQL is
exercised against a real Postgres the gateway itself wrote (CLEARANCE_TEST_DSN; skipped without it)."""
import json
import os
from datetime import timedelta
from pathlib import Path

import pytest
from starlette.testclient import TestClient

from clearance import api
from clearance.audit import AuditLog
from clearance.dispatch import AuthError, Gateway, Principal
from clearance.packs.hangar import HANGAR, app_of
from clearance.tiers import Tier

ROOT = Path(__file__).resolve().parents[1]
DSN = os.environ.get("CLEARANCE_TEST_DSN")
TOWER = "hangar-prod/app-backstage-prod/backstage-ingestor"


class Auth:
    table = {"alice": Principal("user:alice", "user", Tier.T1), "bob": Principal("user:bob", "user", Tier.T1),
             "carol": Principal("user:carol", "user", Tier.T1), "audit": Principal("user:auditor", "user", Tier.T1),
             "tower": Principal(TOWER, "workload", Tier.T1), "svc": Principal("ns/svc", "workload", Tier.T1)}

    def authenticate(self, token):
        if token not in self.table:
            raise AuthError("no")
        return self.table[token]


class Groups:
    def groups(self, p):
        return {"group:default/platform"} if p == "user:bob" else set()


class Owns:
    def apps(self, p):
        return {"flight-api"} if p == "user:carol" else set()


@pytest.fixture
def access(defs):
    return api.Access(Auth(), defs, delegates=(TOWER,), notifiers=(TOWER,), auditors=("user:auditor",),
                      directory=Groups(), ownership=Owns())


def test_the_committed_contract_is_current():
    assert json.loads((ROOT / "contract" / "clearance-api.json").read_text()) == api.contract()
    ops = {o["operationId"] for o in api.contract()["operations"]}
    assert {"listSessions", "listEvidence", "verifyEvidence", "listHumanRequests", "getTask"} <= ops


def test_hangar_app_of():
    assert app_of("glidepath.run.rerun", {"app": "gate-api"}) == "gate-api"
    assert app_of("repo.pr.open", {"repo": "jfillman/gitops-flight-api"}) == "flight-api"
    assert app_of("repo.pr.open", {"repo": "jfillman/gitops-infra-skyport-db"}) == "skyport-db"
    assert app_of("repo.file.get", {"repo": "jfillman/gate-api"}) == "gate-api"
    assert app_of("argo.sync.lower", {"app": "gate-api-dev"}) is None
    assert app_of("metrics.query", {"promql": "up"}) is None


def test_viewers(access):
    with pytest.raises(api.ApiError) as e:
        access.viewer(None, None)
    assert e.value.status == 401
    with pytest.raises(api.ApiError) as e:
        access.viewer("nope", None)
    assert e.value.status == 401
    bob = access.viewer("bob", None)
    assert bob.role == "person" and "orchestrator" in bob.agents and bob.apps == frozenset()
    assert access.viewer("carol", None).apps == {"flight-api"}
    assert access.viewer("audit", None).all
    assert access.viewer("tower", None).role == "notifier"
    t = access.viewer("tower", "user:bob")
    assert (t.role, t.person, t.via) == ("person", "user:bob", TOWER) and "orchestrator" in t.agents
    for tok, who, status in (("svc", None, 403), ("svc", "user:bob", 403), ("tower", "bob", 400)):
        with pytest.raises(api.ApiError) as e:
            access.viewer(tok, who)
        assert e.value.status == status


def test_verify_chain_streams_and_finds_the_first_bad_record():
    log = AuditLog()
    for i in range(5):
        log.append({"ts": "t", "tool": "x", "i": i})
    recs = [dict(r) for r in log.records]
    assert api.verify_chain([recs[:2], recs[2:]]) == {"ok": True, "count": 5, "head": recs[-1]["hash"], "bad_seq": None}
    recs[3]["tool"] = "edited"
    assert api.verify_chain([recs]) == {"ok": False, "count": 3, "head": recs[2]["hash"], "bad_seq": 3}


class MemStore:
    """Just enough of ReadStore for the HTTP layer (the SQL is the pg test's)."""

    def __init__(self, sessions, evidence, human, events=(), holds=None):
        self.s, self.e, self.h, self.ev, self.hd = sessions, evidence, human, list(events), dict(holds or {})

    def holds(self, now):
        return dict(self.hd)

    def _sees(self, r, v):
        return v.all or r.get("principal") == v.person or r.get("agent") in v.agents or r.get("app") in v.apps

    def events(self, f, v, limit, after):
        # Postgres compares timestamps; this fake compares ISO strings, so Z and +00:00 are made alike first.
        t = lambda s: s.replace("Z", "+00:00") if s else s  # noqa: E731

        def sees(e):
            return v.all or e.get("app") in v.apps or any(o.get("agent") in v.agents for o in e["outcomes"])

        def matches(e):
            return (all(e.get({"key": "routing_key"}.get(k, k)) == x for k, x in f.items() if k not in ("agent", "subject", "since", "until"))
                    and (not f.get("agent") or any(o.get("agent") == f["agent"] for o in e["outcomes"]))
                    and (not f.get("subject") or e["subject"].get("id") == f["subject"])
                    and (not f.get("since") or e["received_at"] >= t(f["since"])) and (not f.get("until") or e["received_at"] < t(f["until"])))
        out = sorted((e for e in self.ev if sees(e) and matches(e)), key=lambda e: e["received_at"], reverse=True)
        return [e for e in out if not after or e["received_at"] < t(after)][:limit]

    def sessions(self, f, v, limit, after):
        out = [dict(s, state="live") for s in self.s if self._sees(s, v) and all(s.get({"task": "task_id"}.get(k, k)) == x for k, x in f.items() if k != "state")]
        out.sort(key=lambda s: s["created_at"], reverse=True)
        return [s for s in out if not after or s["created_at"] < after][:limit]

    def session(self, sid):
        return next((dict(s, state="live") for s in self.s if s["id"] == sid), None)

    def task(self, task_id):
        return [dict(s, state="live") for s in self.s if s["task_id"] == task_id]

    def evidence(self, f, v, limit, after):
        out = [r for r in reversed(self.e) if self._sees(r, v) and all(r.get(k) == x for k, x in f.items())]
        return [r for r in out if after is None or r["seq"] < after][:limit]

    def evidence_one(self, seq, v):
        return next((r for r in self.e if r["seq"] == seq and self._sees(r, v)), None)

    def by_pr(self, tools, repo, pr, v):
        return [r for r in self.e if r["tool"] in tools and (r.get("effect") or {}).get("repo") == repo]

    def counts(self, f, v, by):
        out: dict = {}
        for r in self.e:
            if self._sees(r, v) and all(r.get(k) == x for k, x in f.items() if k not in ("since", "until")):
                out[r.get(by)] = out.get(r.get(by), 0) + 1
        return [{"key": k, "count": n} for k, n in sorted(out.items(), key=lambda kv: -kv[1])]

    def chain(self, batch):
        yield list(self.e)

    def human_pending(self, now, since):
        return list(self.h)

    def ping(self):
        pass


@pytest.fixture
def client(access):
    log = AuditLog()
    for i, (who, agent, app) in enumerate([("user:alice", "coding-agent", None), ("user:bob", "orchestrator", None),
                                           ("user:dave", "coding-agent", "flight-api"), ("user:alice", "coding-agent", None)]):
        log.append({"ts": f"2026-10-30T12:0{i}:00Z", "task_id": f"t-{i}", "session": f"s-{i}", "principal": who,
                    "agent": agent, "tool": "repo.pr.open", "decision": "allow", **({"app": app} if app else {}),
                    "effect": {"repo": "jfillman/flight-api", "pr": i}})
    sessions = [{"id": f"s-{i}", "principal": p, "agent": a, "task_id": f"t-{i}", "created_at": f"2026-10-30T12:0{i}:00Z",
                 "expires_at": "2099-01-01T00:00:00+00:00", "closed": False, "used": {"tool_calls": i}}
                for i, (p, a) in enumerate([("user:alice", "coding-agent"), ("user:bob", "orchestrator"), ("user:dave", "coding-agent")])]
    human = [{"id": "h-000000000001", "owner": "user:alice", "group": None}, {"id": "h-000000000002", "owner": "user:x", "group": "group:default/platform"}]
    from clearance.policy import Policy
    app = api.create_api_app(MemStore(sessions, log.records, human), access, gateway_url="http://clearance.example",
                             policy=Policy((HANGAR,)))
    with TestClient(app) as c:
        yield c


def get(c, path, token=None, who=None):
    h = {"Authorization": f"Bearer {token}"} if token else {}
    if who:
        h["X-Hangar-On-Behalf-Of"] = who
    return c.get(path, headers=h)


def test_http_scopes_by_viewer(client):
    assert get(client, "/v1/sessions").status_code == 401
    assert [s["id"] for s in get(client, "/v1/sessions", "alice").json()["items"]] == ["s-0"]
    assert [s["id"] for s in get(client, "/v1/sessions", "tower", "user:bob").json()["items"]] == ["s-1"]
    assert len(get(client, "/v1/sessions", "audit").json()["items"]) == 3
    assert get(client, "/v1/sessions/s-1", "alice").status_code == 404, "another person's session is not found"
    assert [r["seq"] for r in get(client, "/v1/evidence", "alice").json()["items"]] == [3, 0]
    assert [r["seq"] for r in get(client, "/v1/evidence", "carol").json()["items"]] == [2], "app owner sees the record on her app"
    assert get(client, "/v1/evidence?limit=1", "alice").json() == {"items": [get(client, "/v1/evidence/3", "alice").json()], "next": 3}
    assert [r["seq"] for r in get(client, "/v1/evidence?after=3", "alice").json()["items"]] == [0]
    assert get(client, "/v1/evidence/1", "alice").status_code == 404
    assert get(client, "/v1/tasks/t-1", "bob").json()["used"] == {"tool_calls": 1}


def test_http_roles(client):
    assert get(client, "/v1/sessions", "tower").status_code == 403, "a notifier reads the inbox feed only"
    assert len(get(client, "/v1/human/requests", "tower").json()["items"]) == 2
    assert [r["id"] for r in get(client, "/v1/human/requests", "bob").json()["items"]] == ["h-000000000002"]
    assert get(client, "/v1/evidence/verify", "alice").status_code == 403
    assert get(client, "/v1/evidence/verify", "audit").json()["ok"] is True
    assert get(client, "/v1/receipts", "alice").status_code == 400
    assert len(get(client, "/v1/receipts?repo=jfillman/flight-api", "audit").json()["items"]) == 4
    assert get(client, "/v1/contract").json()["gateway"] == "http://clearance.example"
    agents = get(client, "/v1/agents", "alice").json()["items"]
    assert any(a["name"] == "orchestrator" and a["owner"] == "group:default/platform" for a in agents)
    assert all("instructions" not in a for a in agents)
    assert get(client, "/v1/agents/nope", "alice").status_code == 404
    assert get(client, "/healthz").json() == {"ok": True}


# -- Postgres ----------------------------------------------------------------------------------------------------

@pytest.fixture
def pg(defs, gw):
    if not DSN:
        pytest.skip("set CLEARANCE_TEST_DSN to a scratch Postgres")
    psycopg = pytest.importorskip("psycopg")
    from clearance.pg import PgInbox, open_durable
    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute("DROP TABLE IF EXISTS clearance_evidence, clearance_sessions, clearance_artifacts, clearance_human_requests")
    state, audit, store = open_durable(DSN)
    handlers = {k: v for k, v in gw._handlers.items() if k not in ("run.spawn", "run.close", "human.request", "human.request.get")}
    g = Gateway(defs, store, audit, gw.auth, gw.now, packs=(HANGAR,), handlers=handlers, runs=gw.runs, inbox=PgInbox(state))
    yield g
    state.close()


def test_pg_reads_are_scoped_and_read_only(pg, defs):
    a = pg.open_session("alice", "coding-agent").data["session"]
    pg.call("alice", a, "repo.pr.open", {"repo": "jfillman/flight-api", "branch": "agent/t-1", "title": "fix",
                                         "files": [{"path": "charts/values.yaml", "content": "x"}]})
    b = pg.open_session("bob", "orchestrator").data["session"]
    pg.call("bob", b, "human.request", {"question": "ok?"})
    pool = api.Pool(DSN, size=2)
    store = api.PgReadStore(pool, clock=pg.now)
    access = api.Access(Auth(), defs, auditors=("user:auditor",), directory=Groups(), ownership=Owns())
    alice, bob, carol = (access.viewer(t, None) for t in ("alice", "bob", "carol"))
    everyone = access.viewer("audit", None)
    assert [s["id"] for s in store.sessions({}, alice, 50, None)] == [a]
    assert [s["id"] for s in store.sessions({}, bob, 50, None)] == [b]
    assert len(store.sessions({}, everyone, 50, None)) == 2
    assert {s["state"] for s in store.sessions({"state": "live"}, everyone, 50, None)} == {"live"}
    pr = [r for r in store.evidence({"tool": "repo.pr.open"}, alice, 50, None)]
    assert len(pr) == 1 and pr[0]["app"] == "flight-api" and pr[0]["assurance"] == "recorded-not-checkpointed"
    assert [r["tool"] for r in store.evidence({}, carol, 50, None)] == ["repo.pr.open"], "the app owner sees only her app's record"
    assert store.evidence({}, bob, 50, None) and all(r["agent"] == "orchestrator" or r["principal"] == "user:bob" for r in store.evidence({}, bob, 50, None))
    assert store.by_pr(("repo.pr.open",), "jfillman/flight-api", "1", alice)[0]["effect"]["pr"] == 1
    assert store.by_pr(("repo.pr.open",), "jfillman/flight-api", None, bob) == []
    assert api.verify_chain(store.chain(2))["ok"] and api.verify_chain(store.chain(2))["count"] == len(pg.audit.records)
    [h] = store.human_pending(pg.now(), None)
    assert h["owner"] == "user:bob" and store.human_pending(pg.now(), pg.now() + timedelta(days=1)) == []
    assert store.counts({}, everyone, "tool")[0]["count"] >= 1 and sum(i["count"] for i in store.counts({}, everyone, "decision")) == len(pg.audit.records)
    assert store.counts({"decision": "allow"}, carol, "app") == [{"key": "flight-api", "count": 1}]
    with pytest.raises(Exception, match="read-only"):
        pool.run("DELETE FROM clearance_evidence")


def test_counts_are_scoped_and_grouped(client):
    assert get(client, "/v1/evidence/counts?by=agent", "audit").json() == {
        "by": "agent", "items": [{"key": "coding-agent", "count": 3}, {"key": "orchestrator", "count": 1}], "total": 4}
    assert get(client, "/v1/evidence/counts?by=tool", "alice").json()["total"] == 2
    assert get(client, "/v1/evidence/counts?by=nope", "alice").status_code == 400


def test_policy_is_served_as_data(client):
    p = get(client, "/v1/policy", "alice").json()
    ids = [r["id"] for r in p["rules"]]
    assert ids == sorted(ids, key=lambda i: next(r["order"] for r in p["rules"] if r["id"] == i)), "evaluation order"
    assert "R001" in ids and "R021" in ids
    by_id = {r["id"]: r for r in p["rules"]}
    assert by_id["R001"]["pack"] == "core" and by_id["R021"]["pack"] == "hangar" and by_id["R001"]["expression"] == "!ctx.owns_session"
    tools = {t["name"]: t for t in p["tools"]}
    assert tools["repo.pr.open_upper"]["tier"] == "T2" and tools["repo.pr.open_upper"]["propose_only"] is True
    assert tools["glidepath.run.rerun"]["tier"] == "T1" and p["packs"] == ["core", "hangar"]
    assert get(client, "/v1/policy", "tower").status_code == 403, "a notifier reads the inbox feed only"


def test_explain_is_a_dry_run_with_the_path_through_the_rules(client):
    ok = get(client, "/v1/policy/explain?agent=coding-agent&tool=metrics.query&args=" + '{"promql":"up"}', "alice").json()
    assert ok["decision"]["allow"] is True and ok["dry_run"] is True and ok["known_tool"] is True
    assert all(not r["matched"] for r in ok["path"]) and len(ok["path"]) == len(ok["all_rules"])
    deny = get(client, "/v1/policy/explain?agent=orchestrator&tool=repo.pr.open&args=" + '{"repo":"jfillman/x","branch":"b","title":"t","files":[{"path":"a","content":"c"}]}', "alice").json()
    assert deny["decision"]["allow"] is False and deny["decision"]["rule"] == "R006", "orchestrator has no repo.pr.open"
    assert deny["path"][-1]["id"] == "R006" and deny["path"][-1]["matched"] is True
    assert all(not r["matched"] for r in deny["path"][:-1])
    for q, status in (("agent=nope&tool=metrics.query", 404), ("agent=coding-agent", 400),
                      ("agent=coding-agent&tool=metrics.query&args=[1]", 400), ("agent=coding-agent&tool=metrics.query&args=nope", 400)):
        assert get(client, f"/v1/policy/explain?{q}", "alice").status_code == status, q
    # Nothing was recorded or spent.
    assert len(get(client, "/v1/evidence", "audit").json()["items"]) == 4


def test_explain_without_a_policy_is_404(access):
    app = api.create_api_app(MemStore([], [], []), access)
    with TestClient(app) as c:
        assert get(c, "/v1/policy", "alice").status_code == 404
