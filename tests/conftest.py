from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

from clearance import profile
from clearance.audit import AuditLog
from clearance.backends import fake_backends
from clearance.dispatch import Gateway, Principal
from clearance.session import SessionStore
from clearance.tiers import Tier

ROOT = Path(__file__).resolve().parents[1]
T0 = datetime(2026, 9, 26, 12, 0, 0, tzinfo=timezone.utc)


class Clock:
    def __init__(self):
        self.t = T0

    def __call__(self):
        return self.t

    def advance(self, **kw):
        self.t += timedelta(**kw)


class StaticAuth:
    def __init__(self, table):
        self.table = table

    def authenticate(self, token):
        return self.table[token]


@pytest.fixture
def clock():
    return Clock()


@pytest.fixture
def defs():
    return profile.load_dir(ROOT / "agents")


@pytest.fixture
def backends():
    return fake_backends()


@pytest.fixture
def gw(defs, backends, clock):
    counter = iter(range(1, 1000))
    store = SessionStore(id_gen=lambda: f"s-{next(counter):06x}")
    auth = StaticAuth({
        "alice": Principal("user:alice", "user", Tier.T1),
        "bob": Principal("user:bob", "user", Tier.T1),
        "readonly": Principal("user:ro", "user", Tier.T0),
        "holmes": Principal("holmesgpt/holmes", "workload", Tier.T1),
    })
    return Gateway(defs, store, AuditLog(), backends, auth, clock)
