"""The policy decision: deny-if rules written in CEL, first match wins.

CEL is already the isolation boundary elsewhere in Hangar (Tekton Triggers filters), so
this adds no new engine. Rules are data: each has a stable id that goes back to the agent
with a fix hint, which is what lets an agent retry against a deterministic gate instead of
guessing. Anything that raises while evaluating is a deny (R000): the gate fails closed.
"""
from __future__ import annotations

import copy
import fnmatch
import json
import posixpath
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import celpy

import yaml

from .limits import Limits
from .scope import BadPath, diff_pointers, field_outside_allow, field_violations, normalize, path_violations, repo_allowed
from .vendor.cicd_scope import scope_problems
from .session import Session
from .tiers import TOOLS, TRIPWIRES, Tier, ToolSpec

LOWER_ENVS = frozenset({"dev", "development", "sandbox", "scratch"})


@dataclass(frozen=True)
class Rule:
    id: str
    name: str
    expr: str
    message: str
    hint: str


RULES: tuple[Rule, ...] = (
    Rule("R001", "not-session-owner", "!ctx.owns_session",
         "this session belongs to a different principal",
         "Open your own session. Session ids are not transferable."),
    Rule("R002", "tripwire", "ctx.tripwire",
         "this capability is never exposed to any agent",
         "There is no way to do this through Clearance. Ask a human. This attempt has tripped the session breaker."),
    Rule("R003", "unknown-tool", "!ctx.known",
         "unknown tool", "List the available tools with tools/list."),
    Rule("R004", "session-expired", "session.expired",
         "the session has ended", "Open a new session."),
    Rule("R005", "breaker-tripped", "session.tripped",
         "the session breaker is tripped", "A human must review this session; open a new one after review."),
    Rule("R006", "tool-not-permitted", "!(tool.name in effective.tools)",
         "tool is not permitted for this session",
         "Use a tool in your definition, or ask for the definition to be changed by a reviewed commit."),
    # A propose-only tool (T2) opens a PR that a human merges; it applies nothing. So the ceiling
    # caps what a session can APPLY, and proposing only needs enough tier to open a PR (T1).
    Rule("R007", "above-ceiling", "tool.tier > effective.tier && !(tool.propose_only && effective.tier >= 1)",
         "tool tier is above this session's ceiling",
         "Propose the change as a PR for a human to merge instead."),
    Rule("R008", "bad-arguments", "ctx.missing_args > 0",
         "required arguments are missing", "Supply every required argument; see the tool schema."),
    Rule("R009", "tool-calls-exhausted", "session.tool_calls_left < 1",
         "tool-call budget is spent", "Stop and report progress; do not retry."),
    Rule("R010", "github-budget", "session.github_left < cost.github",
         "GitHub API budget would be exceeded", "Batch changes into fewer calls, or stop and report."),
    Rule("R011", "pr-budget", "cost.prs > 0 && session.prs_left < 1",
         "open-PR budget is spent", "Update an existing PR instead of opening another."),
    Rule("R012", "wrong-environment", "ctx.target_mismatch",
         "this tool acts on lower environments only, but the target is an upper environment",
         "Use repo.pr.open_upper to propose the change; a human merge applies it."),
    Rule("R013", "upper-must-propose", "tool.tier == 2 && !tool.propose_only",
         "T2 tools may only propose", "This tool cannot apply changes to an upper environment."),
    Rule("R014", "repo-not-allowed", "ctx.has_repo && !ctx.repo_ok",
         "repository is not on this agent's allowlist", "Work only in repositories your definition names."),
    Rule("R015", "path-not-allowed", "ctx.path_violations > 0",
         "the change touches a protected path", "Remove the protected paths from the change; agents cannot edit their own controls."),
    Rule("R016", "spawn-refused", "false",
         "a spawn was refused (raised by the handler: depth or children budget)",
         "Spawn fewer or smaller child runs, or finish the ones running."),
    Rule("R017", "api-not-allowed", "ctx.has_service && !ctx.service_ok",
         "that application API is not on this agent's allowlist", "Call only the services your definition names under profile.apis.allow."),
    Rule("R022", "glidepath-app-not-allowed", "ctx.has_glidepath_app && !ctx.glidepath_app_ok",
         "that app is not on this agent's Glidepath allowlist", "Read only the apps your definition names under profile.glidepath.apps."),
    Rule("R023", "airframe-app-not-allowed", "ctx.has_airframe_app && !ctx.airframe_app_ok",
         "that app is not on this agent's Airframe allowlist", "Read only the apps your definition names under profile.airframe.apps."),
    Rule("R019", "artifact-too-large", "ctx.artifact_bytes > 1048576",
         "the artifact is larger than 1 MiB", "Store a smaller summary; artifacts are outputs, not datasets."),
    Rule("R018", "chat-outside-session", "ctx.is_chat && !ctx.session_kind",
         "chat tools exist only for session agents", "Only an interactive session agent has a human on the other end."),
    Rule("R020", "field-not-allowed", "ctx.field_violations > 0",
         "the change touches a field this agent may not set",
         "Remove that field from the change and let a human or the release pipeline set it instead."),
    # Glidepath's cicd.yaml scope. The decision carries the GP-SCOPE rule id and hint of the first
    # problem (and every problem under `problems`), not R021, so the agent sees the same rule id the
    # pull request's own `glidepath validate --agent` check would report.
    Rule("R021", "cicd-scope", "ctx.cicd_scope_problems > 0",
         "the cicd.yaml change is outside what an agent may change",
         "Change only the fields Glidepath's contract marks open; see the GP-SCOPE rule in the decision."),
)

