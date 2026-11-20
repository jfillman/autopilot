"""airframe.configure (W-45): a validated change to an EXISTING app's chart values, as one PR for a person to review.

airframe.apply provisions an app once (fixed branches, a merged step skipped, an existing file a conflict), so adding an SLO
to an app that is already running is this tool's. GitHub is the stateful Hub of test_airframe_apply.py behind the real
adapter (github_pr.GitHubPRs), extended to list a directory, so what is asserted is the HTTP the tool really sends. The
Clearance test core decides the scope (R023, the tool's tier and receipt)."""
import json
import re

import pytest
import yaml
from coreenv import core_decide

from autopilot import airframe_apply
from autopilot.airframe_tools import fake_airframe
from autopilot.engine.dispatch import Denied, _effect
from autopilot.github_pr import GitHubPRs
from autopilot.hangar_tools import TOOLS
from test_airframe_apply import APP, META, Hub, Session

HEADER = "# Scaffolded by Glidepath onboarding.\n# Keep this comment.\n"
DEV = HEADER + "envName: dev\nrollout:\n  replicas: 2\n"
SLO = {"name": "availability", "service": "parachute", "objective": 99.9,
       "indicator": {"type": "availability", "metric": "http_requests_total", "totalFilter": 'job="parachute"',
                     "errorFilter": 'code=~"5.."'}}
PATH = "glidepath/envs/dev.yaml"
SPEC_OF_TOOL = next(t for t in TOOLS if t.name == "airframe.configure")


class DirHub(Hub):
    """The Hub, which also lists a directory (GET /contents/<dir>), as GitHub does."""

    def __call__(self, req, timeout):
        m = re.match(r"https://api\.github\.test/repos/([^/]+/[^/]+)/contents/([^?]+)\?ref=(.+)$", req.full_url)
        if req.get_method() == "GET" and m and m.group(1) in self.repos:
            files = self.repos[m.group(1)].get(m.group(3))
            prefix = m.group(2).rstrip("/") + "/"
            if isinstance(files, dict) and m.group(2) not in files and any(k.startswith(prefix) for k in files):
                self.reqs.append(("GET", req.full_url, None))
                names = sorted({k[len(prefix):].split("/")[0]: ("dir" if "/" in k[len(prefix):] else "file") for k in files if k.startswith(prefix)}.items())
                return 200, json.dumps([{"name": n, "type": t, "size": 1} for n, t in names]).encode()
        return super().__call__(req, timeout)


@pytest.fixture
def hub():
    h = DirHub()
    h.create_repo(APP)
    h.repos[APP]["main"][PATH] = DEV
    return h


@pytest.fixture
def git(tmp_path, hub):
    f = tmp_path / "t"
    f.write_text("sa-1\n")
    return GitHubPRs(f, broker_url="http://broker", api="https://api.github.test", opener=hub)


def configure(git, **a):
    args = {"app": "parachute", "env": "dev", "values": {"slos": [SLO]}, **a}
    return airframe_apply.configure(git, fake_airframe(), Session(), META, args)


def refused(git, **a):
    with pytest.raises(Denied) as e:
        configure(git, **a)
    return e.value


# -- the happy path ---------------------------------------------------------------------------------------------------
def test_an_slo_is_added_to_an_existing_dev_file_as_one_pr_with_the_header_and_other_keys_kept(git, hub):
    out = configure(git)
    assert out["state"] == "awaiting-merge" and out["resumed"] is False and out["repo"] == APP
    assert out["paths"] == [PATH] and out["path"] == PATH and out["branch"].startswith("agent/airframe-parachute-configure-dev-")
    text = hub.pr_files(APP, out["pr"])[PATH]
    assert text.startswith(HEADER), "the header comment is kept"
    assert yaml.safe_load(text) == {"envName": "dev", "rollout": {"replicas": 2}, "slos": [SLO]}, "other keys kept, the SLO added"
    assert hub.repos[APP]["main"][PATH] == DEV, "nothing is merged"
    assert "merge" not in out and "Needs a person" not in hub.prs[APP][0]["body"]
    assert "airframe.configure" in hub.prs[APP][0]["title"] + hub.prs[APP][0]["body"]


