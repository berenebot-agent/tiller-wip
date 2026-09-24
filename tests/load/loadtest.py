#!/usr/bin/env python3
"""Repeatable load harness for the routing data plane.

Runs against a local Tiller instance with a mock upstream, so it spends no
provider credits. It measures the router's own overhead and stable concurrency
rather than a vanity requests-per-second number (sass_tech.md section 38).

Usage:
    python3 tests/load/loadtest.py \
        --base-url http://127.0.0.1:18080 \
        --api-key sk-tr-... \
        --model main \
        --concurrency 20 \
        --requests 200 \
        --stream

The mock upstream is tests/compatibility/mock_upstream.py. Point a Tiller
provider at it and create a client key whose model routes to it, then run this
against that client key. See docs/load_test.md for the full recipe.
"""

import argparse
import json
import statistics
import threading
import time
import urllib.request

RESULTS = []
RESULTS_LOCK = threading.Lock()


def one_request(base_url, api_key, model, stream, timeout):
    url = base_url.rstrip("/") + "/v1/chat/completions"
    body = {"model": model, "messages": [{"role": "user", "content": "load probe"}]}
    if stream:
        body["stream"] = True
    data = json.dumps(body).encode()
    request = urllib.request.Request(url, data=data, method="POST")
    request.add_header("Authorization", "Bearer " + api_key)
    request.add_header("Content-Type", "application/json")
    started = time.monotonic()
    status = 0
    error = ""
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            status = response.status
            # Drain the body so the connection completes for both streaming and
            # non-streaming responses.
            while response.read(65536):
                pass
    except Exception as exc:  # noqa: BLE001 - a probe failure is data, not a crash
        error = type(exc).__name__
    elapsed = time.monotonic() - started
    with RESULTS_LOCK:
        RESULTS.append((status, elapsed, error))


def pct(values, percentile):
    if not values:
        return 0.0
    ordered = sorted(values)
    index = min(len(ordered) - 1, int(round((percentile / 100.0) * (len(ordered) - 1))))
    return ordered[index]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--api-key", required=True)
    parser.add_argument("--model", default="main")
    parser.add_argument("--concurrency", type=int, default=20)
    parser.add_argument("--requests", type=int, default=200)
    parser.add_argument("--stream", action="store_true")
    parser.add_argument("--timeout", type=float, default=60.0)
    args = parser.parse_args()

    print(f"load test: {args.requests} requests, concurrency {args.concurrency}, stream={args.stream}")
    wall_start = time.monotonic()
    produced = 0
    lock = threading.Lock()

    def worker():
        nonlocal produced
        while True:
            with lock:
                if produced >= args.requests:
                    return
                produced += 1
            one_request(args.base_url, args.api_key, args.model, args.stream, args.timeout)

    threads = [threading.Thread(target=worker) for _ in range(args.concurrency)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    wall = time.monotonic() - wall_start

    latencies = [elapsed for status, elapsed, _ in RESULTS if status == 200]
    failures = [row for row in RESULTS if row[0] != 200]
    print(f"completed {len(RESULTS)} requests in {wall:.2f}s ({len(RESULTS) / wall:.1f} req/s)")
    print(f"success {len(latencies)}, failures {len(failures)}")
    if latencies:
        print(f"latency p50 {pct(latencies, 50) * 1000:.1f}ms p95 {pct(latencies, 95) * 1000:.1f}ms p99 {pct(latencies, 99) * 1000:.1f}ms max {max(latencies) * 1000:.1f}ms")
        print(f"mean {statistics.fmean(latencies) * 1000:.1f}ms")
    if failures:
        for status, elapsed, error in failures[:10]:
            print(f"  failure: status={status} error={error} ({elapsed * 1000:.1f}ms)")
    # Non-zero exit when any request failed so the harness can gate CI.
    raise SystemExit(1 if failures else 0)


if __name__ == "__main__":
    main()
