"""Agent XRs become Clearance definitions (Autopilot M4). The loader behind clearance's definitions.py.

An `Agent` XR (airframe xrds/agent.yaml, in an app's tenants/<app>/xr-requests/) pins one commit of a definition file
in the app's own repository. Every AGENT_XRS_INTERVAL seconds (60) this lists the XRs through the gateway's allowlisted
Kubernetes reader (GET /apis/catalog.hangar.io/v1alpha1/agents, cluster-wide: the clearance identity may list every
catalog.hangar.io kind), fetches each pinned file through the agents' GitHub App (GitHubPRs.read_file: a per-call token
from Glidepath's broker, which allows an onboarded app's own repositories and nothing else), and hands the documents to
Gateway.apply_cluster_definitions, which decides (definitions.from_pin), records each change once as evidence and
persists the set for clearance-api.

What is fetched when: a pin names a commit, so a file is fetched once per (repo, path, commit) and kept; a poll that
finds the same pins makes no GitHub call. On start the cache is seeded from the gateway's store (the rows the last
process wrote), so a GitHub outage at restart drops no agent. A pin whose file cannot be fetched for a reason that will
not change (no such file at that commit, not a text file, not YAML, the broker refusing the repository) is rejected and
the reason kept; a transient failure (GitHub or the broker unreachable) skips the pin this poll and keeps whatever was
loaded before, so an outage never unloads an agent. A Kubernetes list failure keeps the whole current set.
"""
from __future__ import annotations

import logging
import threading
from datetime import datetime, timezone
from typing import Any, Callable

import yaml
from clearance import definitions as defn
from clearance.dispatch import Denied

from .airframe_api import AirframeError, KubeReader
from .github_pr import GitHubError, GitHubPRs

log = logging.getLogger("autopilot.agent_xrs")

AGENTS_PATH = "/apis/catalog.hangar.io/v1alpha1/agents"
DEFAULT_INTERVAL = 60.0


class Skip(Exception):
    """A transient failure: the pin is neither loaded nor rejected this poll."""


