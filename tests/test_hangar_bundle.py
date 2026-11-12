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


def test_the_corpus_replays_identically_with_this_bundle(tmp_path):
    """Every decision of Clearance's golden corpus (the Python engine with the Hangar pack, over its whole test suite
    and the fixture agents' calls): same decision, rule id, message, hint, tier, problems, spend and record fields,
    with autopilot's bundle in place of the clearance test fixture (CLEARANCE_GOLDEN_BUNDLE)."""
    src = _clearance_src()
    if src is None or shutil.which("go") is None:
        pytest.skip("no clearance checkout with the golden corpus, or no Go toolchain")
    # The corpus froze the Python pack: its tools, rules and PR tools. TestBundleMatchesPython compares the bundle's to
    # them exactly, so a tool added since cannot be in the bundle it replays. airframe.apply (A-3) is one: a T1 PR tool.
    # Adding it changes no decision the corpus holds (it is a tool the corpus never calls, and its policy is R023's
    # app-from-spec reading), so the corpus replays on the bundle without that tool and its pr_tools name, and the
    # bundle is checked to differ from that one in nothing else.
    since_corpus = {"airframe.apply"}
    assert {t["name"] for t in BUNDLE["tools"]} - {t["name"] for t in BUNDLE["tools"] if t["name"] not in since_corpus} == since_corpus
    assert set(BUNDLE["pr_tools"]) - since_corpus == {"glidepath.cicd.propose"}
    frozen = dict(BUNDLE, tools=[t for t in BUNDLE["tools"] if t["name"] not in since_corpus],
                  pr_tools=[t for t in BUNDLE["pr_tools"] if t not in since_corpus])
    # A-11a likewise: descriptions of airframe.capabilities/plan name the appspec section, and the `section` argument
    # check admits it. The corpus holds the earlier wording and allowlist; no decision it records depends on either.
    old = {"airframe.capabilities": "What Airframe offers: chart fields, XR kinds, components and their outputs, "
                                    "conditions and verify checks (kind or section optional)",
           "airframe.plan": "Plan an AppSpec into an ordered change set (pass baseCicd, the app's current cicd.yaml)",
           # A-11b: the propose tool's description gains the human-merge note (its text before it is the corpus's)
           "glidepath.cicd.propose": "Propose a cicd.yaml change: validated by glidepath-api as an agent's change and refused "
                                     "with the GP rule when invalid, else opened as a pull request on the app's repo (repo: "
                                     "owner/<app>) that a human merges; title required, body and branch optional"}
    for t in frozen["tools"]:
        assert t["description"].startswith(old.get(t["name"], t["description"])), t["name"]
    frozen["tools"] = [dict(t, description=old.get(t["name"], t["description"])) for t in frozen["tools"]]
    # A-11b, the `propose` scope class (Glidepath contract 2): the receipts of the two PR tools gain merge and needsPerson, R025
    # and its two vars and the proposeFlight argument check are new, the bundle names contract version 2 and carries the
    # document's `propose` finding, and the Flight environment's hint names the tools that can propose it. The corpus holds none of
    # them (contract version 1 has no propose rule, no call sets proposeFlight, no definition sets it), so the corpus replays on
    # the bundle as it was, which is exactly this bundle minus those, and nothing else.
    frozen["tools"] = [dict(t, receipt=["pr", "commit", "branch"]) if t["name"] == "glidepath.cicd.propose" else t
                       for t in frozen["tools"]]
    assert [r["id"] for r in frozen["rules"] if r["id"] == "R025"] == ["R025"]
    frozen["rules"] = [r for r in frozen["rules"] if r["id"] != "R025"]
    assert {v["name"] for v in frozen["vars"]} >= {"apply_wants_flight", "flight_proposal_allowed"}
    frozen["vars"] = [v for v in frozen["vars"] if v["name"] not in ("apply_wants_flight", "flight_proposal_allowed")]
    frozen["arg_checks"] = [dict(a, checks=[c for c in a["checks"] if "proposeFlight" not in c]) for a in frozen["arg_checks"]]
    assert frozen["sources"][0]["compatible"] == [1, 2]
    frozen["sources"] = [dict(frozen["sources"][0], compatible=[1])]
    doc = dict(frozen["documents"][0])
    assert doc.pop("propose")["rule"] == "GP-SCOPE-004"
    doc["keyed"] = [dict(k, protected_change=dict(k["protected_change"], hint="Flight environments are changed by a human. "
                                                  "An agent may add or change Ground environments.")) for k in doc["keyed"]]
    frozen["documents"] = [doc]
    frozen["version"] = "1.1.0"
    text = json.dumps(frozen)
    assert "'kinds', 'appspec']" in text
    frozen_path = tmp_path / "bundle.json"
    frozen_path.write_text(text.replace("'kinds', 'appspec']", "'kinds']"))
    out = subprocess.run(["go", "test", "-count=1", "-v", "-run", "TestBundleMatchesPython|TestCorpusReplays|TestCorpusCoverage",
                          "./internal/decide/golden/"], cwd=src, capture_output=True, text=True,
                         env={**os.environ, "CLEARANCE_GOLDEN_BUNDLE": str(frozen_path)})
    assert out.returncode == 0, out.stdout[-4000:] + out.stderr[-2000:]
    assert "--- PASS: TestCorpusReplays" in out.stdout and "replayed" in out.stdout, out.stdout[-2000:]
