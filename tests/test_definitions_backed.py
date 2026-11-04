"""A-7's done bar: every bundled definition runs on an image that calls finish (run_finish), no tool a loaded
definition names lacks a backend in a fully configured Hangar deployment, and every Dockerfile's base is pinned by
digest. A definition that names an unbacked tool (chat.*, or airframe.validate without a validator in the image)
fails here, not on a live run."""
import re
from pathlib import Path

import pytest
import yaml
from coreenv import core_env

from autopilot import serve
from autopilot.engine import profile

ROOT = Path(__file__).resolve().parents[1]
BUNDLED = profile.load_dir(ROOT / "agents")
SKYPORT = profile.load_dir(ROOT / "agents" / "skyport")
# What Hangar's gateway on hangar-dev configures (apron 55-autopilot gateway.yaml), plus APP_API_SERVICES, which
# only the Skyport definitions (not loaded today) need.
FULL = {"CLEARANCE_EPHEMERAL": "1", **core_env(), "GLIDEPATH_API_URL": "http://glidepath-api:8080",
        "ARGO_APP_NAMESPACES": "argocd,argocd-apps", "ARGO_SYNC_ENABLED": "1", "PROMETHEUS_URL": "http://thanos:9090",
        "LOKI_URL": "http://loki", "BACKSTAGE_URL": "http://backstage", "GITHUB_PR_ENABLED": "1",
        "APP_API_SERVICES": '{"flight-api": "http://flight-api:8080", "boarding-api": "http://boarding-api:8080", '
                            '"baggage-api": "http://baggage-api:8080"}'}
# The gateway's own tools: dispatch handles them, not a backend handler.
OWN = {"run.spawn", "run.close", "human.request", "human.request.get"}


@pytest.fixture(scope="module")
def backed():
    b = serve.build_backends(serve.config_from_env(FULL))
    # artifact.*: on Postgres in a deployment (build_gateway puts PgArtifacts on the store's connection); this test's
    # store is in memory, so stand that in.
    from autopilot.backends import FakeArtifacts
    b.artifacts = FakeArtifacts()
    return set(serve.build_handlers(b)) | OWN


@pytest.mark.parametrize("name", sorted(BUNDLED) + [f"skyport/{n}" for n in sorted(SKYPORT)])
def test_no_tool_in_a_definition_lacks_a_backend(backed, name):
    d = (SKYPORT if name.startswith("skyport/") else BUNDLED)[name.removeprefix("skyport/")]
    assert not sorted(d.limits.tools - backed), f"{name} names tools with no backend on Hangar"


def test_every_bundled_definition_runs_on_a_catalog_runtime_that_calls_finish():
    """Every `runtime` image in the catalog is built from runtime/ and hands its result back with run_finish
    (hangar_agent.main.run_pod); a bundled definition must pin one of them, by digest."""
    runtimes = {r.image for r in serve.load_runtimes() if r.kind == "runtime" and r.status == "available"}
    for name, d in sorted({**BUNDLED, **SKYPORT}.items()):
        assert d.image in runtimes, f"{name} pins {d.image}, not a catalog runtime"
    main = (ROOT / "runtime" / "src" / "hangar_agent" / "main.py").read_text()
    assert "gateway.finish(" in main


def _dockerfiles():
    """This repo's own Dockerfiles: git's tracked files (CI checks other repos out under tech/, which are not ours)."""
    import subprocess
    out = subprocess.run(["git", "-C", str(ROOT), "ls-files", "*Dockerfile*"], capture_output=True, text=True, check=True)
    return sorted(ROOT / f for f in out.stdout.split())


DOCKERFILES = _dockerfiles()


@pytest.mark.parametrize("path", DOCKERFILES, ids=lambda p: str(p.relative_to(ROOT)))
def test_every_dockerfile_base_is_pinned_by_digest(path):
    assert len(DOCKERFILES) >= 5
    stages = set()
    for line in path.read_text().splitlines():
        m = re.match(r"^\s*FROM\s+(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?", line, re.I)
        if not m:
            continue
        image, alias = m.group(1), m.group(2)
        if image not in stages:
            assert re.search(r"@sha256:[0-9a-f]{64}$", image), f"{path.name}: FROM {image} is not pinned by digest"
        if alias:
            stages.add(alias)


def test_ci_service_images_are_pinned_by_digest():
    wf = yaml.safe_load((ROOT / ".github" / "workflows" / "test.yaml").read_text())
    images = [s["image"] for j in wf["jobs"].values() for s in (j.get("services") or {}).values()]
    assert images and all(re.search(r"@sha256:[0-9a-f]{64}$", i) for i in images), images
