"""A-9 S1: the Go gateway's authenticators (gateway/internal/auth) give the same answer as the Python ones
(src/autopilot/engine/auth.py) for the same token and settings.

This test mints tokens with PyJWT (keys generated here, never committed), runs each through the Python authenticator
and through the Go one (the TestEquivalenceHelper test in gateway/internal/auth, which reads the cases from a file and
writes its answers to another), and compares: accepted or refused, the principal id, kind, tier ceiling and
attributes. Tokens are built relative to the real clock, with margins well clear of the 30 s leeway except the cases
that probe it by 5 s on each side.

Where the Go side differs from Python on purpose it is not fed here; Go tests pin those and
docs/admin/go-gateway.md lists them: a token over 16 KiB, a string exp, a JWKS with a key PyJWT cannot read, a
plain-http JWKS URL, a JWKS fetch that fails (Python raises a non-AuthError).
"""
import json
import os
import shutil
import subprocess
import time
from fnmatch import fnmatchcase
from pathlib import Path

import jwt
import pytest
from cryptography.hazmat.primitives.asymmetric import ec, rsa
from jwt.algorithms import ECAlgorithm, RSAAlgorithm

from autopilot.engine.auth import (JWKS, ForeignWorkloadAuthenticator, GitHubActionsAuthenticator, OIDCAuthenticator,
                                   canonical_sub)
from autopilot.engine.dispatch import AuthError

ROOT = Path(__file__).resolve().parents[1]
GO = shutil.which("go")
pytestmark = pytest.mark.skipif(GO is None, reason="no Go toolchain")

ISS, AUD = "https://dex.example/dex", "tower"
TOWER_ISS = "https://hangarplatform.dev/oidc/hangar-prod"
GH_ISS = "https://token.actions.githubusercontent.com"

RSA1 = rsa.generate_private_key(public_exponent=65537, key_size=2048)
RSA2 = rsa.generate_private_key(public_exponent=65537, key_size=2048)
EC1 = ec.generate_private_key(ec.SECP256R1())


def _jwks():
    k1 = json.loads(RSAAlgorithm.to_jwk(RSA1.public_key()))
    k1["kid"] = "k1"
    e1 = json.loads(ECAlgorithm.to_jwk(EC1.public_key()))
    e1["kid"] = "e1"
    return {"keys": [k1, e1]}


JWKS_DOC = _jwks()


def claims(**over):
    n = int(time.time())
    c = {"iss": ISS, "aud": AUD, "sub": "abc", "iat": n - 10, "exp": n + 3600, "email": "ada@example.com",
         "email_verified": True}
    for k, v in over.items():
        if v is None:
            c.pop(k, None)
        else:
            c[k] = v
    return c


def tok(c, key=RSA1, alg="RS256", kid="k1"):
    return jwt.encode(c, key, algorithm=alg, headers={"kid": kid} if kid else None)


