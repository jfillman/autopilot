import pytest

from clearance import policy
from clearance.limits import LimitRequest
from clearance.tiers import Tier

from conftest import T0

DENY = [".tekton/**", "cicd.yaml", "CODEOWNERS"]
ALLOW = ["jfillman/*"]
PR_ARGS = {"repo": "jfillman/flight-api", "branch": "agent/t-1", "title": "fix probe",
           "files": [{"path": "charts/values.yaml", "content": "x"}]}


def ctx(gw, tool, args, agent="coding-agent", token="alice", req=None):
    r = gw.open_session(token, agent, req)
    assert r.ok, r
    s = gw.store.get(r.data["session"])
    p = gw.auth.authenticate(token)
    return s, lambda **kw: policy.build_context(kw.get("s", s), kw.get("pid", p.id), kw.get("ptier", p.tier_ceiling),
                                                tool, kw.get("args", args), DENY, ALLOW, kw.get("now", T0))


def decide(gw, tool, args, **kw):
    s, mk = ctx(gw, tool, args)
    return policy.evaluate(mk(**kw))


def test_baseline_pr_is_allowed(gw):
    d = decide(gw, "repo.pr.open", PR_ARGS)
    assert d.allow and d.rule is None


@pytest.mark.parametrize("tool,args,rule", [
    ("k8s.exec", {}, "R002"),
    ("secrets.read", {}, "R002"),
    ("no.such.tool", {}, "R003"),
    ("repo.pr.open", {"repo": "jfillman/x", "branch": "b", "title": "t"}, "R008"),
    ("repo.pr.open", {**PR_ARGS, "repo": "someone-else/x"}, "R014"),
    ("repo.pr.open", {**PR_ARGS, "files": [{"path": ".tekton/pr.yaml", "content": "x"}]}, "R015"),
    ("repo.pr.open", {**PR_ARGS, "files": [{"path": "a/../cicd.yaml", "content": "x"}]}, "R015"),
    ("repo.pr.open", {**PR_ARGS, "repo": "jfillman/gitops-flight-api"}, "R012"),
    ("argo.sync.lower", {"app": "flight-api", "project": "flight-api-prod"}, "R012"),
    ("xr.request", {"app": "a", "env": "production", "kind": "K", "spec": {"a": 1}}, "R012"),
])
def test_each_rule_denies_on_its_own(gw, tool, args, rule):
    assert decide(gw, tool, args).rule == rule


def test_lower_targets_are_allowed(gw):
    assert decide(gw, "argo.sync.lower", {"app": "flight-api", "project": "flight-api-lower"}).allow
    assert decide(gw, "xr.request", {"app": "a", "env": "dev", "kind": "K", "spec": {"a": 1}}).allow
    assert decide(gw, "xr.request", {"app": "a", "env": "pr-12", "kind": "K", "spec": {"a": 1}}).allow


def test_upper_pr_is_propose_only_and_allowed_for_a_definition_that_has_it(gw):
    args = {**PR_ARGS, "repo": "jfillman/gitops-flight-api"}
    assert decide(gw, "repo.pr.open_upper", args).allow


def test_read_only_principal_cannot_propose_either(gw):
    args = {**PR_ARGS, "repo": "jfillman/gitops-flight-api"}
    assert decide(gw, "repo.pr.open_upper", args, ptier=Tier.T0).rule == "R007"


def test_tool_not_in_definition(gw):
    r = gw.open_session("alice", "researcher")
    s = gw.store.get(r.data["session"])
    p = gw.auth.authenticate("alice")
    c = policy.build_context(s, p.id, p.tier_ceiling, "repo.pr.open", PR_ARGS, DENY, ALLOW, T0)
    assert policy.evaluate(c).rule == "R006"


def test_principal_tier_lowers_the_effective_ceiling(gw):
    d = decide(gw, "repo.pr.open", PR_ARGS, ptier=Tier.T0)
    assert d.rule == "R007"


def test_not_the_session_owner(gw):
    assert decide(gw, "catalog.read", {"query": "x"}, pid="user:mallory").rule == "R001"


def test_expired_and_tripped(gw):
    s, mk = ctx(gw, "catalog.read", {"query": "x"})
    from datetime import timedelta
    assert policy.evaluate(mk(now=T0 + timedelta(hours=2))).rule == "R004"
    s.trip("x")
    assert policy.evaluate(mk()).rule == "R005"


def test_budgets_deny(gw):
    s, mk = ctx(gw, "repo.pr.open", PR_ARGS)
    s.used["tool_calls"] = s.limits.tool_calls
    assert policy.evaluate(mk()).rule == "R009"
    s.used["tool_calls"] = 0
    s.used["github_calls"] = s.limits.github_calls - 3
    assert policy.evaluate(mk()).rule == "R010"
    s.used["github_calls"] = 0
    s.used["open_prs"] = s.limits.open_prs
    assert policy.evaluate(mk()).rule == "R011"


def test_policy_fails_closed_when_evaluation_breaks(gw):
    s, mk = ctx(gw, "catalog.read", {"query": "x"})
    c = mk()
    c["tool"]["tier"] = "not-an-int"        # a type error inside CEL
    d = policy.evaluate(c)
    assert not d.allow and d.rule == "R000"


def test_rules_have_unique_ids_and_hints():
    ids = [r.id for r in policy.RULES]
    assert len(ids) == len(set(ids))
    assert all(r.hint and r.message for r in policy.RULES)


# AF-9a: field-level scope (R020).

BEFORE_ENV = "envName: dev\nrollout:\n  replicas: 1\n"


def _field_decision(gw, before, content, field_deny=(), field_allow=()):
    r = gw.open_session("alice", "coding-agent", None)
    assert r.ok, r
    s = gw.store.get(r.data["session"])
    p = gw.auth.authenticate("alice")
    args = {**PR_ARGS, "files": [{"path": "platform/envs/dev.yaml", "before": before, "content": content}]}
    c = policy.build_context(s, p.id, p.tier_ceiling, "repo.pr.open", args, DENY, ALLOW, T0,
                             field_deny=list(field_deny), field_allow=list(field_allow))
    return policy.evaluate(c)


def test_field_deny_blocks_a_denied_key_even_inside_an_allowed_path(gw):
    d = _field_decision(gw, before=BEFORE_ENV,
                         content="envName: dev\nrollout:\n  replicas: 1\nreleaseTracking: {x: 1}\n",
                         field_deny=["/releaseTracking"])
    assert not d.allow and d.rule == "R020"


def test_field_deny_allows_an_unrelated_change(gw):
    d = _field_decision(gw, before=BEFORE_ENV,
                         content="envName: dev\nrollout:\n  replicas: 2\n",
                         field_deny=["/releaseTracking"])
    assert d.allow


def test_field_allow_restricts_to_the_listed_fields(gw):
    d = _field_decision(gw, before=BEFORE_ENV,
                         content="envName: dev\nrollout:\n  replicas: 1\ncomponents: [{type: redis}]\n",
                         field_allow=["/rollout/*"])
    assert not d.allow and d.rule == "R020"


def test_field_allow_permits_a_listed_field(gw):
    d = _field_decision(gw, before=BEFORE_ENV,
                         content="envName: dev\nrollout:\n  replicas: 3\n",
                         field_allow=["/rollout/*"])
    assert d.allow


def test_field_scope_skips_files_with_no_before(gw):
    d = _field_decision(gw, before=None, content="envName: dev\nreleaseTracking: {x: 1}\n",
                         field_deny=["/releaseTracking"])
    assert d.allow
