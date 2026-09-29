import pytest

from clearance.scope import (
    BadPath, diff_pointers, field_outside_allow, field_violations, normalize, path_violations,
    repo_allowed,
)

DENY = [".tekton/**", "**/.tekton/**", "cicd.yaml", "**/cicd.yaml", "CODEOWNERS", "**/CODEOWNERS", ".github/**",
        "**/appproject*.yaml"]


@pytest.mark.parametrize("p", ["src/app.py", "charts/x/values.yaml", "README.md"])
def test_ordinary_paths_pass(p):
    assert path_violations([p], DENY) == []


@pytest.mark.parametrize("p", [
    ".tekton/pr.yaml", "sub/.tekton/pr.yaml", "cicd.yaml", "apps/x/cicd.yaml", "CODEOWNERS",
    "docs/CODEOWNERS", ".github/workflows/ci.yaml", "charts/appproject-lower.yaml",
])
def test_protected_paths_are_violations(p):
    assert path_violations([p], DENY) == [p]


@pytest.mark.parametrize("p", ["a/../.tekton/pr.yaml", "./cicd.yaml", "a/./b/../../cicd.yaml"])
def test_traversal_cannot_smuggle_past_a_glob(p):
    assert path_violations([p], DENY) == [p]


@pytest.mark.parametrize("p", ["../etc/passwd", "/etc/passwd", "a\\b", "", "a\x00b", ".."])
def test_unsafe_paths_are_violations_not_exceptions(p):
    assert path_violations([p], DENY) == [p]
    with pytest.raises(BadPath):
        normalize(p)


def test_single_star_does_not_cross_directories():
    assert path_violations(["a/b/appproject-x.yaml"], ["*/appproject*.yaml"]) == []
    assert path_violations(["a/appproject-x.yaml"], ["*/appproject*.yaml"]) == ["a/appproject-x.yaml"]


def test_repo_allow():
    assert repo_allowed("jfillman/flight-api", ["jfillman/*"])
    assert not repo_allowed("evil/flight-api", ["jfillman/*"])
    assert not repo_allowed("jfillman/flight-api", [])
    assert not repo_allowed("jfillman/a/b", ["jfillman/*"])
    assert not repo_allowed("/jfillman/a", ["jfillman/*"])


# AF-9a: field-level (JSON-pointer) scope.

def test_diff_pointers_finds_the_precise_changed_leaf():
    before = {"envName": "dev", "components": [{"type": "redis", "spec": {"size": "small"}}]}
    after = {"envName": "dev", "components": [{"type": "redis", "spec": {"size": "large"}}]}
    assert diff_pointers(before, after) == {"/components/0/spec/size"}


def test_diff_pointers_no_change_is_empty():
    d = {"a": 1, "b": [1, 2, 3]}
    assert diff_pointers(d, dict(d)) == set()


def test_diff_pointers_added_and_removed_keys():
    before = {"a": 1}
    after = {"a": 1, "b": 2}
    assert diff_pointers(before, after) == {"/b"}
    assert diff_pointers(after, before) == {"/b"}


def test_diff_pointers_list_length_change_is_coarse():
    before = {"components": [{"type": "redis"}]}
    after = {"components": [{"type": "redis"}, {"type": "rabbitmq"}]}
    assert diff_pointers(before, after) == {"/components"}


def test_field_violations_exact_and_subtree():
    pointers = {"/release/image/tag", "/env/0/value"}
    assert field_violations(pointers, ["/release"]) == ["/release/image/tag"]
    assert field_violations(pointers, ["/env/*/value"]) == ["/env/0/value"]
    assert field_violations(pointers, ["/nowhere"]) == []


def test_field_outside_allow_empty_allowlist_means_no_restriction():
    assert field_outside_allow({"/anything"}, []) == []


def test_field_outside_allow_restricts_to_the_allowlist():
    pointers = {"/env/0/value", "/rollout/replicas"}
    assert field_outside_allow(pointers, ["/env/*"]) == ["/rollout/replicas"]
