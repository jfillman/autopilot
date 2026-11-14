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
import base64
import json
import os
import random
import shutil
import subprocess
import time
from fnmatch import fnmatchcase
from pathlib import Path

import jwt
import pytest
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.asymmetric import ec, padding, rsa
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


def raw_token(header_json, payload_json, key=RSA1):
    """A token whose JSON is written by hand (duplicate keys, odd numbers), signed RS256 over exactly those bytes."""
    b = lambda x: base64.urlsafe_b64encode(x).rstrip(b"=").decode()  # noqa: E731
    signing = b(header_json.encode()) + "." + b(payload_json.encode())
    return signing + "." + b(key.sign(signing.encode(), padding.PKCS1v15(), hashes.SHA256()))


def body(**over):
    """claims() as JSON text with the given members replaced by raw JSON text (so 1e400 or a duplicate stay as written)."""
    base = {"iss": json.dumps(ISS), "aud": json.dumps(AUD), "sub": '"abc"', "iat": str(int(time.time()) - 10),
            "exp": str(int(time.time()) + 3600), "email": '"ada@example.com"'}
    base.update(over)
    return "{" + ",".join(f'"{k}":{v}' for k, v in base.items() if v is not None) + "}"


def more_cases():
    """The edge cases of numbers, duplicate keys, null kid, aud lists and unusual user claims. A case with `go` set is
    one where Go is deliberately stricter: Python's answer is the natural one and Go's must be exactly `go`."""
    n = int(time.time())
    out = []

    def oidc(name, token, allowed=("*",), claim="email", go=None):
        c = dict(name=name, kind="oidc", token=token, issuer=ISS, audience=AUD, claim=claim, allowed=list(allowed))
        if go is not None:
            c["go"] = go
        out.append(c)

    H = '{"alg":"RS256","kid":"k1"}'
    for label, over in {
        "float exp": {"exp": str(n + 3600) + ".7"}, "float iat": {"iat": str(n - 10) + ".5"},
        "huge exp": {"exp": "1e30"}, "huge integer exp": {"exp": "9" * 30}, "huge iat": {"iat": "1e30"},
        "exp 1e400": {"exp": "1e400"}, "negative exp": {"exp": "-5"}, "negative iat": {"iat": "-5"}, "negative nbf": {"nbf": "-5"},
        "zero exp": {"exp": "0"}, "float nbf future": {"nbf": str(n + 60) + ".5"}, "exp true": {"exp": "true"},
        "iat null": {"iat": "null"}, "exp null": {"exp": "null"}, "nbf null": {"nbf": "null"}, "exp array": {"exp": "[1]"},
        "exp tiny float": {"exp": "1e-9"},
    }.items():
        oidc(label, raw_token(H, body(**over)))
    # Python's int() reads a numeric string; Go insists on a JSON number.
    for k in ("exp", "iat", "nbf"):
        oidc(f"{k} as a string", raw_token(H, body(**{k: f'"{n + 3600 if k == "exp" else n - 10}"'})), go={"ok": False})
    # A token over 16 KiB: Python has no limit.
    oidc("oversized", tok(claims(pad="a" * 20000)), go={"ok": False})
    # Duplicate JSON keys: the last one wins in both.
    oidc("duplicate kid, last good", raw_token('{"alg":"RS256","kid":"nope","kid":"k1"}', body()))
    oidc("duplicate kid, last bad", raw_token('{"alg":"RS256","kid":"k1","kid":"nope"}', body()))
    oidc("duplicate alg, last none", raw_token('{"alg":"RS256","alg":"none","kid":"k1"}', body()))
    oidc("duplicate aud, last good", raw_token(H, body(aud='"x","aud":"tower"')))
    oidc("duplicate aud, last bad", raw_token(H, body(aud='"tower","aud":"x"')))
    oidc("duplicate exp, last expired", raw_token(H, body(exp=f"{n + 3600},\"exp\":{n - 3600}")))
    oidc("duplicate email", raw_token(H, body(email='"eve@evil.test","email":"ada@example.com"')))
    # kid: null, a number, absent
    oidc("null kid", raw_token('{"alg":"RS256","kid":null}', body()))
    oidc("numeric kid", raw_token('{"alg":"RS256","kid":1}', body()))
    oidc("empty kid", raw_token('{"alg":"RS256","kid":""}', body()))
    # aud with non-strings
    for label, a in {"[aud, 1]": f'["{AUD}",1]', "[1, aud]": f'[1,"{AUD}"]', "[null, aud]": f'[null,"{AUD}"]', "[[aud]]": f'[["{AUD}"]]',
                     "object": f'{{"{AUD}":1}}', "empty list": "[]", "empty string": '""', "number": "5", "true": "true",
                     "list of aud twice": f'["{AUD}","{AUD}"]'}.items():
        oidc("aud " + label, raw_token(H, body(aud=a)))
    # unusual user claims: the principal is user:<claim> byte for byte
    for label, v in {"slash": "a/b@example.com", "colon": "a:b@example.com", "newline": "a\nb@example.com", "non-ascii": "\u00e9a@example.com",
                     "space": "a b@example.com", "glob chars": "a*[b]?@example.com", "emoji": "\U0001f600@example.com", "long": "a" * 300 + "@example.com",
                     "empty": "", "only newline": "\n"}.items():
        oidc("user claim " + label, tok(claims(email=v)))
    oidc("claim with newline vs glob", tok(claims(email="a\nb@example.com")), allowed=("a?b@example.com",))
    oidc("claim with newline vs star", tok(claims(email="a\nb@example.com")), allowed=("a*@example.com",))
    oidc("claim with newline vs class", tok(claims(email="a\nb@example.com")), allowed=("a[!x]b@example.com",))
    oidc("non-string preferred_username", tok(claims(preferred_username=["a"])), claim="preferred_username")
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
def generated_globs(count=2500):
    """Seeded pattern/name pairs over an alphabet of the characters that matter to fnmatch."""
    rnd = random.Random(20261114)
    pat_chars, name_chars = "ab/:*?[]!-^\\\n\u00e9", "ab/:]!-^\n\u00e9["
    pairs = []
    for _ in range(count):
        pat = "".join(rnd.choice(pat_chars) for _ in range(rnd.randint(0, 7)))
        name = "".join(rnd.choice(name_chars) for _ in range(rnd.randint(0, 6)))
        pairs.append((name, pat))
        # and a name built from the pattern, so a good share of the pairs match
        derived = "".join(rnd.choice("ab/:") if ch in "*?" else ch for ch in pat if ch not in "[]!")
        pairs.append((derived, pat))
    return pairs


