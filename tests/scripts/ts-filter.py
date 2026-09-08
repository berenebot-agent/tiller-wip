#!/usr/bin/env python3
"""Per-line elapsed-since-start timestamp filter for test-run logs.

Reads stdin line by line, prefixes every line with the elapsed time (in
seconds, millisecond resolution) since the filter process started, and
writes to stdout unbuffered. The filter is spawned at the moment the run
begins (via the run.sh exec-redirect), so its own start IS the run start
— no clock alignment needed with the caller.

One process per run, no per-line forks. Unbuffered writes so each line is
flushed as soon as it arrives, which matters for live tailing of long
streams (e.g. docker build output).
"""
import sys
import time

start = time.monotonic()

for line in sys.stdin:
    elapsed = time.monotonic() - start
    sys.stdout.write(f"[+{elapsed:7.3f}s] {line}")
    sys.stdout.flush()
