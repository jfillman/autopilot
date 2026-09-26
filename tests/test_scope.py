import pytest

from clearance.scope import BadPath, normalize, path_violations, repo_allowed

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
