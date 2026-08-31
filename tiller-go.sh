#!/bin/sh
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

exec docker run $TTY_FLAG --rm \
    --memory="$MEM_LIMIT" \
    --memory-swap="$MEM_LIMIT" \
    -e GOFLAGS="-p=2" \
    -v "$MOD_CACHE:/go/pkg/mod" \
    -v "$BUILD_CACHE:/root/.cache/go-build" \
    -v "$REPO_ROOT:/src" \
    -w /src \
    "$GO_IMAGE" \
    go "$@"
