"""The policy decision: deny-if rules written in CEL, first match wins.

CEL is already the isolation boundary elsewhere in Hangar (Tekton Triggers filters), so
this adds no new engine. Rules are data: each has a stable id that goes back to the agent
with a fix hint, which is what lets an agent retry against a deterministic gate instead of
guessing. Anything that raises while evaluating is a deny (R000): the gate fails closed.

The rules here are Clearance's own, about any tool. A pack (pack.py) adds tools, rules and the
context its rules read; Policy compiles core plus packs into one rule list. Each rule carries an
explicit `order`, because first match wins: the order is part of what a rule means, and a pack's
rule can sit between two core rules (clearance.packs.hangar's R022/R023 run before R019).
"""
from __future__ import annotations

from dataclasses import dataclass, field
from functools import lru_cache
from typing import Any, Sequence

import celpy

import yaml

from .scope import diff_pointers, field_outside_allow, field_violations, path_violations, repo_allowed
from .session import Session
from .tiers import CORE_TOOLS, TRIPWIRES, Tier, ToolSpec, registry

LOWER_ENVS = frozenset({"dev", "development", "sandbox", "scratch"})


@dataclass(frozen=True)
class Rule:
    id: str
    name: str
    expr: str
    message: str
    hint: str
    order: int      # evaluation position, first match wins; core rules are spaced by 10 so a pack's can sit between


RULES: tuple[Rule, ...] = (
    Rule("R001", "not-session-owner", "!ctx.owns_session",
         "this session belongs to a different principal",
         "Open your own session. Session ids are not transferable.", order=10),
    Rule("R002", "tripwire", "ctx.tripwire",
         "this capability is never exposed to any agent",
         "There is no way to do this through Clearance. Ask a human. This attempt has tripped the session breaker.", order=20),
    Rule("R003", "unknown-tool", "!ctx.known",
         "unknown tool", "List the available tools with tools/list.", order=30),
    Rule("R004", "session-expired", "session.expired",
         "the session has ended", "Open a new session.", order=40),
    Rule("R005", "breaker-tripped", "session.tripped",
         "the session breaker is tripped", "A human must review this session; open a new one after review.", order=50),
    Rule("R006", "tool-not-permitted", "!(tool.name in effective.tools)",
         "tool is not permitted for this session",
         "Use a tool in your definition, or ask for the definition to be changed by a reviewed commit.", order=60),
    # A propose-only tool (T2) opens a PR that a human merges; it applies nothing. So the ceiling
    # caps what a session can APPLY, and proposing only needs enough tier to open a PR (T1).
    Rule("R007", "above-ceiling", "tool.tier > effective.tier && !(tool.propose_only && effective.tier >= 1)",
         "tool tier is above this session's ceiling",
         "Propose the change as a PR for a human to merge instead.", order=70),
    Rule("R008", "bad-arguments", "ctx.missing_args > 0",
         "required arguments are missing", "Supply every required argument; see the tool schema.", order=80),
    Rule("R009", "tool-calls-exhausted", "session.tool_calls_left < 1",
         "tool-call budget is spent", "Stop and report progress; do not retry.", order=90),
    Rule("R010", "github-budget", "session.github_left < cost.github",
         "GitHub API budget would be exceeded", "Batch changes into fewer calls, or stop and report.", order=100),
    Rule("R011", "pr-budget", "cost.prs > 0 && session.prs_left < 1",
         "open-PR budget is spent", "Update an existing PR instead of opening another.", order=110),
    Rule("R012", "wrong-environment", "ctx.target_mismatch",
         "this tool acts on lower environments only, but the target is an upper environment",
         "Use repo.pr.open_upper to propose the change; a human merge applies it.", order=120),
    Rule("R013", "upper-must-propose", "tool.tier == 2 && !tool.propose_only",
         "T2 tools may only propose", "This tool cannot apply changes to an upper environment.", order=130),
    Rule("R014", "repo-not-allowed", "ctx.has_repo && !ctx.repo_ok",
         "repository is not on this agent's allowlist", "Work only in repositories your definition names.", order=140),
    Rule("R015", "path-not-allowed", "ctx.path_violations > 0",
         "the change touches a protected path", "Remove the protected paths from the change; agents cannot edit their own controls.", order=150),
    Rule("R016", "spawn-refused", "false",
         "a spawn was refused (raised by the handler: depth or children budget)",
         "Spawn fewer or smaller child runs, or finish the ones running.", order=160),
    Rule("R017", "api-not-allowed", "ctx.has_service && !ctx.service_ok",
         "that application API is not on this agent's allowlist", "Call only the services your definition names under profile.apis.allow.", order=170),
    Rule("R019", "artifact-too-large", "ctx.artifact_bytes > 1048576",
         "the artifact is larger than 1 MiB", "Store a smaller summary; artifacts are outputs, not datasets.", order=200),
    Rule("R018", "chat-outside-session", "ctx.is_chat && !ctx.session_kind",
         "chat tools exist only for session agents", "Only an interactive session agent has a human on the other end.", order=210),
    Rule("R020", "field-not-allowed", "ctx.field_violations > 0",
         "the change touches a field this agent may not set",
         "Remove that field from the change and let a human or the release pipeline set it instead.", order=220),
)

