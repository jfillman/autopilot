"""Helpers for the engine tests that read through clearance-api (test_control.py, test_events.py): an authenticator, a
directory, app ownership and an in-memory ReadStore. They are clearance's tests/test_api.py at v0.26.1 without its
tests: clearance-api (api.py) stays Clearance's, and its own tests stay there (R-3a)."""
from autopilot.engine.dispatch import AuthError, Principal
from autopilot.engine.tiers import Tier

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


class MemStore:
    """Just enough of ReadStore for the HTTP layer (the SQL is the pg test's)."""

    def __init__(self, sessions, evidence, human, events=(), holds=None):
        self.s, self.e, self.h, self.ev, self.hd = sessions, evidence, human, list(events), dict(holds or {})

    def holds(self, now):
        return dict(self.hd)

    def definitions(self):
        return []

    def changes(self, f, v, limit, after):
        return []

    def change(self, change_id):
        return None

    def change_counts(self, f, v, by):
        return []

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

    def result(self, sid, now):
        return {"s-0": {"session": "s-0", "status": "done", "final_text": "Python and Go."}}.get(sid)

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


def get(c, path, token=None, who=None):
    h = {"Authorization": f"Bearer {token}"} if token else {}
    if who:
        h["X-Hangar-On-Behalf-Of"] = who
    return c.get(path, headers=h)
