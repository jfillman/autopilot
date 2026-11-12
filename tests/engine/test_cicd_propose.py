"""glidepath.cicd.validate and glidepath.cicd.propose, the policy side: a read and a PR tool. The PR tool is budgeted
and receipted like repo.pr.open (a PR tool in Hangar's policy bundle), its receipt takes the repo and the path from the handler's
result (the files are the tool's own, not an argument), and its arguments are checked before any call. What the
tools do (ask glidepath-api, open the PR) is Autopilot's handler, tested there."""
import json
from pathlib import Path

from coreenv import HANGAR_BUNDLE, bad_arguments
from autopilot.engine.dispatch import _effect
from autopilot.hangar_tools import TOOLS
from autopilot.engine.tiers import CORE_TOOLS, Tier, registry

SPECS = {t.name: t for t in TOOLS}
CORE = registry(CORE_TOOLS)
PATCH = {"build": {"cache": {"enabled": True}}}


def test_validate_is_a_read_and_propose_is_a_receipted_pr_tool():
    v, p = SPECS["glidepath.cicd.validate"], SPECS["glidepath.cicd.propose"]
    assert v.tier == Tier.T0 and v.required_args == ("app", "patch") and not v.reads_repo and not v.writes_files
    assert p.tier == Tier.T1 and p.target == "lower" and p.reads_repo and not p.writes_files and not p.propose_only
    assert p.required_args == ("app", "repo", "patch", "title")
    assert p.receipt == ("pr", "commit", "branch", "merge", "needsPerson") and p.github_cost == CORE["repo.pr.open"].github_cost
    assert json.loads(HANGAR_BUNDLE.read_text())["pr_tools"] == ["glidepath.cicd.propose", "airframe.apply"]  # beside the core's repo.pr.open*


def test_the_receipt_takes_repo_and_path_from_the_result():
    data = {"pr": 7, "url": "https://github.com/jfillman/gate-api/pull/7", "branch": "agent/cicd-t-1", "commit": "a" * 40,
            "repo": "JFillman/gate-api", "paths": ["./cicd.yaml"], "summary": "valid; 1 change(s)", "changes": [{}]}
    assert _effect(SPECS["glidepath.cicd.propose"], {"repo": "jfillman/gate-api"}, data) == {
        "pr": 7, "commit": "a" * 40, "branch": "agent/cicd-t-1", "repo": "jfillman/gate-api", "paths": ["cicd.yaml"]}
    # A result that says nothing about paths gives a receipt without them, as before.
    assert _effect(SPECS["glidepath.cicd.propose"], {"repo": "jfillman/gate-api"}, {"pr": 7, "commit": "c", "branch": "b"}) == {
        "pr": 7, "commit": "c", "branch": "b"}
    # repo.pr.open's receipt is unchanged: the repo and paths come from its arguments.
    assert _effect(CORE["repo.pr.open"], {"repo": "O/R", "files": [{"path": "./a.yaml"}, {"path": "b/c.yaml"}]},
                   {"pr": 1, "commit": "c", "branch": "b", "paths": ["ignored"]}) == {
        "pr": 1, "commit": "c", "branch": "b", "repo": "o/r", "paths": ["a.yaml", "b/c.yaml"]}


def test_cicd_arguments_are_shaped_before_any_call():
    good = {"app": "gate-api", "repo": "jfillman/gate-api", "patch": PATCH, "title": "Turn the cache on",
            "body": "why", "branch": "agent/cicd-t-1", "baseRef": "main"}
    # The checks are the core's (Hangar's bundle, R008): asked of the test core.
    assert not bad_arguments("glidepath.cicd.propose", good)
    for bad in ({"patch": "not an object"}, {"patch": [PATCH]}, {"title": ""}, {"title": "x" * 201}, {"title": 3},
                {"body": "x" * 4001}, {"branch": "a..b"}, {"branch": "-x"}, {"baseRef": "refs/heads/../x"}, {"baseRef": ""}):
        assert bad_arguments("glidepath.cicd.propose", {**good, **bad}), bad
    assert not bad_arguments("glidepath.cicd.validate", {"app": "gate-api", "patch": {}}), \
        "an empty patch is well-formed; the handler refuses it as a no-op"


