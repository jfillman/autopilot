"""Hangar's policy bundle (policy/hangar/bundle.json) and the names Hangar passes the core (policy/hangar/core-env.json),
which the Clearance core loads from configuration since v0.29.0 (R-5). The tool registry is the gateway's
(autopilot.hangar_tools): the bundle names the same tools with the same tiers and arguments, and the core-env names
the same vocabulary the gateway itself uses. And the Hangar deployment decides exactly as before: Clearance's golden
corpus, recorded from the Python engine with the Hangar pack, replays through the core with this bundle."""
import dataclasses
import json
import os
import shutil
import subprocess
from pathlib import Path

import pytest

from autopilot import manifests
from autopilot.engine import dispatch, profile
from autopilot.hangar_tools import TOOLS
from coreenv import HANGAR_BUNDLE, HANGAR_CORE_ENV

BUNDLE = json.loads(HANGAR_BUNDLE.read_text())


def test_the_bundle_governs_exactly_the_gateways_hangar_tools():
    def spec(t):
        d = dataclasses.asdict(t)
        d.pop("extra")
        d["tier"] = int(t.tier)
        d["required_args"], d["receipt"] = list(t.required_args), list(t.receipt)
        return d
    assert BUNDLE["tools"] == [spec(t) for t in TOOLS]


def test_the_core_env_is_the_gateways_own_vocabulary():
    env = HANGAR_CORE_ENV
    assert env["CLEARANCE_BASELINE_DENY_PATHS"].split(",") == list(profile.BASELINE_DENY_PATHS)
    assert env["CLEARANCE_BASELINE_DENY_FIELDS"].split(",") == list(profile.BASELINE_DENY_FIELDS)
    assert env["CLEARANCE_RUN_IDENTITY_NAMESPACE"] == manifests.RUN_NAMESPACE
    assert set(env["CLEARANCE_ENV_LOWER_NAMES"].split(",")) == set(dispatch.LOWER_ENVS)
    # env_tier_of: the gateway's record `target` and the core's R012 read arguments the same way.
    spec = next(t for t in TOOLS if t.name == "glidepath.run.rerun")
    for args, want in (({"project": "x-lower"}, "lower"), ({"env": "pr-7"}, "lower"), ({"env": "agent-x"}, "lower"),
                       ({"env": "prod"}, "upper"), ({"repo": "o/gitops-x"}, "upper"), ({"repo": "o/x"}, "lower")):
        assert dispatch.env_tier_of(spec, args) == want, args
    assert env["CLEARANCE_ENV_LOWER_PREFIXES"] == "pr-,agent-" and env["CLEARANCE_ENV_LOWER_PROJECT_SUFFIXES"] == "-lower"
    assert env["CLEARANCE_ENV_UPPER_REPO_PREFIXES"] == "gitops-"
    src = (Path(__file__).resolve().parents[1] / "src" / "autopilot" / "github_pr.py").read_text()
    assert f'\\n{env["CLEARANCE_TRAILER_PREFIX"]}{{k.title()}}' in src, "the trailers the PR adapter writes"


def _clearance_src() -> Path | None:
    for cand in (os.environ.get("CLEARANCE_SRC"),
                 os.path.join(os.environ["HANGAR_TECH"], "clearance") if os.environ.get("HANGAR_TECH") else None,
                 str(Path(__file__).resolve().parents[2] / "clearance")):
        if cand and (Path(cand) / "internal" / "decide" / "golden").is_dir():
            return Path(cand)
    return None


def test_the_corpus_replays_identically_with_this_bundle():
    """Every decision of Clearance's golden corpus (the Python engine with the Hangar pack, over its whole test suite
    and the fixture agents' calls): same decision, rule id, message, hint, tier, problems, spend and record fields,
    with autopilot's bundle in place of the clearance test fixture (CLEARANCE_GOLDEN_BUNDLE)."""
    src = _clearance_src()
    if src is None or shutil.which("go") is None:
        pytest.skip("no clearance checkout with the golden corpus, or no Go toolchain")
    out = subprocess.run(["go", "test", "-count=1", "-v", "-run", "TestBundleMatchesPython|TestCorpusReplays|TestCorpusCoverage",
                          "./internal/decide/golden/"], cwd=src, capture_output=True, text=True,
                         env={**os.environ, "CLEARANCE_GOLDEN_BUNDLE": str(HANGAR_BUNDLE)})
    assert out.returncode == 0, out.stdout[-4000:] + out.stderr[-2000:]
    assert "--- PASS: TestCorpusReplays" in out.stdout and "replayed" in out.stdout, out.stdout[-2000:]
