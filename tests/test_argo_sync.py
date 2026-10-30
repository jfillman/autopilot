"""argo_sync.ArgoSync: one merge PATCH of a lower Application's `operation`, only after the Application itself is
checked (its namespace, its real project, nothing in flight), carrying the resourceVersion that was read."""
import json

import pytest

from autopilot.argo_sync import ArgoSync, ArgoSyncError
from clearance.dispatch import Denied

PATH = "/apis/argoproj.io/v1alpha1/namespaces/argocd-apps/applications/gate-api-dev"
WHO = {"agent": "coding-agent", "task": "t-1", "session": "s-1", "principal": "user:jfillman"}


class Reader:
    def __init__(self, objs):
        self.objs, self.paths = objs, []

    def get(self, path, query=None):
        self.paths.append(path)
        return self.objs.get(path)


def app(project="gate-api-lower", rv="42", **extra):
    return {"metadata": {"name": "gate-api-dev", "namespace": "argocd-apps", "resourceVersion": rv},
            "spec": {"project": project}, "status": {}, **extra}


def syncer(obj, status=200, tmp_path=None):
    sent = []

    def opener(req, timeout, ctx):
        sent.append((req.get_method(), req.full_url, req.headers, json.loads(req.data)))
        return status, b"{}"
    token = tmp_path / "token"
    token.write_text("tok\n")
    s = ArgoSync(Reader({PATH: obj} if obj else {}), ("argocd", "argocd-apps"), server="https://k8s",
                 token_file=token, ca_file=None, opener=opener)
    return s, sent


def test_a_lower_app_gets_one_merge_patch_naming_who_asked(tmp_path):
    s, sent = syncer(app(), tmp_path=tmp_path)
    r = s.sync("gate-api-dev", "gate-api-lower", WHO)
    assert r["requested"] and r["namespace"] == "argocd-apps"
    [(method, url, headers, body)] = sent
    assert method == "PATCH" and url == "https://k8s" + PATH
    assert headers["Content-type"] == "application/merge-patch+json" and headers["Authorization"] == "Bearer tok"
    assert body == {"metadata": {"resourceVersion": "42"}, "operation": {
        "initiatedBy": {"username": "clearance:coding-agent"},
        "info": [{"name": "principal", "value": "user:jfillman"}, {"name": "session", "value": "s-1"},
                 {"name": "task", "value": "t-1"}],
        "sync": {}}}


@pytest.mark.parametrize("obj, project, rule", [
    (app(), "gate-api", "ARGO-PROJECT"),                              # not a -lower project at all
    (app(project="gate-api"), "gate-api-lower", "ARGO-PROJECT"),      # the agent's label does not match the app's
    (app(project="other-lower"), "gate-api-lower", "ARGO-PROJECT"),
    (app(operation={"sync": {}}), "gate-api-lower", "ARGO-BUSY"),
    (app(status={"operationState": {"phase": "Running"}}), "gate-api-lower", "ARGO-BUSY"),
])
def test_refusals_send_nothing(tmp_path, obj, project, rule):
    s, sent = syncer(obj, tmp_path=tmp_path)
    with pytest.raises(Denied) as e:
        s.sync("gate-api-dev", project, WHO)
    assert e.value.rule == rule and sent == []


def test_a_bad_name_is_refused_before_any_read(tmp_path):
    s, sent = syncer(app(), tmp_path=tmp_path)
    with pytest.raises(Denied) as e:
        s.sync("../secrets", "gate-api-lower", WHO)
    assert e.value.rule == "READ-ARGO-NAME" and s.kube.paths == [] and sent == []


def test_a_missing_app_is_an_answer(tmp_path):
    s, sent = syncer(None, tmp_path=tmp_path)
    assert s.sync("gate-api-dev", "gate-api-lower", WHO) == {"app": "gate-api-dev", "found": False,
                                                             "searched": ["argocd", "argocd-apps"]}
    assert sent == []


@pytest.mark.parametrize("status, rule", [(409, "ARGO-CHANGED"), (403, "ARGO-K8S-403")])
def test_api_refusals_are_denials(tmp_path, status, rule):
    s, _ = syncer(app(), status=status, tmp_path=tmp_path)
    with pytest.raises(Denied) as e:
        s.sync("gate-api-dev", "gate-api-lower", WHO)
    assert e.value.rule == rule


def test_other_failures_are_backend_errors(tmp_path):
    s, _ = syncer(app(), status=500, tmp_path=tmp_path)
    with pytest.raises(ArgoSyncError):
        s.sync("gate-api-dev", "gate-api-lower", WHO)
