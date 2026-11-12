"""agent_xrs: Agent XRs in, Clearance definitions out. One fetch per pin ever, rejections with reasons, a transient
failure that drops nothing, a seed from the store, and the read API's refresher. No network."""
import copy

import pytest
from coreenv import chain_head, core_env
import yaml
from autopilot.engine import definitions as defn
from autopilot.engine.dispatch import Denied, Principal
from autopilot.engine.tiers import Tier

from autopilot import serve
from autopilot.agent_xrs import AGENTS_PATH, AgentXRs, DefinitionsRefresher
from autopilot.airframe_api import AirframeError
from autopilot.backends import Backends, FakeRuns
from autopilot.github_pr import GitHubError

EPH = {"AUTOPILOT_EPHEMERAL": "1", "AIRFRAME_ENABLED": "0", "ARGO_ENABLED": "0", **core_env()}
COMMIT = "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b"
OWNER = "group:default/team-gate"


class Auth:
    def authenticate(self, token):
        return Principal(token, "workload", Tier.T1)


def definition(name="gate-triage", owner=OWNER, **over):
    d = yaml.safe_load((serve.REPO_AGENTS / "triage-agent.yaml").read_text())
    d["name"], d["owner"] = name, owner
    d["identity"] = {"type": "workload", "serviceAccount": f"autopilot-runs/{name}"}
    d.update(over)
    return d


def xr(name="gate-triage", app="gate-api", owner=OWNER, repo="jfillman/gate-api", path=None, commit=COMMIT, ns=None):
    return {"apiVersion": "catalog.hangar.io/v1alpha1", "kind": "Agent",
            "metadata": {"name": name, "namespace": ns or f"app-{app}-cicd"},
            "spec": {"app": app, "owner": owner, "source": {"repo": repo, "path": path or f"agents/{name}.yaml", "commit": commit}}}


class Reader:
    def __init__(self, items=(), fail=None):
        self.items, self.fail, self.calls = list(items), fail, 0

    def list(self, path, label_selector=None):
        assert path == AGENTS_PATH and label_selector is None
        self.calls += 1
        if self.fail:
            raise self.fail
        return copy.deepcopy(self.items)


class GitHub:
    """read_file by (repo, path, ref): a document, a GitHubPRs-shaped answer, or an exception to raise."""
    def __init__(self, files=None):
        self.files, self.calls = dict(files or {}), []

    def read_file(self, repo, path, ref="main"):
        self.calls.append((repo, path, ref))
        v = self.files.get((repo, path, ref))
        if isinstance(v, Exception):
            raise v
        if v is None:
            return {"path": path, "ref": ref, "found": False}
        if isinstance(v, dict) and "type" in v:
            return v
        return {"path": path, "ref": ref, "type": "file", "size": 10, "content": yaml.safe_dump(v)}


@pytest.fixture
def gw(clock):
    g, _ = serve.build_gateway(serve.config_from_env(EPH), backends=Backends(None, None, FakeRuns(), None, None),
                               auth=Auth(), clock=clock)
    # The test core is shared: empty its published set (another test's pins), then read only this test's records.
    assert g.core.publish_definitions([], [])
    g.core.start = chain_head()
    return g


def def_records(gw):
    """The definition.* records the core wrote for this gateway's publishes (it is their one writer)."""
    return [r for r in gw.core.records if r["tool"].startswith("definition.") and not r["principal"].startswith("provider:")]


def loader(gw, items, files, clock):
    return AgentXRs(gw, Reader(items), GitHub(files), clock=clock)


def test_a_pin_loads_once_and_is_an_agent(gw, clock):
    ld = loader(gw, [xr()], {("jfillman/gate-api", "agents/gate-triage.yaml", COMMIT): definition()}, clock)
    ch = ld.poll()
    assert ch.added == ["gate-triage"] and "gate-triage" in gw.defs and gw.defs.source("gate-triage").commit == COMMIT
    assert gw.defs.source("gate-triage").loaded_at == "2026-09-26T12:00:00Z"
    [rec] = def_records(gw)
    assert (rec["tool"], rec["agent"], rec["principal"], rec["decision"]) == (
        "definition.load", "gate-triage", "pin:app-gate-api-cicd/gate-triage", "allow")
    assert rec["result"] == f"loaded jfillman/gate-api@{COMMIT[:7]} agents/gate-triage.yaml" and rec["caller"]
    assert not gw.core.held(), "the gateway holds no definition.* record: the core writes them"
    # the same pin next poll: no GitHub call, no change, no publish, no evidence
    clock.advance(minutes=1)
    assert not ld.poll() and ld.github.calls == [("jfillman/gate-api", "agents/gate-triage.yaml", COMMIT)]
    assert len(def_records(gw)) == 1 and ld.fetches == 1
    # a run can open a session as it
    assert gw.open_session("autopilot-runs/gate-triage", "gate-triage").ok


