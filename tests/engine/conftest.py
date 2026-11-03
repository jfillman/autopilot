"""Fixtures for Clearance's own tests: the core gateway with the Hangar pack's policy, core handlers over
in-memory fakes, and a run backend that starts nothing. Agent definitions are tests/fixtures/agents/."""
from datetime import datetime, timedelta, timezone
from pathlib import Path
from types import SimpleNamespace

import pytest

from clearance import profile
from clearance.audit import AuditLog
from clearance.backends import FakeArgo, FakeArtifacts, FakeGit, FakeHuman, FakeRead, FakeRunBackend, core_handlers
from clearance.dispatch import Gateway, Principal
from clearance.packs.hangar import HANGAR
from clearance.session import SessionStore
from clearance.tiers import Tier

ROOT = Path(__file__).resolve().parents[1]
AGENTS = ROOT / "tests" / "fixtures" / "agents"
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
    return profile.load_dir(AGENTS)


@pytest.fixture
def backends():
    return SimpleNamespace(git=FakeGit(), argo=FakeArgo(), read=FakeRead(), human=FakeHuman(),
                           artifacts=FakeArtifacts(), runs=FakeRunBackend())


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
    handlers = core_handlers(git=backends.git, argo=backends.argo, read=backends.read, human=backends.human,
                             artifacts=backends.artifacts)
    return Gateway(defs, store, AuditLog(), auth, clock, packs=(HANGAR,), handlers=handlers, runs=backends.runs)
