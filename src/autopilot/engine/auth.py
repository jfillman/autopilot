"""Who is calling: authenticators that turn a bearer token into a Principal.

Interim, until agent identity comes from ID-JAG (Hangar decision D19 picks the issuer): two kinds of caller, told
apart by the token's issuer and each verified in full by its own authority.

  WorkloadAuthenticator  a Kubernetes service-account token, checked by the API server's TokenReview for the
                         `clearance` audience. Principal "<namespace>/<name>", kind "workload".
  OIDCAuthenticator      a person's OIDC ID token (Dex in the lab), checked against the issuer's JWKS: signature,
                         issuer, audience, expiry. Principal "user:<claim>", kind "user".
  GitHubActionsAuthenticator  a GitHub Actions OIDC ID token (a workflow's `id-token: write`), checked against
                         GitHub's JWKS. Principal "github:<sub>", kind "workload", with the token's `repository`
                         as a verified attribute (the gate binds such a caller to its own repository).
  RoutingAuthenticator   reads the token's `iss` without trusting it, only to choose which of the two verifies it.
  CachingAuthenticator   remembers a verified token for a short time (never past its expiry, never a failure), so a
                         session's calls cost one TokenReview a minute rather than one each.

Every failure is AuthError with no detail about the token: a caller learns it was refused, not why.
"""
from __future__ import annotations

import hashlib
import json
import re
import ssl
import threading
import time
import urllib.request
from pathlib import Path
from fnmatch import fnmatchcase
from typing import Any, Callable, Mapping, Sequence

import jwt

from .dispatch import AuthError, Authenticator, Principal
from .tiers import Tier

SA_DIR = Path("/var/run/secrets/kubernetes.io/serviceaccount")
SA_PREFIX = "system:serviceaccount:"


def _unverified_claims(token: str) -> dict[str, Any]:
    try:
        return jwt.decode(token, options={"verify_signature": False})
    except jwt.PyJWTError as e:
        raise AuthError("not a JWT") from e


class WorkloadAuthenticator:
    """A service-account token, verified by TokenReview. Clearance's own identity needs `create` on tokenreviews
    (the system:auth-delegator ClusterRole). `opener` is injectable for tests; the token and CA files are re-read
    every call, because the kubelet rotates the projected token."""

    def __init__(self, audience: str = "clearance", api: str = "https://kubernetes.default.svc",
                 token_file: Path = SA_DIR / "token", ca_file: Path = SA_DIR / "ca.crt",
                 tier_ceiling: Tier = Tier.T1, opener: Callable[..., Any] | None = None, timeout: float = 10):
        self.audience, self.api, self.token_file, self.ca_file = audience, api.rstrip("/"), token_file, ca_file
        self.tier_ceiling, self.timeout, self._opener = tier_ceiling, timeout, opener

    def _post(self, body: dict) -> dict:
        req = urllib.request.Request(
            f"{self.api}/apis/authentication.k8s.io/v1/tokenreviews", method="POST",
            data=json.dumps(body).encode(),
            headers={"Authorization": f"Bearer {self.token_file.read_text().strip()}",
                     "Content-Type": "application/json", "Accept": "application/json"})
        if self._opener is not None:
            return self._opener(req, self.timeout)
        ctx = ssl.create_default_context(cafile=str(self.ca_file))
        with urllib.request.urlopen(req, context=ctx, timeout=self.timeout) as r:
            return json.load(r)

    def authenticate(self, token: str) -> Principal:
        try:
            review = self._post({"apiVersion": "authentication.k8s.io/v1", "kind": "TokenReview",
                                 "spec": {"token": token, "audiences": [self.audience]}})
        except Exception as e:  # noqa: BLE001 - the API server unreachable is a refusal, never a pass
            raise AuthError("token review failed") from e
        status = review.get("status") or {}
        user = status.get("user") or {}
        name = user.get("username", "")
        if not status.get("authenticated") or self.audience not in (status.get("audiences") or []):
            raise AuthError("token not authenticated for this audience")
        if not name.startswith(SA_PREFIX) or name.count(":") != 3:
            raise AuthError("not a service-account token")
        ns, sa = name[len(SA_PREFIX):].split(":")
        return Principal(f"{ns}/{sa}", "workload", self.tier_ceiling)