def cases():
    n = int(time.time())
    out = []

    def oidc(name, token, allowed=("*@example.com",), claim="email", issuer=ISS):
        out.append(dict(name=name, kind="oidc", token=token, issuer=issuer, audience=AUD, claim=claim, allowed=list(allowed)))

    oidc("rs256", tok(claims()))
    oidc("es256", tok(claims(), EC1, "ES256", "e1"))
    oidc("aud list", tok(claims(aud=["x", AUD])))
    oidc("aud list without", tok(claims(aud=["x", "y"])))
    oidc("wrong aud", tok(claims(aud="other")))
    oidc("wrong iss", tok(claims(iss=ISS + "x")))
    oidc("iss trailing slash", tok(claims(iss=ISS + "/")))
    oidc("expired", tok(claims(exp=n - 60)))
    oidc("inside leeway after exp", tok(claims(exp=n - 25)))
    oidc("nbf future", tok(claims(nbf=n + 60)))
    oidc("nbf inside leeway", tok(claims(nbf=n + 25)))
    oidc("iat future", tok(claims(iat=n + 60)))
    oidc("iat inside leeway", tok(claims(iat=n + 25)))
    for miss in ("exp", "iat", "sub", "aud", "iss"):
        oidc("missing " + miss, tok(claims(**{miss: None})))
    oidc("bad signature", tok(claims(), RSA2))
    oidc("unknown kid", tok(claims(), kid="nope"))
    oidc("no kid", tok(claims(), kid=None))
    oidc("ES256 header on RSA key", tok(claims(), EC1, "ES256", "k1"))
    oidc("email not verified", tok(claims(email_verified=False)))
    oidc("email_verified absent", tok(claims(email_verified=None)))
    oidc("email not allowed", tok(claims(email="eve@evil.test")))
    oidc("no email", tok(claims(email=None)))
    oidc("email not a string", tok(claims(email=5)))
    oidc("no allow-list", tok(claims()), allowed=())
    oidc("other claim", tok(claims(preferred_username="ada", email_verified=False)), allowed=("a?a",), claim="preferred_username")
    oidc("malformed", "abc.def.ghi")
    oidc("two parts", "a.b")
    oidc("alg none", jwt.encode(claims(), None, algorithm="none", headers={"kid": "k1"}))
    oidc("hs256", jwt.encode(claims(), "x" * 40, algorithm="HS256", headers={"kid": "k1"}))

    def foreign(sub, allowed=("app-backstage-prod/backstage", "tower-*/*"), **over):
        out.append(dict(name="hangar-prod", kind="foreign", token=tok(claims(iss=TOWER_ISS, sub=sub, **over)), issuer=TOWER_ISS,
                        audience=AUD, allowed=list(allowed), claim=""))

    foreign("system:serviceaccount:app-backstage-prod:backstage")
    foreign("system:serviceaccount:tower-x:y")
    foreign("system:serviceaccount:kube-system:coredns")
    foreign("someone")
    foreign("system:serviceaccount:a")
    foreign("system:serviceaccount:a:b:c")
    foreign("system:serviceaccount:app-backstage-prod:backstage", allowed=())
    foreign("system:serviceaccount:app-backstage-prod:backstage", aud="x")
    foreign("system:serviceaccount:app-backstage-prod:backstage", exp=n - 60)

    def gh(sub, allowed=("repo:*",), repo="Acme/Shop", **over):
        c = claims(**{"iss": GH_ISS, "sub": sub, "repository": repo, **over})
        out.append(dict(name="", kind="github", token=tok(c), issuer=GH_ISS, audience=AUD, allowed=list(allowed), claim=""))

    gh("repo:acme/shop:environment:prod", repository_id="12", repository_owner_id="34", ref="refs/heads/main", sha="abc",
       environment="prod", run_id="9", actor="ada", workflow_ref="w", job_workflow_ref="ignored")
    gh("repo:acme/shop:ref:x", repository_id=12)
    gh("repo:acme@111/shop@222:ref:refs/heads/main")
    gh("repo:acme@111/shop@222:ref:refs/heads/main", allowed=("repo:acme/shop:*",))
    gh("repo:evil/shop:ref:x", allowed=("repo:acme/shop:*",))
    gh("repo:acme/shop2:ref:x", allowed=("repo:acme/shop:*",))
    gh("repo:acme/shop:ref:x", allowed=())
    gh("org:acme")
    gh("repo:acme/shop:ref:x", repo="")
    gh("repo:acme/shop:ref:x", iss="https://dex.example")
    out.append(dict(name="", kind="github", token=tok(claims(iss=GH_ISS, sub="repo:acme/shop:x", repository="acme/shop"), EC1, "ES256", "e1"),
                    issuer=GH_ISS, audience=AUD, allowed=["repo:*"], claim=""))
    return out


