#!/bin/bash
# go-test-timings.sh — run `go test -count=1 -json ./...`, report slowest packages/tests.
#
# Usage:
#   ./tests/scripts/go-test-timings.sh                # run + summary, discard raw JSON
#   ./tests/scripts/go-test-timings.sh --save-json    # retain raw JSON under tests/logs/
#
# Exit status mirrors the underlying go test run (0 = all pass). Parsing failures
# never mask a test failure: if go test exits non-zero we always exit non-zero.

set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
save_json=false
for arg in "$@"; do
    case "$arg" in
        --save-json) save_json=true ;;
        *) echo "usage: $0 [--save-json]" >&2; exit 2 ;;
    esac
done

# Wrapper that runs Go inside the pinned image with persistent caches. We invoke
# it with `go test -json` and parse the streaming JSON output.
go_wrapper="$repo_root/tiller-go.sh"
if [ ! -x "$go_wrapper" ]; then
    echo "go-test-timings.sh: missing $go_wrapper" >&2
    exit 2
fi

json_file=""
if $save_json; then
    mkdir -p "$repo_root/tests/logs/go-test-timings"
    json_file="$repo_root/tests/logs/go-test-timings/$(date -u +%Y%m%dT%H%M%SZ)-go-test.json"
fi

# Run the suite, capturing JSON. We let go test's exit code propagate via
# pipefail; tee -a writes to both stdout (progress) and the optional raw file.
start=$(date +%s.%N)
output="$(mktemp)"
trap 'rm -f "$output"' EXIT
"$go_wrapper" test -count=1 -json ./... 2>&1 | tee "$output" >/dev/null || true
end=$(date +%s.%N)
elapsed_total=$(awk "BEGIN {printf \"%.3f\", $end - $start}")

if $save_json && [ -n "$json_file" ]; then
    # The raw JSON lines are the ones starting with '{'; progress lines from the
    # wrapper (e.g. "==> tiller-go.sh: ...") are already filtered by go test
    # -json, but the wrapper may emit its own summary lines. Keep only JSON.
    grep -E '^\{' "$output" > "$json_file" || true
fi

# Extract the overall pass/fail: a top-level {"Action":"fail"} on the final
# package means the run failed. Simpler: look for any test failure action.
if grep -q '"Action":"fail"' "$output"; then
    run_rc=1
else
    run_rc=0
fi

# Parse per-package and per-test timings from the JSON stream. Each package
# emits a {"Action":"pass"/"fail","Package":"...","Elapsed":N} line. Tests emit
# {"Action":"pass"/"fail","Package":"...","Test":"...","Elapsed":N}.
python3 - "$output" "$elapsed_total" <<'PY'
import json
import sys

path = sys.argv[1]
total = sys.argv[2]

pkgs = {}
tests = {}
with open(path) as fh:
    for line in fh:
        line = line.strip()
        if not line.startswith('{'):
            continue
        try:
            evt = json.loads(line)
        except json.JSONDecodeError:
            continue
        action = evt.get('Action', '')
        if action not in ('pass', 'fail'):
            continue
        pkg = evt.get('Package', '')
        elapsed = evt.get('Elapsed')
        if pkg and isinstance(elapsed, (int, float)):
            pkgs[pkg] = pkgs.get(pkg, 0.0) + elapsed
        test = evt.get('Test', '')
        if test and isinstance(elapsed, (int, float)):
            tests[f"{pkg}::{test}"] = elapsed

sorted_pkgs = sorted(pkgs.items(), key=lambda kv: kv[1], reverse=True)
sorted_tests = sorted(tests.items(), key=lambda kv: kv[1], reverse=True)

print("Go test timing summary")
print("----------------------")
print(f"Total wall time: {total}s")
print()
print("Slowest packages:")
for pkg, secs in sorted_pkgs[:10]:
    print(f"  {secs:7.3f}s {pkg}")
print()
print("Slowest tests:")
for name, secs in sorted_tests[:15]:
    print(f"  {secs:7.3f}s {name}")
PY

# Suppress unused-variable warning from shellcheck for end.
: "$end"

exit "$run_rc"