def test_the_receipt_is_the_one_apply_and_repo_pr_open_have(git, hub):
    args = {"app": "parachute", "env": "dev", "values": {"slos": [SLO]}}
    out = configure(git)
    spec = SPEC_OF_TOOL
    assert _effect(spec, args, out) == {"pr": out["pr"], "commit": out["commit"], "branch": out["branch"], "repo": APP, "paths": [PATH]}
    assert out["url"].endswith(f"/pull/{out['pr']}")


def test_the_pr_carries_who_opened_it(git, hub):
    configure(git)
    assert "- session: `s-1`" in hub.prs[APP][0]["body"]
    assert "Hangar-Session: s-1" in hub.commits[hub.repos[APP][hub.prs[APP][0]["head"] + "\0commit"]]["message"]


def test_a_critical_risk_key_marks_the_pr_for_a_person(git, hub):
    out = configure(git, values={"extraManifests": [{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "x"}}]})
    assert out["merge"] == "person" and out["needsPerson"] == [PATH]
    assert "Hangar-Merge: person" in hub.commits[hub.repos[APP][hub.prs[APP][0]["head"] + "\0commit"]]["message"]


# -- lists and conflicts ----------------------------------------------------------------------------------------------
def test_a_second_slo_is_appended_by_name_and_the_first_is_kept(git, hub):
    other = {**SLO, "name": "errors"}
    hub.repos[APP]["main"][PATH] = DEV + yaml.safe_dump({"slos": [SLO]})
    out = configure(git, values={"slos": [other]})
    assert [s["name"] for s in yaml.safe_load(hub.pr_files(APP, out["pr"])[PATH])["slos"]] == ["availability", "errors"]


def test_the_same_slo_again_is_a_noop_and_opens_nothing(git, hub):
    hub.repos[APP]["main"][PATH] = DEV + yaml.safe_dump({"slos": [SLO]})
    out = configure(git)
    assert out["state"] == "noop" and "pr" not in out and hub.prs == {}
    assert not [1 for m, u, _ in hub.reqs if m == "POST" and "/repos/" in u]


def test_the_same_name_with_other_content_is_a_conflict_naming_the_entry(git, hub):
    hub.repos[APP]["main"][PATH] = DEV + yaml.safe_dump({"slos": [SLO]})
    e = refused(git, values={"slos": [{**SLO, "objective": 99.0}]})
    assert e.rule == "CONFIGURE-CONFLICT" and "slos[availability]" in e.message and "replace" in e.hint
    assert hub.prs == {}


def test_a_scalar_a_person_set_is_a_conflict_and_replace_true_makes_the_patch_win(git, hub):
    e = refused(git, values={"rollout": {"replicas": 3}})
    assert e.rule == "CONFIGURE-CONFLICT" and "rollout.replicas" in e.message
    out = configure(git, values={"rollout": {"replicas": 3}}, replace=True)
    assert yaml.safe_load(hub.pr_files(APP, out["pr"])[PATH])["rollout"]["replicas"] == 3
    assert out["replaced"] == ["rollout.replicas"] and "rollout.replicas" in hub.prs[APP][0]["body"]


def test_replace_true_replaces_a_same_named_entry_in_place(git, hub):
    other = {**SLO, "name": "latency"}
    hub.repos[APP]["main"][PATH] = DEV + yaml.safe_dump({"slos": [SLO, other]})
    out = configure(git, values={"slos": [{**SLO, "objective": 99.0}]}, replace=True)
    slos = yaml.safe_load(hub.pr_files(APP, out["pr"])[PATH])["slos"]
    assert [s["name"] for s in slos] == ["availability", "latency"] and slos[0]["objective"] == 99.0 and out["replaced"] == ["slos[availability]"]