# --- A-11: Glidepath's `propose` class: a Flight change is marked for a person to merge -------------------------------
from types import SimpleNamespace

import pytest
from autopilot import cicd_propose
from autopilot.backends import FakeGlidepath
from autopilot.engine.dispatch import Denied

FLIGHT = {"deploy": {"environments": [{"name": "dev", "tier": "ground"},
                                      {"name": "prod", "tier": "flight", "cluster": "hangar-prod", "production": True}]}}


class MetaGit:
    """A git backend that takes the trailers (the real adapter's shape): records what propose passes it."""
    takes_meta = True

    def __init__(self):
        self.calls = []

    def open_pr(self, repo, branch, title, files, base, meta=None, body=None):
        self.calls.append({"repo": repo, "branch": branch, "meta": meta, "body": body, "files": files})
        return {"pr": 5, "url": "u", "branch": branch, "commit": "a" * 40}


def propose(patch, git=None):
    git = git or MetaGit()
    session = SimpleNamespace(principal="user:alice", id="s-1", task_id="t-1")
    out = cicd_propose.propose(git, FakeGlidepath(), session, {"agent": "a", "task": "t-1", "session": "s-1"},
                               {"app": "gate-api", "repo": "jfillman/gate-api", "patch": patch, "title": "t"})
    return out, git


def test_a_flight_environment_is_proposed_marked_for_a_person():
    out, git = propose(FLIGHT)
    assert out["merge"] == "person" and out["needsPerson"] == ["cicd.yaml"]
    assert "/deploy/environments/prod" in out["needsPersonFields"]
    (call,) = git.calls
    assert call["meta"]["merge"] == "person", "the trailer Hangar-Merge: person"
    assert "### Needs a person to merge" in call["body"] and "`/deploy/environments/prod`" in call["body"]
    spec = SPECS["glidepath.cicd.propose"]
    assert _effect(spec, {"repo": "jfillman/gate-api"}, out) == {
        "pr": 5, "commit": "a" * 40, "branch": call["branch"], "merge": "person", "needsPerson": ["cicd.yaml"],
        "repo": "jfillman/gate-api", "paths": ["cicd.yaml"]}


def test_an_ordinary_change_is_not_marked_and_the_receipt_does_not_change():
    out, git = propose(PATCH)
    assert "merge" not in out and "needsPerson" not in out
    assert "merge" not in git.calls[0]["meta"] and "Needs a person" not in git.calls[0]["body"]
    spec = SPECS["glidepath.cicd.propose"]
    assert "merge" not in _effect(spec, {"repo": "jfillman/gate-api"}, out)


def test_a_flight_change_with_a_human_field_is_refused_not_marked():
    gp = FakeGlidepath()
    gp.refuse_flight = True
    git = MetaGit()
    session = SimpleNamespace(principal="user:alice", id="s-1", task_id="t-1")
    with pytest.raises(Denied) as e:
        cicd_propose.propose(git, gp, session, {}, {"app": "gate-api", "repo": "jfillman/gate-api", "patch": FLIGHT, "title": "t"})
    assert e.value.rule == "GP-SCOPE-001" and git.calls == []


def test_an_older_glidepath_api_without_needsPerson_marks_nothing():
    class Old(FakeGlidepath):
        def validate_cicd(self, app, body, who):
            v = super().validate_cicd(app, body, who)
            v.pop("needsPerson")
            return v
    git = MetaGit()
    session = SimpleNamespace(principal="user:alice", id="s-1", task_id="t-1")
    out = cicd_propose.propose(git, Old(), session, {}, {"app": "gate-api", "repo": "jfillman/gate-api", "patch": FLIGHT, "title": "t"})
    assert "merge" not in out
