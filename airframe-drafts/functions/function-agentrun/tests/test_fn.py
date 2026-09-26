import unittest
from datetime import datetime, timedelta, timezone

from crossplane.function import logging, resource
from crossplane.function.proto.v1 import run_function_pb2 as fnv1

from function import fn


def request(expires_in_minutes: int, name="r-1a2b3c") -> fnv1.RunFunctionRequest:
    now = datetime.now(timezone.utc)
    xr = {
        "apiVersion": "catalog.idp.io/v1alpha1", "kind": "AgentRun",
        "metadata": {"name": name, "namespace": "autopilot-runs",
                     "creationTimestamp": (now - timedelta(minutes=1)).strftime("%Y-%m-%dT%H:%M:%SZ")},
        "spec": {"agent": "coding-agent", "image": "ghcr.io/x/y@sha256:" + "0" * 64, "sandbox": "standard",
                 "compute": "small", "network": {"mode": "clearance"},
                 "expiresAt": (now + timedelta(minutes=expires_in_minutes)).strftime("%Y-%m-%dT%H:%M:%SZ"),
                 "taskId": "t-1a2b3c", "sessionId": "s-1a2b3c",
                 "limits": {"toolCalls": 1, "githubCalls": 1, "modelTokens": 1}},
    }
    return fnv1.RunFunctionRequest(observed=fnv1.State(composite=fnv1.Resource(resource=resource.dict_to_struct(xr))))


class TestFn(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        logging.configure(level=logging.Level.DISABLED)

    async def test_live_run_desires_the_sandbox_and_sets_status(self):
        rsp = await fn.FunctionRunner().RunFunction(request(30), None)
        assert set(rsp.desired.resources) == {"namespace", "quota", "netpol-default-deny", "netpol-egress", "serviceaccount", "job"}
        status = resource.struct_to_dict(rsp.desired.composite.resource)["status"]
        assert status["phase"] == "Provisioning" and status["namespace"] == "agent-r-1a2b3c"
        assert rsp.meta.ttl.seconds <= 60

    async def test_expired_run_desires_nothing(self):
        rsp = await fn.FunctionRunner().RunFunction(request(-5), None)
        assert len(rsp.desired.resources) == 0
        assert resource.struct_to_dict(rsp.desired.composite.resource)["status"]["phase"] == "Expired"
