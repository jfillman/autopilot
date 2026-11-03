"""Live agent definitions (Autopilot M4): the platform's bundled definitions plus the ones pinned on the cluster.

A definition is still the reviewed YAML that `profile.parse` validates. What changed is where one may come from. Beside
the image's agents/ directory (the platform's own agents, bundled at build time), an `Agent` XR (airframe
xrds/agent.yaml) pins one commit of a definition file in an app's own repository. The gateway's loader (autopilot
agent_xrs.py) lists those XRs, fetches each pinned file through the agents' GitHub App, and hands the documents here;
this module decides whether a pin becomes a definition, and keeps the live set.

`Definitions` is the Mapping the Gateway and the read API hold. Bundled names come first and can never be shadowed: a
pin that reuses a platform agent's name is rejected and listed, not loaded. `replace_cluster` swaps the whole cluster
set at once and says what changed, so the gateway records `definition.load` / `definition.unload` /
`definition.reject` evidence once per change, never once per poll.

Lowering authority is immediate: a running session resolves its definition by name on every call (dispatch.py reads
`self.defs[s.agent]`), so a re-pin to a tighter definition applies to runs in flight. Raising it is a reviewed commit
(the pin in the tenants repo, and the file in the app's repository).

What a pin must satisfy (`from_pin`), beyond the schema:
  - the file's `name` is the XR's name (one agent, one pin, one identity)
  - the file's `owner` is the XR's `spec.owner` (the team the platform's review named is the team Clearance obeys)
  - a workload identity is `autopilot-runs/<name>` (a team cannot name its agent's identity as another's)
  - the name is not a bundled definition's
"""
from __future__ import annotations

import threading
from collections.abc import Iterator, Mapping
from dataclasses import dataclass, field
from typing import Any, Protocol

from .profile import AgentDefinition, DefinitionError, parse

RUN_IDENTITY_NAMESPACE = "autopilot-runs"


@dataclass(frozen=True)
class Source:
    """Where a definition came from. `bundled`: the image's agents/ directory. `cluster`: an Agent XR's pin."""
    kind: str
    repo: str | None = None
    path: str | None = None
    commit: str | None = None
    namespace: str | None = None   # the XR's namespace
    xr: str | None = None          # the XR's name (and the agent's)
    loaded_at: str | None = None   # when the loader fetched it (ISO)

    @property
    def pin(self) -> str:
        """One line: repo@short path, or `bundled`."""
        if self.kind != "cluster":
            return "bundled"
        return f"{self.repo}@{(self.commit or '')[:7]} {self.path}"

    def view(self) -> dict[str, Any]:
        out: dict[str, Any] = {"kind": self.kind}
        for k in ("repo", "path", "commit", "namespace", "xr", "loaded_at"):
            v = getattr(self, k)
            if v is not None:
                out[k] = v
        out["pin"] = self.pin
        return out

    @classmethod
    def from_view(cls, v: Mapping[str, Any]) -> "Source":
        return cls(kind=v.get("kind", "cluster"), repo=v.get("repo"), path=v.get("path"), commit=v.get("commit"),
                   namespace=v.get("namespace"), xr=v.get("xr"), loaded_at=v.get("loaded_at"))


@dataclass(frozen=True)
class Loaded:
    """A pin that became a definition: the parsed definition, its source, and the document it was parsed from (kept
    for the read API, which re-parses it from the store rather than fetching anything)."""
    definition: AgentDefinition
    source: Source
    doc: dict[str, Any] = field(compare=False)

    @property
    def name(self) -> str:
        return self.definition.name


@dataclass(frozen=True)
class Rejected:
    """A pin that did not become a definition, and why. Listed by the read API so the team sees it."""
    name: str
    source: Source
    reason: str

    def view(self) -> dict[str, Any]:
        return {"name": self.name, "source": self.source.view(), "reason": self.reason}


@dataclass
class Changes:
    """What one `replace_cluster` changed. Empty means the poll found the same set as before."""
    added: list[str] = field(default_factory=list)
    changed: list[str] = field(default_factory=list)
    removed: list[str] = field(default_factory=list)
    rejected: list[Rejected] = field(default_factory=list)   # rejections that were not there before
    cleared: list[Rejected] = field(default_factory=list)    # rejections that are gone (fixed, or the pin removed)
    sources: dict[str, Source] = field(default_factory=dict)  # the source of every name above (removed: the old one)

    def __bool__(self) -> bool:
        return bool(self.added or self.changed or self.removed or self.rejected or self.cleared)


def from_pin(doc: Any, source: Source, *, owner: str, reserved: Mapping[str, Any] = ()) -> Loaded | Rejected:
    """Decide one pin. `doc` is the fetched file parsed as YAML; `source.xr` is the XR's name; `owner` its spec.owner;
    `reserved` the bundled definitions. Never raises for a bad document: a bad pin is a Rejected, with the reason
    the team needs."""
    name = source.xr or "?"
    if not isinstance(doc, dict):
        return Rejected(name, source, "the pinned file is not a YAML mapping (an agent definition)")
    try:
        d = parse(doc)
    except DefinitionError as e:
        return Rejected(name, source, f"schema: {e}")
    if d.name != name:
        return Rejected(name, source, f"the file's name is {d.name!r}; a pin named {name!r} must pin a definition named {name!r}")
    if d.owner != owner:
        return Rejected(name, source, f"the file's owner is {d.owner!r}; the pin's owner is {owner!r}: they must agree")
    if d.identity_type == "workload" and d.service_account != f"{RUN_IDENTITY_NAMESPACE}/{d.name}":
        return Rejected(name, source, f"a pinned agent's workload identity is {RUN_IDENTITY_NAMESPACE}/{d.name}, not {d.service_account!r}")
    if name in reserved:
        return Rejected(name, source, f"{name!r} is a bundled platform definition; a pin cannot replace it")
    return Loaded(d, source, dict(doc))


