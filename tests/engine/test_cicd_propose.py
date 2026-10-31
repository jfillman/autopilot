"""glidepath.cicd.validate and glidepath.cicd.propose, the policy side: a read and a PR tool. The PR tool is budgeted
and receipted like repo.pr.open (provenance.PR_TOOLS), its receipt takes the repo and the path from the handler's
result (the files are the tool's own, not an argument), and its arguments are checked before any call. What the
tools do (ask glidepath-api, open the PR) is Autopilot's handler, tested there."""
from clearance import provenance
from clearance.dispatch import _effect
from clearance.packs.hangar import TOOLS
from clearance.packs.hangar.args import glidepath_bad_args
from clearance.tiers import CORE_TOOLS, Tier, registry

SPECS = {t.name: t for t in TOOLS}
CORE = registry(CORE_TOOLS)
PATCH = {"build": {"cache": {"enabled": True}}}


def test_validate_is_a_read_and_propose_is_a_receipted_pr_tool():
    v, p = SPECS["glidepath.cicd.validate"], SPECS["glidepath.cicd.propose"]
    assert v.tier == Tier.T0 and v.required_args == ("app", "patch") and not v.reads_repo and not v.writes_files
    assert p.tier == Tier.T1 and p.target == "lower" and p.reads_repo and not p.writes_files and not p.propose_only
    assert p.required_args == ("app", "repo", "patch", "title")
    assert p.receipt == ("pr", "commit", "branch") and p.github_cost == CORE["repo.pr.open"].github_cost
    assert "glidepath.cicd.propose" in provenance.PR_TOOLS and set(provenance.PR_TOOLS) >= {"repo.pr.open", "repo.pr.open_upper"}


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
    assert glidepath_bad_args(good) == 0
    for bad in ({"patch": "not an object"}, {"patch": [PATCH]}, {"title": ""}, {"title": "x" * 201}, {"title": 3},
                {"body": "x" * 4001}, {"branch": "a..b"}, {"branch": "-x"}, {"baseRef": "refs/heads/../x"}, {"baseRef": ""}):
        assert glidepath_bad_args({**good, **bad}) == 1, bad
    assert glidepath_bad_args({"app": "gate-api", "patch": {}}) == 0, "an empty patch is well-formed; the handler refuses it as a no-op"
