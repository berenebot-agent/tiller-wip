# syntax=docker/dockerfile:1.7
FROM python:3.13-alpine
WORKDIR /probe
# BuildKit cache mounts make the apk/npm/pip downloads durable across rebuilds,
# so even a clean rebuild (--no-cache) or a changed requirements layer reuses
# the packages we already fetched instead of re-downloading them.
RUN --mount=type=cache,target=/var/cache/apk \
    apk add --no-cache nodejs npm
# npm 7+ keeps its content-addressed store under /root/.npm; caching it means
# the pinned CLI installs are not re-downloaded on a later clean build.
RUN --mount=type=cache,target=/root/.npm \
    npm install --global \
        @anthropic-ai/claude-code@2.1.251 \
        @openai/codex@0.150.1 \
        opencode-ai@1.18.25
COPY requirements.txt .
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install -r requirements.txt
COPY probe.py .
ENTRYPOINT ["python", "/probe/probe.py"]
