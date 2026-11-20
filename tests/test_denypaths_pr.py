"""I-48: a PR refuses a file the agent definition's denyPaths (with the baseline) protect, in Python as Go's EnsurePR does.

One check (github_pr.check_deny_paths) runs in open_pr and ensure_pr before any request, so airframe.apply, airframe.configure,
repo.pr.open, glidepath.cicd.propose and xr.request are all covered. GitHub is the Hub of test_airframe_apply.py."""
from types import SimpleNamespace

import pytest
from coreenv import core_env  # noqa: F401  (keeps the shared conftest path set up)

from autopilot import airframe_apply
from autopilot.airframe_tools import fake_airframe
from autopilot.backends import FakeGlidepath
from autopilot.backends import Backends
from autopilot.engine.dispatch import Denied
from autopilot.gateway import hangar_handlers
from autopilot.github_pr import GitHubPRs
from test_airframe_apply import APP, META, SPEC, TENANTS, Hub
from test_airframe_configure import DEV, PATH, SLO, DirHub

XR = "tenants/parachute/xr-requests/parachute.yaml"


@pytest.fixture
def hub():
    return Hub()


@pytest.fixture
def git(tmp_path, hub):
    f = tmp_path / "t"
    f.write_text("sa-1\n")
    return GitHubPRs(f, broker_url="http://broker", api="https://api.github.test", opener=hub)


def no_requests(hub):
    assert hub.reqs == [] and hub.prs == {}, "refused before any request, so before any token or write"


def session(agent="a"):
    return SimpleNamespace(agent=agent, principal="user:alice", id="s-1", task_id="t-1", run="r-1")


def test_apply_refuses_a_path_the_definition_denies_with_no_request(git, hub):
    with pytest.raises(Denied) as e:
        airframe_apply.apply(git, FakeGlidepath(), fake_airframe(), session(), META, {"spec": SPEC},
                             deny_paths=("tenants/*/xr-requests/**",))
    assert e.value.rule == "GH-PATH" and repr(XR) in e.value.message
    assert e.value.hint == "An agent does not change protected paths (CI, ownership, release files); ask a person."
    # apply reads the repository first (as Go's does); nothing is written
    assert not [1 for m, u, _ in hub.reqs if m == "POST" and "/repos/" in u] and hub.prs == {}


def test_apply_allows_a_path_that_is_not_denied(git, hub):
    out = airframe_apply.apply(git, FakeGlidepath(), fake_airframe(), session(), META, {"spec": SPEC},
                               deny_paths=("glidepath/envs/**",))
    assert out["state"] == "awaiting-merge" and out["repo"] == TENANTS


def test_repo_pr_open_refuses_a_denied_path_with_no_request(git, hub):
    h = hangar_handlers(Backends(git, None, None, None, None, None, None, None),
                        {"a": SimpleNamespace(deny_paths=("docs/**",))})
    with pytest.raises(Denied) as e:
        h["repo.pr.open"](session(), {"repo": APP, "branch": "agent/x", "title": "t", "files": [{"path": "docs/a.md", "content": "x"}]})
    assert e.value.rule == "GH-PATH" and e.value.message == "'docs/a.md' is a protected path"
    no_requests(hub)
    hub.create_repo(APP)
    out = h["repo.pr.open"](session(), {"repo": APP, "branch": "agent/x", "title": "t", "files": [{"path": "src/a.md", "content": "x"}]})
    assert out["pr"] and out["branch"] == "agent/x", "a path the definition does not deny is opened"


@pytest.mark.parametrize("path", [".github/workflows/x.yml", "docs/Codeowners", ".github", "a/../.tekton/x", "/etc/x", "CODEOWNERS/x",
                                  "deploy/release.yaml"])
