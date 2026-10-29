r"""The XRD sidecar contract: xrds/<kind>.meta.yaml (AF-3, review A3).

Why sidecars: the agent-facing contract (what a kind produces, what its conditions mean, how to check
that an XR really works) was meant to live on the XRD as x-hangar-* extension keys, but every XRD is
rendered into a CRD and a CRD's structural schema rejects unknown x- keys. So it lives beside the XRD,
one file per kind, and three things read it: the chart's fromComponent (generated
charts/airframe-application/files/component-outputs.yaml), the contract bundle
(contract/airframe-contract.json, airframe-capabilities), and airframe-verify.

A sidecar:

    kind: PostgreSQL                 # the XRD's kind
    role: component                  # component | target | dependency | bootstrap | environment | platform | observability
    summary: ...
    outputs:                         # what an XR of this kind produces, by name
      uri: {kind: secretKeyRef, secret: "{name}-app", key: uri}
      host: {kind: literal, value: "{name}-rw.{namespace}.svc", at: {kind: ..., field: ...}}
    sources:                         # where every named object an output or verify step points at comes from
      - object: {kind: Secret, name: "{name}-app"}
        createdBy: operator          # composition | operator | crossplane | function
        from: {kind: Cluster, name: "{name}", field: spec.bootstrap.initdb}
    conditions:                      # closed reason sets for this kind's custom conditions
      - type: ComponentReady
        reasons: [{reason: PostgreSQLReady, status: "True", meaning: ...}]
    knownFailures:                   # messages a condition carries for a known external cause (review C8)
      - id: CNPGWebhookUnavailable   # stable id an agent can branch on
        condition: Synced            # the condition whose message is matched
        match: 'failed calling webhook "[a-z]+\.cnpg\.io"'
        example: <a real message>    # validated: match must find it
        meaning: ...
        hint: ...
    verify:                          # executable checks; `check` is the human sentence, `run` the machine form
      - id: reachable
        check: ...
        run: {kind: tcp, service: "{name}-rw", port: 5432}
    owner: human

Only role `component` feeds the chart's fromComponent table. tools/test_sidecars.py holds every source,
literal `at`, condition reason and verify reference against the committed composition renders, so a
sidecar cannot silently disagree with its composition.

Templates: {name} and {namespace} are the XR's; {spec.a.b} and {status.a.b} read the XR.
"""
import re
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent
XRDS = ROOT / "xrds"

ROLES = {"component", "target", "dependency", "bootstrap", "environment", "platform", "observability"}
OUTPUT_KINDS = {"literal", "secretKeyRef", "configMapKeyRef"}
CREATED_BY = {"composition", "operator", "crossplane", "function"}

# verify run kinds -> (required fields, optional fields)
RUN_KINDS = {
    "condition": ({"type"}, {"status", "reasons"}),
    "xrField": ({"path"}, {"equals", "in"}),
    "secretKeys": ({"secret", "keys"}, {"namespace"}),
    "configMapKeys": ({"configMap", "keys"}, {"namespace"}),
    "resource": ({"resource", "name"}, {"namespace", "clusterScoped", "condition", "field", "equals"}),
    "tcp": ({"service", "port"}, {"namespace"}),
    "http": (set(), {"url", "service", "port", "path", "namespace", "expectStatus"}),
    "oauthClientCredentials": ({"secret"}, {"namespace", "clientIdKey", "clientSecretKey", "tokenUrlKey", "jwksUrlKey"}),
    "cloudCli": ({"cli", "args"}, {"expect"}),
    "manual": (set(), set()),
}
COMMON_RUN_FIELDS = {"kind"}

# read-only verbs a cloudCli step may use: airframe-verify never changes anything
AWS_READ_PREFIXES = ("get-", "describe-", "list-")
AZ_READ_VERBS = {"show", "list"}

TEMPLATE_RE = re.compile(r"\{([a-zA-Z][\w.]*)\}")


class SidecarError(ValueError):
    pass


def _err(f, msg):
    raise SidecarError(f"{f.name}: {msg}")


def role_of(d):
    if "role" in d:
        return d["role"]
    # pre-role sidecars: `component: false` meant a cloud target
    return "component" if d.get("component", True) else "target"


