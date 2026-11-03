"""The seam between the gateway engine and Clearance: the only module in autopilot.engine that imports `clearance`.

Today (R-3a) every name below is the pinned clearance package (pyproject.toml), called in-process, exactly as before
the move. R-3b replaces this module, and nothing else in the engine changes shape:

  policy.Policy          Gateway.policy: build_context + evaluate decide each tool call (dispatch.Gateway.call), and
                         Policy.tools is the registry the Gateway dispatches against (Gateway.tools). R-3b: the core's
                         POST /access/v1/evaluation through clients/python `clearance_client`, the gateway applying
                         the spend it returns; the registry becomes the engine's tiers.py.
  AuditLog               Gateway.audit (and ModelProxy.audit): append writes each record, through Gateway._audit and
                         the Gateway's other self.audit.append calls (gate, provenance and definition records), and
                         ModelProxy.authorize. R-3b: the decision is recorded by the evaluation, its result by
                         POST /v1/decisions/{id}/outcome, and runtime events (session.open, run.launch, human.answer,
                         person controls) by POST /v1/records.
  gate, review,          the deployment gate, definition review and PR provenance the Gateway serves today; they stay
  provenance,            Clearance's and leave the gateway when the core serves them (R-2, R-4).
  definitions, Pack
  args_digest, iso,      the record's helpers (canonical digests and timestamps), so a record the engine builds hashes
  sha256_hex             the same as Clearance's own.

AuthError and Denied are clearance.dispatch's own classes, not copies: clearance.auth's authenticators raise that
AuthError and clearance.pg.PgArtifacts raises that Denied, and the engine must catch them (a copy would let them
escape as a 500 and a backend error). They become the engine's own when the removal PR moves them (R-3b).
"""
from __future__ import annotations

from clearance import definitions, gate, policy, provenance, review
from clearance.audit import AuditLog, args_digest, iso, sha256_hex
from clearance.dispatch import AuthError, Denied
from clearance.pack import Pack

__all__ = ["AuditLog", "AuthError", "Denied", "Pack", "args_digest", "definitions", "gate", "iso", "policy",
           "provenance", "review", "sha256_hex"]