class DefinitionsStore(Protocol):
    """Where the gateway keeps the cluster set between restarts and for the read API (pg.py PgDefinitions): the rows
    `Definitions.records()` gives, replaced whole on every change."""
    def replace(self, records: list[dict[str, Any]]) -> None: ...
    def all(self) -> list[dict[str, Any]]: ...


class MemoryDefinitions:
    """DefinitionsStore for ephemeral deployments and tests."""
    def __init__(self) -> None:
        self.rows: list[dict[str, Any]] = []

    def replace(self, records: list[dict[str, Any]]) -> None:
        self.rows = [dict(r) for r in records]

    def all(self) -> list[dict[str, Any]]:
        return [dict(r) for r in self.rows]


class Definitions(Mapping[str, AgentDefinition]):
    """The live set: bundled definitions, then cluster definitions, by name. Reads are plain dict reads of the current
    snapshot; `replace_cluster` swaps the snapshot under a lock, so a reader never sees a half-applied set."""

    def __init__(self, bundled: Mapping[str, AgentDefinition] | None = None):
        self._bundled: dict[str, AgentDefinition] = dict(bundled or {})
        self._cluster: dict[str, Loaded] = {}
        self._rejected: tuple[Rejected, ...] = ()
        self._all: dict[str, AgentDefinition] = dict(self._bundled)
        self._lock = threading.Lock()
        self.version = 0   # bumps on every change; the read API's refresher and tests watch it

    # -- Mapping ------------------------------------------------------------------------------------------------
    def __getitem__(self, name: str) -> AgentDefinition:
        return self._all[name]

    def __iter__(self) -> Iterator[str]:
        return iter(list(self._all))

    def __len__(self) -> int:
        return len(self._all)

    def __contains__(self, name: object) -> bool:
        return name in self._all

    # -- what the read API shows ---------------------------------------------------------------------------------
    @property
    def bundled(self) -> Mapping[str, AgentDefinition]:
        return self._bundled

    @property
    def rejected(self) -> tuple[Rejected, ...]:
        return self._rejected

    def source(self, name: str) -> Source | None:
        if name in self._cluster:
            return self._cluster[name].source
        if name in self._bundled:
            return Source("bundled")
        return None

    def records(self) -> list[dict[str, Any]]:
        """The cluster set as rows for the store (pg.py PgDefinitions): loaded ones with their document, rejected ones
        with their reason. Bundled definitions are not rows; every process has them from its image."""
        out = [{"name": l.name, "state": "loaded", "source": l.source.view(), "doc": l.doc}
               for l in sorted(self._cluster.values(), key=lambda l: l.name)]
        out += [{"name": r.name, "state": "rejected", "source": r.source.view(), "reason": r.reason}
                for r in self._rejected]
        return out

    # -- the loader's and the refresher's write ------------------------------------------------------------------
    def replace_cluster(self, loaded: list[Loaded] | tuple[Loaded, ...], rejected: list[Rejected] | tuple[Rejected, ...] = ()) -> Changes:
        """Make `loaded` + `rejected` the cluster set. A name that appears twice in `loaded` (two XRs, two namespaces,
        one agent name) is a rejection of both: Clearance will not guess. Returns what changed."""
        by_name: dict[str, Loaded] = {}
        dupes: set[str] = set()
        for l in loaded:
            if l.name in by_name:
                dupes.add(l.name)
            by_name[l.name] = l
        rej = list(rejected)
        for l in loaded:
            if l.name in dupes:
                rej.append(Rejected(l.name, l.source, f"two pins name the agent {l.name!r}; neither is loaded"))
        for n in dupes:
            by_name.pop(n, None)
        for n in list(by_name):
            if n in self._bundled:
                rej.append(Rejected(n, by_name[n].source, f"{n!r} is a bundled platform definition; a pin cannot replace it"))
                del by_name[n]
        rej_t = tuple(sorted(dict.fromkeys(rej), key=lambda r: (r.name, r.reason)))
        with self._lock:
            old, old_rej = self._cluster, self._rejected
            ch = Changes()
            for n, l in by_name.items():
                if n not in old:
                    ch.added.append(n)
                elif old[n].source.commit != l.source.commit or old[n].doc != l.doc:
                    ch.changed.append(n)
            ch.removed = [n for n in old if n not in by_name]
            ch.rejected = [r for r in rej_t if r not in old_rej]
            ch.cleared = [r for r in old_rej if r not in rej_t]
            ch.sources = {**{n: old[n].source for n in ch.removed}, **{n: by_name[n].source for n in ch.added + ch.changed}}
            if ch:
                self._cluster, self._rejected = dict(by_name), rej_t
                self._all = {**{n: l.definition for n, l in by_name.items()}, **self._bundled}
                self.version += 1
            return ch

    def apply_records(self, records: list[dict[str, Any]]) -> Changes:
        """The read API's side: rebuild the cluster set from the store's rows (the gateway wrote them). A row whose
        document no longer parses (a schema change between images) is shown as rejected, not dropped silently."""
        loaded, rejected = [], []
        for r in records:
            src = Source.from_view(r.get("source") or {})
            if r.get("state") == "loaded":
                try:
                    loaded.append(Loaded(parse(r["doc"]), src, dict(r["doc"])))
                except (DefinitionError, KeyError, TypeError) as e:
                    rejected.append(Rejected(r.get("name", "?"), src, f"stored definition no longer parses: {e}"))
            else:
                rejected.append(Rejected(r.get("name", "?"), src, str(r.get("reason", "rejected"))))
        return self.replace_cluster(loaded, rejected)
