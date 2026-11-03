"""The runtime catalog: the agent images a deployment offers, by name (Autopilot's runtime/catalog.yaml on Hangar).

Clearance does not build or own images; a deployment hands it a list, and Clearance uses it twice: as the allowlist a
definition's runtime.image is checked against (review.validate, with the deployment's extra globs for images built
for an agent's own repository), and as GET /v1/runtimes for the screens that let a person pick one. An entry:

    name: generic                 # a short name, unique
    image: ghcr.io/o/r@sha256:..  # pinned by digest
    kind: runtime | base          # runtime: runs a prompt-only agent as is; base: a FROM image for a code agent
    framework: generic            # the definition's runtime.framework it implies
    summary: one line
    status: available | planned   # planned entries are listed, never allowed
"""
from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Any, Iterable

DIGEST = re.compile(r"^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$")
NAME = re.compile(r"^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$")
KINDS = ("runtime", "base")
STATUSES = ("available", "planned")


@dataclass(frozen=True)
class Runtime:
    name: str
    image: str | None
    kind: str
    framework: str
    summary: str
    status: str

    def view(self) -> dict[str, Any]:
        return {"name": self.name, "image": self.image, "kind": self.kind, "framework": self.framework,
                "summary": self.summary, "status": self.status}


def load(entries: Iterable[Any]) -> tuple[Runtime, ...]:
    """Parse a catalog; a malformed entry is a configuration error (ValueError), never skipped."""
    out: list[Runtime] = []
    seen: set[str] = set()
    for e in entries or ():
        if not isinstance(e, dict):
            raise ValueError("a runtime catalog entry is a mapping")
        name, status, kind = e.get("name"), e.get("status", "available"), e.get("kind", "runtime")
        if not isinstance(name, str) or not NAME.match(name) or name in seen:
            raise ValueError(f"runtime catalog: bad or duplicate name {name!r}")
        if status not in STATUSES or kind not in KINDS:
            raise ValueError(f"runtime catalog: {name}: status is {STATUSES}, kind is {KINDS}")
        image = e.get("image")
        if status == "available" and (not isinstance(image, str) or not DIGEST.match(image)):
            raise ValueError(f"runtime catalog: {name}: an available runtime is pinned by digest")
        seen.add(name)
        out.append(Runtime(name, image if isinstance(image, str) else None, kind, str(e.get("framework") or "generic"),
                           str(e.get("summary") or ""), status))
    return tuple(out)


def allowed_images(runtimes: Iterable[Runtime]) -> tuple[str, ...]:
    """The exact images a definition may pin from the catalog: available runtimes (base images are FROM images, not
    something a definition runs)."""
    return tuple(r.image for r in runtimes if r.status == "available" and r.kind == "runtime" and r.image)