class JWKS:
    """An issuer's signing keys, fetched from its JWKS URL and refreshed when a token names a key not seen yet
    (at most once a minute, so a stream of bad kids can't hammer the issuer)."""

    def __init__(self, url: str, fetch: Callable[[str], dict] | None = None, min_refresh: float = 60,
                 clock: Callable[[], float] = time.monotonic):
        self.url, self.min_refresh, self.clock = url, min_refresh, clock
        self._fetch = fetch or self._get
        self._keys: dict[str, Any] = {}
        self._fetched_at: float | None = None
        self._lock = threading.Lock()

    @staticmethod
    def _get(url: str) -> dict:
        with urllib.request.urlopen(url, timeout=10) as r:
            return json.load(r)

    def key(self, kid: str | None) -> Any:
        with self._lock:
            if kid not in self._keys and (self._fetched_at is None
                                          or self.clock() - self._fetched_at >= self.min_refresh):
                self._fetched_at = self.clock()
                self._keys = {k.get("kid"): jwt.PyJWK(k).key for k in self._fetch(self.url).get("keys", [])
                              if k.get("use", "sig") == "sig"}
            if kid not in self._keys:
                raise AuthError("unknown signing key")
            return self._keys[kid]


class OIDCAuthenticator:
    """A person's ID token: signed by the issuer (RS256 or ES256 only, never `none` or HMAC), for this audience, not
    expired. The principal is `user:` + the configured claim (email by default, which Dex sets from the connector).

    `allowed` names who may sign in at all (globs on the claim's value): an issuer that federates a public identity
    provider (Dex with a GitHub connector) authenticates anyone with an account there, so a valid token alone is not
    a grant. Empty allows nobody."""

    ALGORITHMS = ("RS256", "ES256")

    def __init__(self, issuer: str, audience: str, keys: JWKS, user_claim: str = "email",
                 tier_ceiling: Tier = Tier.T1, leeway: float = 30, allowed: Sequence[str] = ()):
        self.issuer, self.audience, self.keys, self.user_claim = issuer, audience, keys, user_claim
        self.tier_ceiling, self.leeway, self.allowed = tier_ceiling, leeway, tuple(allowed)

    def authenticate(self, token: str) -> Principal:
        try:
            header = jwt.get_unverified_header(token)
            claims = jwt.decode(token, self.keys.key(header.get("kid")), algorithms=list(self.ALGORITHMS),
                                audience=self.audience, issuer=self.issuer, leeway=self.leeway,
                                options={"require": ["exp", "iat", "iss", "aud", "sub"]})
        except AuthError:
            raise
        except jwt.PyJWTError as e:
            raise AuthError("ID token rejected") from e
        who = claims.get(self.user_claim)
        if not isinstance(who, str) or not who:
            raise AuthError(f"ID token has no {self.user_claim}")
        if self.user_claim == "email" and claims.get("email_verified") is False:
            raise AuthError("email not verified")
        if not any(fnmatchcase(who, g) for g in self.allowed):
            raise AuthError(f"{self.user_claim} is not allowed to sign in to this Clearance")
        return Principal(f"user:{who}", "user", self.tier_ceiling)


class ForeignWorkloadAuthenticator:
    """A service-account token from ANOTHER cluster, verified against that cluster's public issuer (its JWKS), since
    this cluster's TokenReview cannot. The principal is `<name>/<namespace>/<serviceaccount>` (e.g.
    hangar-prod/app-backstage-prod/backstage), a workload, and only the service accounts its `allowed` globs name
    (on `<namespace>/<serviceaccount>`) get in: an issuer signs a token for every pod of that cluster.
    Used for delegates such as Tower's backend; route it by issuer like an OIDC verifier."""

    ALGORITHMS = ("RS256", "ES256")

    def __init__(self, name: str, issuer: str, audience: str, keys: JWKS, allowed: Sequence[str],
                 tier_ceiling: Tier = Tier.T1, leeway: float = 30):
        self.name, self.issuer, self.audience, self.keys = name, issuer, audience, keys
        self.allowed, self.tier_ceiling, self.leeway = tuple(allowed), tier_ceiling, leeway

    def authenticate(self, token: str) -> Principal:
        try:
            header = jwt.get_unverified_header(token)
            claims = jwt.decode(token, self.keys.key(header.get("kid")), algorithms=list(self.ALGORITHMS),
                                audience=self.audience, issuer=self.issuer, leeway=self.leeway,
                                options={"require": ["exp", "iat", "iss", "aud", "sub"]})
        except AuthError:
            raise
        except jwt.PyJWTError as e:
            raise AuthError("workload token rejected") from e
        sub = claims["sub"]
        prefix = "system:serviceaccount:"
        if not isinstance(sub, str) or not sub.startswith(prefix) or sub.count(":") != 3:
            raise AuthError("not a service-account token")
        ns_sa = sub[len(prefix):].replace(":", "/", 1)
        if not any(fnmatchcase(ns_sa, g) for g in self.allowed):
            raise AuthError(f"{self.name}/{ns_sa} may not call this Clearance")
        return Principal(f"{self.name}/{ns_sa}", "workload", self.tier_ceiling)


GITHUB_ACTIONS_ISSUER = "https://token.actions.githubusercontent.com"


_IMMUTABLE_REPO = re.compile(r"^repo:([^/@:]+)(?:@\d+)?/([^/@:]+)(?:@\d+)?(:.*)?$")


