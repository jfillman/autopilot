"""airframe.apply (A-3): the planner's change set opened as receipted PRs by the agents' App, one per call, resumable,
the Flight half opened only as a proposal marked for a person to merge (A-11, proposeFlight) and nothing ever merged by the tool.

GitHub is a stateful stand-in (Hub) behind the real adapter (github_pr.GitHubPRs), so what is asserted is the HTTP the
tool really sends: every request of a whole run is read for a merge, a push to an existing branch or a write outside the
two repos the tool is allowed. The human is the test: Hub.merge and Hub.create_repo are the clicks and the platform's
onboarding. The gateway tests go through the real Clearance test core (policy, budgets, receipts)."""
import base64
import json
import re
import urllib.parse
from pathlib import Path

import pytest
from coreenv import core_env
import yaml

from autopilot import airframe_apply, airframe_plan
from autopilot.engine import results
from autopilot.airframe_tools import fake_airframe
from autopilot.backends import FakeGlidepath
from autopilot.engine.dispatch import Denied, _effect
from autopilot.github_pr import GitHubPRs
from autopilot.engine.tiers import Tier
from autopilot.hangar_tools import TOOLS

ROOT = Path(__file__).resolve().parents[1]
SPEC = yaml.safe_load((ROOT / "examples" / "parachute.appspec.yaml").read_text())
TENANTS = "jfillman/gitops-cluster-dev-tenants"      # the fake fleet's tenants repo (airframe_tools.fake_airframe)
APP = "jfillman/parachute"
GITOPS = "jfillman/gitops-parachute"
META = {"agent": "parachute", "task": "t-1", "session": "s-1", "run": "r-1"}


class Killed(Exception):
    """The core process dying: not a GitHub or network error, so nothing in the tool catches it."""


class Hub:
    """GitHub and the broker, enough for the Git data API calls the adapter makes. Records every request."""

    def __init__(self):
        self.reqs: list[tuple[str, str, dict | None]] = []
        self.repos: dict[str, dict[str, dict[str, str]]] = {TENANTS: {"main": {}}}     # repo -> branch -> files
        self.onboarded: set[str] = set()
        self.prs: dict[str, list[dict]] = {}
        self.trees: dict[str, dict[str, str]] = {}
        self.commits: dict[str, dict] = {}
        self.n = 0
        self.kill: tuple[str, str, int] | None = None       # (method, url suffix, nth match) to die on, before it is sent
        self.seen: dict[tuple[str, str], int] = {}

    # -- the people and the platform -----------------------------------------------------------------------------
    def create_repo(self, repo: str) -> None:
        """The platform creates and onboards the repository once the tenants PR has merged."""
        self.repos[repo] = {"main": {}}
        self.onboarded.add(repo)

    def merge(self, repo: str, number: int) -> None:
        """A person's click."""
        pr = next(p for p in self.prs[repo] if p["number"] == number)
        self.repos[repo]["main"].update(self.repos[repo][pr["head"]])
        pr["merged_at"], pr["state"] = "2026-11-09T12:00:00Z", "closed"

    def delete_branch(self, repo: str, branch: str) -> None:
        """A person deletes the branch of a closed PR."""
        del self.repos[repo][branch]
        self.repos[repo].pop(branch + "\0commit", None)

    def close(self, repo: str, number: int) -> None:
        next(p for p in self.prs[repo] if p["number"] == number)["state"] = "closed"

    # -- the wire ------------------------------------------------------------------------------------------------
    def __call__(self, req, timeout):
        method, url = req.get_method(), req.full_url
        body = json.loads(req.data) if req.data else None
        if self.kill:
            m, suffix, nth = self.kill
            if method == m and url.split("?")[0].endswith(suffix):
                self.seen[(m, suffix)] = self.seen.get((m, suffix), 0) + 1
                if self.seen[(m, suffix)] == nth:
                    self.kill = None
                    raise Killed()
        self.reqs.append((method, url, body))
        if url == "http://broker/agent-installation-token":
            repo = f"{body['owner']}/{body['repo']}"
            ok = ("app" not in body) if repo == TENANTS else (repo in self.onboarded and body.get("app") == body["repo"].removeprefix("gitops-"))
            return (200, b'{"token": "ghs_x", "expiresAt": "z"}') if ok else (403, b'{"error": "not allowed"}')
        m = re.match(r"https://api\.github\.test/repos/([^/]+/[^/]+)(/.*)$", url)
        repo, rest = m.group(1), m.group(2)
        if repo not in self.repos:
            return 404, b"{}"
        branches = self.repos[repo]
        if method == "GET" and (g := re.match(r"/git/ref/heads/(.+)$", rest)):
            return (200, json.dumps({"object": {"sha": branches.get(g.group(1) + "\0commit", f"sha-{repo}-{g.group(1)}")}}).encode()) if g.group(1) in branches else (404, b"{}")
        if method == "GET" and (g := re.match(r"/git/commits/(.+)$", rest)):
            return 200, json.dumps({"tree": {"sha": "tree-of-" + g.group(1)}}).encode()
        if method == "POST" and rest == "/git/trees":
            base = next(b for b in branches if "tree-of-sha-" + f"{repo}-{b}" == body["base_tree"])
            self.n += 1
            files = {**branches[base], **{e["path"]: e["content"] for e in body["tree"]}}
            self.trees[f"t{self.n}"] = files
            return 201, json.dumps({"sha": f"t{self.n}"}).encode()
        if method == "POST" and rest == "/git/commits":
            self.n += 1
            sha = f"{self.n:040x}"
            self.commits[sha] = {"files": self.trees[body["tree"]], "message": body["message"]}
            return 201, json.dumps({"sha": sha}).encode()
        if method == "POST" and rest == "/git/refs":
            name = body["ref"].removeprefix("refs/heads/")
            if name in branches:
                return 422, b"{}"
            branches[name] = self.commits[body["sha"]]["files"]
            branches[name + "\0commit"] = body["sha"]
            return 201, b"{}"
        if method == "POST" and rest == "/pulls":
            self.n += 1
            pr = {"number": self.n, "head": body["head"], "state": "open", "merged_at": None, "title": body["title"],
                  "body": body["body"], "base": body["base"]}
            self.prs.setdefault(repo, []).append(pr)
            return 201, json.dumps({"number": pr["number"], "html_url": f"https://github.com/{repo}/pull/{pr['number']}"}).encode()
        if method == "GET" and rest.startswith("/pulls?"):
            head = urllib.parse.unquote(re.search(r"head=([^&]+)", rest).group(1)).split(":", 1)[1]
            out = [{"number": p["number"], "state": p["state"], "merged_at": p["merged_at"],
                    "html_url": f"https://github.com/{repo}/pull/{p['number']}",
                    "head": {"ref": p["head"], "sha": branches.get(p["head"] + "\0commit", "0" * 40)}}
                   for p in self.prs.get(repo, []) if p["head"] == head]
            return 200, json.dumps(out).encode()
        if method == "GET" and (g := re.match(r"/pulls/(\d+)/files", rest)):
            pr = next(p for p in self.prs[repo] if p["number"] == int(g.group(1)))
            head, main = branches[pr["head"]], branches["main"]
            return 200, json.dumps([{"filename": k} for k, v in head.items() if main.get(k) != v]).encode()
        if method == "GET" and (g := re.match(r"/contents/([^?]+)\?ref=(.+)$", rest)):
            ref = urllib.parse.unquote(g.group(2))
            text = branches.get(ref, {}).get(g.group(1)) if isinstance(branches.get(ref), dict) else None
            if text is None:
                return 404, b"{}"
            return 200, json.dumps({"type": "file", "size": len(text), "encoding": "base64", "sha": "x",
                                    "content": base64.b64encode(text.encode()).decode()}).encode()
        raise AssertionError(f"unexpected {method} {url}")

    def pr_files(self, repo: str, number: int) -> dict[str, str]:
        return self.repos[repo][next(p for p in self.prs[repo] if p["number"] == number)["head"]]


