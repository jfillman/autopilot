"""The gateway's conformance control interface (Clearance's provider protocol, docs/provider-protocol.md): what
`clearance conformance --provider <url> --role runtime` drives to prove the gateway is a conformant runtime provider.

TEST ONLY, OFF BY DEFAULT. It is served by its own subcommand, `autopilot-gateway conformance-control`, which refuses
to start unless GATEWAY_CONFORMANCE_CONTROL=1 is set as well, and binds only a loopback address, on its own port
(default 127.0.0.1:9091). No chart, manifest or Apron template sets either switch (tests/test_conformance.py
checks the repo's manifests). It never starts the deployable gateway (serve.main): it reads no GATEWAY_DSN, no core
token file, no model key, no backend credential.

Routes, on the control address:
  GET  /conformance/v1/info            {"name": "autopilot-gateway", "version": <gateway version>, "roles": ["runtime"]}
  POST /conformance/v1/apply           a runtime ApplyRequest (strict: unknown fields refused, 1 MiB); 202 {"id"}
  GET  /conformance/v1/applies/{id}    {id, state, reason, decision_id, spend, outcome {seq, hash}}

Each apply runs the gateway's own tool-call path (dispatch.Gateway.call) on a throwaway Gateway: an in-memory session
store holding the session the evaluation names, a Core on the harness's core URL with the bearer it was handed (the
client's real outage policy: 250 ms, one retry, a breaker), and one handler, the harness's fake target, for the tool
the evaluation names. The only thing the harness controls is the evaluation itself: the gateway sends it exactly,
where in production it builds it from the session it holds (ConformanceGateway._tool_request). Everything after the
answer is production code: the spend applied to the session (SessionStore.charge), acting only on allow, the outcome
against the decision (Core.outcome), fail closed when the core is unavailable (CORE-UNAVAILABLE). The spend the status
reports is measured on the session afterwards, not echoed from the core's answer.
"""
from __future__ import annotations

import copy
import ipaddress
import json
import logging
import os
import re
import threading
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, Mapping

from clearance_client import CoreClient

from .engine.core import OUTAGE_RULE, PROVIDER, REFUSED_RULE, Core, canonical
from .engine.dispatch import AuthError, Gateway, Principal
from .engine.outbox import MemoryOutbox
from .engine.session import SessionStore
from .engine.tiers import Tier
from .version import gateway_version

log = logging.getLogger("autopilot.conformance")

ENABLE_ENV = "GATEWAY_CONFORMANCE_CONTROL"
DEFAULT_LISTEN = "127.0.0.1:9091"

MAX_BODY = 1 << 20
MAX_APPLIES = 256
TARGET_TIMEOUT = 5.0

STATE_ACCEPTED, STATE_APPLIED, STATE_REFUSED, STATE_FAILED = "accepted", "applied", "refused", "failed"

_ID = re.compile(r"[a-z0-9][a-z0-9-]{0,63}")
_APPLY_KEYS = {"id", "role", "provider", "core", "target", "poll_interval_ms", "evaluation", "change", "manifest"}
_ENDPOINT_KEYS = {"url", "bearer"}


class BadApply(ValueError):
    pass


@dataclass(frozen=True)
class Endpoint:
    url: str
    bearer: str


@dataclass(frozen=True)
class Apply:
    id: str
    core: Endpoint
    target: Endpoint
    evaluation: dict[str, Any]


def _http_url(name: str, v: Any) -> str:
    if not isinstance(v, str) or len(v) > 2048:
        raise BadApply(f"{name} is an http(s) URL")
    u = urllib.parse.urlsplit(v)
    if u.scheme not in ("http", "https") or not u.netloc or u.username or u.password or u.query or u.fragment:
        raise BadApply(f"{name} is an http(s) URL without credentials, query or fragment")
    return v.rstrip("/")


def _endpoint(name: str, v: Any) -> Endpoint:
    if not isinstance(v, dict) or set(v) - _ENDPOINT_KEYS:
        raise BadApply(f"{name} is {{url, bearer}}")
    bearer = v.get("bearer", "")
    if not isinstance(bearer, str) or len(bearer) > 4096:
        raise BadApply("a bearer is a string of at most 4096 bytes")
    return Endpoint(_http_url(name + ".url", v.get("url")), bearer)


