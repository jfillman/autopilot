"""Agent definitions: durable, schema-validated, loaded from git-managed YAML."""
from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import jsonschema
import yaml

from .limits import ComputeClass, Limits, Network
from .tiers import Tier

SCHEMA_PATH = Path(__file__).resolve().parents[2] / "schemas" / "agent-definition.schema.json"

# Always denied, for every agent, in addition to whatever a definition adds. A definition
# can extend this list but cannot shrink it: an agent must never be able to edit the
# controls that constrain it.
BASELINE_DENY_PATHS: tuple[str, ...] = (
    ".tekton/**", "**/.tekton/**", "cicd.yaml", "**/cicd.yaml",
    "CODEOWNERS", "**/CODEOWNERS", ".github/**",
    "**/appproject*.yaml", "**/AppProject*.yaml",
    # D9/AF-5 release files (release.image, releaseTracking): machine-owned, written only by
    # Glidepath's deploy/release stages - see hangar/docs/autopilot/release-file-split.md.
    "release.yaml", "**/release.yaml", "*.release.yaml", "**/*.release.yaml",
)

# AF-9a: field-level equivalent of BASELINE_DENY_PATHS - these JSON pointers (and anything
# under them) can never be the field an agent's diff touches, even inside an otherwise-writable
# file, and even if a definition's own fieldDeny omits them. Same risky fields AGENTS.md already
# names by convention (extraManifests, networkPolicy, httpRoute need a human's approval) plus the
# release-owned keys, for a file that hasn't done the AF-5 split yet.
BASELINE_DENY_FIELDS: tuple[str, ...] = (
    "/extraManifests", "/networkPolicy", "/httpRoute",
    "/release", "/releaseTracking", "/rollout/image",
)


class DefinitionError(ValueError):
    pass


@dataclass(frozen=True)
class AgentDefinition:
    name: str
    kind: str
    identity_type: str
    service_account: str | None
    limits: Limits
    repos_allow: tuple[str, ...]
    api_allow: tuple[str, ...]
    deny_paths: tuple[str, ...]
    field_allow: tuple[str, ...]
    field_deny: tuple[str, ...]
    image: str | None
    framework: str
    sandbox: str
    sidecars: tuple[dict[str, str], ...]
    triggers: tuple[dict[str, Any], ...]


def _schema() -> dict:
    return json.loads(SCHEMA_PATH.read_text())


def parse(doc: dict[str, Any]) -> AgentDefinition:
    try:
        jsonschema.validate(doc, _schema())
    except jsonschema.ValidationError as e:
        path = "/".join(str(p) for p in e.absolute_path) or "<root>"
        raise DefinitionError(f"{doc.get('name', '?')}: {path}: {e.message}") from e
    p = doc["profile"]
    lim = p["limits"]
    limits = Limits(
        tier_ceiling=Tier[p["tierCeiling"]], ttl_minutes=lim["ttlMinutes"],
        tool_calls=lim["toolCalls"], github_calls=lim["githubCalls"], open_prs=lim["openPrs"],
        model_tokens=lim["modelTokens"], children=lim["children"],
        tools=frozenset(p["tools"]), models=frozenset(p.get("models", [])),
        network=Network.parse(p.get("network", "clearance")),
        compute=ComputeClass.parse(p.get("compute", "small")),
    )
    repos = p.get("repos", {})
    extra_deny = tuple(repos.get("denyPaths", []))
    extra_field_deny = tuple(repos.get("fieldDeny", []))
    rt = doc.get("runtime", {})
    return AgentDefinition(
        name=doc["name"], kind=doc["kind"], identity_type=doc["identity"]["type"],
        service_account=doc["identity"].get("serviceAccount"), limits=limits,
        repos_allow=tuple(repos.get("allow", [])),
        api_allow=tuple(p.get("apis", {}).get("allow", [])),
        deny_paths=tuple(dict.fromkeys(BASELINE_DENY_PATHS + extra_deny)),
        field_allow=tuple(repos.get("fieldAllow", [])),
        field_deny=tuple(dict.fromkeys(BASELINE_DENY_FIELDS + extra_field_deny)),
        image=rt.get("image"), framework=rt.get("framework", "generic"),
        sandbox=rt.get("sandbox", "standard"), sidecars=tuple(rt.get("sidecars", [])),
        triggers=tuple(doc.get("triggers", [{"type": "manual"}])),
    )


def load_dir(directory: str | Path) -> dict[str, AgentDefinition]:
    out: dict[str, AgentDefinition] = {}
    for f in sorted(Path(directory).glob("*.yaml")):
        d = parse(yaml.safe_load(f.read_text()))
        if d.name in out:
            raise DefinitionError(f"duplicate agent name {d.name!r} ({f.name})")
        if f.stem != d.name:
            raise DefinitionError(f"{f.name}: file name must match agent name {d.name!r}")
        out[d.name] = d
    return out
