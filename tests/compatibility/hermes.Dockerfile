# syntax=docker/dockerfile:1.7
FROM python:3.11-slim
# Durable pip cache so the pinned hermes-agent wheel isn't re-downloaded on
# clean rebuilds (used by the hermes compatibility probe).
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install --no-cache-dir hermes-agent==0.19.0
COPY hermes_probe.py /probe/hermes_probe.py
ENTRYPOINT ["python", "/probe/hermes_probe.py"]
