"""Hash-chained audit log.

Each record carries the hash of the one before it, so editing, deleting or reordering a
record breaks the chain. That proves consistency, not completeness: truncating the tail
leaves a valid chain. Completeness comes from anchoring `checkpoint()` somewhere the
writer cannot rewrite (WORM storage, the self-hosted Rekor) and comparing later.

Arguments are never stored, only their hash, so a secret that slips into a tool call
does not land in the log.
"""
from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any

GENESIS = "0" * 64


def canonical(obj: Any) -> bytes:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def sha256_hex(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def args_digest(args: Any) -> str:
    return sha256_hex(canonical(args))


@dataclass(frozen=True)
class Checkpoint:
    count: int
    head: str


class AuditLog:
    def __init__(self, path: str | Path | None = None):
        self.path = Path(path) if path else None
        self.records: list[dict[str, Any]] = []
        if self.path and self.path.exists():
            self.records = [json.loads(l) for l in self.path.read_text().splitlines() if l.strip()]
            ok, bad = verify(self.records)
            if not ok:
                raise ValueError(f"audit log {self.path} is corrupt at record {bad}")

    @property
    def head(self) -> str:
        return self.records[-1]["hash"] if self.records else GENESIS

    def append(self, event: dict[str, Any]) -> dict[str, Any]:
        if "hash" in event or "prev_hash" in event or "seq" in event:
            raise ValueError("event must not set seq/prev_hash/hash")
        rec = {"seq": len(self.records), **event, "prev_hash": self.head}
        rec["hash"] = sha256_hex(canonical(rec))
        self.records.append(rec)
        if self.path:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            with open(self.path, "a") as f:
                f.write(json.dumps(rec, sort_keys=True) + "\n")
                f.flush()
                os.fsync(f.fileno())
        return rec

    def checkpoint(self) -> Checkpoint:
        return Checkpoint(len(self.records), self.head)


def verify(records: list[dict[str, Any]]) -> tuple[bool, int | None]:
    """Return (ok, index of the first bad record)."""
    prev = GENESIS
    for i, r in enumerate(records):
        if r.get("seq") != i or r.get("prev_hash") != prev:
            return False, i
        body = {k: v for k, v in r.items() if k != "hash"}
        if sha256_hex(canonical(body)) != r.get("hash"):
            return False, i
        prev = r["hash"]
    return True, None


def verify_against(records: list[dict[str, Any]], anchored: Checkpoint) -> tuple[bool, str]:
    """Check a log against a checkpoint that was anchored earlier and elsewhere."""
    ok, bad = verify(records)
    if not ok:
        return False, f"chain broken at record {bad}"
    if len(records) < anchored.count:
        return False, f"truncated: {len(records)} records, anchored {anchored.count}"
    if records[anchored.count - 1]["hash"] != anchored.head:
        return False, "history rewritten: anchored head does not match"
    return True, "ok"
