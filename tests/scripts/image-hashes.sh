#!/bin/bash
# tests/scripts/image-hashes.sh — compute content-derived image tags for the
# test images. Sourced by build-router.sh, tests/browser/run.sh, and the CI
# workflow so the tag a script computes always matches the tag CI prebuilds.
# Required env: repo_dir (absolute path to the repo root).
#
# Exports:
#   ROUTER_IMAGE   — tiller-router:dev-<hash of Go source + web assets + Dockerfile>
#   BROWSER_IMAGE  — tiller-router-browser-tests:dev-<hash of tests/browser inputs>
#   FIXTURE_IMAGE  — tiller-router-fixturectl:dev-<hash of fixturectl inputs>
#
# Each hash is deterministic (files sorted before hashing) so the result is
# stable across runs and machines. A code change changes the hash and therefore
# the tag, forcing a rebuild; an unchanged codebase reuses the existing image.

# Hash a set of files (paths passed as args) plus optional extra stdin.
hash_files() {
    {
        for f in "$@"; do
            [ -f "$f" ] && cat "$f"
        done
    } | sha256sum | cut -c1-12
}

# Router: Go source, embedded web assets, migrations, Dockerfile, go.mod/go.sum.
router_hash_inputs() {
    {
        cat "$repo_dir/go.mod" "$repo_dir/go.sum" "$repo_dir/Dockerfile"
        find "$repo_dir/cmd" "$repo_dir/internal" -type f \
            \( -name '*.go' -o -name '*.sql' -o -name '*.html' -o -name '*.js' -o -name '*.css' -o -name '*.svg' -o -name '*.png' \) \
            -print0 2>/dev/null | sort -z | xargs -0 cat 2>/dev/null
    } | sha256sum | cut -c1-12
}

# Browser: the JS specs/helpers, package manifests, and Dockerfile.
browser_hash_inputs() {
    find "$repo_dir/tests/browser" -maxdepth 1 -type f \
        \( -name '*.js' -o -name 'package.json' -o -name 'package-lock.json' -o -name 'Dockerfile' \) \
        -print0 2>/dev/null | sort -z | xargs -0 cat 2>/dev/null | sha256sum | cut -c1-12
}

# Fixturectl: its Go source + Dockerfile, plus go.mod/go.sum (it builds from the
# repo root context).
fixture_hash_inputs() {
    {
        find "$repo_dir/tests/fixturectl" -type f \( -name '*.go' -o -name 'Dockerfile' \) -print0 2>/dev/null | sort -z | xargs -0 cat 2>/dev/null
        cat "$repo_dir/go.mod" "$repo_dir/go.sum"
    } | sha256sum | cut -c1-12
}

ROUTER_IMAGE="tiller-router:dev-$(router_hash_inputs)"
BROWSER_IMAGE="tiller-router-browser-tests:dev-$(browser_hash_inputs)"
FIXTURE_IMAGE="tiller-router-fixturectl:dev-$(fixture_hash_inputs)"
export ROUTER_IMAGE BROWSER_IMAGE FIXTURE_IMAGE
