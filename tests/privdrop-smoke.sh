#!/usr/bin/env bash
# Privilege-drop smoke test: run the compiled test binary as root in a
# container with a root-owned bind-mounted data directory and verify Tiller's
# startup drop-to-runtime-user path end to end.
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE="${TILLER_TEST_IMAGE:-golang:1.26.7-alpine}"
work=$(mktemp -d)
# The drop test chowns the bind-mounted data tree to 65532, which the host user
# can no longer delete; restore ownership before cleanup.
trap 'docker run --rm -v "$work:/w" --user root alpine chown -R '"$(id -u):$(id -g)"' /w >/dev/null 2>&1; rm -rf "$work"' EXIT

echo "==> Building privdrop test binary"
# tiller-go.sh mounts only the repo tree at /src, so the binary must be built
# inside it and moved out on the host.
./tiller-go.sh test -c -o ./privdrop.test ./internal/privdrop
mv ./privdrop.test "$work/privdrop.test"

# The container's root creates the data dir, so the bind mount is root-owned —
# exactly the state a fresh `docker compose` up produces for the quick start.
mkdir -p "$work/data"

echo "==> Running drop-to-runtime-user as root (uid 0) against a root-owned data dir"
docker run --rm \
	-v "$work:/tiller-test:ro" \
	-v "$work/data:/tiller-data" \
	-e TILLER_PRIVDROP_TEST_TARGET=1 \
	-e TILLER_PRIVDROP_TEST_DIR=/tiller-data \
	"$IMAGE" /tiller-test/privdrop.test -test.run TestRootDropIntegration -test.v

echo "==> Verifying ownership of the bind mount from outside"
owner=$(docker run --rm -v "$work/data:/d:ro" --user root alpine stat -c '%u:%g' /d/a/b/f.txt)
[ "$owner" = "65532:65532" ] || {
	echo "FAIL: nested file owner is $owner, want 65532:65532" >&2
	exit 1
}
owner=$(docker run --rm -v "$work/data:/d:ro" --user root alpine stat -c '%u:%g' /d)
[ "$owner" = "65532:65532" ] || {
	echo "FAIL: data dir owner is $owner, want 65532:65532" >&2
	exit 1
}

echo "PASS: root startup dropped to 65532:65532 and chowned the data tree"