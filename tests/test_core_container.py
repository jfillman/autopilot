"""The gateway against the released core image (ghcr.io/jfillman/clearance:vX, `clearance core --memory
--insecure-dev-auth`), unmodified: the clock-skew check is on, so these run on the real clock. AUTOPILOT_CORE_CONTAINER_URL
names it (CI's core-container job starts the image); skipped without it. The rest of the suite runs against the
test core built from the same tag (tests/clearance_core), which reads back its chain."""
import os
from datetime import datetime, timezone
from pathlib import Path

import pytest
from clearance_client import CoreClient

from autopilot.engine import control as ctl, inbox as hr, profile
from autopilot.engine.backends import FakeArgo, FakeArtifacts, FakeGit, FakeHuman, FakeRead, FakeRunBackend, core_handlers
from autopilot.engine.core import Core
from autopilot.engine.dispatch import Gateway, Principal
from autopilot.engine.modelproxy import ModelRoute
from autopilot.engine.outbox import MemoryOutbox
from autopilot.engine.session import SessionStore
from autopilot.engine.tiers import Tier
from autopilot.hangar_tools import HANGAR

URL = os.environ.get("AUTOPILOT_CORE_CONTAINER_URL")
pytestmark = pytest.mark.skipif(not URL, reason="AUTOPILOT_CORE_CONTAINER_URL is not set (a core container)")
AGENTS = Path(__file__).resolve().parent / "engine" / "fixtures" / "agents"
PR = {"repo": "jfillman/flight-api", "branch": "agent/t-1", "title": "fix probe",
      "files": [{"path": "charts/values.yaml", "content": "x"}]}


class Auth:
    def authenticate(self, token):
        return {"alice": Principal("user:alice", "user", Tier.T1)}.get(token) or Principal(token, "workload", Tier.T1)


@pytest.fixture
def g():
    git = FakeGit()
    core = Core(CoreClient(URL, token=os.environ.get("AUTOPILOT_CORE_CONTAINER_TOKEN", "autopilot-system/autopilot-gateway"),
                           provider="autopilot-gateway"), MemoryOutbox())
    route = {"claude-sonnet-5": ModelRoute("claude-sonnet-5", "anthropic", "https://api.anthropic.com", "m")}
    gw = Gateway(profile.load_dir(AGENTS), SessionStore(), core, Auth(), lambda: datetime.now(timezone.utc),
                 packs=(HANGAR,), runs=FakeRunBackend(), inbox=hr.MemoryInbox(), holds=ctl.MemoryHolds(),
                 admins=("user:alice",), models=route, model_auth=Auth(),
                 handlers=core_handlers(git=git, argo=FakeArgo(), read=FakeRead(), human=FakeHuman(),
                                        artifacts=FakeArtifacts()))
    gw.git = git
    return gw


def test_a_session_calls_spawns_and_records_through_the_released_core(g):
    r = g.open_session("alice", "orchestrator")
    assert r.ok and r.audit_seq is not None, r.as_dict()
    sid = r.data["session"]
    assert g.call("alice", sid, "catalog.read", {"query": "x"}).ok
    denied = g.call("alice", sid, "k8s.exec", {})
    assert not denied.ok and denied.decision["rule"] == "R002" and g.store.get(sid).tripped
    sid = g.open_session("alice", "coding-agent").data["session"]
    r = g.call("alice", sid, "repo.pr.open", PR)
    assert r.ok and r.audit_seq is not None and g.git.calls
    orch = g.open_session("alice", "orchestrator").data["session"]
    r = g.call("alice", orch, "run.spawn", {"agent": "researcher", "limits": {"tool_calls": 5}})
    assert r.ok and g.store.get(r.data["session"]).parent_id == orch, r.as_dict()
    assert g.model_authorize("alice", orch, "claude-sonnet-5", 100).allow
    out = g.freeze_session("alice", orch, "enough")
    assert out["audit_seq"] is not None, "session.freeze is a record kind the released core takes"
    assert g.core.outbox.all() == [], "nothing waited: every decision, outcome and record landed"