def decode_apply(raw: bytes) -> Apply:
    """conformance.DecodeApply, for the runtime role: strictly decoded, at most MAX_BODY bytes."""
    if len(raw) > MAX_BODY:
        raise BadApply("the body is over its cap")
    try:
        a = json.loads(raw)
    except ValueError as e:
        raise BadApply(f"the body is not JSON: {e}") from None
    if not isinstance(a, dict):
        raise BadApply("the body is an object")
    unknown = set(a) - _APPLY_KEYS
    if unknown:
        raise BadApply(f"unknown fields {sorted(unknown)}")
    if not isinstance(a.get("id"), str) or not _ID.fullmatch(a["id"]):
        raise BadApply("id is 1 to 64 lowercase letters, digits and dashes")
    if a.get("role") != "runtime":
        raise BadApply("this provider is tested in the runtime role only")
    if a.get("provider") != PROVIDER:
        raise BadApply(f"provider is {PROVIDER!r}, the name GET /conformance/v1/info gave")
    p = a.get("poll_interval_ms", 0)
    if not isinstance(p, int) or isinstance(p, bool) or not 0 <= p <= 60_000:
        raise BadApply("poll_interval_ms is 0 to 60000")
    ev = a.get("evaluation")
    # As DecodeApply: manifest is a string, empty (or absent) on a runtime apply; any other value is refused.
    if not isinstance(ev, dict) or a.get("change") is not None or a.get("manifest") not in (None, ""):
        raise BadApply("a runtime apply carries evaluation (an object) and no change or manifest")
    for k in ("subject", "action", "resource", "context"):
        if not isinstance(ev.get(k), dict):
            raise BadApply(f"evaluation.{k} is an object")
    if ev["action"].get("name") != "tool.call":
        raise BadApply("the runtime apply is a tool.call evaluation")
    if not isinstance(ev["resource"].get("id"), str) or not isinstance(ev["subject"].get("id"), str):
        raise BadApply("evaluation names its subject and its tool")
    return Apply(a["id"], _endpoint("core", a.get("core")), _endpoint("target", a.get("target")), ev)


# -- the throwaway gateway one apply runs on --------------------------------------------------------------------------

class _OneCaller:
    """The evaluation's subject is the caller: the harness's token stands for it."""
    TOKEN = "conformance-caller"

    def __init__(self, principal: Principal):
        self.principal = principal

    def authenticate(self, token: str) -> Principal:
        if token != self.TOKEN:
            raise AuthError("not the conformance caller")
        return self.principal


class _NotingCore(Core):
    """The production Core, noting the decision it was answered and the outcome record it wrote (for the status)."""

    def __init__(self, *a, **k):
        super().__init__(*a, **k)
        self.decision_id: str | None = None
        self.outcome_ref: dict[str, Any] | None = None

    def evaluate(self, request):
        v = super().evaluate(request)
        self.decision_id = v.decision_id
        return v

    def _post(self, entry):
        if entry["op"] != "outcome":
            return super()._post(entry)
        ref = self.client.outcome(entry["decision_id"], entry["body"])
        self.outcome_ref = {"seq": int(ref.seq), "hash": str(ref.hash)}
        return ref.seq


class ConformanceGateway(Gateway):
    """dispatch.Gateway with one difference: the tool call's evaluation is the harness's, sent exactly."""

    def __init__(self, evaluation: dict[str, Any], *a, **k):
        super().__init__(*a, **k)
        self._evaluation = evaluation

    def _tool_request(self, principal, session_id, s, tool, args, now):
        return copy.deepcopy(self._evaluation)


def _target_handler(apply_id: str, target: Endpoint):
    """The harness's fake target as the tool's backend: POST /v1/apply {"id", "content"} with the target bearer;
    content is the tool's arguments (canonical JSON). A refusal raises, so Gateway.call reports error:<Kind>."""
    def handler(_session, args: dict[str, Any]) -> dict[str, Any]:
        body = json.dumps({"id": apply_id, "content": canonical(args).decode()}).encode()
        headers = {"content-type": "application/json"}
        if target.bearer:
            headers["authorization"] = "Bearer " + target.bearer
        req = urllib.request.Request(target.url + "/v1/apply", data=body, method="POST", headers=headers)
        with urllib.request.urlopen(req, timeout=TARGET_TIMEOUT) as r:  # noqa: S310 - the harness's loopback target
            out = json.loads(r.read(64 << 10))
        if not isinstance(out, dict):
            raise ValueError("the target answered no receipt")
        return {"seq": out.get("seq"), "sha256": out.get("sha256")}
    return handler