def test_a_repin_fetches_the_new_commit_and_records_it(gw, clock):
    new = "b" * 40
    files = {("jfillman/gate-api", "agents/gate-triage.yaml", COMMIT): definition(),
             ("jfillman/gate-api", "agents/gate-triage.yaml", new): definition(profile={**definition()["profile"], "tools": ["glidepath.runs.list"]})}
    ld = AgentXRs(gw, Reader([xr()]), GitHub(files), clock=clock)
    ld.poll()
    ld.reader.items = [xr(commit=new)]
    ch = ld.poll()
    assert ch.changed == ["gate-triage"] and gw.defs["gate-triage"].limits.tools == frozenset({"glidepath.runs.list"})
    assert def_records(gw)[-1]["result"].startswith("re-pinned jfillman/gate-api@bbbbbbb")
    ld.reader.items = []
    assert ld.poll().removed == ["gate-triage"] and "gate-triage" not in gw.defs
    assert def_records(gw)[-1]["tool"] == "definition.unload"


@pytest.mark.parametrize("answer, reason", [
    (None, "no file agents/gate-triage.yaml in jfillman/gate-api at 1a2b3c4"),
    ({"path": "agents/gate-triage.yaml", "ref": COMMIT, "type": "dir", "entries": []}, "is a dir, not a file"),
    ({"path": "agents/gate-triage.yaml", "ref": COMMIT, "type": "file", "size": 5, "content": None, "note": "binary"}, "is binary"),
    ({"path": "agents/gate-triage.yaml", "ref": COMMIT, "type": "file", "size": 5, "content": "a: [b"}, "not valid YAML"),
    ({"path": "agents/gate-triage.yaml", "ref": COMMIT, "type": "file", "size": 5, "content": "- just\n- a list\n"}, "not a YAML mapping"),
    (Denied("GH-BROKER", "jfillman/other is not an onboarded app's own repository"), "GH-BROKER: jfillman/other is not"),
    (definition(owner="group:default/x"), "they must agree"),
    (definition(name="someone-else"), "must pin a definition named 'gate-triage'"),
])
def test_a_pin_that_cannot_load_is_rejected_with_the_reason(gw, clock, answer, reason):
    ld = loader(gw, [xr()], {("jfillman/gate-api", "agents/gate-triage.yaml", COMMIT): answer}, clock)
    ch = ld.poll()
    assert "gate-triage" not in gw.defs and len(ch.rejected) == 1 and reason in ch.rejected[0].reason
    [rec] = def_records(gw)
    assert (rec["tool"], rec["decision"], rec["rule"], rec["agent"]) == ("definition.reject", "deny", "DF-PIN", "gate-triage")
    assert rec["principal"] == "pin:app-gate-api-cicd/gate-triage" and reason in rec["result"]
    # final for this pin: no second fetch, no second record
    assert not ld.poll() and len(ld.github.calls) == 1 and len(def_records(gw)) == 1


def test_the_set_is_published_on_its_own_client_with_a_longer_deadline(gw):
    from autopilot.engine.core import DEFINITIONS_TIMEOUT
    assert gw.core.definitions_client is not gw.core.client
    assert gw.core.definitions_client.timeout == DEFINITIONS_TIMEOUT == 5.0 and gw.core.client.timeout < DEFINITIONS_TIMEOUT


