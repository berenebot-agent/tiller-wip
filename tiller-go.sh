#!/bin/bash
# tiller-go.sh — run Go commands inside Docker (build/test/vet/tidy) with a
# persistent cache so repeated runs are fast and low-RAM, and the container is
# memory-capped to avoid OOM-killing this 4.4GiB box.
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
#   GOFLAGS=-p=2 caps compiler parallelism to cut peak RAM further (~2-3x margin)
#
# Go image is pinned to match the Dockerfile build stage.

set -eu

GO_IMAGE="golang:1.26.7-alpine"
CACHE_DIR="${TILLER_GO_CACHE:-$HOME/.cache/tiller-go}"
MOD_CACHE="$CACHE_DIR/mod"
BUILD_CACHE="$CACHE_DIR/build"
MEM_LIMIT="${TILLER_GO_MEM:-1g}"        # hard RAM cap for the container (override with TILLER_GO_MEM)

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
ENV_FLAGS="-e GOFLAGS=-p=2"
if [ -n "${TILLER_NOTIFY_EXAMPLES:-}" ]; then
    ENV_FLAGS="$ENV_FLAGS -e TILLER_NOTIFY_EXAMPLES=$TILLER_NOTIFY_EXAMPLES"
fi

# Log capture: always write full output to a repo-local log file under
# tests/logs/ (gitignored, persistent across runs). The summary printed at
# the end shows the path; on failure the first FAIL line is also inlined.
LOG_DIR="tests/logs/tiller-go"
mkdir -p "$LOG_DIR"
LOG_FILE="$LOG_DIR/$(date -u +%Y%m%dT%H%M%S)-go-$(echo "$*" | tr ' /' '__').log"
ts=$(date +%s)

docker run $TTY_FLAG --rm \
    --memory="$MEM_LIMIT" \
    --memory-swap="$MEM_LIMIT" \
    $ENV_FLAGS \
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
