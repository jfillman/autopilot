"""Thin protobuf wrapper around compose(). All decisions live in compose.py."""
from __future__ import annotations

import datetime

import grpc
from crossplane.function import logging, resource, response
from crossplane.function.proto.v1 import run_function_pb2 as fnv1
from crossplane.function.proto.v1 import run_function_pb2_grpc as grpcv1

from function.compose import ClusterConfig, compose


class FunctionRunner(grpcv1.FunctionRunnerService):
    def __init__(self) -> None:
        self.log = logging.get_logger()

    async def RunFunction(self, req: fnv1.RunFunctionRequest, _: grpc.aio.ServicerContext) -> fnv1.RunFunctionResponse:
        rsp = response.to(req)
        xr = resource.struct_to_dict(req.observed.composite.resource)
        cfg = ClusterConfig.from_input(resource.struct_to_dict(req.input) if req.HasField("input") else {})

        observed: dict = {"status": xr.get("status", {})}
        job = req.observed.resources.get("job")
        if job is not None and job.HasField("resource"):
            # provider-kubernetes reports the applied object, status included, under atProvider.
            observed["job"] = (resource.struct_to_dict(job.resource).get("status", {})
                               .get("atProvider", {}).get("manifest"))

        comp = compose(xr, observed, datetime.datetime.now(datetime.timezone.utc), cfg)
        for name, manifest in comp.resources.items():
            rsp.desired.resources[name].resource.update(manifest)
            if comp.ready:
                rsp.desired.resources[name].ready = fnv1.READY_TRUE
        rsp.desired.composite.resource.update({"status": comp.status})
        rsp.meta.ttl.FromTimedelta(datetime.timedelta(seconds=comp.ttl_seconds))
        phase = comp.status.get("phase")
        if phase == "Rejected":
            response.warning(rsp, f"AgentRun rejected: {comp.status.get('reason')}")
        else:
            response.normal(rsp, f"AgentRun {xr['metadata']['name']} phase={phase}")
        return rsp