def test_the_baseline_list_applies_when_the_definition_is_not_known(git, hub, path):
    for defs in (None, {"other": SimpleNamespace(deny_paths=())}):      # no definitions, or none for this agent
        h = hangar_handlers(Backends(git, None, None, None, None, None, None, None), defs)
        with pytest.raises(Denied) as e:
            h["repo.pr.open_upper"](session(), {"repo": APP, "branch": "agent/x", "title": "t", "files": [{"path": path, "content": "x"}]})
        assert e.value.rule in ("GH-PATH", "GH-FILES")
    no_requests(hub)


def test_a_deny_list_that_omits_the_baseline_still_gets_it(git, hub):
    with pytest.raises(Denied) as e:
        git.open_pr(APP, "agent/x", "t", [{"path": ".github/x.yml", "content": "x"}], deny_paths=("docs/**",))
    assert e.value.rule == "GH-PATH"
    no_requests(hub)


def test_a_denied_directory_is_not_replaced_by_a_file(git, hub):
    with pytest.raises(Denied) as e:
        git.ensure_pr(APP, "agent/x", "t", [{"path": "glidepath", "content": "x"}], deny_paths=("glidepath/envs/**",))
    assert e.value.rule == "GH-PATH"
    no_requests(hub)


def test_configure_refuses_a_denied_path_with_no_write():
    hub = DirHub()
    hub.create_repo(APP)
    hub.repos[APP]["main"][PATH] = DEV
    import tempfile, pathlib
    f = pathlib.Path(tempfile.mkdtemp()) / "t"
    f.write_text("sa-1\n")
    git = GitHubPRs(f, broker_url="http://broker", api="https://api.github.test", opener=hub)
    with pytest.raises(Denied) as e:
        airframe_apply.configure(git, fake_airframe(), session(), META, {"app": "parachute", "env": "dev", "values": {"slos": [SLO]}},
                                 deny_paths=("glidepath/envs/**",))
    assert e.value.rule == "GH-PATH" and repr(PATH) in e.value.message
    assert not [1 for m, u, _ in hub.reqs if m == "POST" and "/repos/" in u] and hub.prs == {}


def test_the_gateway_hands_apply_and_propose_the_agents_deny_paths():
    import inspect
    from autopilot import gateway
    src = inspect.getsource(gateway.hangar_handlers)
    assert src.count("deny_paths=_deny_paths(definitions, s)") >= 5     # open, open_upper, apply, configure, propose


# -- CodeRabbit: `**` crosses a newline, `[` is a literal, odd names are refused outright --------------------------------------------
def test_a_double_star_deny_matches_a_path_with_a_newline():
    from autopilot.engine.scope import path_protected
    assert path_protected("docs/a\nb.md", ["docs/**"])
    assert path_protected("a\nb/release.yaml", ["**/release.yaml"])
    assert not path_protected("docs2/a\nb.md", ["docs/**"]), "`*` still stays inside one segment"


def test_a_bracket_in_a_deny_glob_is_a_literal_in_the_ancestor_test_too():
    from autopilot.engine.scope import path_protected
    deny = ["deploy/[prod]/values.yaml"]
    assert path_protected("deploy/[prod]/values.yaml", deny)
    assert path_protected("deploy/[prod]", deny), "a file written there replaces the directory"
    assert path_protected("deploy", deny)
    assert not path_protected("deploy/p", deny)


@pytest.mark.parametrize("path,rule", [("docs/a\nb.md", "GH-FILES"), ("docs/a\x07b.md", "GH-FILES"), ("docs/a\tb.md", "GH-FILES"),
                                       ("docs/a:b", "GH-PATH"), ("docs/a ", "GH-PATH"), ("docs/a.", "GH-PATH"), ("docs/A~1/x", "GH-PATH")])
def test_control_characters_and_odd_names_are_refused_outright_as_go_does(git, hub, path, rule):
    with pytest.raises(Denied) as e:
        git.open_pr(APP, "agent/x", "t", [{"path": path, "content": "x"}], deny_paths=("docs/**",))
    assert e.value.rule == rule
    if rule == "GH-PATH":
        assert e.value.message == f"{path!r} has a name a file system reads as another name"
    no_requests(hub)
