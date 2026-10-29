"""github_pr: repo.pr.open as the agents' App. A broker token per PR (never kept), the Git data API only, nothing
pushed to an existing branch, and the PR carries the agent, task, session and run. No network: a fake opener."""
import json

import pytest

from autopilot.github_pr import GitHubError, GitHubPRs, app_of
from clearance.dispatch import Denied

FILES = [{"path": "src/app.py", "content": "print('fixed')\n"}]
META = {"agent": "triage-agent", "task": "t-abc123", "session": "s-1", "run": "r-1"}


class GH:
    def __init__(self, over=None):
        self.reqs, self.over = [], over or {}

    def __call__(self, req, timeout):
        body = json.loads(req.data) if req.data else None
        self.reqs.append((req.get_method(), req.full_url, req.get_header("Authorization"), body))
        url = req.full_url
        key = (req.get_method(), url.split("://", 1)[1].split("/", 1)[1])
        if key in self.over:
            return self.over[key]
        if url.endswith("/agent-installation-token"):
            return 200, json.dumps({"token": "ghs_x", "expiresAt": "z"}).encode()
        if url.endswith("/git/ref/heads/main"):
            return 200, b'{"object": {"sha": "base1"}}'
        if url.endswith("/git/commits/base1"):
            return 200, b'{"tree": {"sha": "tree0"}}'
        if url.endswith("/git/trees"):
            return 201, b'{"sha": "tree1"}'
        if url.endswith("/git/commits"):
            return 201, b'{"sha": "c1"}'
        if url.endswith("/git/refs"):
            return 201, b'{}'
        if url.endswith("/pulls"):
            return 201, b'{"number": 7, "html_url": "https://github.com/jfillman/gate-api/pull/7"}'
        raise AssertionError(f"unexpected {key}")


@pytest.fixture
def token(tmp_path):
    f = tmp_path / "t"
    f.write_text("sa-1\n")
    return f


def prs(token, gh):
    return GitHubPRs(token, broker_url="http://broker", api="https://api.github.test", opener=gh)


def test_opens_a_pr_with_a_per_call_token_and_the_metadata(token):
    gh = GH()
    out = prs(token, gh).open_pr("jfillman/gate-api", "agent/t-abc123", "Fix the probe", FILES, meta=META)
    assert out == {"pr": 7, "url": "https://github.com/jfillman/gate-api/pull/7", "branch": "agent/t-abc123", "commit": "c1"}
    broker = gh.reqs[0]
    assert broker[:3] == ("POST", "http://broker/agent-installation-token", "Bearer sa-1")
    assert broker[3] == {"owner": "jfillman", "repo": "gate-api", "app": "gate-api"}
    gh_calls = gh.reqs[1:]
    assert all(r[2] == "Bearer ghs_x" and r[1].startswith("https://api.github.test/repos/jfillman/gate-api/") for r in gh_calls)
    assert [r[0] for r in gh_calls] == ["GET", "GET", "POST", "POST", "POST", "POST"]
    tree = gh_calls[2][3]
    assert tree == {"base_tree": "tree0", "tree": [{"path": "src/app.py", "mode": "100644", "type": "blob",
                                                    "content": "print('fixed')\n"}]}
    assert "Hangar-Task: t-abc123" in gh_calls[3][3]["message"] and gh_calls[3][3]["parents"] == ["base1"]
    assert gh_calls[4][3] == {"ref": "refs/heads/agent/t-abc123", "sha": "c1"}
    assert "`t-abc123`" in gh_calls[5][3]["body"] and gh_calls[5][3]["base"] == "main"


def test_token_is_reread_each_pr(token):
    gh = GH()
    p = prs(token, gh)
    p.open_pr("jfillman/gate-api", "agent/a", "t", FILES)
    token.write_text("sa-2")
    p.open_pr("jfillman/gate-api", "agent/b", "t", FILES)
    assert [r[2] for r in gh.reqs if r[1].endswith("agent-installation-token")] == ["Bearer sa-1", "Bearer sa-2"]


def test_an_existing_branch_is_refused_not_forced(token):
    gh = GH({("POST", "repos/jfillman/gate-api/git/refs"): (422, b'{"message": "Reference already exists"}')})
    with pytest.raises(Denied) as e:
        prs(token, gh).open_pr("jfillman/gate-api", "agent/x", "t", FILES)
    assert e.value.rule == "GH-BRANCH-EXISTS"
    assert not any(r[1].endswith("/pulls") for r in gh.reqs) and not any(r[0] == "PATCH" for r in gh.reqs)


def test_the_brokers_refusal_is_an_answer(token):
    gh = GH({("POST", "agent-installation-token"): (403, b'{"error": "no Repository CR"}')})
    with pytest.raises(Denied) as e:
        prs(token, gh).open_pr("jfillman/gitops-hangar-dev", "agent/x", "t", FILES)
    assert e.value.rule == "GH-REPO-NOT-APP" and len(gh.reqs) == 1


@pytest.mark.parametrize("repo, branch, files", [
    ("gate-api", "agent/x", FILES), ("jfillman/../x", "agent/x", FILES), ("jfillman/gate-api", "../main", FILES),
    ("jfillman/gate-api", "agent/x.lock", FILES), ("jfillman/gate-api", "agent/x", []),
    ("jfillman/gate-api", "agent/x", [{"path": "a"}]), ("jfillman/gate-api", "agent/x", [{"path": "a", "content": "x" * (2 << 20)}]),
])
def test_bad_requests_are_refused_before_any_token(token, repo, branch, files):
    gh = GH()
    with pytest.raises(Denied):
        prs(token, gh).open_pr(repo, branch, "t", files)
    assert gh.reqs == []


def test_broker_failure_is_hidden(token):
    gh = GH({("POST", "agent-installation-token"): (503, b'{"error": "not mounted"}')})
    with pytest.raises(GitHubError):
        prs(token, gh).open_pr("jfillman/gate-api", "agent/x", "t", FILES)


def test_app_of():
    assert [app_of(r) for r in ("gate-api", "gitops-gate-api", "gitops-infra-broker")] == ["gate-api", "gate-api", "broker"]


def test_read_file_returns_text_and_lists_directories(token):
    import base64
    body = json.dumps({"type": "file", "size": 6, "sha": "f1", "encoding": "base64",
                       "content": base64.b64encode(b"hello\n").decode()}).encode()
    gh = GH({("GET", "repos/jfillman/gate-api/contents/app.js?ref=main"): (200, body),
             ("GET", "repos/jfillman/gate-api/contents/test?ref=main"): (200, b'[{"name": "app.test.js", "type": "file", "size": 9}]'),
             ("GET", "repos/jfillman/gate-api/contents/nope?ref=main"): (404, b"{}")})
    p = prs(token, gh)
    assert p.read_file("jfillman/gate-api", "app.js")["content"] == "hello\n"
    assert p.read_file("jfillman/gate-api", "test")["entries"] == [{"name": "app.test.js", "type": "file", "size": 9}]
    assert p.read_file("jfillman/gate-api", "nope")["found"] is False
    assert all(r[0] in ("GET", "POST") for r in gh.reqs)
    assert [r[0] for r in gh.reqs if "api.github.test" in r[1]] == ["GET", "GET", "GET"]


def test_read_file_refuses_a_bad_ref_before_any_token(token):
    gh = GH()
    with pytest.raises(Denied):
        prs(token, gh).read_file("jfillman/gate-api", "app.js", "../x")
    assert gh.reqs == []