def validate(d, f):
    """Structural check of one sidecar. Raises SidecarError with the file name on the first problem."""
    for k in ("kind", "summary"):
        if not d.get(k):
            _err(f, f"missing {k}")
    role = role_of(d)
    if role not in ROLES:
        _err(f, f"role {role!r} not one of {sorted(ROLES)}")
    for name, spec in (d.get("outputs") or {}).items():
        if spec.get("kind") not in OUTPUT_KINDS:
            _err(f, f"output {name!r} has invalid kind {spec.get('kind')!r}")
        if spec["kind"] == "secretKeyRef" and not (spec.get("secret") and spec.get("key")):
            _err(f, f"output {name!r}: secretKeyRef needs secret and key")
        if spec["kind"] == "configMapKeyRef" and not (spec.get("configMap") and spec.get("key")):
            _err(f, f"output {name!r}: configMapKeyRef needs configMap and key")
        if spec["kind"] == "literal" and "value" not in spec:
            _err(f, f"output {name!r}: literal needs value")
    for i, s in enumerate(d.get("sources") or []):
        obj = s.get("object") or {}
        if not (obj.get("kind") and obj.get("name")):
            _err(f, f"sources[{i}]: object needs kind and name")
        if s.get("createdBy") not in CREATED_BY:
            _err(f, f"sources[{i}] ({obj.get('name')}): createdBy must be one of {sorted(CREATED_BY)}")
        if s["createdBy"] in ("operator", "crossplane") and not (s.get("from") or {}).get("kind"):
            _err(f, f"sources[{i}] ({obj.get('name')}): createdBy {s['createdBy']} needs `from` (the composed resource that makes it)")
        if s["createdBy"] == "function" and not s.get("checkedBy"):
            _err(f, f"sources[{i}] ({obj.get('name')}): createdBy function needs `checkedBy` (the function test that holds it)")
    for c in d.get("conditions") or []:
        if not c.get("type") or not c.get("reasons"):
            _err(f, "conditions entries need type and reasons")
    for k in d.get("knownFailures") or []:
        for field in ("id", "condition", "match", "example", "meaning", "hint"):
            if not k.get(field):
                _err(f, f"knownFailures entry {k.get('id')!r} needs {field}")
        try:
            rx = re.compile(k["match"])
        except re.error as e:
            _err(f, f"knownFailures {k['id']}: match is not a valid regex: {e}")
        if not rx.search(k["example"]):
            _err(f, f"knownFailures {k['id']}: match does not match its own example message")
    ids = set()
    for v in d.get("verify") or []:
        if not v.get("id") or not v.get("check"):
            _err(f, "verify entries need id and check")
        if v["id"] in ids:
            _err(f, f"verify id {v['id']!r} repeated")
        ids.add(v["id"])
        run = v.get("run")
        if run is None:
            _err(f, f"verify {v['id']!r}: no run (use run: {{kind: manual}} for a check no tool can make)")
        rk = run.get("kind")
        if rk not in RUN_KINDS:
            _err(f, f"verify {v['id']!r}: run kind {rk!r} not one of {sorted(RUN_KINDS)}")
        req, opt = RUN_KINDS[rk]
        missing = req - set(run)
        extra = set(run) - req - opt - COMMON_RUN_FIELDS
        if missing:
            _err(f, f"verify {v['id']!r}: {rk} needs {sorted(missing)}")
        if extra:
            _err(f, f"verify {v['id']!r}: {rk} does not take {sorted(extra)}")
        if rk == "http" and not (run.get("url") or (run.get("service") and run.get("port"))):
            _err(f, f"verify {v['id']!r}: http needs url, or service and port")
        if rk == "cloudCli":
            args = run["args"]
            if run["cli"] == "aws":
                if len(args) < 2 or not args[1].startswith(AWS_READ_PREFIXES):
                    _err(f, f"verify {v['id']!r}: aws subcommand must be read-only ({', '.join(AWS_READ_PREFIXES)})")
            elif run["cli"] == "az":
                verbs = [a for a in args if not a.startswith("-")]
                if not any(a in AZ_READ_VERBS for a in verbs):
                    _err(f, f"verify {v['id']!r}: az command must be read-only (show/list)")
            else:
                _err(f, f"verify {v['id']!r}: cli must be aws or az")
        w = v.get("when")
        if w is not None and not (w.get("field") and ("equals" in w or "in" in w)):
            _err(f, f"verify {v['id']!r}: when needs field and equals/in")


def load_sidecars():
    """{kind (lowercase): sidecar dict}, every xrds/*.meta.yaml, validated."""
    out = {}
    for f in sorted(XRDS.glob("*.meta.yaml")):
        d = yaml.safe_load(f.read_text())
        validate(d, f)
        d = dict(d)
        d["role"] = role_of(d)
        d.pop("component", None)
        d["_file"] = f.name
        out[d["kind"].lower()] = d
    return out


# ---- templating and lookups shared by the drift test and airframe-verify ---------------------------
def dig(obj, path):
    """`a.b.c` into nested dicts. `a.list[].b` collects b from every element of list (returns a list)."""
    cur = [obj]
    multi = False
    for part in path.split("."):
        nxt = []
        is_list = part.endswith("[]")
        key = part[:-2] if is_list else part
        for c in cur:
            if isinstance(c, dict) and key in c:
                val = c[key]
                if is_list:
                    multi = True
                    nxt.extend(val if isinstance(val, list) else [])
                else:
                    nxt.append(val)
        cur = nxt
    if multi:
        return cur
    return cur[0] if cur else None


class Unresolved(KeyError):
    pass


def render(tpl, xr):
    """Fill {name}, {namespace}, {spec.x}, {status.x} from an XR. Non-strings pass through."""
    if not isinstance(tpl, str):
        return tpl
    md = xr.get("metadata") or {}

    def sub(m):
        key = m.group(1)
        if key == "name":
            return md.get("name", "")
        if key == "namespace":
            return md.get("namespace", "")
        if key.startswith(("spec.", "status.")):
            v = dig(xr, key)
            if v is None or isinstance(v, (dict, list)):
                raise Unresolved(key)
            return str(v)
        raise Unresolved(key)
    return TEMPLATE_RE.sub(sub, tpl)


def when_matches(when, xr):
    if not when:
        return True
    v = dig(xr, when["field"])
    if "equals" in when:
        return v == when["equals"]
    return v in when["in"]
