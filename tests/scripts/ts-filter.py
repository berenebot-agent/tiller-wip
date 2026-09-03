#!/usr/bin/env python3
"""Per-line elapsed-since-start timestamp filter for test-run logs.

Reads stdin line by line, prefixes every line with the elapsed time (in
seconds, millisecond resolution) since the start of the run, and writes to
stdout. The start point is taken from the TS_START environment variable (set
by the caller to bash's $EPOCHREALTIME so the timestamp aligns with the
caller's clock); falls back to process start time if unset.

One process per run, no per-line forks. Unbuffered writes so each line is
flushed as soon as it arrives, which matters for live tailing of long
streams (e.g. docker build output).
"""
import os
import sys
import time

start = float(os.environ.get("TS_START") or time.monotonic())

for line in sys.stdin:
    elapsed = time.monotonic() - start
    sys.stdout.write(f"[+{elapsed:7.3f}s] {line}")
    sys.stdout.flush()
