#!/usr/bin/env python3
"""Sign in to Clearance as a person (Hangar D19 interim: people-dex, GitHub sign-in) and hand the ID token to an MCP
client.

  hangar_login.py login [--issuer URL]     the OAuth device flow against people-dex: open the printed URL, sign in with
                                           GitHub, and the ID token (1 hour) is kept in the macOS Keychain
  hangar_login.py headers [--issuer URL]   for Claude Code's headersHelper: prints {"Authorization": "Bearer ..."}
                                           from the Keychain, or exits 1 naming the login command when there is no
                                           token or it has expired. Never signs in itself (headersHelper has 10 s)
  hangar_login.py logout [--issuer URL]    removes the token
  hangar_login.py token [--issuer URL]     prints the bare ID token (for curl), or exits 1 like headers

--client glidepath-api signs in to glidepath-api instead (its own Dex client and Keychain entry, hangar-glidepath-api):
its ID tokens carry aud glidepath-api, which Clearance refuses, and the reverse. For glidepath-api over the Gateway:
  curl -H "Authorization: Bearer $(python3 hangar_login.py token --client glidepath-api)" \
    http://glidepath-api.hangar-dev.local/v1/whoami

Only the ID token is kept, never a refresh token (the device flow asks for none), so a stolen Keychain entry is good
for at most an hour. The token reaches `security` on its argument list for the moment it takes to store it.

Claude Code:
  claude mcp add-json clearance '{"type":"http","url":"http://clearance.hangar-dev.local/mcp",
    "headersHelper":"python3 /path/to/autopilot/tools/hangar_login.py headers"}'
"""
from __future__ import annotations

import argparse
import base64
import json
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import webbrowser
from typing import Any, Callable

ISSUER = "http://dex.hangar-dev.local/dex"
CLIENT = "clearance"
SERVICE = "hangar-clearance"
CLIENTS = ("clearance", "glidepath-api")  # people-dex's public clients (apron 55-autopilot/people-dex)
DEVICE_GRANT = "urn:ietf:params:oauth:grant-type:device_code"
MARGIN = 60   # a token with less than this left is treated as expired


def _post(url: str, form: dict[str, str]) -> tuple[int, dict[str, Any]]:
    req = urllib.request.Request(url, data=urllib.parse.urlencode(form).encode(), method="POST",
                                 headers={"Content-Type": "application/x-www-form-urlencoded"})
    try:
        with urllib.request.urlopen(req, timeout=15) as r:  # noqa: S310 - the issuer is configuration
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def _get(url: str) -> dict[str, Any]:
    with urllib.request.urlopen(url, timeout=15) as r:  # noqa: S310
        return json.loads(r.read())


def claims(token: str) -> dict[str, Any]:
    """The JWT's payload, unverified: only to read its expiry and name here. Clearance verifies the signature."""
    payload = token.split(".")[1]
    return json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))


def usable(token: str | None, now: float) -> bool:
    if not token:
        return False
    try:
        return float(claims(token).get("exp", 0)) - MARGIN > now
    except (ValueError, IndexError, json.JSONDecodeError):
        return False


# -- the Keychain --------------------------------------------------------------------------------------------------
def kc_read(issuer: str) -> str | None:
    r = subprocess.run(["security", "find-generic-password", "-s", SERVICE, "-a", issuer, "-w"],
                       capture_output=True, text=True)
    return r.stdout.strip() if r.returncode == 0 and r.stdout.strip() else None


def kc_write(issuer: str, token: str) -> None:
    subprocess.run(["security", "add-generic-password", "-U", "-s", SERVICE, "-a", issuer, "-w", token],
                   check=True, capture_output=True)


def kc_delete(issuer: str) -> None:
    subprocess.run(["security", "delete-generic-password", "-s", SERVICE, "-a", issuer], capture_output=True)


# -- commands ------------------------------------------------------------------------------------------------------
def login(issuer: str, *, post=_post, get=_get, store: Callable[[str, str], None] = kc_write,
          sleep=time.sleep, out=print, open_url=webbrowser.open) -> int:
    meta = get(issuer.rstrip("/") + "/.well-known/openid-configuration")
    device_url = meta.get("device_authorization_endpoint")
    if not device_url:
        out(f"{issuer} does not offer the device flow")
        return 1
    status, dev = post(device_url, {"client_id": CLIENT, "scope": "openid profile email"})
    if status != 200:
        out(f"device authorization refused ({status}): {dev.get('error', '')}")
        return 1
    url = dev.get("verification_uri_complete") or dev["verification_uri"]
    out(f"Sign in at {url}" + ("" if dev.get("verification_uri_complete") else f" with code {dev['user_code']}"))
    open_url(url)
    interval = int(dev.get("interval", 5))
    deadline = time.time() + int(dev.get("expires_in", 300))
    while time.time() < deadline:
        sleep(interval)
        status, tok = post(meta["token_endpoint"], {"grant_type": DEVICE_GRANT, "device_code": dev["device_code"],
                                                    "client_id": CLIENT})
        if status == 200 and tok.get("id_token"):
            store(issuer, tok["id_token"])
            c = claims(tok["id_token"])
            out(f"Signed in as {c.get('preferred_username') or c.get('email')}; the token lasts until "
                f"{time.strftime('%H:%M', time.localtime(c['exp']))}.")
            return 0
        err = tok.get("error")
        if err == "slow_down":
            interval += 5
        elif err != "authorization_pending":
            out(f"sign-in failed: {err or status}")
            return 1
    out("sign-in timed out")
    return 1


def headers(issuer: str, *, read: Callable[[str], str | None] = kc_read, now=time.time, out=print,
            err=lambda m: print(m, file=sys.stderr)) -> int:
    token = read(issuer)
    if not usable(token, now()):
        flag = "" if CLIENT == "clearance" else f" --client {CLIENT}"
        err(f"No {CLIENT} sign-in for {issuer}, or it has expired: run `python3 {sys.argv[0]} login{flag}`.")
        return 1
    out(json.dumps({"Authorization": f"Bearer {token}"}))
    return 0


def main(argv: list[str] | None = None) -> int:
    global CLIENT, SERVICE
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("command", choices=["login", "headers", "token", "logout"])
    p.add_argument("--issuer", default=ISSUER)
    p.add_argument("--client", choices=CLIENTS, default="clearance")
    a = p.parse_args(argv)
    CLIENT, SERVICE = a.client, f"hangar-{a.client}"
    if a.command == "login":
        return login(a.issuer)
    if a.command == "headers":
        return headers(a.issuer)
    if a.command == "token":
        return headers(a.issuer, out=lambda h: print(json.loads(h)["Authorization"].split(" ", 1)[1]))
    kc_delete(a.issuer)
    return 0


if __name__ == "__main__":
    sys.exit(main())