def test_a_set_the_core_did_not_take_is_published_again_whole_on_the_next_poll(gw, clock):
    from clearance_client import CoreClient
    ld = loader(gw, [xr()], {("jfillman/gate-api", "agents/gate-triage.yaml", COMMIT): definition()}, clock)
    good = gw.core.definitions_client
    gw.core.definitions_client = CoreClient("http://127.0.0.1:9", token="x", provider="autopilot-gateway", timeout=0.05)
    assert ld.poll().added == ["gate-triage"] and "gate-triage" in gw.defs, "the gateway enforces its set regardless"
    assert def_records(gw) == [] and gw.core.outbox.all() == [], "nothing recorded, nothing queued"
    gw.core.definitions_client = good
    assert not ld.poll(), "no change this poll..."
    assert [r["tool"] for r in def_records(gw)] == ["definition.load"], "...but the whole set is published again"
    assert not ld.poll() and len(def_records(gw)) == 1, "and once the core has it, not again"


def test_the_first_poll_publishes_even_a_set_that_did_not_change(gw, clock):
    """The core keeps the set across a gateway restart: a pin removed while the gateway was down is unloaded on its
    first poll, though the new process's set (empty to empty) did not change."""
    loader(gw, [xr()], {("jfillman/gate-api", "agents/gate-triage.yaml", COMMIT): definition()}, clock).poll()
    assert [r["tool"] for r in def_records(gw)] == ["definition.load"]
    g2, _ = serve.build_gateway(serve.config_from_env(EPH), backends=Backends(None, None, FakeRuns(), None, None),
                                auth=Auth(), clock=clock)
    g2.core.start = chain_head()
    assert not loader(g2, [], {}, clock).poll()
    [rec] = def_records(g2)
    assert (rec["tool"], rec["agent"]) == ("definition.unload", "gate-triage")


def test_a_pin_cannot_take_a_bundled_name(gw, clock):
    ld = loader(gw, [xr(name="triage-agent")], {("jfillman/gate-api", "agents/triage-agent.yaml", COMMIT): definition("triage-agent")}, clock)
    ch = ld.poll()
    assert [r.name for r in ch.rejected] == ["triage-agent"] and "bundled platform definition" in ch.rejected[0].reason
    assert gw.defs.source("triage-agent").kind == "bundled" and gw.defs["triage-agent"].owner != OWNER


def test_a_transient_failure_skips_the_pin_and_keeps_what_was_loaded(gw, clock):
    files = {("jfillman/gate-api", "agents/gate-triage.yaml", COMMIT): definition(),
             ("jfillman/gate-api", "agents/gate-two.yaml", COMMIT): GitHubError("GitHub unreachable: URLError")}
    ld = AgentXRs(gw, Reader([xr(), xr(name="gate-two")]), GitHub(files), clock=clock)
    ch = ld.poll()
    assert ch.added == ["gate-triage"] and not ch.rejected and "gate-two" not in gw.defs
    # next poll, gate-triage's file is unreachable too (nothing cached would be refetched, so it stays), gate-two still is
    assert not ld.poll() and "gate-triage" in gw.defs
    # the file turns up: loaded, and the earlier skip left no trace
    ld.github.files[("jfillman/gate-api", "agents/gate-two.yaml", COMMIT)] = definition("gate-two")
    assert ld.poll().added == ["gate-two"]
    assert [(r["tool"], r["agent"]) for r in def_records(gw)] == [("definition.load", "gate-triage"),
                                                                  ("definition.load", "gate-two")]


def test_everything_unreachable_keeps_the_current_set(gw, clock):
    ld = loader(gw, [xr()], {("jfillman/gate-api", "agents/gate-triage.yaml", COMMIT): definition()}, clock)
    ld.poll()
    ld.github.files[("jfillman/gate-api", "agents/gate-two.yaml", COMMIT)] = GitHubError("down")
    ld.reader.items = [xr(name="gate-two")]          # the loaded pin is gone AND the only pin is unreachable
    assert not ld.poll() and "gate-triage" in gw.defs    # not applied: an outage never empties the set
    ld.reader.fail = AirframeError("API server unreachable")
    assert not ld.poll() and "gate-triage" in gw.defs


def test_a_malformed_xr_is_rejected_not_fetched(gw, clock):
    bad = xr(); del bad["spec"]["source"]["commit"]
    ld = loader(gw, [bad], {}, clock)
    ch = ld.poll()
    assert "no complete spec.source" in ch.rejected[0].reason and ld.github.calls == []


