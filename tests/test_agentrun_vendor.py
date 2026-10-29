"""AgentRun lives in airframe (AP-A3). Autopilot keeps byte-identical copies of the two files it uses, so the canary
and the manifest tests run without an airframe checkout, and this test fails the moment airframe changes them."""
import os
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
# CI clones airframe under HANGAR_TECH (.github/workflows/test.yaml); locally it is ~/tech.
AIRFRAME = Path(os.environ.get("HANGAR_TECH", "/Users/jerf/tech")) / "airframe"
VENDORED = {
    "src/autopilot/vendor/agentrun_compose.py": "functions/function-agentrun/function/compose.py",
    "schemas/agentrun.xrd.yaml": "xrds/agentrun.yaml",
}


@pytest.mark.parametrize("vendored,upstream", sorted(VENDORED.items()))
def test_vendored_agentrun_file_matches_airframe(vendored, upstream):
    src = AIRFRAME / upstream
    assert src.exists(), f"airframe has no {upstream} at {AIRFRAME} (merged in airframe's AP-A3 PR; set HANGAR_TECH)"
    assert (ROOT / vendored).read_bytes() == src.read_bytes(), (
        f"{vendored} drifted from airframe's {upstream} - copy it over byte for byte")


def test_the_canary_renders_through_the_vendored_compose():
    from autopilot.vendor import agentrun_compose
    from importlib.util import module_from_spec, spec_from_file_location
    spec = spec_from_file_location("netpol_canary_v", ROOT / "tools" / "netpol_canary.py")
    nc = module_from_spec(spec)
    import sys
    sys.modules["netpol_canary_v"] = nc
    spec.loader.exec_module(nc)
    assert nc.compose is agentrun_compose.compose


def test_the_canary_run_namespace_is_a_run_namespace_without_the_pr_sweep_label():
    """The canary's namespaces must look like real runs (agent-r-*, hangar.io/agent-run) and, like real runs, never
    carry hangar.io/ephemeral-env, which the PR-namespace TTL sweep reaps."""
    from datetime import datetime, timezone
    import netpol_canary_v as nc
    docs = nc.render_run("r-ca0001", "clearance", "canary-svc-x", datetime(2026, 10, 28, tzinfo=timezone.utc))
    ns = docs[0]
    assert ns["kind"] == "Namespace" and ns["metadata"]["name"].startswith("agent-r-")
    assert ns["metadata"]["labels"]["hangar.io/agent-run"] == "true"
    assert all("hangar.io/ephemeral-env" not in (d["metadata"].get("labels") or {}) for d in docs)
