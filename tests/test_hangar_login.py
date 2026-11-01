"""tools/hangar_login.py: the device flow stores only the ID token; headers prints it while it is fresh and refuses
(naming the login command) when it is missing or expired."""
import base64
import importlib.util
import json
from pathlib import Path

spec = importlib.util.spec_from_file_location("hangar_login", Path(__file__).resolve().parents[1] / "tools" / "hangar_login.py")
hl = importlib.util.module_from_spec(spec)
spec.loader.exec_module(hl)

ISS = "http://dex.test/dex"


def jwt(exp, user="jfillman"):
    seg = lambda d: base64.urlsafe_b64encode(json.dumps(d).encode()).rstrip(b"=").decode()  # noqa: E731
    return f"{seg({'alg': 'RS256'})}.{seg({'exp': exp, 'preferred_username': user})}.sig"


def test_headers_prints_a_fresh_token_and_refuses_an_expired_or_missing_one():
    out, err = [], []
    assert hl.headers(ISS, read=lambda i: jwt(2000), now=lambda: 1000, out=out.append, err=err.append) == 0
    assert json.loads(out[0]) == {"Authorization": f"Bearer {jwt(2000)}"}
    for token in (jwt(1030), None, "not-a-jwt"):
        out.clear()
        assert hl.headers(ISS, read=lambda i, t=token: t, now=lambda: 1000, out=out.append, err=err.append) == 1
        assert out == [] and "login" in err[-1]


def test_login_runs_the_device_flow_and_keeps_only_the_id_token():
    stored, posts = {}, []
    answers = iter([(400, {"error": "authorization_pending"}),
                    (200, {"id_token": jwt(9999999999), "access_token": "a", "refresh_token": "r"})])

    def post(url, form):
        posts.append((url, form))
        if url.endswith("/device/code"):
            return 200, {"device_code": "dc", "user_code": "ABCD", "verification_uri": f"{ISS}/device",
                         "interval": 1, "expires_in": 60}
        return next(answers)

    meta = {"device_authorization_endpoint": f"{ISS}/device/code", "token_endpoint": f"{ISS}/token"}
    rc = hl.login(ISS, post=post, get=lambda u: meta, store=lambda i, t: stored.update({i: t}),
                  sleep=lambda s: None, out=lambda m: None, open_url=lambda u: None)
    assert rc == 0 and stored == {ISS: jwt(9999999999)}
    assert posts[0][1] == {"client_id": "clearance", "scope": "openid profile email"}
    assert all(f.get("client_id") == "clearance" for _, f in posts) and "offline_access" not in json.dumps(posts)


def test_login_stops_on_a_refusal():
    def post(url, form):
        if url.endswith("/device/code"):
            return 200, {"device_code": "dc", "user_code": "X", "verification_uri": "v", "interval": 1}
        return 400, {"error": "access_denied"}
    meta = {"device_authorization_endpoint": f"{ISS}/device/code", "token_endpoint": f"{ISS}/token"}
    stored = {}
    assert hl.login(ISS, post=post, get=lambda u: meta, store=lambda i, t: stored.update({i: t}),
                    sleep=lambda s: None, out=lambda m: None, open_url=lambda u: None) == 1
    assert stored == {}



def test_glidepath_api_client_has_its_own_keychain_entry_and_a_bare_token(monkeypatch, capsys):
    import time as _time
    payload = base64.urlsafe_b64encode(json.dumps({"exp": _time.time() + 3600}).encode()).decode().rstrip("=")
    tok = f"h.{payload}.s"
    services = []

    class Done:
        returncode, stdout = 0, tok + "\n"

    def run(args, **_):
        services.append(args[args.index("-s") + 1])
        return Done()

    monkeypatch.setattr(hl.subprocess, "run", run)
    assert hl.main(["token", "--client", "glidepath-api"]) == 0
    assert capsys.readouterr().out.strip() == tok
    assert hl.main(["headers"]) == 0
    assert json.loads(capsys.readouterr().out)["Authorization"] == f"Bearer {tok}"
    assert services == ["hangar-glidepath-api", "hangar-clearance"]