def test_the_seed_warms_the_cache_from_the_store(gw, clock):
    first = loader(gw, [xr()], {("jfillman/gate-api", "agents/gate-triage.yaml", COMMIT): definition()}, clock)
    first.poll()
    rows = gw.definitions_store.all()
    # a new process: the store has the rows, GitHub is down
    g2, _ = serve.build_gateway(serve.config_from_env(EPH), backends=Backends(None, None, FakeRuns(), None, None),
                                auth=Auth(), clock=clock)
    g2.definitions_store.replace(rows)
    second = AgentXRs(g2, Reader([xr()]), GitHub({("jfillman/gate-api", "agents/gate-triage.yaml", COMMIT): GitHubError("down")}), clock=clock)
    assert second.seed() == 1
    ch = second.poll()
    assert ch.added == ["gate-triage"] and second.github.calls == [] and g2.defs.source("gate-triage").loaded_at == "2026-09-26T12:00:00Z"


def test_the_refresher_rebuilds_the_read_apis_set_from_rows(gw, clock):
    ld = loader(gw, [xr(), xr(name="gate-two")], {("jfillman/gate-api", "agents/gate-triage.yaml", COMMIT): definition(),
                                                    ("jfillman/gate-api", "agents/gate-two.yaml", COMMIT): None}, clock)
    ld.poll()

    class Store:
        def definitions(self):
            return gw.definitions_store.all()

    reader_defs = defn.Definitions(dict(gw.defs.bundled))
    ch = DefinitionsRefresher(reader_defs, Store()).refresh()
    assert ch.added == ["gate-triage"] and [r.name for r in ch.rejected] == ["gate-two"]
    assert reader_defs["gate-triage"].owner == OWNER and reader_defs.source("gate-triage").pin == f"jfillman/gate-api@{COMMIT[:7]} agents/gate-triage.yaml"


def test_config_and_describe():
    c = serve.config_from_env(EPH)
    assert not c.agent_xrs_enabled and c.agent_xrs_interval == 60
    c = serve.config_from_env({**EPH, "GITHUB_PR_ENABLED": "1", "AGENT_XRS_INTERVAL": "15"})
    assert c.agent_xrs_enabled and c.agent_xrs_interval == 15
    c = serve.config_from_env({**EPH, "GITHUB_PR_ENABLED": "1", "AGENT_XRS_ENABLED": "0"})
    assert not c.agent_xrs_enabled
    with pytest.raises(serve.ConfigError):
        serve.config_from_env({**EPH, "AGENT_XRS_ENABLED": "1"})
    with pytest.raises(serve.ConfigError):
        serve.config_from_env({**EPH, "GITHUB_PR_ENABLED": "1", "AGENT_XRS_INTERVAL": "1"})


# -- the gateway's bundled agents reach the core ------------------------------------------------------------------------

@pytest.fixture
def bare_core(tmp_path):
    """A core configured as Hangar's is: no CLEARANCE_AGENTS_DIR (an empty directory), so the only definitions it can
    know are the ones the gateway publishes. The shared test core mounts the image's agents/, which hid the defect."""
    import os
    from pathlib import Path

    from coreenv import CoreProcess
    (tmp_path / "agents").mkdir()
    c = CoreProcess(Path(os.environ["AUTOPILOT_TEST_CORE_BIN"]), env={"CLEARANCE_AGENTS_DIR": str(tmp_path / "agents")}).start()
    yield c
    c.stop()


def test_the_bundled_agents_are_published_to_the_core_once(bare_core, clock):
    """Seen live 2026-11-12: parachute's PR failed Clearance / agent-scope (AS004) because the core had never been told
    about the image's agents. The set rides the same publish as the pins; the core records each once."""
    from coreenv import tap_core
    g, _ = serve.build_gateway(serve.config_from_env(EPH), backends=Backends(None, None, FakeRuns(), None, None),
                               auth=Auth(), clock=clock, core=tap_core(url=bare_core.url))
    assert g.publish_definitions()
    names = sorted(g.defs.bundled)
    assert "parachute" in names
    loads = [r for r in bare_core.records() if r["tool"].startswith("definition.")]
    assert sorted(r["agent"] for r in loads) == names and {r["tool"] for r in loads} == {"definition.load"}
    assert {r["principal"] for r in loads} == {"provider:autopilot-gateway"}
    assert all(r["target"].startswith("provider:autopilot-gateway@") and r["result"].startswith("loaded provider:") for r in loads)
    n = len(bare_core.records())
    g._definitions_unpublished = True
    assert g.publish_definitions() and len(bare_core.records()) == n, "the same set again records nothing"
    # A poll with a pin keeps the bundled set: the pins are published with it, nothing bundled is unloaded.
    ld = loader(g, [xr()], {("jfillman/gate-api", "agents/gate-triage.yaml", COMMIT): definition()}, clock)
    assert ld.poll().added == ["gate-triage"]
    tools = [(r["tool"], r["agent"]) for r in bare_core.records()[n:] if r["tool"].startswith("definition.")]
    assert tools == [("definition.load", "gate-triage")]


