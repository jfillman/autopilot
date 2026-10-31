"""runtime/catalog.yaml: parses (Clearance runtimes.py), every available runtime is pinned and is what the platform's
own agents run, and the gateway allows exactly its images plus RUNTIME_IMAGES_EXTRA."""
from clearance.dispatch import Principal
from clearance.tiers import Tier

from autopilot import serve
from autopilot.backends import Backends, FakeRuns


class Auth:
    def authenticate(self, token):
        return Principal("app-x-cicd/pipeline-runner", "workload", Tier.T1)


def test_the_catalog_parses_and_the_platform_agents_use_it():
    cat = serve.load_runtimes()
    names = {r.name: r for r in cat}
    assert {"generic", "base-python"} <= set(names) and names["generic"].status == "available"
    import yaml
    images = {yaml.safe_load(f.read_text()).get("runtime", {}).get("image") for f in serve.REPO_AGENTS.glob("*.yaml")} - {None}
    available = {r.image for r in cat if r.status == "available"}
    assert images, "the platform's agents pin images"
    assert images <= available | {i for i in images if "agent-triage" in i or "@sha256:0000" in i}


def test_the_gateway_allows_catalog_and_extra_images(clock):
    cfg = serve.config_from_env({"CLEARANCE_EPHEMERAL": "1", "AIRFRAME_ENABLED": "0", "ARGO_ENABLED": "0",
                                 "CLEARANCE_VERIFIERS": "app-*-cicd/pipeline-runner", "RUNTIME_IMAGES_EXTRA": "ghcr.io/jfillman/flight/*@sha256:*"})
    g, _ = serve.build_gateway(cfg, backends=Backends(None, None, FakeRuns(), None, None), auth=Auth(), clock=clock)
    generic = next(r.image for r in g.runtimes if r.name == "generic")
    assert generic in g.runtime_images and "ghcr.io/jfillman/flight/*@sha256:*" in g.runtime_images
