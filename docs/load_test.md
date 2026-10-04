# Load test harness

This is a repeatable load harness around the routing service. It runs against a
**local Tiller instance with the mock upstream**
(`tests/compatibility/mock_upstream.py`), so it spends no provider credits.

## What it measures

- completed requests and sustained request rate;
- success/failure counts;
- router-visible latency p50/p95/p99/max and mean.

Run it once with `--stream` and once without to compare the streaming and
non-streaming paths. Long SSE requests are the more meaningful concurrency
constraint than request rate alone.

## Recipe

1. Build and run Tiller locally (see the main README), for example with a data
   dir under `/tmp`.
2. Start the mock upstream:
   ```bash
   TILLER_MOCK_PORT=18081 python3 tests/compatibility/mock_upstream.py
   ```
3. In Tiller, add a `generic-openai` provider pointing at
   `http://127.0.0.1:18081/v1` (local mode allows private/LAN URLs), refresh
   its catalogue so `mock-model` is discovered, and create a Single client key
   with model name `main` routed at `mock-model`.
4. Run the harness:
   ```bash
   python3 tests/load/loadtest.py \
     --base-url http://127.0.0.1:8080 \
     --api-key sk-tr-... \
     --model main \
     --concurrency 20 --requests 200
   # streaming
   python3 tests/load/loadtest.py ... --stream
   ```
5. Record the numbers (stable concurrency, p95/p99 router overhead, failure
   count) against the deployment shape you are validating. Escalate
   `--concurrency` until failures appear to find the stable ceiling; that is the
   number an operator runbook needs.

## Interpreting results

- A small number of failures at high concurrency is expected once the plan's
  concurrent-stream cap is hit; the harness reports the HTTP status (429 means
  the cap is doing its job).
- Router overhead is the difference from the mock's near-zero upstream latency,
  so p95/p99 here are a useful upper bound on Tiller's own cost.
- The mock upstream is single-process and will itself become the bottleneck at
  high concurrency; treat the mock's own limits, not Tiller's, as the first
  suspect if failures appear with 5xx from upstream.
