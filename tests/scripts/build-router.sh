#!/bin/bash
# tests/scripts/build-router.sh — build tiller-router:dev once, skip if cached.
# All three containerized test runners source (`.`) this. Required env from caller:
#   repo_dir — absolute path to the tiller-router repo root
#   image    — image:tag (default: tiller-router:dev)
image=${image:-tiller-router:dev}
if docker image inspect "$image" >/dev/null 2>&1; then
    echo "==> $image (cached)"
else
    echo "==> Building $image"
    docker build --pull=false -t "$image" "$repo_dir"
fi