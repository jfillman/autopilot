from pathlib import Path

import jsonschema
import pytest
import yaml

from clearance import manifests

ROOT = Path(__file__).resolve().parents[1]
XRD = yaml.safe_load((ROOT / "airframe-drafts" / "xrds" / "agentrun.yaml").read_text())
SCHEMA = XRD["spec"]["versions"][0]["schema"]["openAPIV3Schema"]


def validate(m):
    jsonschema.validate(m, SCHEMA)


def make(gw, defs, agent="coding-agent", claim=None):
    sid = gw.open_session("alice", agent, claim).data["session"]
    return gw.store.get(sid), defs[agent]


@pytest.mark.parametrize("agent", ["coding-agent", "researcher", "orchestrator"])
def test_manifest_satisfies_the_xrd_schema(gw, defs, agent):
    s, d = make(gw, defs, agent)
    m = manifests.agentrun_manifest(s, d)
    validate({"spec": m["spec"]})
    assert m["metadata"]["namespace"] == "autopilot-runs" and m["kind"] == "AgentRun"


def test_manifest_carries_the_narrowed_limits_not_the_definitions(gw, defs):
    s, d = make(gw, defs, claim={"tool_calls": 7, "network": "clearance", "compute": "small"})
    spec = manifests.agentrun_manifest(s, d)["spec"]
    assert spec["limits"]["toolCalls"] == 7 and spec["network"]["mode"] == "clearance"
    assert spec["expiresAt"].endswith("Z")


def test_labels_carry_task_and_session(gw, defs):
    s, d = make(gw, defs)
    lab = manifests.agentrun_manifest(s, d)["metadata"]["labels"]
    assert lab["hangar.io/task"] == s.task_id and lab["hangar.io/session"] == s.id


def test_a_definition_without_an_image_cannot_run(gw, defs):
    import dataclasses
    s, d = make(gw, defs)
    with pytest.raises(ValueError):
        manifests.agentrun_manifest(s, dataclasses.replace(d, image=None))


def test_xrd_rejects_a_tag_instead_of_a_digest(gw, defs):
    s, d = make(gw, defs)
    m = manifests.agentrun_manifest(s, d, image="ghcr.io/x/y:latest")
    with pytest.raises(jsonschema.ValidationError):
        validate({"spec": m["spec"]})
