# autopilot-agent-langgraph: the LangGraph agent runtime (runtime/langgraph; runtime/catalog.yaml `langgraph`).
# Build context: runtime/ (so hangar_agent, the pod contract, is copied from runtime/src). The same base, venv and
# hardening as the generic runtime (runtime/Dockerfile): multi-arch, every wheel hashed, uid 65532, read-only root.

# python:3.13-slim, multi-arch index (2026-10-29). Bump the tag and the digest together, with runtime/Dockerfile.
FROM python:3.13-slim@sha256:2d44738ebf4ce5f63b9d9ea871e9c7cfdc0426f7b0c97cac915fd88ded1d86bb AS build
ENV PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
RUN python -m venv /opt/venv
WORKDIR /src
COPY langgraph/requirements.lock ./
RUN /opt/venv/bin/pip install --require-hashes --only-binary=:all: -r requirements.lock
COPY src/hangar_agent/ /opt/venv/lib/python3.13/site-packages/hangar_agent/
COPY langgraph/src/hangar_langgraph/ /opt/venv/lib/python3.13/site-packages/hangar_langgraph/
RUN /opt/venv/bin/python -m compileall -q /opt/venv/lib \
    && /opt/venv/bin/python -c "import hangar_langgraph.main, langgraph.graph, anthropic"

FROM python:3.13-slim@sha256:2d44738ebf4ce5f63b9d9ea871e9c7cfdc0426f7b0c97cac915fd88ded1d86bb
ENV PATH=/opt/venv/bin:$PATH PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 HOME=/tmp \
    HANGAR_OUTPUT_DIR=/run/output HANGAR_WORKSPACE=/workspace
COPY --from=build /opt/venv /opt/venv
WORKDIR /workspace
USER 65532:65532
ENTRYPOINT ["python", "-m", "hangar_langgraph"]
