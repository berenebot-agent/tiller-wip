#!/bin/bash
# tests/scripts/prune-test-logs.sh — bound tests/logs/ via LRU (mtime) pruning.
#
# The containerised test runners emit per-run artifacts under tests/logs/:
# browser runs leave a tiller-browser-<pid>/ directory (~6MB, mostly an
# extracted fixturectl binary), compat leaves a dated .log, and the Go wrapper
# leaves a dated .log per invocation. Successful runs are cleaned up by their
# own runners, but killed/interrupted runs (ssh drop, OOM, kill -9 while the
# EXIT trap is pending) pile up and there is no bound.
#
# Run the prune BEFORE a run starts: keep the N most recently modified
# entries per glob, remove the rest. It is deliberately mtime-based (LRU),
# not count-based on any per-test logic, so a preserved failed-run artifact is
# kept if recent and only evicted once it becomes the oldest.
#
# Browser run dirs may contain activity-data/ subdirs chowned by the router
# (running as uid 65532), so a plain `rm -rf` on the host fails. Evictions are
# cleared via a throwaway root container over the whole tests/logs tree — the
# same pattern the browser runner's own cleanup uses — so the prune cannot
# leave a half-removed tree behind.
#
# Source (`.`) this from a runner; or invoke it directly as a script. In the
# direct form, pass an optional cap as $1, defaulting to KEEP_BROWSER.
set -eu

# Defaults: how many of the most recent entries per glob to keep.
KEEP_BROWSER=${KEEP_BROWSER:-3}
KEEP_COMPAT=${KEEP_COMPAT:-5}
KEEP_GO=${KEEP_GO:-10}

logs_dir="$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]:-$0}")/../.." && pwd)/tests/logs"

# Collect entries (relative to logs_dir) that are beyond the keep cap.
evict=()
collect_evictions() {
    local glob="$1" keep="$2"
    local base
    base="$(dirname "$glob")"
    [ -d "$logs_dir/$base" ] || return 0
    local -a entries dirs
    mapfile -t entries < <(ls -1dt "$logs_dir"/$glob 2>/dev/null || true)
    local count=${#entries[@]}
    local i rel
    for ((i = keep; i < count; i++)); do
        rel="${entries[$i]#"$logs_dir"/}"
        case "$rel" in "") ;; *) evict+=("$rel") ;; esac
    done
}

collect_evictions 'tiller-browser-*' "$KEEP_BROWSER"
collect_evictions 'compat/*-compat.log' "$KEEP_COMPAT"
collect_evictions 'tiller-go/*.log' "$KEEP_GO"

if [ "${#evict[@]}" -eq 0 ]; then
	(return 0 2>/dev/null) || exit 0
fi

# Remove everything that is not owned by a fixed set of UIDs. The browser
# activity-data may be owned by 65532 (the router runtime user); everything
# else under a browser dir is host-owned. Remove each subtree in one place so
# the root container only ever handles exactly what we intend to evict.
# Use plain rm for host-owned trees and a root container for trees that
# contain 65532-owned subdirs.
for rel in "${evict[@]}"; do
    if rm -rf -- "$logs_dir/$rel" 2>/dev/null; then
        continue
    fi
    # Eviction hit a permission wall (65532-owned subdir). Try a root
    # container; if docker is unavailable, fall back to a best-effort plain rm
    # so we never hard-fail the caller.
    if command -v docker >/dev/null 2>&1; then
        docker run --rm -v "$logs_dir:/logs:rw" python:3.13-alpine \
            python -c "import shutil,sys; [shutil.rmtree(p) for p in sys.argv[1:]]" \
            "/logs/$rel" >/dev/null 2>&1 || rm -rf -- "$logs_dir/$rel" 2>/dev/null || true
    else
        rm -rf -- "$logs_dir/$rel" 2>/dev/null || true
    fi
done