def _session_of(store: SessionStore, ctx: Mapping[str, Any], now: datetime):
    """The session the evaluation names, as the gateway would hold it: adopted with its limits and expiry, its used
    and reserved budgets, denials and breaker. None when the evaluation names no session (the core denies it)."""
    st = ctx.get("session")
    if not isinstance(st, dict):
        return None
    s = store.adopt({"session": st["id"], "principal": st["principal"], "agent": st["agent"],
                     "task_id": st.get("task_id") or "", "parent": st.get("parent_id"), "depth": st.get("depth") or 0,
                     "limits": st["limits"], "expires_at": st["expires_at"]}, now)
    for f, n in (st.get("used") or {}).items():
        if f in s.used:
            s.used[f] = int(n)
    for f, n in (st.get("reserved") or {}).items():
        if f in s.reserved:
            s.reserved[f] = int(n)
    s.denials = int(st.get("denials") or 0)
    if st.get("tripped"):
        s.trip(str(st.get("trip_reason") or "tripped"))
    return s


def _applied_spend(before: dict[str, Any], s) -> dict[str, Any]:
    """What was applied to the session, measured: the counters' growth, a denial counted, a breaker tripped."""
    out: dict[str, Any] = {}
    for f in ("tool_calls", "github_calls", "open_prs", "children"):
        d = s.used[f] - before["used"][f]
        if d:
            out[f] = d
    reserve = {f: s.reserved[f] - before["reserved"][f] for f in s.reserved if s.reserved[f] != before["reserved"][f]}
    if reserve:
        out["reserve"] = reserve
    if s.denials > before["denials"]:
        out["denial"] = True
    if s.tripped and not before["tripped"]:
        out["trip"] = s.trip_reason or "tripped"
    return out


def run_apply(a: Apply, clock=lambda: datetime.now(timezone.utc)) -> dict[str, Any]:
    """One apply, start to finish: the status it ends in."""
    from .gateway import PACKS

    status: dict[str, Any] = {"id": a.id, "state": STATE_ACCEPTED}
    ev = a.evaluation
    props = ev["subject"].get("properties") or {}
    try:
        tier = Tier(int(props.get("tier_ceiling", 1)))
    except (TypeError, ValueError):
        tier = Tier.T1
    principal = Principal(ev["subject"]["id"], str(props.get("kind") or "user"), tier)
    tool = ev["resource"]["id"]
    args = (ev["action"].get("properties") or {}).get("args") or {}
    ctx = ev["context"]
    session_id = str(ctx.get("session_id") or "")
    client = CoreClient(a.core.url, token=a.core.bearer, provider=PROVIDER)
    core = _NotingCore(client, MemoryOutbox())
    store = SessionStore()
    try:
        s = _session_of(store, ctx, clock())
        gw = ConformanceGateway(ev, {}, store, core, _OneCaller(principal), clock, packs=PACKS,
                                handlers={tool: _target_handler(a.id, a.target)})
    except (KeyError, TypeError, ValueError) as e:
        status.update(state=STATE_FAILED, reason=f"the gateway cannot hold this call: {type(e).__name__}: {e}")
        return status
    before = {"used": dict(s.used), "reserved": dict(s.reserved), "denials": s.denials, "tripped": s.tripped} if s else None
    r = gw.call(_OneCaller.TOKEN, session_id, tool, args)
    if core.decision_id:
        status["decision_id"] = core.decision_id
    status["spend"] = _applied_spend(before, s) if s is not None else {}
    rule = r.decision.get("rule")
    if r.decision.get("allow") is not True:
        if rule in (OUTAGE_RULE, REFUSED_RULE):
            status.update(state=STATE_REFUSED, reason=f"core-unavailable: failed closed ({rule}): {r.error}")
        else:
            status.update(state=STATE_REFUSED, reason=f"denied {rule}: {r.decision.get('message') or r.error}")
    elif r.ok:
        status["state"] = STATE_APPLIED
    else:
        status.update(state=STATE_FAILED, reason=f"the target refused: {r.error}")
    if core.outcome_ref is not None:
        status["outcome"] = core.outcome_ref
    elif r.decision.get("allow") is True:
        status["reason"] = (status.get("reason", "") + " (outcome not recorded yet: it waits in the outbox)").strip()
    client.close()
    return status


# -- the control server ---------------------------------------------------------------------------------------------

