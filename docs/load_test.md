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

## Public-beta pass criteria

Record a run (both stream and non-stream) against the deployment shape, then
treat these as the gate before opening the free tier. Adjust the concurrency
number to the smallest figure that stays clean; that is the supported ceiling.

- **Zero 5xx** (and zero unexpected failures) at the chosen target concurrency
  for the full request count. 429s from the plan stream cap are expected and do
  not count as failures once the cap is reached.
- **Target concurrency:** at least **20 concurrent streams** stable; record the
  highest clean figure and the first figure where 5xx appear.
- **Router overhead bound:** p95 router overhead (end-to-end p95 minus the
  mock's own latency) within low tens of milliseconds; p99 not more than ~2x
  p95. Router overhead, not the mock, is what the number means.
- **No memory spiral:** RSS after the run is within a small multiple of the
  pre-run baseline (the review TR-002 ceilings — 64x8 MiB inbound, 32x16 MiB
  outbound — bound the worst case; this checks actual behaviour).
- Run it twice back-to-back to confirm the warm-cache steady state.

## Probing the DoS guards

The throughput run above uses small bodies and one key, so it does not exercise
the pre-SaaS review's admission controls. Probe those separately on the same
instance:

- **Body-read gate (TR-002):** run the harness with `--body-bytes` near the
  router's per-request cap (8 MiB total JSON, so ~8_000_000) and a concurrency
  above the gate (64) and confirm the instance stays up, returns bounded
  `request_too_large` / `body_read_busy` errors rather than growing without
  bound, and recovers. Example:
  ```bash
  python3 tests/load/loadtest.py --base-url ... --api-key ... \
      --model main --concurrency 80 --requests 160 --body-bytes 8000000
  ```
- **Concurrent-stream cap:** with a finite plan cap, confirm the (cap+1)th
  concurrent request gets `429 stream_limit_exceeded` with `Retry-After`.
- **Live-SSE cap (TR-007):** open more than 8 `/api/admin/live` connections for
  one account and confirm the extra ones get `429 live_limit_exceeded` before
  any SSE bytes.
- **Multi-account isolation:** repeat a short run against a second client key
  from a different account and confirm neither account's failures or activity
  bleed into the other.