class Session:
    principal = "user:alice"
    id = "s-1"


@pytest.fixture
def token(tmp_path):
    f = tmp_path / "t"
    f.write_text("sa-1\n")
    return f


@pytest.fixture
def hub():
    return Hub()


@pytest.fixture
def git(token, hub):
    return GitHubPRs(token, broker_url="http://broker", api="https://api.github.test", opener=hub)


@pytest.fixture
def gp():
    return FakeGlidepath()


def apply(git, gp, spec=SPEC, **kw):
    return airframe_apply.apply(git, gp, fake_airframe(), Session(), META, {"spec": spec, **kw})


def statuses(out):
    return {s["id"]: s["status"] for s in out["steps"]}


def run_to_the_end(git, gp, hub):
    """The whole parachute: apply, merge what it opened, let the platform create the repo, apply again."""
    outs = [apply(git, gp)]
    hub.merge(TENANTS, outs[-1]["pr"])
    assert apply(git, gp)["state"] == "waiting"
    hub.create_repo(APP)
    outs.append(apply(git, gp))
    hub.merge(APP, outs[-1]["pr"])
    outs.append(apply(git, gp))
    return outs


# -- the change set opens as receipted PRs -------------------------------------------------------------------------
def test_the_plan_opens_as_one_pr_per_call_tenants_first_then_the_app_repo(git, gp, hub):
    first = apply(git, gp)
    assert first["state"] == "awaiting-merge" and first["step"] == "tenants" and first["resumed"] is False
    assert first["repo"] == TENANTS and first["branch"] == "agent/airframe-parachute-tenants"
    paths = first["paths"]
    assert "tenants/parachute/xr-requests/parachute.yaml" in paths and len(paths) == 3, "the app XR and staging, prod"
    assert hub.pr_files(TENANTS, first["pr"]).keys() >= set(paths)
    assert statuses(first) == {"tenants": "open", "repos-ready": "platform", "app-repo": "blocked", "cicd-flight": "human",
                               "tekton-resync": "platform", "gitops": "human", "verify": "check"}
    # the app's repo does not exist yet: nothing opens there, and it says why
    hub.merge(TENANTS, first["pr"])
    waiting = apply(git, gp)
    assert waiting["state"] == "waiting" and "does not exist yet" in waiting["reason"] and "pr" not in waiting
    assert statuses(waiting)["tenants"] == "merged" and statuses(waiting)["app-repo"] == "waiting"
    hub.create_repo(APP)
    second = apply(git, gp)
    assert second["state"] == "awaiting-merge" and second["step"] == "app-repo" and second["repo"] == APP
    assert second["paths"] == ["cicd.yaml", "glidepath/envs/dev.yaml", "glidepath/envs/test.yaml"]
    files = hub.pr_files(APP, second["pr"])
    assert yaml.safe_load(files["glidepath/envs/dev.yaml"])["envName"] == "dev"
    # cicd.yaml is glidepath-api's `resulting` under the agent rules, as for glidepath.cicd.propose
    assert gp.calls[-1][0] == "glidepath.validate_cicd" and gp.calls[-1][1][1]["agent"] is True
    assert "dev" in files["cicd.yaml"]
    hub.merge(APP, second["pr"])
    done = apply(git, gp)
    assert done["state"] == "agent-steps-merged" and "pr" not in done
    assert [h["id"] for h in done["humanSteps"]] == ["cicd-flight", "gitops"]


def test_each_pr_names_who_opened_it_and_why_a_person_must_merge(git, gp, hub):
    out = apply(git, gp)
    pr = next(p for p in hub.prs[TENANTS] if p["number"] == out["pr"])
    assert pr["base"] == "main" and "airframe.apply" in pr["body"] and "A person merges this" in pr["body"]
    assert all(f"- {k}: `{v}`" in pr["body"] for k, v in META.items())
    commit = hub.commits[out["commit"]]["message"]
    assert all(f"Hangar-{k.title()}: {v}" in commit for k, v in META.items()), "the trailers Clearance's provenance reads"


def test_the_receipt_is_the_one_a_repo_pr_open_has(git, gp, hub):
    spec = next(t for t in TOOLS if t.name == "airframe.apply")
    out = apply(git, gp)
    assert _effect(spec, {"spec": SPEC}, out) == {"pr": out["pr"], "commit": out["commit"], "branch": out["branch"],
                                                  "repo": TENANTS, "paths": sorted(out["paths"])}
    # a call that opened nothing records no repo or paths
    hub.merge(TENANTS, out["pr"])
    assert _effect(spec, {"spec": SPEC}, apply(git, gp)) == {}