def test_a_core_that_mounts_the_same_agents_has_nothing_to_load_and_nothing_to_reject(gw):
    """The shared test core mounts the image's agents/ (CLEARANCE_AGENTS_DIR): the same documents are ignored."""
    gw.core.start = chain_head()
    gw._definitions_unpublished = True
    assert gw.publish_definitions()
    assert [r for r in gw.core.records if r["tool"].startswith("definition.")] == []


class RecordingClient:
    def __init__(self, fail_first_with=None):
        self.calls, self.fail = [], fail_first_with

    def publish_definitions(self, pins, rejected=None, *, provider=None, bundled=None):
        self.calls.append({"pins": pins, "rejected": rejected, "bundled": bundled})
        if bundled is not None and self.fail is not None:
            raise self.fail
        from clearance_client import DefinitionsResult
        return DefinitionsResult(added=[], changed=[], removed=[], rejected=[], cleared=[], cluster=0)


def test_publish_sends_the_bundled_set_with_the_build_version():
    from autopilot.engine.core import Core
    c = RecordingClient()
    core = Core(client=None, definitions_client=c, bundled_version="gateway-v9.9.9")
    assert core.publish_definitions([], [], [("a", {"name": "a"})])
    assert c.calls[0]["bundled"] == [{"name": "a", "version": "gateway-v9.9.9", "doc": {"name": "a"}}]
    assert core.publish_definitions([], [], None) and c.calls[1]["bundled"] is None


def test_an_older_core_that_refuses_bundled_is_published_without_it_and_not_asked_again():
    from autopilot.engine.core import Core
    from clearance_client import CoreRequestError
    c = RecordingClient(CoreRequestError("publishDefinitions", 400, "json: unknown field \"bundled\""))
    core = Core(client=None, definitions_client=c)
    assert core.publish_definitions([], [], [("a", {"name": "a"})])
    assert [x["bundled"] is not None for x in c.calls] == [True, False]
    assert core.publish_definitions([], [], [("a", {"name": "a"})])
    assert [x["bundled"] is not None for x in c.calls] == [True, False, False]


def test_an_unknown_field_other_than_bundled_is_not_taken_for_an_older_core():
    from autopilot.engine.core import Core
    from clearance_client import CoreRequestError
    c = RecordingClient(CoreRequestError("publishDefinitions", 400, "json: unknown field \"rejected\""))
    core = Core(client=None, definitions_client=c)
    assert core.publish_definitions([], [], [("a", {"name": "a"})]) is False
    assert len(c.calls) == 1 and core._core_takes_bundled


def test_another_refusal_of_the_bundled_set_is_not_swallowed():
    from autopilot.engine.core import Core
    from clearance_client import CoreRequestError
    c = RecordingClient(CoreRequestError("publishDefinitions", 403, "provider may not publish definitions"))
    core = Core(client=None, definitions_client=c)
    assert core.publish_definitions([], [], [("a", {"name": "a"})]) is False
    assert len(c.calls) == 1


def test_the_bundled_set_is_the_images_documents():
    from autopilot.engine import profile
    defs = defn.Definitions(profile.load_dir(serve.REPO_AGENTS))
    got = dict(defs.bundled_set())
    assert set(got) == set(defs.bundled) and got["parachute"]["name"] == "parachute"
    assert all(profile.parse(d).name == n for n, d in got.items())


def test_a_gateway_without_the_loader_publishes_its_bundled_agents(gw):
    from autopilot.agent_xrs import DefinitionsPublisher
    gw._definitions_unpublished = True
    p = DefinitionsPublisher(gw, interval=0.05).start()
    p._thread.join(timeout=10)
    assert not p._thread.is_alive() and gw._definitions_unpublished is False
