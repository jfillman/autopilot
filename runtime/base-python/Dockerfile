# base-python: the FROM image for a Python code agent (runtime/catalog.yaml). The same base, venv and hardening as the
# generic runtime (runtime/Dockerfile), with hangar_agent installed as a library and no entrypoint of its own:
#
#   FROM ghcr.io/jfillman/autopilot-agent-base-python@sha256:...
#   COPY --chown=65532:65532 my_agent/ /app/my_agent/
#   ENTRYPOINT ["python", "-m", "my_agent"]
#
# and in the agent: `from hangar_agent import env, clearance, model` for the pod contract, the gateway client and the
# model-proxy client, `hangar_agent.main.write_result` for result.json. Built from the runtime/ context.
FROM python:3.13-slim@sha256:2d44738ebf4ce5f63b9d9ea871e9c7cfdc0426f7b0c97cac915fd88ded1d86bb AS build
ENV PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
RUN python -m venv /opt/venv
WORKDIR /src
COPY requirements.lock ./
RUN /opt/venv/bin/pip install --require-hashes --only-binary=:all: -r requirements.lock
COPY src/hangar_agent/ /opt/venv/lib/python3.13/site-packages/hangar_agent/
RUN /opt/venv/bin/python -m compileall -q /opt/venv/lib \
    && /opt/venv/bin/python -c "import hangar_agent.env, hangar_agent.clearance, hangar_agent.model, hangar_agent.main"

FROM python:3.13-slim@sha256:2d44738ebf4ce5f63b9d9ea871e9c7cfdc0426f7b0c97cac915fd88ded1d86bb
ENV PATH=/opt/venv/bin:$PATH PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 HOME=/tmp \
    HANGAR_OUTPUT_DIR=/run/output HANGAR_WORKSPACE=/workspace
COPY --from=build /opt/venv /opt/venv
WORKDIR /workspace
USER 65532:65532