SUBS = ["repo:acme/shop:ref:refs/heads/main", "repo:acme@1/shop@2:ref:refs/heads/main", "repo:acme@1/shop@2",
        "repo:acme/shop@2:environment:prod", "repo:acme@x/shop", "org:acme", "repo:a/b/c:ref", "repo:acme@1/shop@2:ref:a:b"]


def test_go_and_python_authenticators_agree(tmp_path):
    cs = cases() + more_cases()
    infile, outfile = tmp_path / "in.json", tmp_path / "out.json"
    infile.write_text(json.dumps({"jwks": JWKS_DOC, "cases": cs, "globs": GLOBS + generated_globs(), "subs": SUBS}))
    env = {**os.environ, "AUTOPILOT_AUTH_EQUIV_IN": str(infile), "AUTOPILOT_AUTH_EQUIV_OUT": str(outfile), "GOFLAGS": "-mod=readonly"}
    r = subprocess.run([GO, "test", "-count=1", "-run", "TestEquivalenceHelper", "./internal/auth"], cwd=ROOT / "gateway",
                       capture_output=True, text=True, env=env, timeout=600)
    assert r.returncode == 0, r.stdout[-3000:] + r.stderr[-3000:]
    go = json.loads(outfile.read_text())
    assert len(go["cases"]) == len(cs) > 50, "the Go side answered a different number of cases (a vacuous comparison)"
    accepted = different = 0
    for c, g in zip(cs, go["cases"]):
        py = python_outcome(c)
        g = {k: v for k, v in g.items() if v not in (None, 0, {}) or k == "ok"}
        if "go" in c:
            # Go is deliberately stricter here: pin the exact difference, so it cannot widen or quietly vanish.
            assert py["ok"] and g == c["go"], f"{c['name']}: expected python accepts and go {c['go']}, got python {py} go {g}"
            different += 1
            continue
        assert g == py, f"{c['kind']} {c.get('name')}: python {py} != go {g}\n{c['token'][:60]}..."
        accepted += py["ok"]
    assert different == 4, "the deliberate differences are exactly the numeric-string claims and the oversized token"
    assert 20 <= accepted < len(cs), "the cases must include both accepted and refused tokens"
    pairs = GLOBS + generated_globs()
    assert len(go["globs"]) == len(pairs) >= 5000
    wrong = [(n, p, got) for (n, p), got in zip(pairs, go["globs"]) if got != fnmatchcase(n, p)]
    assert not wrong, f"{len(wrong)} glob differences, first: {wrong[:5]}"
    assert 200 < sum(go["globs"]) < len(pairs) - 200, "the generated globs must both match and miss"
    assert go["subs"] == [canonical_sub(s) for s in SUBS]