def test_the_tenants_repo_is_asked_for_with_no_app_and_an_app_repo_as_its_own(git, gp, hub):
    out = apply(git, gp)
    hub.merge(TENANTS, out["pr"])
    hub.create_repo(APP)
    apply(git, gp)
    tokens = [b for m, u, b in hub.reqs if u.endswith("/agent-installation-token")]
    assert {"owner": "jfillman", "repo": "gitops-cluster-dev-tenants"} in tokens
    assert {"owner": "jfillman", "repo": "parachute", "app": "parachute"} in tokens


# -- the Flight half is never opened, and nothing is merged ---------------------------------------------------------
def test_the_flight_half_is_never_opened_and_the_tool_never_merges(git, gp, hub):
    outs = run_to_the_end(git, gp, hub)
    human = outs[-1]["humanSteps"]
    assert {h["id"]: h["repo"] for h in human} == {"cicd-flight": APP, "gitops": GITOPS}
    assert all("proposeFlight" in h["reason"] and "person" in h["reason"] for h in human)
    # the only PRs that exist are the two the tool opens
    assert {r: [p["head"] for p in ps] for r, ps in hub.prs.items()} == {
        TENANTS: ["agent/airframe-parachute-tenants"], APP: ["agent/airframe-parachute-app-repo"]}
    assert GITOPS not in hub.repos
    # every request of the whole run: GET, or the POSTs that build a new branch and its PR; never a merge, a review, a
    # workflow, an update or delete, and never a ref that already existed
    assert {m for m, _, _ in hub.reqs} == {"GET", "POST"}
    assert not [u for _, u, _ in hub.reqs if re.search(r"/merge|/reviews|/dispatches|/actions|/branches|/protection", u)]
    refs = [b["ref"] for m, u, b in hub.reqs if u.endswith("/git/refs")]
    assert sorted(refs) == ["refs/heads/agent/airframe-parachute-app-repo", "refs/heads/agent/airframe-parachute-tenants"]
    assert not any(re.search(r"gitops-parachute", u) for _, u, _ in hub.reqs if "/repos/" in u)


# -- the Flight half as a proposal: opened with proposeFlight, marked for a person to merge (A-11) --------------------
def to_the_flight_half(git, gp, hub, **kw):
    """Tenants and the app repo merged; the platform has made the gitops repo and bootstrapped each environment's values."""
    out = apply(git, gp, **kw)
    hub.merge(TENANTS, out["pr"])
    hub.create_repo(APP)
    out = apply(git, gp, **kw)
    hub.merge(APP, out["pr"])
    return apply(git, gp, **kw)


def bootstrap_gitops(hub, **extra):
    hub.create_repo(GITOPS)
    hub.repos[GITOPS]["main"].update({"kind-prod/staging/values.yaml": "# Scaffolded.\nenvName: staging\n",
                                      "kind-prod/prod/values.yaml": "# Scaffolded.\nenvName: prod\n", **extra})


def test_proposeFlight_opens_the_cicd_flight_pr_marked_for_a_person(git, gp, hub):
    out = to_the_flight_half(git, gp, hub, proposeFlight=True)
    assert out["state"] == "awaiting-merge" and out["step"] == "cicd-flight" and out["repo"] == APP and out["paths"] == ["cicd.yaml"]
    assert out["merge"] == "person" and out["needsPerson"] == ["cicd.yaml"] and "Only a person may merge" in out["next"]
    assert out["humanSteps"] == []
    row = {s["id"]: s for s in out["steps"]}["cicd-flight"]
    assert (row["status"], row["mergeBy"]) == ("awaiting-merge", "person")
    doc = yaml.safe_load(hub.pr_files(APP, out["pr"])["cicd.yaml"])
    flight = {e["name"]: e for e in doc["deploy"]["environments"] if e["tier"] == "flight"}
    assert set(flight) == {"staging", "prod"} and all(e["cluster"] == "kind-prod" for e in flight.values())
    assert gp.calls[-1][0] == "glidepath.validate_cicd" and gp.calls[-1][1][1]["agent"] is True, "validated as an agent's change"
    # the mark is in the commit (a trailer), the PR body, and the receipt
    assert "Hangar-Merge: person" in hub.commits[out["commit"]]["message"]
    pr = next(p for p in hub.prs[APP] if p["number"] == out["pr"])
    assert "Needs a person to merge" in pr["body"] and "- merge: `person`" in pr["body"]
    spec = next(t for t in TOOLS if t.name == "airframe.apply")
    assert _effect(spec, {"spec": SPEC}, out) == {"pr": out["pr"], "commit": out["commit"], "branch": out["branch"], "merge": "person",
                                                  "needsPerson": ["cicd.yaml"], "repo": APP, "paths": ["cicd.yaml"]}


def test_proposeFlight_opens_the_gitops_values_once_the_environments_exist_and_waits_before(git, gp, hub):
    out = to_the_flight_half(git, gp, hub, proposeFlight=True)
    hub.merge(APP, out["pr"])
    # the gitops repo does not exist yet: waiting, nothing opened there
    w = apply(git, gp, proposeFlight=True)
    assert w["state"] == "waiting" and "gitops-parachute does not exist yet" in w["reason"] and GITOPS not in hub.prs
    hub.create_repo(GITOPS)
    w = apply(git, gp, proposeFlight=True)
    assert w["state"] == "waiting" and "does not have kind-prod/" in w["reason"] and "bootstrapped" in w["reason"]
    assert GITOPS not in hub.prs
    bootstrap_gitops(hub)
    g = apply(git, gp, proposeFlight=True)
    assert g["state"] == "awaiting-merge" and g["step"] == "gitops" and g["repo"] == GITOPS
    assert g["merge"] == "person" and g["needsPerson"] == g["paths"] and "kind-prod/prod/values.yaml" in g["paths"]
    text = hub.pr_files(GITOPS, g["pr"])["kind-prod/prod/values.yaml"]
    assert text.startswith("# Scaffolded.") and yaml.safe_load(text)["envName"] == "prod", "the scaffold's header and key are kept"
    assert "Hangar-Merge: person" in hub.commits[g["commit"]]["message"]
    hub.merge(GITOPS, g["pr"])
    done = apply(git, gp, proposeFlight=True)
    assert done["state"] == "agent-steps-merged" and done["humanSteps"] == []
    assert {s["id"]: s["status"] for s in done["steps"]}["gitops"] == "merged"