# Glidepath's contract, vendored byte for byte from glidepath (contract/glidepath-contract.json) with
# the code that reads it (vendor/cicd_scope.py); tests/test_cicd_scope.py fails if either drifts.
CICD_SCOPE = json.loads((Path(__file__).resolve().parent / "vendor" / "glidepath-contract.json").read_text())["scope"]

_ENV = celpy.Environment()
_PROGRAMS = {r.id: _ENV.program(_ENV.compile(r.expr)) for r in RULES}
_ERROR = Rule("R000", "policy-error", "", "the policy could not be evaluated",
              "This is a platform fault, not something to retry around. It fails closed.")


@dataclass(frozen=True)
class Decision:
    allow: bool
    rule: str | None
    name: str | None
    message: str
    hint: str
    tier: int | None = None
    problems: tuple[dict[str, str], ...] = field(default=())

    def as_dict(self) -> dict[str, Any]:
        out = {"allow": self.allow, "rule": self.rule, "name": self.name,
               "message": self.message, "hint": self.hint, "tier": self.tier}
        if self.problems:
            out["problems"] = [dict(p) for p in self.problems]
        return out


def env_tier_of(spec: ToolSpec, args: dict[str, Any]) -> str:
    """Which environment tier do these arguments actually point at?"""
    if spec.target == "none":
        return "none"
    project = args.get("project")
    if isinstance(project, str):
        return "lower" if project.endswith("-lower") else "upper"
    env = args.get("env")
    if isinstance(env, str):
        return "lower" if (env in LOWER_ENVS or env.startswith(("pr-", "agent-"))) else "upper"
    repo = args.get("repo")
    if isinstance(repo, str):
        # gitops-<app> repos carry the upper environments.
        return "upper" if repo.split("/")[-1].startswith("gitops-") else "lower"
    return spec.target


def _file_field_violations(files: list[dict[str, Any]], field_deny: list[str],
                            field_allow: list[str]) -> int:
    """AF-9a: for each file carrying both `before` (its prior content) and `content` (the
    proposed new content), parse both as YAML and diff them into JSON pointers, then count how
    many touch a denied field or (with an allowlist set) fall outside every allowed field. Only
    applies to files that parse as YAML/JSON mappings or lists - a file Clearance can't structure
    isn't one AF-9a's field scope reasons about, and the whole-file path scope (R015) still
    covers it. A file with no `before` (a new file) has nothing to diff against, so it's exempt -
    the agent is authoring it whole, within whatever path scope already allows."""
    if not field_deny and not field_allow:
        return 0
    total = 0
    for f in files:
        if not isinstance(f, dict):
            continue
        before, content = f.get("before"), f.get("content")
        if not isinstance(before, str) or not isinstance(content, str):
            continue
        try:
            before_data, after_data = yaml.safe_load(before), yaml.safe_load(content)
        except yaml.YAMLError:
            continue
        if not isinstance(before_data, (dict, list)) or not isinstance(after_data, (dict, list)):
            continue
        pointers = diff_pointers(before_data, after_data)
        total += len(field_violations(pointers, field_deny)) + len(field_outside_allow(pointers, field_allow))
    return total


