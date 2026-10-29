import copy
from pathlib import Path

import pytest
import yaml

from clearance import profile
from clearance.limits import Network
from clearance.tiers import Tier

AGENTS = Path(__file__).resolve().parent / "fixtures" / "agents"


def doc():
    return yaml.safe_load((AGENTS / "coding-agent.yaml").read_text())


def test_all_shipped_definitions_load(defs):
    assert set(defs) == {"coding-agent", "researcher", "orchestrator", "triage-agent", "nightly-reviewer"}
    assert defs["coding-agent"].limits.tier_ceiling == Tier.T1
    assert defs["coding-agent"].limits.network == Network.CLEARANCE_MODEL


def test_every_kind_is_represented(defs):
    assert {d.kind for d in defs.values()} >= {"task", "team", "event", "scheduled"}


def test_baseline_deny_paths_always_present_and_cannot_be_removed():
    d = doc()
    d["profile"]["repos"] = {"allow": ["jfillman/*"], "denyPaths": ["extra/**"]}
    out = profile.parse(d)
    assert ".tekton/**" in out.deny_paths and "extra/**" in out.deny_paths


def test_cicd_yaml_is_not_path_denied_it_is_field_scoped():
    """2026-10-25: cicd.yaml moved from a whole-file deny to Glidepath's field scope (policy R021,
    GP-SCOPE-*); a definition can still deny it whole with its own denyPaths."""
    d = doc()
    d["profile"]["repos"] = {"allow": ["jfillman/*"]}
    out = profile.parse(d)
    assert "cicd.yaml" not in out.deny_paths and "**/cicd.yaml" not in out.deny_paths
    d["profile"]["repos"]["denyPaths"] = ["cicd.yaml"]
    assert "cicd.yaml" in profile.parse(d).deny_paths


def test_baseline_deny_paths_cover_release_files():
    """AF-5b: an agent can never write a machine-owned release file (release.image,
    releaseTracking), even if a definition's own denyPaths omits it - D9/AF-5."""
    d = doc()
    d["profile"]["repos"] = {"allow": ["jfillman/*"]}
    out = profile.parse(d)
    assert "release.yaml" in out.deny_paths
    assert "**/release.yaml" in out.deny_paths
    assert "**/*.release.yaml" in out.deny_paths


@pytest.mark.parametrize("mutate", [
    lambda d: d.update(bogus=1),
    lambda d: d["profile"].update(tierCeiling="T3"),
    lambda d: d["profile"]["limits"].pop("openPrs"),
    lambda d: d["runtime"].update(image="ghcr.io/x/y:latest"),
    lambda d: d.update(name="Bad Name"),
    lambda d: d["identity"].update(type="workload"),
    lambda d: d["profile"].update(network="everything"),
    lambda d: d["profile"]["limits"].update(ttlMinutes=99999),
    lambda d: d["profile"]["repos"].update(allow=["not-a-repo"]),
])
def test_schema_rejects(mutate):
    d = copy.deepcopy(doc())
    d["profile"]["repos"] = {"allow": ["jfillman/*"]}
    mutate(d)
    with pytest.raises(profile.DefinitionError):
        profile.parse(d)


def test_file_name_must_match_agent_name(tmp_path):
    d = doc()
    (tmp_path / "other.yaml").write_text(yaml.safe_dump(d))
    with pytest.raises(profile.DefinitionError):
        profile.load_dir(tmp_path)


def test_a_glidepath_event_trigger_validates(tmp_path):
    import yaml
    from clearance import profile
    base = yaml.safe_load((profile_dir() / "nightly-reviewer.yaml").read_text())
    base.update(kind="event", triggers=[{"type": "event", "source": "glidepath", "bindingKey": "*.pipelinerun.finished",
                                         "match": {"subject.content.outcome": "failure"}, "maxPerHour": 6}])
    (tmp_path / "nightly-reviewer.yaml").write_text(yaml.safe_dump(base))
    d = profile.load_dir(tmp_path)[base["name"]]
    assert d.triggers[0]["match"] == {"subject.content.outcome": "failure"}
    for bad in ({"bindingKey": "Bad Key"}, {"match": {"a b": "x"}}, {"exchange": "x"}, {"match": {"a": 1}}):
        t = {"type": "event", "source": "glidepath", "bindingKey": "*.pipelinerun.finished", **bad}
        (tmp_path / "nightly-reviewer.yaml").write_text(yaml.safe_dump({**base, "triggers": [t]}))
        with pytest.raises(Exception):
            profile.load_dir(tmp_path)


def profile_dir():
    from pathlib import Path
    return Path(__file__).resolve().parent / "fixtures" / "agents"


def test_runtime_instructions(tmp_path):
    base = yaml.safe_load((profile_dir() / "nightly-reviewer.yaml").read_text())
    base["runtime"]["instructions"] = "Review every app nightly."
    (tmp_path / "nightly-reviewer.yaml").write_text(yaml.safe_dump(base))
    assert profile.load_dir(tmp_path)["nightly-reviewer"].instructions == "Review every app nightly."
    base["runtime"]["instructions"] = "x" * 1501
    (tmp_path / "nightly-reviewer.yaml").write_text(yaml.safe_dump(base))
    with pytest.raises(profile.DefinitionError):
        profile.load_dir(tmp_path)