def test_a_resumed_flight_pr_is_still_marked(git, gp, hub):
    out = to_the_flight_half(git, gp, hub, proposeFlight=True)
    again = apply(git, gp, proposeFlight=True)
    assert again["resumed"] is True and again["pr"] == out["pr"] and again["merge"] == "person" and again["needsPerson"] == out["paths"]
    assert len([p for p in hub.prs[APP] if p["head"] == "agent/airframe-parachute-cicd-flight"]) == 1


def test_proposeFlight_must_be_literally_true_and_off_means_reported_not_opened(git, gp, hub):
    for off in ({}, {"proposeFlight": False}, {"proposeFlight": "true"}, {"proposeFlight": 1}):
        h = Hub()
        g = GitHubPRs(git.token_file, broker_url="http://broker", api="https://api.github.test", opener=h)
        out = to_the_flight_half(g, FakeGlidepath(), h, **off)
        assert out["state"] == "agent-steps-merged", off
        assert [x["id"] for x in out["humanSteps"]] == ["cicd-flight", "gitops"], off
        assert not [1 for p in h.prs.get(APP, []) if p["head"].endswith("cicd-flight")], off


def test_a_flight_field_that_stays_human_is_reported_for_a_person_not_opened(git, gp, hub):
    """glidepath-api refuses a human field in the Flight change (GP-SCOPE-001: a cloud block, a pre-ADR key to drop): the
    call is not failed, the step is a humanStep with the rule, and nothing is opened for it."""
    gp.refuse_flight = True
    out = to_the_flight_half(git, gp, hub, proposeFlight=True)
    assert out["state"] == "waiting" and "pr" not in out, "the gitops values wait for their repo; nothing was opened for cicd-flight"
    human = {h["id"]: h for h in out["humanSteps"]}
    assert "cicd-flight" in human and "GP-SCOPE-001" in human["cicd-flight"]["reason"]
    assert {s["id"]: s["status"] for s in out["steps"]}["cicd-flight"] == "human"
    assert not [1 for p in hub.prs.get(APP, []) if p["head"].endswith("cicd-flight")]
    # the rest of the Flight half is independent: with the environments bootstrapped the values are proposed
    bootstrap_gitops(hub)
    g = apply(git, gp, proposeFlight=True)
    assert g["step"] == "gitops" and g["merge"] == "person" and [h["id"] for h in g["humanSteps"]] == ["cicd-flight"]


def test_a_schema_error_in_the_flight_change_is_still_a_refusal(git, gp, hub, monkeypatch):
    real = gp.validate_cicd
    out = apply(git, gp, proposeFlight=True)
    hub.merge(TENANTS, out["pr"])
    hub.create_repo(APP)
    out = apply(git, gp, proposeFlight=True)
    hub.merge(APP, out["pr"])
    monkeypatch.setattr(gp, "validate_cicd", lambda app, body, who: {**real(app, body, who), "valid": False, "problems": [
        {"pointer": "/deploy", "rule": "GP-SCHEMA-002", "severity": "error", "message": "bad", "hint": "fix"}]})
    with pytest.raises(Denied) as e:
        apply(git, gp, proposeFlight=True)
    assert e.value.rule == "GP-SCHEMA-002"


def test_a_value_a_person_set_in_the_gitops_file_is_a_conflict_not_an_overwrite(git, gp, hub):
    out = to_the_flight_half(git, gp, hub, proposeFlight=True)
    hub.merge(APP, out["pr"])
    bootstrap_gitops(hub, **{"kind-prod/prod/values.yaml": "envName: prod\nrollout:\n  strategy: rolling\n"})
    with pytest.raises(Denied) as e:
        apply(git, gp, proposeFlight=True)
    assert e.value.rule == "APPLY-CONFLICT" and GITOPS not in hub.prs


def test_a_whole_proposing_run_still_has_no_merge_and_no_push_to_an_existing_branch(git, gp, hub):
    out = to_the_flight_half(git, gp, hub, proposeFlight=True)
    hub.merge(APP, out["pr"])
    bootstrap_gitops(hub)
    g = apply(git, gp, proposeFlight=True)
    hub.merge(GITOPS, g["pr"])
    assert apply(git, gp, proposeFlight=True)["state"] == "agent-steps-merged"
    assert {m for m, _, _ in hub.reqs} == {"GET", "POST"}
    assert not [u for _, u, _ in hub.reqs if re.search(r"/merge|/reviews|/dispatches|/actions|/branches|/protection", u)]
    refs = [b["ref"] for m, u, b in hub.reqs if u.endswith("/git/refs")]
    assert len(refs) == len(set(refs)) == 4, "tenants, app-repo, cicd-flight, gitops: each branch created once, none written to twice"


def test_the_module_has_no_way_to_merge():
    """Not behaviour: the adapter's surface. A merge, approve, delete or force would have to be added to github_pr.py,
    and this fails when it is."""
    src = (ROOT / "src" / "autopilot" / "github_pr.py").read_text() + (ROOT / "src" / "autopilot" / "airframe_apply.py").read_text()
    assert not re.search(r'"(PUT|PATCH|DELETE)"|/merge|"force"|force_push|auto_merge|enable_auto', src)


# -- resume ----------------------------------------------------------------------------------------------------------
def test_a_repeated_call_returns_the_open_pr_and_opens_no_second(git, gp, hub):
    first = apply(git, gp)
    again = apply(git, gp)
    assert again["state"] == "awaiting-merge" and again["resumed"] is True
    assert (again["pr"], again["branch"], again["commit"], again["paths"]) == (first["pr"], first["branch"], first["commit"], first["paths"])
    assert len(hub.prs[TENANTS]) == 1
    assert sum(1 for m, u, _ in hub.reqs if m == "POST" and u.endswith("/pulls")) == 1


