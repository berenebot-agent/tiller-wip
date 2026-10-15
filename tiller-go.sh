#!/bin/bash
# tiller-go.sh — run Go commands inside Docker (build/test/vet/tidy) with a
# persistent cache so repeated runs are fast, and the container is memory-capped
# (default: 50% of host RAM) to avoid OOM-killing the host box.
#
# Usage:
#   ./tiller-go.sh <go args...>     e.g. ./tiller-go.sh test ./...
#   ./tiller-go.sh vet ./...
#   ./tiller-go.sh mod tidy
#
# Caches (bind-mounted, NOT named volumes — per deployment rule):
#   gomod   = ~/.cache/tiller-go/mod      module download cache (/go/pkg/mod)
#   gobuild = ~/.cache/tiller-go/build   compiled package cache (/root/.cache/go-build)
#
# RAM safety:
#   --memory/-m limits the container's hard memory ceiling (OOM inside, not host OOM)
#   --memory-swap = same value => no swap growth for this container
#   GOFLAGS=-p=N sets how many packages are built/tested in parallel. Defaults to
#   the host core count (Go's own default is GOMAXPROCS, so this only makes it
#   explicit); override with TILLER_GO_PARALLEL.
#
# Scratch disk:
#   Test binaries and per-test SQLite data files live under TMPDIR (/tmp). The
#   suite is DB-heavy and fsyncs constantly, so /tmp is mounted as a tmpfs to
#   keep that I/O in RAM instead of the overlayfs on real disk. `exec` is
#   MANDATORY: Docker's --tmpfs defaults to noexec, and Go both builds and runs
#   test binaries under TMPDIR (without it: "fork/exec ...x.test: permission
#   denied"). tmpfs pages count against --memory, so the cap still bounds the
#   host. Override the size with TILLER_GO_TMPFS; set TILLER_GO_TMPFS=0 to
#   disable the tmpfs (e.g. on a runner where --tmpfs is unavailable).
#
# Go image is pinned to match the Dockerfile build stage.

set -eu

GO_IMAGE="golang:1.26.7-alpine"
CACHE_DIR="${TILLER_GO_CACHE:-$HOME/.cache/tiller-go}"
MOD_CACHE="$CACHE_DIR/mod"
BUILD_CACHE="$CACHE_DIR/build"

# How many packages to build/test in parallel (go -p). Explicit rather than
# implicit so a low core count doesn't silently serialise the suite.
CORES=$(nproc 2>/dev/null || echo 2)
PARALLEL="${TILLER_GO_PARALLEL:-$CORES}"

# Hard RAM cap for the container: default 50% of host physical RAM (was a
# hardcoded 1g sized for an old 4.4GiB box). Override with TILLER_GO_MEM.
mem_total_kb=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null || true)
if [ -n "${mem_total_kb:-}" ] && [ "$mem_total_kb" -gt 0 ] 2>/dev/null; then
    default_mem_mb=$(( mem_total_kb / 1024 / 2 ))
else
    default_mem_mb=1024
fi
MEM_LIMIT="${TILLER_GO_MEM:-${default_mem_mb}m}"

# tmpfs for /tmp, sized as a lazy ceiling (tmpfs allocates on use). Defaults to
# the RAM cap; TILLER_GO_TMPFS=0 disables the mount entirely.
TMPFS_SIZE="${TILLER_GO_TMPFS:-$MEM_LIMIT}"
TMPFS_FLAG=""
if [ "$TMPFS_SIZE" != "0" ]; then
    TMPFS_FLAG="--tmpfs /tmp:rw,exec,size=$TMPFS_SIZE,mode=1777"
fi

# Create cache dirs if absent
mkdir -p "$MOD_CACHE" "$BUILD_CACHE"

# Repo root = this script's own directory (tiller-go.sh lives in the repo root,
# the dir that contains go.mod). Resolve symlinks + get absolute path.
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT="$SCRIPT_DIR"

# A container without a TTY/terminal shouldn't try to allocate one
TTY_FLAG=""
if [ -t 1 ]; then TTY_FLAG="-it"; fi

# Pass through the opt-in example-notifications flag so the test can reach the
# real ntfy topic. Other env vars are intentionally not forwarded.
#
# -trimpath is in GOFLAGS (not a command line) so it applies to every
# invocation this pass-through wrapper makes — test, vet, build alike. Without
# it an object's cache key includes its absolute build path, so the same source
# built from a different mount point or workdir stores a SEPARATE copy: two
# lineages of identical objects. See the same note in mailmoose-go.sh; measured
# there as +1,322 entries for the second lineage.
ENV_FLAGS=(-e "GOFLAGS=-p=$PARALLEL -trimpath")
if [ -n "${TILLER_NOTIFY_EXAMPLES:-}" ]; then
    ENV_FLAGS+=(-e "TILLER_NOTIFY_EXAMPLES=$TILLER_NOTIFY_EXAMPLES")
fi

# Log capture: always write full output to a repo-local log file under
# tests/logs/ (gitignored, persistent across runs). The summary printed at
# the end shows the path; on failure the first FAIL line is also inlined.
#
# When wrapped by tests/run.sh, TILLER_TEST_DIR is set: write straight to
# <dir>/out.log so the unified runner owns the run folder. When unset, use
# the dated per-invocation file (direct-invocation behavior is unchanged).
if [ -n "${TILLER_TEST_DIR:-}" ]; then
    LOG_DIR="$TILLER_TEST_DIR"
else
    LOG_DIR="tests/logs/tiller-go"
fi
mkdir -p "$LOG_DIR"
if [ -n "${TILLER_TEST_DIR:-}" ]; then
    LOG_FILE="$LOG_DIR/out.log"
else
    LOG_FILE="$LOG_DIR/$(date -u +%Y%m%dT%H%M%S)-go-$(echo "$*" | tr ' /' '__').log"
fi
ts=$(date +%s)

docker run $TTY_FLAG --rm \
    --memory="$MEM_LIMIT" \
    --memory-swap="$MEM_LIMIT" \
    $TMPFS_FLAG \
    "${ENV_FLAGS[@]}" \
    -v "$MOD_CACHE:/go/pkg/mod" \
    -v "$BUILD_CACHE:/root/.cache/go-build" \
    -v "$REPO_ROOT:/src" \
    -w /src \
    "$GO_IMAGE" \
    go "$@" 2>&1 | tee "$LOG_FILE"
rc=${PIPESTATUS[0]}
te=$(date +%s)

echo "==> tiller-go.sh: rc=$rc, $((te - ts))s elapsed, log: $LOG_FILE" >&2
if [ "$rc" -ne 0 ]; then
    first_fail=$(grep -m1 -E '^--- FAIL|^FAIL\b' "$LOG_FILE" 2>/dev/null | head -c 400 || true)
    [ -n "$first_fail" ] && echo "    first failure: $first_fail" >&2
fi
exit "$rc"
