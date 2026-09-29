import copy
from pathlib import Path

import pytest
import yaml

from clearance import profile
from clearance.limits import Network
from clearance.tiers import Tier

ROOT = Path(__file__).resolve().parents[1]


def doc():
    return yaml.safe_load((ROOT / "agents" / "coding-agent.yaml").read_text())


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
    assert ".tekton/**" in out.deny_paths and "cicd.yaml" in out.deny_paths and "extra/**" in out.deny_paths


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