@pytest.mark.parametrize("dies_on", [("POST", "/git/trees"), ("POST", "/git/commits"), ("POST", "/git/refs"), ("POST", "/pulls")])
def test_a_call_killed_at_any_point_is_finished_by_the_next_with_no_duplicate(git, gp, hub, dies_on):
    hub.kill = (*dies_on, 1)
    with pytest.raises(Killed):
        apply(git, gp)
    out = apply(git, gp)
    assert out["state"] == "awaiting-merge" and out["step"] == "tenants" and out["pr"]
    assert [p["head"] for p in hub.prs[TENANTS]] == ["agent/airframe-parachute-tenants"], "exactly one PR on one branch"
    # a branch that got as far as its ref is adopted (resumed); one that died earlier is built fresh
    assert out["resumed"] is (dies_on == ("POST", "/pulls"))
    assert hub.pr_files(TENANTS, out["pr"]).keys() >= set(out["paths"])
    refs = [b["ref"] for m, u, b in hub.reqs if m == "POST" and u.endswith("/git/refs")]
    assert len(refs) == len(set(refs)), "no ref is created twice: an existing branch is never written to"


def test_a_branch_left_without_its_pr_is_adopted_only_if_its_files_are_the_plans(git, gp, hub):
    hub.kill = ("POST", "/pulls", 1)
    with pytest.raises(Killed):
        apply(git, gp)
    branch = "agent/airframe-parachute-tenants"
    assert hub.prs.get(TENANTS, []) == [] and branch in hub.repos[TENANTS]
    out = apply(git, gp)
    assert out["resumed"] is True and out["commit"] == hub.repos[TENANTS][branch + "\0commit"]
    # a branch somebody else wrote under that name is not adopted, and not touched
    hub2 = Hub()
    hub2.repos[TENANTS]["agent/airframe-parachute-tenants"] = {"tenants/parachute/xr-requests/parachute.yaml": "other\n"}
    g2 = GitHubPRs(git.token_file, broker_url="http://broker", api="https://api.github.test", opener=hub2)
    with pytest.raises(Denied) as e:
        apply(g2, gp)
    assert e.value.rule == "APPLY-BRANCH-DIVERGED"
    assert hub2.prs == {} and not [1 for m, u, _ in hub2.reqs if m == "POST" and not u.endswith("agent-installation-token")]


def test_a_run_killed_between_steps_resumes_from_github_not_from_memory(token, gp, hub):
    """A different adapter instance, as a restarted core and a new run: nothing in the process is shared."""
    def fresh():
        return GitHubPRs(token, broker_url="http://broker", api="https://api.github.test", opener=hub)
    a = apply(fresh(), gp)
    hub.merge(TENANTS, a["pr"])
    hub.create_repo(APP)
    hub.kill = ("POST", "/git/commits", 1)         # the app-repo step dies halfway
    with pytest.raises(Killed):
        apply(fresh(), gp)
    b = apply(fresh(), gp)
    assert b["step"] == "app-repo" and b["state"] == "awaiting-merge" and statuses(b)["tenants"] == "merged"
    hub.merge(APP, b["pr"])
    assert apply(fresh(), gp)["state"] == "agent-steps-merged"
    assert [len(v) for v in hub.prs.values()] == [1, 1]


def test_a_pr_a_person_closed_is_never_reopened_or_replaced_until_they_delete_its_branch(git, gp, hub):
    out = apply(git, gp)
    hub.close(TENANTS, out["pr"])
    again = apply(git, gp)
    assert again["state"] == "waiting" and "closed without merging" in again["reason"] and "pr" not in again
    assert "delete its branch" in again["reason"] and len(hub.prs[TENANTS]) == 1
    # the message and the behaviour agree: with the branch deleted the next call opens a new PR
    hub.delete_branch(TENANTS, "agent/airframe-parachute-tenants")
    fresh = apply(git, gp)
    assert fresh["state"] == "awaiting-merge" and fresh["resumed"] is False and fresh["pr"] != out["pr"]
    assert len(hub.prs[TENANTS]) == 2


def test_a_resumed_receipt_names_only_what_the_pr_carries(git, gp, hub, monkeypatch):
    """The plan's cicd.yaml change was a no-op when the PR opened (so the PR carries the environment files only). A later
    call, when the plan has a cicd.yaml change again, must not put cicd.yaml in the receipt of the PR it returns."""
    out = apply(git, gp)
    hub.merge(TENANTS, out["pr"])
    hub.create_repo(APP)
    real = gp.validate_cicd
    monkeypatch.setattr(gp, "validate_cicd", lambda app, body, who: {**real(app, body, who), "changes": []})
    first = apply(git, gp)
    assert first["paths"] == ["glidepath/envs/dev.yaml", "glidepath/envs/test.yaml"]
    monkeypatch.setattr(gp, "validate_cicd", real)
    again = apply(git, gp)
    assert again["resumed"] is True and again["pr"] == first["pr"]
    assert again["paths"] == first["paths"] and "cicd.yaml" not in again["paths"]
    assert [s for s in again["steps"] if s["id"] == "app-repo"][0]["status"] == "open"


# -- the seeded bad run -----------------------------------------------------------------------------------------------
BAD = yaml.safe_load((ROOT / "examples" / "parachute-bad.appspec.yaml").read_text())


def test_a_bad_spec_opens_nothing(git, gp, hub):
    """The seeded bad run, the spec side: a value the chart rejects. The planner refuses it, and no request is sent."""
    out = apply(git, gp, BAD)
    assert out["error"]["rule"] == "AF-SPEC-001" and "replicaCount" in out["error"]["message"]
    assert hub.reqs == [] and gp.calls == []


def test_an_ambiguous_spec_asks_instead_of_guessing(token, gp, hub):
    two = airframe_plan.Registry("kind-dev", ("kind-prod", "kind-prod-2"), "gitops-cluster-dev-tenants")
    out = airframe_apply.apply(GitHubPRs(token, broker_url="http://broker", api="https://api.github.test", opener=hub), gp,
                               fake_airframe(registry=two), Session(), META, {"spec": SPEC})
    assert out["needsInput"]["options"] == ["kind-prod", "kind-prod-2"]
    assert hub.reqs == []