def test_a_list_that_is_not_of_named_entries_is_compared_whole():
    conflicts, replaced = [], []
    assert airframe_apply._merge_values({"a": [1, 2]}, {"a": [1, 2]}, "", conflicts, replaced, False) == {"a": [1, 2]} and not conflicts
    airframe_apply._merge_values({"a": [1, 2]}, {"a": [1, 3]}, "", conflicts, replaced, False)
    assert conflicts == ["a"]
    # a duplicate name is not an identity: the list is compared whole too
    airframe_apply._merge_values({"l": [{"name": "x"}, {"name": "x"}]}, {"l": [{"name": "y"}]}, "", conflicts, replaced, False)
    assert conflicts == ["a", "l"]


# -- finding the file by envName -----------------------------------------------------------------------------------------
def test_the_file_is_found_by_envName_not_by_its_name(git, hub):
    """dev.yaml says envName: staging, and the file for dev is team-a.yaml: the identity is the key."""
    hub.repos[APP]["main"] = {"glidepath/envs/dev.yaml": "envName: staging\n", "glidepath/envs/team-a.yaml": DEV.replace("dev", "dev"),
                              "glidepath/envs/zz.yaml": "envName: test\n", "glidepath/envs/README.md": "not yaml"}
    out = configure(git)
    assert out["paths"] == ["glidepath/envs/team-a.yaml"]
    assert set(hub.pr_files(APP, out["pr"])) == {"glidepath/envs/dev.yaml", "glidepath/envs/team-a.yaml", "glidepath/envs/zz.yaml", "glidepath/envs/README.md"}
    assert hub.pr_files(APP, out["pr"])["glidepath/envs/dev.yaml"] == "envName: staging\n", "the decoy is untouched"


def test_two_files_for_one_environment_are_ambiguous(git, hub):
    hub.repos[APP]["main"]["glidepath/envs/other.yaml"] = "envName: dev\n"
    assert refused(git).rule == "CONFIGURE-AMBIGUOUS"


def test_an_environment_with_no_file_is_refused_and_names_where_flight_values_are(git, hub):
    e = refused(git, env="staging")
    assert e.rule == "CONFIGURE-NO-ENV" and "gitops-parachute/<cluster>/staging/values.yaml" in e.hint and "person" in e.hint
    hub.repos[APP]["main"] = {}
    assert refused(git).rule == "CONFIGURE-NO-ENV"
    assert not [1 for m, u, _ in hub.reqs if "gitops-parachute" in u], "the Flight repository is not touched"


# -- what it refuses -----------------------------------------------------------------------------------------------------
def test_a_pr_preview_is_refused_with_where_its_values_live(git, hub):
    e = refused(git, env="pr-12")
    assert e.rule == "CONFIGURE-PR-ENV" and "glidepath/pr-env.yaml" in e.hint and "pull request's branch" in e.hint and "static environment" in e.hint
    assert hub.reqs == [], "nothing was asked of GitHub"


def test_an_agent_environment_and_a_cicd_name_are_refused(git, hub):
    assert refused(git, env="agent-7").rule == "CONFIGURE-AGENT-ENV"
    assert refused(git, env="x-cicd").rule == "CONFIGURE-ENV"
    assert hub.reqs == []


@pytest.mark.parametrize("a", [{"app": "Not An App"}, {"app": 3}, {"env": "../x"}, {"env": None}, {"values": {}}, {"values": [1]},
                               {"values": "slos: []"}, {"replace": "yes"}])
def test_malformed_arguments_are_refused_before_any_request(git, hub, a):
    with pytest.raises(Denied) as e:
        configure(git, **a)
    assert e.value.rule.startswith("CONFIGURE-") and hub.reqs == []


