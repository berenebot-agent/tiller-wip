FROM python:3.13-alpine
WORKDIR /probe
RUN apk add --no-cache nodejs npm \
    && npm install --global \
        @anthropic-ai/claude-code@2.1.251 \
        @openai/codex@0.150.1 \
        opencode-ai@1.18.25
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY probe.py .
ENTRYPOINT ["python", "/probe/probe.py"]
