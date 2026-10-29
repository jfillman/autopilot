# autopilot-agent-runtime: the generic agent runtime an AgentRun pod runs (runtime/; README "The agent runtime").
# Build context: runtime/. Multi-arch (linux/amd64 and linux/arm64): the base is pinned by its multi-arch index digest
# and every wheel in requirements.lock is hashed for both.
# Runs as uid 65532 with a read-only root filesystem: nothing is written outside /run/output and /tmp at runtime.
# function-agentrun's pod sets its own runAsUser (10001 today), so nothing here depends on the uid: the files are
# root-owned and world-readable, and HOME points at the writable /tmp.

# python:3.13-slim, multi-arch index (2026-10-29). Bump the tag and the digest together.
FROM python:3.13-slim@sha256:2d44738ebf4ce5f63b9d9ea871e9c7cfdc0426f7b0c97cac915fd88ded1d86bb AS build
ENV PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
RUN python -m venv /opt/venv
WORKDIR /src
COPY requirements.lock ./
RUN /opt/venv/bin/pip install --require-hashes --only-binary=:all: -r requirements.lock
# The package is copied, not pip-installed: building it would fetch hatchling, an input the lock does not hash.
COPY src/hangar_agent/ /opt/venv/lib/python3.13/site-packages/hangar_agent/
RUN /opt/venv/bin/python -m compileall -q /opt/venv/lib \
    && /opt/venv/bin/python -c "import hangar_agent.main, anthropic"

FROM python:3.13-slim@sha256:2d44738ebf4ce5f63b9d9ea871e9c7cfdc0426f7b0c97cac915fd88ded1d86bb
ENV PATH=/opt/venv/bin:$PATH PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 HOME=/tmp \
    HANGAR_OUTPUT_DIR=/run/output HANGAR_WORKSPACE=/workspace
COPY --from=build /opt/venv /opt/venv
WORKDIR /workspace
USER 65532:65532
ENTRYPOINT ["python", "-m", "hangar_agent"]