def test_a_patch_the_chart_schema_refuses_is_refused_with_the_path(git, hub):
    e = refused(git, values={"slos": [{**SLO, "objective": 101}]})
    assert e.rule == "CONFIGURE-VALUES" and "slos/0/objective" in e.message
    e = refused(git, values={"slos": [{**SLO, "indicator": {**SLO["indicator"], "totalFilter": 'namespace="other"} or {x="y"'}}]})
    assert e.rule == "CONFIGURE-VALUES"
    assert hub.reqs == []


@pytest.mark.parametrize("key", ["envName", "release", "releaseTracking", "image", "devClusterName", "nonsense"])
def test_keys_that_are_not_human_chart_values_are_not_settable(git, hub, key):
    e = refused(git, values={key: "x"})
    assert e.rule == "CONFIGURE-VALUES"


def test_the_merged_file_is_validated_as_a_whole(git, hub):
    hub.repos[APP]["main"][PATH] = DEV + "autoscaling: 5\n"
    e = refused(git)
    assert e.rule == "CONFIGURE-MERGED" and "autoscaling" in e.message
    assert hub.prs == {}


def test_keys_of_the_file_that_are_not_chart_values_do_not_fail_the_whole_file_check(git, hub):
    hub.repos[APP]["main"][PATH] = DEV + "release:\n  image: x\n"
    out = configure(git)
    assert yaml.safe_load(hub.pr_files(APP, out["pr"])[PATH])["release"] == {"image": "x"}


def test_a_file_that_is_only_a_header_and_envName_still_takes_values(git, hub):
    hub.repos[APP]["main"][PATH] = HEADER + "envName: dev\n"
    out = configure(git)
    assert yaml.safe_load(hub.pr_files(APP, out["pr"])[PATH]) == {"envName": "dev", "slos": [SLO]}


# -- the branch, and a retry --------------------------------------------------------------------------------------------
def test_a_retry_returns_the_same_pr_and_opens_no_second(git, hub):
    first = configure(git)
    again = configure(git)
    assert again["state"] == "awaiting-merge" and again["resumed"] is True
    assert (again["pr"], again["branch"], again["commit"], again["paths"]) == (first["pr"], first["branch"], first["commit"], first["paths"])
    assert len(hub.prs[APP]) == 1 and sum(1 for m, u, _ in hub.reqs if m == "POST" and u.endswith("/pulls")) == 1


def test_the_branch_is_derived_from_the_content_not_from_the_key_order(git, hub):
    a = configure(git, values={"slos": [SLO], "serviceMonitor": {"enabled": True}})
    b = configure(git, values={"serviceMonitor": {"enabled": True}, "slos": [{**SLO}]})
    assert a["branch"] == b["branch"] and b["resumed"] is True
    c = configure(git, values={"slos": [{**SLO, "name": "other"}]})
    assert c["branch"] != a["branch"] and c["resumed"] is False and len(hub.prs[APP]) == 2
    assert airframe_apply.configure_branch("parachute", "dev", {"a": 1}, False) != airframe_apply.configure_branch("parachute", "dev", {"a": 1}, True)
    assert airframe_apply.configure_branch("parachute", "dev", {"a": 1}, False) != airframe_apply.configure_branch("parachute", "test", {"a": 1}, False)


def test_a_call_killed_after_the_branch_and_before_the_pr_is_finished_by_the_retry(git, hub):
    from test_airframe_apply import Killed
    hub.kill = ("POST", "/pulls", 1)
    with pytest.raises(Killed):
        configure(git)
    assert hub.prs == {}
    out = configure(git)
    assert out["resumed"] is True and len(hub.prs[APP]) == 1 and out["paths"] == [PATH]


def test_a_pr_a_person_closed_is_not_reopened_or_replaced(git, hub):
    first = configure(git)
    hub.close(APP, first["pr"])
    out = configure(git)
    assert out["state"] == "closed" and out["pr"] == first["pr"] and "never reopens" in out["reason"] and out["paths"] == []
    assert len(hub.prs[APP]) == 1
    hub.delete_branch(APP, first["branch"])
    assert configure(git)["pr"] != first["pr"], "once a person deletes the branch, a new PR opens"