def test_a_cicd_change_glidepath_refuses_opens_nothing_for_that_step(git, gp, hub, monkeypatch):
    """The seeded bad run, the policy side: the Ground cicd.yaml change turns a gate off (GP-SCOPE-002). The tenants step
    has merged; the app-repo step is refused with the rule and no branch or PR is made in the app's repo."""
    out = apply(git, gp)
    hub.merge(TENANTS, out["pr"])
    hub.create_repo(APP)
    real = gp.validate_cicd

    def refusing(app, body, on_behalf_of):
        v = real(app, {"patch": {"governance": {"sast": False}}, "agent": True}, on_behalf_of)
        return v
    monkeypatch.setattr(gp, "validate_cicd", refusing)
    with pytest.raises(Denied) as e:
        apply(git, gp)
    assert e.value.rule == "GP-SCOPE-002"
    assert APP not in hub.prs and not [1 for m, u, _ in hub.reqs if m == "POST" and "/repos/jfillman/parachute/" in u]


def test_an_app_glidepath_does_not_know_waits_rather_than_failing(git, gp, hub, monkeypatch):
    out = apply(git, gp)
    hub.merge(TENANTS, out["pr"])
    hub.create_repo(APP)
    monkeypatch.setattr(gp, "validate_cicd", lambda *a: {"error": {"rule": "GP-API-404", "message": "no app", "hint": ""}})
    w = apply(git, gp)
    assert w["state"] == "waiting" and "does not know parachute" in w["reason"] and APP not in hub.prs


SCAFFOLD = "# Scaffolded by Glidepath onboarding. Environment values for dev.\nenvName: dev\n"


def _ready_app(git, gp, hub, **files):
    out = apply(git, gp)
    hub.merge(TENANTS, out["pr"])
    hub.create_repo(APP)
    hub.repos[APP]["main"].update(files)


def test_onboarding_scaffold_gets_the_plans_keys_merged_in_with_its_header_kept(git, gp, hub):
    _ready_app(git, gp, hub, **{"glidepath/envs/dev.yaml": SCAFFOLD})
    out = apply(git, gp)
    assert out["state"] == "awaiting-merge" and "glidepath/envs/dev.yaml" in out["paths"]
    text = hub.pr_files(APP, out["pr"])["glidepath/envs/dev.yaml"]
    assert text.startswith("# Scaffolded by Glidepath onboarding")
    doc = yaml.safe_load(text)
    assert doc["envName"] == "dev" and doc["env"] == SPEC["config"]["env"] and doc["rollout"] == SPEC["config"]["rollout"]
    assert "glidepath/envs/test.yaml" in out["paths"], "the file the repo does not have yet is still created"


def test_extra_keys_a_person_added_are_kept_when_merging(git, gp, hub):
    _ready_app(git, gp, hub, **{"glidepath/envs/dev.yaml": "envName: dev\nsomething: else\n"})
    out = apply(git, gp)
    doc = yaml.safe_load(hub.pr_files(APP, out["pr"])["glidepath/envs/dev.yaml"])
    assert doc["something"] == "else" and doc["rollout"]


def test_a_key_the_plan_sets_with_a_different_value_is_a_conflict_naming_the_key(git, gp, hub):
    _ready_app(git, gp, hub, **{"glidepath/envs/dev.yaml": "envName: dev\nrollout:\n  strategy: rolling\n"})
    with pytest.raises(Denied) as e:
        apply(git, gp)
    assert e.value.rule == "APPLY-CONFLICT" and "rollout.strategy" in e.value.message and APP not in hub.prs


def test_a_file_that_is_not_a_mapping_is_still_refused_as_existing(git, gp, hub):
    _ready_app(git, gp, hub, **{"glidepath/envs/dev.yaml": "- a\n- b\n"})
    with pytest.raises(Denied) as e:
        apply(git, gp)
    assert e.value.rule == "APPLY-EXISTS" and APP not in hub.prs


def test_a_file_that_already_holds_every_key_is_not_rewritten(git, gp, hub):
    _ready_app(git, gp, hub)
    planned = apply(git, gp)
    files = hub.pr_files(APP, planned["pr"])
    hub.close(APP, planned["pr"]), hub.delete_branch(APP, planned["branch"])
    # same keys, different formatting and an extra comment: equal values, nothing to write for that file
    hub.repos[APP]["main"]["glidepath/envs/dev.yaml"] = "# mine\n" + yaml.safe_dump(yaml.safe_load(files["glidepath/envs/dev.yaml"]), default_flow_style=True)
    out = apply(git, gp)
    assert "glidepath/envs/dev.yaml" not in out["paths"]


def test_the_merge_helper_keeps_only_a_leading_header():
    out = airframe_apply.merge_onboarding_file("o/r", "glidepath/envs/dev.yaml", "# a\n\n# b\nenvName: dev\n# tail\n", "envName: dev\nenv: [{name: A, value: b}]\n")
    assert out.startswith("# a\n\n# b\n") and yaml.safe_load(out) == {"envName": "dev", "env": [{"name": "A", "value": "b"}]}


# -- the repos-ready wait covers the onboarding resync PR ---------------------------------------------------------
def test_apply_waits_while_the_onboarding_resync_pr_is_open(git, gp, hub):
    _ready_app(git, gp, hub)
    hub.repos[APP]["onboarding-resync"] = {}
    hub.prs.setdefault(APP, []).append({"number": 900, "head": "onboarding-resync", "state": "open", "merged_at": None,
                                       "title": "resync", "body": "", "base": "main"})
    w = apply(git, gp)
    assert w["state"] == "waiting" and "onboarding resync PR" in w["reason"] and "/pull/900" in w["reason"]
    assert statuses(w)["app-repo"] == "waiting" and "pr" not in w
    hub.merge(APP, 900)
    assert apply(git, gp)["state"] == "awaiting-merge"