_ENV = celpy.Environment()
_ERROR = Rule("R000", "policy-error", "", "the policy could not be evaluated",
              "This is a platform fault, not something to retry around. It fails closed.", order=0)


@lru_cache(maxsize=None)
def _program(expr: str):
    return _ENV.program(_ENV.compile(expr))


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


class Policy:
    """Core rules plus every pack's, in `order`, and the context they read.

    Packs are duck-typed (pack.Pack): `tools`, `rules`, and optionally `context(tool, spec, args,
    session, definition) -> (ctx entries, extra bad-argument count)` and `decide(rule, context) ->
    Decision | None`, which turns a matched rule into a structured decision. Nothing registers
    itself: a deployment passes the packs it runs."""

    def __init__(self, packs: Sequence[Any] = ()):
        self.packs = tuple(packs)
        self.tools: dict[str, ToolSpec] = registry(CORE_TOOLS, *(p.tools for p in self.packs))
        rules = RULES + tuple(r for p in self.packs for r in p.rules)
        for attr in ("id", "order"):
            seen = [getattr(r, attr) for r in rules]
            dup = sorted({v for v in seen if seen.count(v) > 1})
            if dup:
                raise ValueError(f"rule {attr} registered twice: {dup}")
        self.rules: tuple[Rule, ...] = tuple(sorted(rules, key=lambda r: r.order))
        self._programs = {r.id: _program(r.expr) for r in self.rules}

    def build_context(self, session: Session, principal_id: str, principal_tier: Tier, tool: str,
                      args: dict[str, Any], deny_globs: list[str], repo_allow: list[str],
                      now, api_allow: list[str] | None = None, agent_kind: str = "task",
                      field_deny: list[str] | None = None, field_allow: list[str] | None = None,
                      definition: Any = None) -> dict[str, Any]:
        spec = self.tools.get(tool)
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
        eff_tier = min(session.limits.tier_ceiling, principal_tier)
        tier_val = int(spec.tier) if spec else 0
        ctx = {
            "owns_session": session.principal == principal_id,
            "tripwire": tool in TRIPWIRES,
            "known": known,
            "missing_args": missing,
            "target_mismatch": bool(spec and spec.target == "lower" and env_tier_of(spec, args) == "upper"),
            "has_repo": repo is not None and bool(spec and spec.writes_files),
            "repo_ok": repo_allowed(repo, repo_allow) if repo else True,
            "path_violations": len(path_violations(paths, deny_globs)) if paths else 0,
            "field_violations": _file_field_violations(files, field_deny or [], field_allow or []) if files else 0,
            "has_service": tool == "app.api.get" and service is not None,
            "service_ok": service in (api_allow or []) if service else True,
            "is_chat": tool in ("chat.recv", "chat.send"),
            "artifact_bytes": len(args.get("content", "")) if tool == "artifact.put" and isinstance(args.get("content"), str) else 0,
            "session_kind": agent_kind == "session",
        }
        for p in self.packs:
            if p.context is None:
                continue
            extra, bad = p.context(tool, spec, args, session, definition)
            clash = set(extra) & set(ctx)
            if clash:
                raise ValueError(f"pack {p.name!r} redefines context keys {sorted(clash)}")
            ctx.update(extra)
            ctx["missing_args"] += bad
        return {
            "tool": {"name": tool, "tier": tier_val,
                     "propose_only": bool(spec.propose_only) if spec else False},
            "effective": {"tools": sorted(session.limits.tools), "tier": int(eff_tier)},
            "session": {"expired": session.expired(now), "tripped": session.tripped,
                        "tool_calls_left": session.remaining("tool_calls"),
                        "github_left": session.remaining("github_calls"),
                        "prs_left": session.remaining("open_prs")},
            "cost": {"github": spec.github_cost if spec else 0,
                     "prs": 1 if tool in ("repo.pr.open", "repo.pr.open_upper") else 0},
            "ctx": ctx,
        }

    def evaluate(self, context: dict[str, Any]) -> Decision:
        try:
            act = {k: celpy.json_to_cel(v) for k, v in context.items()}
            for rule in self.rules:
                result = self._programs[rule.id].evaluate(act)
                if isinstance(result, Exception):
                    raise result
                if bool(result):
                    for p in self.packs:
                        d = p.decide(rule, context) if p.decide is not None else None
                        if d is not None:
                            return d
                    return Decision(False, rule.id, rule.name, rule.message, rule.hint, context["tool"]["tier"])
        except Exception as e:  # noqa: BLE001 - fail closed on anything
            return Decision(False, _ERROR.id, _ERROR.name, f"{_ERROR.message}: {type(e).__name__}", _ERROR.hint)
        return Decision(True, None, None, "allowed", "", context["tool"]["tier"])


# Core rules and tools only, no packs: what a deployment with no packs runs, and what the module-level
# helpers below use. A Gateway builds its own Policy from the packs it was given.
CORE = Policy()
build_context = CORE.build_context
evaluate = CORE.evaluate