def test_a_change_that_was_merged_and_has_since_been_undone_is_not_silently_reported_done(git, hub):
    first = configure(git)
    hub.merge(APP, first["pr"])
    assert configure(git)["state"] == "noop", "merged and present: nothing left to do"
    hub.repos[APP]["main"][PATH] = DEV     # a person removed it again
    e = refused(git)
    assert e.rule == "CONFIGURE-SUPERSEDED" and f"#{first['pr']}" in e.message


# -- what the tool can send ----------------------------------------------------------------------------------------------
def test_a_whole_call_sends_only_gets_and_the_prs_own_posts_and_writes_one_file_in_one_repo(git, hub):
    out = configure(git)
    again = configure(git)
    wrote = {re.sub(r"https://api\.github\.test/repos/[^/]+/[^/]+", "", u) for m, u, _ in hub.reqs if m == "POST" and "/repos/" in u}
    assert wrote == {"/git/trees", "/git/commits", "/git/refs", "/pulls"}
    assert all(m in ("GET", "POST") for m, _, _ in hub.reqs)
    posts = [u for m, u, _ in hub.reqs if m == "POST" and "/repos/" in u]
    assert all("jfillman/parachute/" in u for u in posts), "no other repository is written"
    trees = [b for m, u, b in hub.reqs if m == "POST" and u.endswith("/git/trees")]
    assert len(trees) == 1 and [e["path"] for e in trees[0]["tree"]] == [PATH]
    refs = [b["ref"] for m, u, b in hub.reqs if u.endswith("/git/refs")]
    assert refs == [f"refs/heads/{out['branch']}"] and again["resumed"], "no push to an existing branch"


def test_the_configure_code_has_no_way_to_merge():
    src = (airframe_apply.__file__)
    text = open(src).read()
    assert not re.search(r'"(PUT|PATCH|DELETE)"|/merge|"force"|force_push|auto_merge|enable_auto', text)


# -- through the Clearance core: scope, tier, receipt ----------------------------------------------------------------------
def test_the_core_scopes_it_by_the_app_argument_alone_R023():
    args = {"app": "parachute", "env": "dev", "values": {"slos": [SLO]}}
    profile = {"airframe": {"apps": ["parachute"]}}
    assert core_decide("airframe.configure", args, profile)["decision"]
    out = core_decide("airframe.configure", {**args, "app": "flight-api"}, profile)
    assert not out["decision"] and out["context"]["rule"] == "R023"
    out = core_decide("airframe.configure", args, {"airframe": {"apps": []}})
    assert not out["decision"] and out["context"]["rule"] == "R023"
    assert core_decide("airframe.configure", args, {"airframe": {"apps": ["para*"]}})["decision"]
    out = core_decide("airframe.configure", {k: v for k, v in args.items() if k != "app"}, profile)
    assert not out["decision"], "no app named is no call"


def test_the_core_does_not_read_proposeFlight_for_it():
    """configure has no Flight half: R025 is about an AppSpec's plan, and a proposeFlight argument is not one of its arguments."""
    args = {"app": "parachute", "env": "dev", "values": {"slos": [SLO]}, "proposeFlight": True}
    out = core_decide("airframe.configure", args, {"airframe": {"apps": ["parachute"]}})
    assert out["context"].get("rule") != "R025"


def test_the_tool_is_a_t1_pr_tool_with_apply_s_receipt():
    spec = SPEC_OF_TOOL
    assert int(spec.tier) == 1 and spec.target == "lower" and spec.github_cost > 0 and not spec.writes_files
    assert spec.required_args == ("app", "env", "values")
    assert "airframe.configure" in json.load(open(airframe_apply.__file__.replace("src/autopilot/airframe_apply.py", "policy/hangar/bundle.json")))["pr_tools"]