def test_apply_waits_for_the_gitops_repos_resync_pr_too(git, gp, hub):
    _ready_app(git, gp, hub)
    hub.repos[GITOPS] = {"main": {}, "onboarding-resync": {}}
    hub.onboarded.add(GITOPS)
    hub.prs[GITOPS] = [{"number": 7, "head": "onboarding-resync", "state": "open", "merged_at": None, "title": "r",
                        "body": "", "base": "main"}]
    assert apply(git, gp)["state"] == "waiting"
    hub.merge(GITOPS, 7)
    assert apply(git, gp)["state"] == "awaiting-merge"


def test_no_glidepath_backend_means_no_cicd_change_is_written(git, hub):
    out = apply(git, FakeGlidepath())          # control: the fake answers
    hub.merge(TENANTS, out["pr"])
    hub.create_repo(APP)
    with pytest.raises(Denied) as e:
        apply(git, None)
    assert e.value.rule == "APPLY-NO-GLIDEPATH"


# -- through the gateway: policy, budget, receipt ------------------------------------------------------------------
@pytest.fixture
def wired(backends, token, hub, gp):
    backends.git = GitHubPRs(token, broker_url="http://broker", api="https://api.github.test", opener=hub)
    backends.glidepath = gp
    backends.airframe = fake_airframe()
    return backends


def test_through_the_gateway_each_pr_has_a_receipt_in_the_chain(wired, gw, hub):
    sid = gw.open_session("alice", "parachute").data["session"]
    r = gw.call("alice", sid, "airframe.apply", {"spec": SPEC})
    assert r.ok, r.as_dict()
    last = gw.core.records[-1]
    assert last["tool"] == "airframe.apply" and last["decision"] == "allow" and last["tier"] == 1
    assert last["effect"] == {"pr": r.data["pr"], "commit": r.data["commit"], "branch": "agent/airframe-parachute-tenants",
                              "repo": TENANTS, "paths": sorted(r.data["paths"])}
    hub.merge(TENANTS, r.data["pr"])
    hub.create_repo(APP)
    r2 = gw.call("alice", sid, "airframe.apply", {"spec": SPEC})
    assert r2.ok and gw.core.records[-1]["effect"]["repo"] == APP and gw.core.records[-1]["effect"]["pr"] == r2.data["pr"]
    # the PRs carry the session that opened them (provenance reads it)
    assert f"- session: `{sid}`" in hub.prs[TENANTS][0]["body"]


def test_the_agent_can_apply_only_to_the_app_its_definition_names(wired, gw, hub):
    sid = gw.open_session("alice", "parachute").data["session"]
    r = gw.call("alice", sid, "airframe.apply", {"spec": {**SPEC, "app": "flight-api"}})
    assert not r.ok and r.decision["rule"] == "R023"
    assert hub.reqs == []
    r = gw.call("alice", sid, "airframe.apply", {"spec": {"stack": "python"}})
    assert not r.ok and r.decision["rule"] == "R023", "no app named is no app allowed"
    r = gw.call("alice", sid, "airframe.apply", {"spec": "not a mapping"})
    assert not r.ok and r.decision["rule"] == "R008"


def test_app_provisioner_admits_any_app_and_parachute_still_refuses_one(wired, gw, hub):
    """The general agent's scope is `*`: an AppSpec for an app that is not parachute passes R023 and opens its PR.
    parachute's stays the one app, so the same spec is still refused by it."""
    other = yaml.safe_load((ROOT / "examples" / "app-provisioner.appspec.yaml").read_text())
    assert other["app"] != "parachute"
    sid = gw.open_session("alice", "app-provisioner").data["session"]
    r = gw.call("alice", sid, "airframe.apply", {"spec": other})
    assert r.ok, r.as_dict()
    assert r.data["branch"] == "agent/airframe-inventory-api-tenants" and r.data["repo"] == TENANTS
    assert gw.core.records[-1]["tool"] == "airframe.apply" and gw.core.records[-1]["decision"] == "allow"
    sid2 = gw.open_session("alice", "parachute").data["session"]
    n = len(hub.reqs)
    r = gw.call("alice", sid2, "airframe.apply", {"spec": other})
    assert not r.ok and r.decision["rule"] == "R023" and len(hub.reqs) == n, "parachute still refuses another app"
    r = gw.call("alice", sid, "airframe.apply", {"spec": {"stack": "python"}})
    assert not r.ok and r.decision["rule"] == "R023", "even `*` needs an app named"


def test_through_the_gateway_the_flight_receipt_says_a_person_must_merge(wired, gw, hub):
    """app-provisioner sets profile.airframe.proposeFlight: its Flight PR's receipt in the chain says merge: person and names
    the paths Clearance's agent-scope check reads (neutral until a person merges)."""
    sid = gw.open_session("alice", "app-provisioner").data["session"]
    spec = {**SPEC, "app": "parachute"}
    r = gw.call("alice", sid, "airframe.apply", {"spec": spec, "proposeFlight": True})
    assert r.ok, r.as_dict()
    hub.merge(TENANTS, r.data["pr"])
    hub.create_repo(APP)
    r = gw.call("alice", sid, "airframe.apply", {"spec": spec, "proposeFlight": True})
    hub.merge(APP, r.data["pr"])
    r = gw.call("alice", sid, "airframe.apply", {"spec": spec, "proposeFlight": True})
    assert r.ok and r.data["step"] == "cicd-flight", r.as_dict()
    last = gw.core.records[-1]
    assert last["effect"]["merge"] == "person" and last["effect"]["needsPerson"] == ["cicd.yaml"] and last["effect"]["repo"] == APP
    assert last["effect"]["paths"] == ["cicd.yaml"]


def test_a_definition_without_proposeFlight_is_refused_R025_before_anything_is_sent(wired, gw, hub):
    sid = gw.open_session("alice", "parachute").data["session"]
    r = gw.call("alice", sid, "airframe.apply", {"spec": SPEC, "proposeFlight": True})
    assert not r.ok and r.decision["rule"] == "R025" and hub.reqs == []
    assert "proposeFlight" in r.decision["hint"]
    # the same call without it is the ordinary one
    assert gw.call("alice", sid, "airframe.apply", {"spec": SPEC}).ok
    # a non-boolean is refused by the argument check, a false is a no-ask
    r = gw.call("alice", sid, "airframe.apply", {"spec": SPEC, "proposeFlight": "yes"})
    assert not r.ok and r.decision["rule"] == "R008"
    assert gw.call("alice", sid, "airframe.apply", {"spec": SPEC, "proposeFlight": False}).ok


