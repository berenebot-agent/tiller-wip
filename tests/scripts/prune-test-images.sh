#!/bin/bash
# tests/scripts/prune-test-images.sh — bound the content-tagged test images.
#
# The test runners tag their images with a content hash (tests/scripts/
# image-hashes.sh): tiller-router:dev-<hash>, tiller-router-browser-tests:
# dev-<hash>, tiller-router-fixturectl:dev-<hash>, ... That is right for reuse —
# an unchanged tree hits the cache and skips the build — but every source edit
# mints a NEW tag, and the browser image is ~3.7GB (a full Chromium). An
# edit-and-test loop therefore adds ~3.7GB per run with no upper bound; that is
# what filled the Docker data root to 100% on 2026-11-22 (11 browser images in
# under an hour).
#
# run_prune_test_images keeps the newest KEEP_IMAGES images of each
# tiller-router* repo and removes the older dev-* ones. It is safe to call
# while other runs are in flight:
#   - images named in $PRUNE_KEEP            are never removed
#   - images backing ANY container           are never removed (docker ps -a)
#   - images younger than KEEP_MINUTES       are never removed
#
# Env:
#   PRUNE_KEEP       space-separated image refs in use by the caller
#   KEEP_IMAGES      newest images kept per repo (default 2)
#   KEEP_MINUTES     never remove anything younger than this (default 15)
#   PRUNE_DRY_RUN=1  print what would be removed, remove nothing
#
# Source (`.`) it from a runner and call run_prune_test_images, or run it
# directly: ./tests/scripts/prune-test-images.sh [--dry-run]
set -u

TEST_IMAGE_REPO_GLOB=${TEST_IMAGE_REPO_GLOB:-^tiller-router}
TEST_IMAGE_TAG_GLOB=${TEST_IMAGE_TAG_GLOB:-^dev-}
KEEP_IMAGES=${KEEP_IMAGES:-2}
KEEP_MINUTES=${KEEP_MINUTES:-15}
PRUNE_KEEP=${PRUNE_KEEP:-}

# Short ids of every image referenced by a container, running or stopped.
_test_images_in_use() {
    docker ps -aq 2>/dev/null \
        | xargs -r docker inspect --format '{{.Image}}' 2>/dev/null \
        | sed 's/^sha256://' | cut -c1-12 | sort -u | tr '\n' ' '
}

run_prune_test_images() {
    command -v docker >/dev/null 2>&1 || return 0
    docker info >/dev/null 2>&1 || return 0   # daemon down: nothing to do

    local now inuse rows repo tag id created ref age idx removed=0
    now=$(date +%s)
    inuse=$(_test_images_in_use)

    # repo|tag|id|created_at, grouped by repo newest-first
    rows=$(docker images --format '{{.Repository}}|{{.Tag}}|{{.ID}}|{{.CreatedAt}}' 2>/dev/null \
        | awk -F'|' -v r="$TEST_IMAGE_REPO_GLOB" -v t="$TEST_IMAGE_TAG_GLOB" \
            'NF == 4 && $1 ~ r && $2 ~ t' \
        | sort -t'|' -k1,1 -k4,4r)

    local seen; declare -A seen
    while IFS='|' read -r repo tag id created; do
        [ -n "${repo:-}" ] || continue
        ref="$repo:$tag"
        seen[$repo]=$(( ${seen[$repo]:-0} + 1 ))
        idx=${seen[$repo]}

        # newest KEEP_IMAGES of each repo stay
        [ "$idx" -le "$KEEP_IMAGES" ] && continue
        # caller's current images stay
        case " $PRUNE_KEEP " in *" $ref "*) continue ;; esac
        # images behind a container (ours, or a concurrent run's) stay
        case " $inuse " in *" $id "*) continue ;; esac
        # a parallel runner's freshly built image stays
        age=$(( now - $(date -d "$created" +%s 2>/dev/null || echo "$now") ))
        [ "$age" -lt $(( KEEP_MINUTES * 60 )) ] && continue

        if [ "${PRUNE_DRY_RUN:-0}" = "1" ]; then
            echo "[prune-images] would remove $ref ($id)"
            removed=$((removed + 1))
        elif docker image rm -f "$id" >/dev/null 2>&1; then
            echo "[prune-images] removed $ref ($id)"
            removed=$((removed + 1))
        fi
    done <<< "$rows"

    if [ "$removed" -eq 0 ]; then
        echo "[prune-images] nothing to reclaim (keep $KEEP_IMAGES/repo, <${KEEP_MINUTES}m untouched)"
    else
        echo "[prune-images] reclaimed $removed image(s)"
    fi
    return 0
}

# Direct invocation
if [ "${BASH_SOURCE[0]:-$0}" = "$0" ]; then
    [ "${1:-}" = "--dry-run" ] && PRUNE_DRY_RUN=1
    run_prune_test_images
fi