class AgentXRs:
    """The poller. `gw` is the gateway (its defs a definitions.Definitions); `reader` lists the XRs; `github` reads the
    pinned files. Tests pass fakes for both and call poll() directly."""

    def __init__(self, gw: Any, reader: KubeReader, github: GitHubPRs, *, interval: float = DEFAULT_INTERVAL,
                 clock: Callable[[], datetime] = lambda: datetime.now(timezone.utc)):
        self.gw, self.reader, self.github, self.interval, self.now = gw, reader, github, interval, clock
        # (repo, path, commit) -> (document or None, reason or None, loaded_at ISO). A pin is immutable, so is this.
        self._cache: dict[tuple[str, str, str], tuple[dict | None, str | None, str]] = {}
        self.fetches = 0
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._loop, name="clearance-agent-xrs", daemon=True)

    # -- lifecycle --------------------------------------------------------------------------------------------
    def start(self) -> "AgentXRs":
        self.seed()
        self._thread.start()
        return self

    def stop(self) -> None:
        self._stop.set()
        self._thread.join(timeout=self.interval + 5)

    def _loop(self) -> None:
        while True:
            try:
                self.poll()
            except Exception as e:  # noqa: BLE001 - one bad poll must not end the thread
                log.error("agent pins: poll failed: %s", type(e).__name__)
            if self._stop.wait(self.interval):
                return

    def seed(self) -> int:
        """Warm the cache from the gateway's store: the documents the last process loaded, by their pins."""
        store = getattr(self.gw, "definitions_store", None)
        if store is None:
            return 0
        n = 0
        try:
            rows = store.all()
        except Exception as e:  # noqa: BLE001 - a cold cache is the only cost
            log.warning("agent pins: could not read the stored set: %s", type(e).__name__)
            return 0
        for r in rows:
            s = r.get("source") or {}
            key = (s.get("repo"), s.get("path"), s.get("commit"))
            if r.get("state") == "loaded" and all(key) and isinstance(r.get("doc"), dict):
                self._cache[key] = (r["doc"], None, s.get("loaded_at") or self._iso())
                n += 1
        return n

    # -- one poll ---------------------------------------------------------------------------------------------
    def poll(self) -> defn.Changes:
        try:
            items = self.reader.list(AGENTS_PATH)
        except (AirframeError, Denied) as e:
            log.warning("agent pins: could not list Agent XRs (%s); keeping the current set", type(e).__name__)
            return defn.Changes()
        loaded: list[defn.Loaded] = []
        rejected: list[defn.Rejected] = []
        skipped = 0
        for xr in items:
            meta, spec = xr.get("metadata") or {}, xr.get("spec") or {}
            src_spec = spec.get("source") or {}
            source = defn.Source("cluster", repo=str(src_spec.get("repo") or ""), path=str(src_spec.get("path") or ""),
                                 commit=str(src_spec.get("commit") or ""), namespace=meta.get("namespace"),
                                 xr=meta.get("name"), loaded_at=None)
            if not (source.repo and source.path and source.commit and source.xr):
                rejected.append(defn.Rejected(source.xr or "?", source, "the XR has no complete spec.source (repo, path, commit)"))
                continue
            try:
                doc, reason, at = self._fetch(source)
            except Skip:
                skipped += 1
                continue
            source = defn.Source(**{**source.__dict__, "loaded_at": at})
            if reason is not None:
                rejected.append(defn.Rejected(source.xr, source, reason))
                continue
            out = defn.from_pin(doc, source, owner=str(spec.get("owner") or ""), reserved=self.gw.defs.bundled)
            (loaded if isinstance(out, defn.Loaded) else rejected).append(out)
        if skipped and not loaded and not rejected and items:
            # Everything unreachable (GitHub or the broker down): a poll that would empty the set is not applied.
            log.warning("agent pins: %d pin(s) unreachable; keeping the current set", skipped)
            return defn.Changes()
        if skipped:
            # The unreachable pins keep their last state: re-add what is loaded for them so the swap drops nothing.
            seen = {l.name for l in loaded} | {r.name for r in rejected}
            for name in list(self.gw.defs):
                s = self.gw.defs.source(name)
                if s is not None and s.kind == "cluster" and name not in seen:
                    key = (s.repo, s.path, s.commit)
                    doc = (self._cache.get(key) or (None, None, None))[0]
                    if doc is not None:
                        loaded.append(defn.Loaded(self.gw.defs[name], s, doc))
        ch = self.gw.apply_cluster_definitions(loaded, rejected)
        if ch:
            log.info("agent pins: +%d ~%d -%d rejected %d cleared %d (now %d cluster definition(s))",
                     len(ch.added), len(ch.changed), len(ch.removed), len(ch.rejected), len(ch.cleared),
                     sum(1 for n in self.gw.defs if (self.gw.defs.source(n) or defn.Source("bundled")).kind == "cluster"))
        return ch

    def _iso(self) -> str:
        return self.now().isoformat().replace("+00:00", "Z")

    def _fetch(self, s: defn.Source) -> tuple[dict | None, str | None, str]:
        key = (s.repo, s.path, s.commit)
        hit = self._cache.get(key)
        if hit is not None:
            return hit
        try:
            r = self.github.read_file(s.repo, s.path, ref=s.commit)
        except Denied as e:
            # The broker's rule (not an onboarded app's own repository) or a malformed name: final for this pin.
            out = (None, f"{e.rule}: {e.message}", self._iso())
        except GitHubError as e:
            log.warning("agent pins: %s@%s %s unreachable this poll: %s", s.repo, s.commit[:7], s.path, e)
            raise Skip() from e
        else:
            out = self._decide(r, s)
        self.fetches += 1
        self._cache[key] = out
        return out

    def _decide(self, r: dict[str, Any], s: defn.Source) -> tuple[dict | None, str | None, str]:
        at = self._iso()
        if r.get("found") is False:
            return None, f"no file {s.path} in {s.repo} at {s.commit[:7]}", at
        if r.get("type") != "file":
            return None, f"{s.path} in {s.repo} at {s.commit[:7]} is a {r.get('type', 'non-file')}, not a file", at
        if r.get("content") is None:
            return None, f"{s.path} in {s.repo} at {s.commit[:7]} is {r.get('note') or 'not text'}", at
        try:
            doc = yaml.safe_load(r["content"])
        except yaml.YAMLError as e:
            return None, f"{s.path} is not valid YAML: {str(e).splitlines()[0][:160]}", at
        if not isinstance(doc, dict):
            return None, f"{s.path} is not a YAML mapping (an agent definition)", at
        return doc, None, at


class DefinitionsRefresher:
    """clearance-api's side: every `interval` seconds, rebuild the cluster set from the rows the gateway persisted
    (api.ReadStore.definitions -> definitions.Definitions.apply_records). Fetches nothing, decides nothing."""

    def __init__(self, defs: defn.Definitions, store: Any, interval: float = 30.0):
        self.defs, self.store, self.interval = defs, store, interval
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._loop, name="clearance-api-definitions", daemon=True)

    def refresh(self) -> defn.Changes:
        return self.defs.apply_records(self.store.definitions())

    def start(self) -> "DefinitionsRefresher":
        try:
            ch = self.refresh()
            if ch:
                log.info("definitions: %d cluster definition(s), %d rejected pin(s) from the store",
                         len(ch.added), len(ch.rejected))
        except Exception as e:  # noqa: BLE001 - the next tick retries; /v1/agents shows the bundled set meanwhile
            log.warning("definitions: first refresh failed: %s", type(e).__name__)
        self._thread.start()
        return self

    def stop(self) -> None:
        self._stop.set()
        self._thread.join(timeout=self.interval + 5)

    def _loop(self) -> None:
        while not self._stop.wait(self.interval):
            try:
                self.refresh()
            except Exception as e:  # noqa: BLE001
                log.warning("definitions: refresh failed: %s", type(e).__name__)