def merge_patch(target: Any, patch: Any) -> Any:
    """RFC 7386 JSON merge patch: dicts merge recursively, null deletes a key, anything else replaces."""
    if not isinstance(patch, dict):
        return copy.deepcopy(patch)
    out = copy.deepcopy(target) if isinstance(target, dict) else {}
    for k, v in patch.items():
        if v is None:
            out.pop(k, None)
        else:
            out[k] = merge_patch(out.get(k), v)
    return out


def _is_cicd_yaml(path: Any) -> bool:
    try:
        return posixpath.basename(normalize(path)) == "cicd.yaml"
    except BadPath:
        return False            # path_violations (R015) already denies an unsafe path


def _whole_file(path: str, why: str) -> dict[str, str]:
    return {"pointer": "", "rule": "GP-SCOPE-001", "message": f"{path}: {why}",
            "hint": "Send cicd.yaml as YAML (a mapping), with `before` set to its current content, "
                    "so Clearance can tell which fields change."}


def cicd_scope_problems(files: list[dict[str, Any]], scope: dict = CICD_SCOPE) -> list[dict[str, str]]:
    """Glidepath's GP-SCOPE rules on every changed cicd.yaml, at any depth.

    base: the file's `before` (its current content, the same field AF-9a diffs). No `before` means
    the file is new, so every field counts as added, and a new cicd.yaml with apiVersion/kind is a
    human's change (GP-SCOPE-001). Leaving `before` out therefore never widens what passes.
    head: `content`, parsed as YAML; with `op: merge-patch` (the planner's FileChange) it is a merge
    patch (a dict, or YAML text of one) applied to the base first. Anything that doesn't parse to a
    mapping fails closed as a whole-file change."""
    problems: list[dict[str, str]] = []
    for f in files:
        if not isinstance(f, dict) or not _is_cicd_yaml(f.get("path")):
            continue
        path = f["path"]
        before, content = f.get("before"), f.get("content")
        try:
            base = yaml.safe_load(before) if isinstance(before, str) else before
            head = yaml.safe_load(content) if isinstance(content, str) else content
        except yaml.YAMLError:
            problems.append(_whole_file(path, "does not parse as YAML"))
            continue
        if base is not None and not isinstance(base, dict):
            problems.append(_whole_file(path, "the current content (`before`) is not a YAML mapping"))
            continue
        if f.get("op") == "merge-patch":
            if not isinstance(head, dict):
                problems.append(_whole_file(path, "a merge-patch must be a mapping"))
                continue
            head = merge_patch(base or {}, head)
        if not isinstance(head, dict):
            problems.append(_whole_file(path, "the new content is not a YAML mapping"))
            continue
        try:
            found = scope_problems(base, head, scope)
        except Exception as e:  # noqa: BLE001 - a rule that can't be evaluated denies, never allows
            problems.append(_whole_file(path, f"the scope check could not be evaluated ({type(e).__name__})"))
            continue
        for p in found:
            problems.append({**p, "message": f"{path}: {p['message']}"})
    return problems


# glidepath-api's own path rules (api/openapi.yaml): an argument that would not be a valid path
# segment there is a bad argument here, so nothing odd is ever put into a URL.
_GP_ARGS = {
    "app": re.compile(r"^[a-z0-9][a-z0-9-]{0,62}$"),
    "result": re.compile(r"^[0-9a-f-]{36}$"),
    "taskRun": re.compile(r"^[a-z0-9][a-z0-9.-]{0,252}$"),
    "pipelinerun": re.compile(r"^[a-z0-9][a-z0-9.-]{0,252}$"),
    "chainId": re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$"),
    # Phase 4 (environments/promote/rollback): glidepath-api's NameRE, imageRepoRE and imageTagRE.
    "env": re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$"),
    "cluster": re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$"),
    "sourceEnv": re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$"),
    "sourceCluster": re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$"),
    "sourceImageRepo": re.compile(r"^[a-z0-9][a-z0-9._/:-]{0,254}$"),
    "sourceImageTag": re.compile(r"^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$"),
    # Goes in the body, not a path; glidepath-api caps it at 256 and checks it against the rollback plan.
    "targetReleaseId": re.compile(r"^[\x21-\x7e]{1,256}$"),
}


