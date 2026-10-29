# autopilot-gateway: the Hangar Clearance gateway (src/autopilot/serve.py; README "Running the gateway").
# Multi-arch (linux/amd64 and linux/arm64): nothing here is architecture-specific.
# Runs as uid 65532 with a read-only root filesystem: nothing is written at runtime.

FROM python:3.13-slim AS build
# git: the clearance dependency is pinned as a git tag (pyproject.toml).
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
ENV PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
RUN python -m venv /opt/venv
WORKDIR /app
COPY pyproject.toml ./
COPY src/ src/
COPY agents/ agents/
COPY schemas/ schemas/
# Editable, so the package resolves /app/schemas and /app/agents from its own location (airframe_tools, serve).
RUN /opt/venv/bin/pip install -e /app \
    && /opt/venv/bin/python -m compileall -q /app/src /opt/venv/lib

FROM python:3.13-slim
ENV PATH=/opt/venv/bin:$PATH PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 \
    CLEARANCE_AGENTS_DIR=/app/agents PORT=8080
COPY --from=build /opt/venv /opt/venv
COPY --from=build /app /app
WORKDIR /app
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["autopilot-gateway"]
