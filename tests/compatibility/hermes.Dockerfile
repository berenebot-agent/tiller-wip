FROM python:3.11-slim
RUN pip install --no-cache-dir hermes-agent==0.19.0
COPY hermes_probe.py /probe/hermes_probe.py
ENTRYPOINT ["python", "/probe/hermes_probe.py"]