class Control:
    """The three routes, over a table of applies run on background threads."""

    def __init__(self, clock=lambda: datetime.now(timezone.utc)):
        self.clock = clock
        self._lock = threading.Lock()
        self._applies: dict[str, dict[str, Any]] = {}

    def info(self) -> dict[str, Any]:
        return {"name": PROVIDER, "version": gateway_version(), "roles": ["runtime"]}

    def start(self, raw: bytes) -> tuple[int, dict[str, Any]]:
        try:
            a = decode_apply(raw)
        except BadApply as e:
            return 400, {"error": str(e)}
        with self._lock:
            if a.id in self._applies:
                return 409, {"error": f"apply {a.id} exists"}
            if len(self._applies) >= MAX_APPLIES:
                return 429, {"error": "too many applies: restart the control interface"}
            self._applies[a.id] = {"id": a.id, "state": STATE_ACCEPTED}
        threading.Thread(target=self._run, args=(a,), name=f"conformance-{a.id}", daemon=True).start()
        return 202, {"id": a.id}

    def _run(self, a: Apply) -> None:
        try:
            st = run_apply(a, self.clock)
        except Exception as e:  # noqa: BLE001 - the status says what broke; the harness fails the assertion
            st = {"id": a.id, "state": STATE_FAILED, "reason": f"the gateway failed: {type(e).__name__}"}
        with self._lock:
            self._applies[a.id] = st

    def status(self, apply_id: str) -> dict[str, Any] | None:
        with self._lock:
            st = self._applies.get(apply_id)
            return dict(st) if st is not None else None


def create_control_app(control: Control | None = None):
    from starlette.applications import Starlette
    from starlette.requests import Request
    from starlette.responses import JSONResponse
    from starlette.routing import Route

    c = control or Control()

    async def info(_: Request) -> JSONResponse:
        return JSONResponse(c.info())

    async def apply(request: Request) -> JSONResponse:
        raw = b""
        async for chunk in request.stream():
            raw += chunk
            if len(raw) > MAX_BODY:
                return JSONResponse({"error": "the body is over its cap"}, status_code=413)
        status, body = c.start(raw)
        return JSONResponse(body, status_code=status)

    async def applies(request: Request) -> JSONResponse:
        st = c.status(request.path_params["apply_id"])
        if st is None:
            return JSONResponse({"error": "no such apply"}, status_code=404)
        return JSONResponse(st)

    return Starlette(routes=[Route("/conformance/v1/info", info, methods=["GET"]),
                             Route("/conformance/v1/apply", apply, methods=["POST"]),
                             Route("/conformance/v1/applies/{apply_id}", applies, methods=["GET"])])


def listen_address(value: str) -> tuple[str, int]:
    """host:port, the host a loopback address: the control interface is never reachable off the machine (or pod)."""
    host, sep, port = value.rpartition(":")
    host = host.strip("[]")
    if not sep or not port.isdigit() or not 0 < int(port) < 65536:
        raise ValueError(f"--listen is host:port, not {value!r}")
    try:
        loopback = ipaddress.ip_address(host).is_loopback
    except ValueError:
        loopback = host == "localhost"
    if not loopback:
        raise ValueError(f"the conformance control interface binds a loopback address only, not {host!r}")
    return host, int(port)


def main(argv: list[str], env: Mapping[str, str] | None = None) -> int:
    """`autopilot-gateway conformance-control [--listen 127.0.0.1:9091]`, with GATEWAY_CONFORMANCE_CONTROL=1."""
    import argparse

    env = os.environ if env is None else env
    p = argparse.ArgumentParser(prog="autopilot-gateway conformance-control",
                                description="the test-only conformance control interface (docs/admin/provider.md)")
    p.add_argument("--listen", default=DEFAULT_LISTEN, help="loopback host:port (default %(default)s)")
    args = p.parse_args(argv)
    logging.basicConfig(level=(env.get("LOG_LEVEL") or "INFO").upper(),
                        format="%(asctime)s %(levelname)s %(name)s %(message)s")
    if (env.get(ENABLE_ENV) or "") != "1":
        log.error("the conformance control interface is test-only and off: set %s=1 to serve it (never in a "
                  "deployment)", ENABLE_ENV)
        return 2
    try:
        host, port = listen_address(args.listen)
    except ValueError as e:
        log.error("%s", e)
        return 2
    import uvicorn
    log.warning("TEST ONLY: serving the conformance control interface on %s:%d as %s %s", host, port, PROVIDER,
                gateway_version())
    uvicorn.run(create_control_app(), host=host, port=port, log_level=(env.get("LOG_LEVEL") or "info").lower(),
                proxy_headers=False, server_header=False)
    return 0