def glidepath_bad_args(args: dict[str, Any]) -> int:
    bad = 0
    for k, pat in _GP_ARGS.items():
        if k in args and not (isinstance(args[k], str) and pat.fullmatch(args[k])):
            bad += 1
    for k, hi in (("sinceHours", 744), ("limit", 1000)):
        if k in args and not (isinstance(args[k], int) and not isinstance(args[k], bool) and 1 <= args[k] <= hi):
            bad += 1
    if "dryRun" in args and not isinstance(args["dryRun"], bool):
        bad += 1
    if "reason" in args and not (isinstance(args["reason"], str) and 1 <= len(args["reason"].strip()) <= 1024):
        bad += 1
    return bad


def glidepath_promote_source_ok(args: dict[str, Any]) -> bool:
    """A promotion names exactly one whole source pair: a deployed environment, or (a first deploy) an image."""
    env_keys, image_keys = ("sourceEnv", "sourceCluster"), ("sourceImageRepo", "sourceImageTag")
    given = {k for k in env_keys + image_keys if args.get(k) not in (None, "")}
    return given in (set(env_keys), set(image_keys))


def glidepath_app_allowed(app: str, allow: list[str]) -> bool:
    return any(fnmatch.fnmatchcase(app, g) for g in allow)


# airframe.* arguments. Shape only: every namespace and name the tools read is built from `app` (and `env`), so a
# value that is not a Kubernetes name never reaches a path; the adapter's own allowlist is the second wall.
_AF_ARGS = {
    "app": re.compile(r"^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$"),
    "env": re.compile(r"^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$"),
    "name": re.compile(r"^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$"),
    "kind": re.compile(r"^[A-Za-z][A-Za-z0-9]{0,62}$"),
    "rule": re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$"),
}
_AF_SECTIONS = frozenset({"chart", "xrds", "components", "targets", "kinds"})
_AF_DOC_NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.ya?ml$")
_AF_MAX_DOCS, _AF_MAX_BYTES = 20, 256 << 10


def _af_document_ok(d: Any) -> bool:
    if not isinstance(d, dict) or set(d) - {"path", "content"} or not isinstance(d.get("content"), str):
        return False
    try:
        path = normalize(d.get("path"))
    except BadPath:
        return False
    return bool(_AF_DOC_NAME.match(posixpath.basename(path))) and len(d["content"].encode()) <= _AF_MAX_BYTES


def airframe_bad_args(args: dict[str, Any]) -> int:
    bad = sum(1 for k, pat in _AF_ARGS.items() if k in args and not (isinstance(args[k], str) and pat.match(args[k])))
    if "section" in args and args["section"] not in _AF_SECTIONS:
        bad += 1
    if "documents" in args:
        docs = args["documents"]
        bad += not (isinstance(docs, list) and 1 <= len(docs) <= _AF_MAX_DOCS and all(_af_document_ok(d) for d in docs))
    if "mode" in args and args["mode"] not in ("values", "xr"):
        bad += 1
    if "render" in args and not isinstance(args["render"], bool):
        bad += 1
    if "spec" in args and not isinstance(args["spec"], dict):
        bad += 1
    if "baseCicd" in args and not (isinstance(args["baseCicd"], dict)
                                   or (isinstance(args["baseCicd"], str) and len(args["baseCicd"]) <= _AF_MAX_BYTES)):
        bad += 1
    return bad