def python_outcome(c):
    keys = JWKS("http://unused", fetch=lambda url: JWKS_DOC)
    if c["kind"] == "oidc":
        a = OIDCAuthenticator(c["issuer"], c["audience"], keys, user_claim=c["claim"], allowed=c["allowed"])
    elif c["kind"] == "foreign":
        a = ForeignWorkloadAuthenticator(c["name"], c["issuer"], c["audience"], keys, allowed=c["allowed"])
    else:
        a = GitHubActionsAuthenticator(c["audience"], keys, allowed=c["allowed"])
    try:
        p = a.authenticate(c["token"])
    except AuthError:
        return {"ok": False}
    except TypeError:
        # An alg that does not fit the key's type (ES256 named for an RSA key's kid) makes PyJWT raise TypeError, which
        # is not a PyJWTError, so Python lets it escape the authenticator as a non-AuthError (a 500, not a 401): still a
        # refusal, never a pass. Go returns the opaque AuthError. Reported in the PR as a weakness of the Python one.
        return {"ok": False}
    r = {"ok": True, "id": p.id, "kind": p.kind}
    if int(p.tier_ceiling):
        r["tier"] = int(p.tier_ceiling)
    if p.attrs:
        r["attrs"] = dict(p.attrs)
    return r


GLOBS = [("ada@example.com", "*@example.com"), ("ada@example.com", "*@example.org"), ("ADA@example.com", "ada@example.com"),
         ("ns/a/b", "ns/*"), ("ns/sa", "n?/s?"), ("ns/sa", "n?/s"), ("", "*"), ("", ""), ("a", ""), ("abbbc", "a*b*c"),
         ("acd", "a*c"), ("a1", "a[0-9]"), ("ab", "a[0-9]"), ("ab", "a[!0-9]"), ("a1", "a[!0-9]"), ("a]", "a[]]"), ("a-", "a[a-]"),
         ("a[", "a["), ("a[b", "a[b"), ("ab", "a["), ("a\nb", "a*b"), ("repo:acme/shop:ref:x", "repo:acme/shop:*"),
         ("repo:acme/shop2:ref:x", "repo:acme/shop:*"), ("user:x", "[!u]ser:x"), ("é", "?"), ("ab", "[b-a]b"),
         ("app-backstage-prod/backstage", "app-*/backstage")]
SUBS = ["repo:acme/shop:ref:refs/heads/main", "repo:acme@1/shop@2:ref:refs/heads/main", "repo:acme@1/shop@2",
        "repo:acme/shop@2:environment:prod", "repo:acme@x/shop", "org:acme", "repo:a/b/c:ref", "repo:acme@1/shop@2:ref:a:b"]


def test_go_and_python_authenticators_agree(tmp_path):
    cs = cases()
    infile, outfile = tmp_path / "in.json", tmp_path / "out.json"
    infile.write_text(json.dumps({"jwks": JWKS_DOC, "cases": cs, "globs": GLOBS, "subs": SUBS}))
    env = {**os.environ, "AUTOPILOT_AUTH_EQUIV_IN": str(infile), "AUTOPILOT_AUTH_EQUIV_OUT": str(outfile), "GOFLAGS": "-mod=readonly"}
    r = subprocess.run([GO, "test", "-count=1", "-run", "TestEquivalenceHelper", "./internal/auth"], cwd=ROOT / "gateway",
                       capture_output=True, text=True, env=env, timeout=600)
    assert r.returncode == 0, r.stdout[-3000:] + r.stderr[-3000:]
    go = json.loads(outfile.read_text())
    assert len(go["cases"]) == len(cs) > 50, "the Go side answered a different number of cases (a vacuous comparison)"
    accepted = 0
    for c, g in zip(cs, go["cases"]):
        py = python_outcome(c)
        g = {k: v for k, v in g.items() if v not in (None, 0, {}) or k == "ok"}
        assert g == py, f"{c['kind']} {c.get('name')}: python {py} != go {g}\n{c['token'][:60]}..."
        accepted += py["ok"]
    assert 8 <= accepted < len(cs), "the cases must include both accepted and refused tokens"
    assert go["globs"] == [fnmatchcase(n, p) for n, p in GLOBS]
    assert go["subs"] == [canonical_sub(s) for s in SUBS]
