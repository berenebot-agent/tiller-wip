#!/bin/bash
# check-fmt.sh — fail fast on unformatted Go files, before GitHub CI does.
#
# Mirrors the CI gate (.github/workflows/ci.yml, `gofmt -l .`) using the same
# pinned Go image as ./tiller-go.sh and the Dockerfile build stage, so a pass
# here means a pass in CI. gofmt needs no module cache, so this runs a bare
# container (no cache mounts) with the repo mounted read-only — it never
# rewrites anything.
#
# Usage:
#   ./tests/scripts/check-fmt.sh        # check tracked + untracked (non-ignored) .go files
#
# Fix failures with:
#   ./tiller-go.sh fmt ./...
#
# Exit 0 when clean, 1 when files need formatting, 2 on usage/environment errors.

set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Single source of truth for the Go image is ./tiller-go.sh (mirrors the
# Dockerfile build stage). Fall back to the pinned image if parsing fails.
GO_IMAGE="$(grep -m1 '^GO_IMAGE=' "$repo_root/tiller-go.sh" | cut -d'"' -f2 || true)"
if [ -z "${GO_IMAGE:-}" ]; then
	GO_IMAGE="golang:1.26.7-alpine"
fi

# Only files git knows about (tracked) or would commit (untracked,
# non-ignored). This keeps unreadable runtime dirs (./data), build caches,
# and node_modules out of the walk while covering everything CI can see.
mapfile -t go_files < <(
	cd "$repo_root" && {
		git ls-files '*.go' 2>/dev/null
		git ls-files --others --exclude-standard '*.go' 2>/dev/null
	} | sort -u
)

if [ "${#go_files[@]}" -eq 0 ]; then
	echo "check-fmt: no Go files found — clean"
	exit 0
fi

unformatted="$(
	docker run --rm \
		--memory=512m \
		--memory-swap=512m \
		-v "$repo_root:/src:ro" \
		-w /src \
		"$GO_IMAGE" \
		gofmt -l "${go_files[@]}"
)"

if [ -z "$unformatted" ]; then
	echo "check-fmt: clean (${#go_files[@]} files)"
	exit 0
fi

count="$(echo "$unformatted" | wc -l | tr -d ' ')"
echo "check-fmt: FAIL — $count file(s) need formatting:" >&2
echo "$unformatted" >&2
echo "fix with: ./tiller-go.sh fmt ./... (then re-run $0)" >&2
exit 1