def build_context(session: Session, principal_id: str, principal_tier: Tier, tool: str,
                  args: dict[str, Any], deny_globs: list[str], repo_allow: list[str],
                  now, api_allow: list[str] | None = None, agent_kind: str = "task",
                  field_deny: list[str] | None = None, field_allow: list[str] | None = None,
                  glidepath_apps: list[str] | None = None,
                  airframe_apps: list[str] | None = None) -> dict[str, Any]:
    spec = TOOLS.get(tool)
    known = spec is not None
    files = args.get("files") if isinstance(args.get("files"), list) else []
    paths = [f.get("path", "") for f in files if isinstance(f, dict)]
    repo = args.get("repo") if isinstance(args.get("repo"), str) else None
    missing = 0
    if spec:
        missing = sum(1 for a in spec.required_args if a not in args or args[a] in (None, "", []))
        if spec.writes_files and not paths:
            missing += 1
    service = args.get("service") if isinstance(args.get("service"), str) else None
    api_path = args.get("path") if isinstance(args.get("path"), str) else None
    if tool == "app.api.get" and (api_path is None or not api_path.startswith("/") or ".." in api_path
                                  or "?" in api_path or "#" in api_path or "\\" in api_path):
        missing += 1            # a bad path is a bad argument
    gp_app = args.get("app") if tool.startswith("glidepath.") and isinstance(args.get("app"), str) else None
    if tool.startswith("glidepath.") and spec:
        missing += glidepath_bad_args(args)
        if tool == "glidepath.promote" and not glidepath_promote_source_ok(args):
            missing += 1
    # Every airframe.* call that names an app is scoped by it; the ones that read no app state (capabilities,
    # explain, plan, and validate without an app) are not, so an agent with no apps listed reads no cluster state.
    af_app = args.get("app") if tool.startswith("airframe.") and isinstance(args.get("app"), str) else None
    if tool.startswith("airframe.") and spec:
        missing += airframe_bad_args(args)
    eff_tier = min(session.limits.tier_ceiling, principal_tier)
    tier_val = int(spec.tier) if spec else 0
    cicd = cicd_scope_problems(files) if (files and spec and spec.writes_files) else []
    return {
        "cicd_scope": cicd,
        "tool": {"name": tool, "tier": tier_val,
                 "propose_only": bool(spec.propose_only) if spec else False},
        "effective": {"tools": sorted(session.limits.tools), "tier": int(eff_tier)},
        "session": {"expired": session.expired(now), "tripped": session.tripped,
                    "tool_calls_left": session.remaining("tool_calls"),
                    "github_left": session.remaining("github_calls"),
                    "prs_left": session.remaining("open_prs")},
        "cost": {"github": spec.github_cost if spec else 0,
                 "prs": 1 if tool in ("repo.pr.open", "repo.pr.open_upper") else 0},
        "ctx": {
            "owns_session": session.principal == principal_id,
            "tripwire": tool in TRIPWIRES,
            "known": known,
            "missing_args": missing,
            "target_mismatch": bool(spec and spec.target == "lower" and env_tier_of(spec, args) == "upper"),
            "has_repo": repo is not None and bool(spec and spec.writes_files),
            "repo_ok": repo_allowed(repo, repo_allow) if repo else True,
            "path_violations": len(path_violations(paths, deny_globs)) if paths else 0,
            "field_violations": _file_field_violations(files, field_deny or [], field_allow or []) if files else 0,
            "cicd_scope_problems": len(cicd),
            "has_service": tool == "app.api.get" and service is not None,
            "service_ok": service in (api_allow or []) if service else True,
            "has_glidepath_app": gp_app is not None,
            "glidepath_app_ok": glidepath_app_allowed(gp_app, glidepath_apps or []) if gp_app else True,
            "has_airframe_app": af_app is not None,
            "airframe_app_ok": glidepath_app_allowed(af_app, airframe_apps or []) if af_app else True,
            "is_chat": tool in ("chat.recv", "chat.send"),
            "artifact_bytes": len(args.get("content", "")) if tool == "artifact.put" and isinstance(args.get("content"), str) else 0,
            "session_kind": agent_kind == "session",
        },
    }


def evaluate(context: dict[str, Any]) -> Decision:
    try:
        act = {k: celpy.json_to_cel(v) for k, v in context.items()}
        for rule in RULES:
            result = _PROGRAMS[rule.id].evaluate(act)
            if isinstance(result, Exception):
                raise result
            if bool(result):
                if rule.id == "R021":
                    probs = tuple(context["cicd_scope"])
                    first = probs[0]
                    return Decision(False, first["rule"], rule.name,
                                    "; ".join(p["message"] for p in probs), first["hint"],
                                    context["tool"]["tier"], probs)
                return Decision(False, rule.id, rule.name, rule.message, rule.hint, context["tool"]["tier"])
    except Exception as e:  # noqa: BLE001 - fail closed on anything
        return Decision(False, _ERROR.id, _ERROR.name, f"{_ERROR.message}: {type(e).__name__}", _ERROR.hint)
    return Decision(True, None, None, "allowed", "", context["tool"]["tier"])