def test_a_definition_that_does_not_hold_apply_cannot_call_it(wired, gw, hub):
    sid = gw.open_session("alice", "airframe-operator").data["session"]
    r = gw.call("alice", sid, "airframe.apply", {"spec": SPEC})
    assert not r.ok and hub.reqs == []


def test_apply_has_no_handler_without_the_resumable_adapter(backends):
    from autopilot.gateway import hangar_handlers
    assert "airframe.apply" not in hangar_handlers(backends), "the default fake git cannot resume"


# -- the validator and the definition -------------------------------------------------------------------------------
def test_no_definition_names_airframe_validate_while_the_image_has_no_validator():
    """A-3 removed it from every definition rather than restoring airframe-validate (and helm) to the image: a definition
    that names it again fails here, and test_definitions_backed fails it too unless a validator is configured."""
    from autopilot.engine import profile
    named = [n for n, d in {**profile.load_dir(ROOT / "agents"), **profile.load_dir(ROOT / "agents" / "skyport")}.items()
             if "airframe.validate" in d.limits.tools]
    assert named == []


def test_the_parachute_definition_can_plan_and_apply_and_nothing_that_reaches_github_another_way():
    from autopilot.engine import profile
    d = profile.load_dir(ROOT / "agents")["parachute"]
    assert {"airframe.apply", "airframe.plan", "airframe.status"} <= d.limits.tools
    assert not {t for t in d.limits.tools if t.startswith(("repo.", "xr.", "argo.", "run.", "glidepath."))}
    assert d.limits.children == 0 and d.limits.tier_ceiling == Tier.T1
    assert d.raw_profile["airframe"] == {"apps": ["parachute"]}
    # the agent-scope gate (AS003) holds each PR's repo to the definition's repos.allow: the two repos apply writes, on
    # Hangar (the default AIRFRAME_TENANTS_REPO), and no more
    from autopilot import serve
    tenants = serve.config_from_env({"AUTOPILOT_EPHEMERAL": "1", **core_env()}).airframe_tenants_repo
    assert sorted(d.repos_allow) == sorted(["jfillman/parachute", f"jfillman/{tenants}"])


def test_the_app_provisioner_definition_has_parachutes_tools_and_limits_with_a_wider_scope():
    from autopilot.engine import profile
    defs = profile.load_dir(ROOT / "agents")
    d, p = defs["app-provisioner"], defs["parachute"]
    assert d.limits.tools == p.limits.tools and d.limits.children == 0 and d.limits.tier_ceiling == Tier.T1
    assert not {t for t in d.limits.tools if t.startswith(("repo.", "xr.", "argo.", "run.", "glidepath."))}
    assert d.raw_profile["airframe"] == {"apps": ["*"], "proposeFlight": True} and list(d.repos_allow) == ["jfillman/*"]
    assert "proposeFlight" not in p.raw_profile["airframe"], "parachute never proposes the Flight half"


# -- run warnings (A-11i): the gateway notes what the calls did, results.warnings flags the run ----------------------
def test_a_run_that_guessed_the_appspec_is_flagged(wired, gw):
    sid = gw.open_session("alice", "parachute").data["session"]
    for _ in range(3):
        assert gw.call("alice", sid, "airframe.plan", {"spec": SPEC}).ok
    s = gw.store.get(sid)
    assert s.signals["plan_calls"] == 3 and results.warnings(s.signals) == []
    gw.call("alice", sid, "airframe.plan", {"spec": SPEC})
    assert any("airframe.plan was called 4 times" in w for w in results.warnings(s.signals))
    assert s.to_record()["signals"]["plan_calls"] == 4 and type(s).from_record(s.to_record()).signals["plan_calls"] == 4


def test_an_apply_that_answers_ok_but_opens_no_pr_is_flagged(wired, gw, hub):
    sid = gw.open_session("alice", "parachute").data["session"]
    r = gw.call("alice", sid, "airframe.apply", {"spec": SPEC})
    assert r.ok and gw.store.get(sid).signals["apply_pr"] == 1 and results.warnings(gw.store.get(sid).signals) == []
    hub.merge(TENANTS, r.data["pr"])                    # the app's repo does not exist: ok, `waiting`, no PR
    sid2 = gw.open_session("alice", "parachute").data["session"]
    assert gw.call("alice", sid2, "airframe.apply", {"spec": SPEC}).data["state"] == "waiting"
    w = results.warnings(gw.store.get(sid2).signals)
    assert len(w) == 1 and "opened no PR" in w[0] and "waiting" in w[0]


def test_finish_run_keeps_the_warnings_with_the_result():
    sig = {}
    for _ in range(4):
        results.observe(sig, "airframe.plan")
    results.observe(sig, "airframe.apply", {"state": "agent-steps-merged"})
    assert [w.split(" ")[0] for w in results.warnings(sig)] == ["airframe.plan"], "nothing left to open is not a warning"


def test_the_parachute_definition_carries_the_provisioning_contract():
    doc = yaml.safe_load((ROOT / "agents" / "parachute.yaml").read_text())
    text, tools = doc["runtime"]["instructions"], doc["profile"]["tools"]
    assert len(text) <= 1500, "the definition schema's instructions limit"
    assert "human.request" in tools and "airframe.capabilities" in tools
    for needle in ("section `appspec`", "at most 3 plan calls", "Never proceed with less than asked", "RESUME PROMPT",
                   "merge <url>, then reply", "human.request.get"):
        assert needle in text, needle


def test_onboarding_file_patterns_match_the_gitops_cluster_env_layout():
    match = lambda p: any(r.match(p) for r in airframe_apply.ONBOARDING_FILES)
    assert all(match(p) for p in ("glidepath/envs/dev.yaml", "hangar-prod/prod/values.yaml", "base/values.yaml"))
    assert not any(match(p) for p in ("cicd.yaml", "glidepath/envs/a/b.yaml", "a/b/c/values.yaml", "values.yaml"))