def canonical_sub(sub: str) -> str:
    """A workflow token's `sub` by name: GitHub's immutable-ID form (`repo:owner@<id>/name@<id>:ref:...`, the default
    for newer repositories) without the `@<id>` parts, so allow-lists and verifier globs are written by name
    (`repo:acme/shop:*`). The ids stay on the principal's attributes (sub, repository_id, repository_owner_id)."""
    m = _IMMUTABLE_REPO.match(sub)
    return f"repo:{m.group(1)}/{m.group(2)}{m.group(3) or ''}" if m else sub


class GitHubActionsAuthenticator:
    """A GitHub Actions workflow's OIDC ID token (`clearance register` in a customer's CI): signed by GitHub (RS256),
    for this audience, not expired. The principal is `github:` + the token's `sub` (for example
    `github:repo:acme/shop:environment:prod` or `github:repo:acme/shop:ref:refs/heads/main`), a workload; who may do
    what is still the gateway's lists (CLEARANCE_VERIFIERS globs on that id). `allowed` (globs on `sub`) narrows
    which workflows get in at all; empty allows none. The `repository` claim travels as a verified attribute."""

    ALGORITHMS = ("RS256",)

    def __init__(self, audience: str, keys: JWKS, allowed: Sequence[str] = ("repo:*",),
                 issuer: str = GITHUB_ACTIONS_ISSUER, tier_ceiling: Tier = Tier.T1, leeway: float = 30):
        self.issuer, self.audience, self.keys, self.allowed = issuer, audience, keys, tuple(allowed)
        self.tier_ceiling, self.leeway = tier_ceiling, leeway

    def authenticate(self, token: str) -> Principal:
        try:
            header = jwt.get_unverified_header(token)
            claims = jwt.decode(token, self.keys.key(header.get("kid")), algorithms=list(self.ALGORITHMS),
                                audience=self.audience, issuer=self.issuer, leeway=self.leeway,
                                options={"require": ["exp", "iat", "iss", "aud", "sub"]})
        except AuthError:
            raise
        except jwt.PyJWTError as e:
            raise AuthError("GitHub Actions token rejected") from e
        raw, repo = claims.get("sub"), claims.get("repository")
        if not isinstance(raw, str) or not raw.startswith("repo:") or not isinstance(repo, str) or not repo:
            raise AuthError("not a GitHub Actions repository token")
        sub = canonical_sub(raw)
        if not any(fnmatchcase(sub, g) for g in self.allowed):
            raise AuthError("this workflow may not call this Clearance")
        attrs = {"repository": repo.lower(), "sub": raw}
        for k in ("repository_id", "repository_owner_id"):
            if isinstance(claims.get(k), str):
                attrs[k] = claims[k]
        for k in ("environment", "ref", "sha", "workflow_ref", "run_id", "actor"):
            if isinstance(claims.get(k), str):
                attrs[k] = claims[k]
        return Principal(f"github:{sub}", "workload", self.tier_ceiling, attrs)


class RoutingAuthenticator:
    """Choose the verifier by the token's (unverified) issuer: a listed issuer's OIDC verifier, else the workload one.
    The choice is not trust: the chosen verifier checks everything, the issuer included."""

    def __init__(self, oidc: Mapping[str, Authenticator], workload: Authenticator | None):
        self.oidc, self.workload = dict(oidc), workload

    def authenticate(self, token: str) -> Principal:
        iss = _unverified_claims(token).get("iss")
        verifier = self.oidc.get(iss) if isinstance(iss, str) else None
        verifier = verifier or self.workload
        if verifier is None:
            raise AuthError("no verifier for this token")
        return verifier.authenticate(token)


class CachingAuthenticator:
    """Remember a verified token by its hash for `ttl` seconds, never past its own `exp`. Failures are not cached.
    Bounded: the oldest entries go first once `max_entries` is reached."""

    def __init__(self, inner: Authenticator, ttl: float = 60, max_entries: int = 4096,
                 clock: Callable[[], float] = time.time):
        self.inner, self.ttl, self.max_entries, self.clock = inner, ttl, max_entries, clock
        self._cache: dict[str, tuple[Principal, float]] = {}
        self._lock = threading.Lock()

    def authenticate(self, token: str) -> Principal:
        key = hashlib.sha256(token.encode()).hexdigest()
        now = self.clock()
        with self._lock:
            hit = self._cache.get(key)
            if hit and hit[1] > now:
                return hit[0]
        principal = self.inner.authenticate(token)
        exp = _unverified_claims(token).get("exp")
        until = now + self.ttl
        if isinstance(exp, (int, float)):
            until = min(until, float(exp))
        with self._lock:
            if len(self._cache) >= self.max_entries:
                for k in sorted(self._cache, key=lambda k: self._cache[k][1])[: self.max_entries // 4 or 1]:
                    del self._cache[k]
            self._cache[key] = (principal, until)
        return principal
