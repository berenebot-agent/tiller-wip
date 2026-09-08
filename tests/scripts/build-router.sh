#!/bin/bash
# tests/scripts/build-router.sh — build the router test image once, skip if
# cached. All three containerized test runners source (`.`) this. Required env
# from caller:
#   repo_dir — absolute path to the tiller-router repo root
#
# The image tag is content-derived (see image-hashes.sh): a code change changes
# the hash and therefore the tag, forcing a rebuild; an unchanged codebase
# reuses the existing image. This avoids the stale-image bug where an
# existence-only check silently reuses an image built from older code.
#
# On success this exports ROUTER_IMAGE (the fully-resolved image:tag) for the
# caller to use in `docker run`. TILLER_ROUTER_REBUILD=1 forces a rebuild.
. "$repo_dir/tests/scripts/image-hashes.sh"

if [ "${TILLER_ROUTER_REBUILD:-0}" = "1" ] || ! docker image inspect "$ROUTER_IMAGE" >/dev/null 2>&1; then
    echo "==> Building $ROUTER_IMAGE"
    docker build --pull=false -t "$ROUTER_IMAGE" "$repo_dir"
else
    echo "==> $ROUTER_IMAGE (cached)"
fi
