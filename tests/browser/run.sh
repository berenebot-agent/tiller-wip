#!/bin/sh
# tests/browser/run.sh — run the Playwright admin UI browser tests.
#
# Fully containerized (no host Node/Go needed). Builds the router + browser
# images ONCE, then starts the mock upstream and a router from a FRESH temp
# data dir so each run is isolated (no cross-test state leaks from a shared
# live DB). Builds use BuildKit RUN-cache mounts and --pull=false so base
# image + package layers are reused locally instead of re-downloaded every run.
#
# Usage:  ./tests/browser/run.sh
#
# Env overrides:
#   TILLER_BROWSER_BASE_URL  base URL for playwright (default http://127.0.0.1:18080)
#   TILLER_BROWSER_MOCK_BASE_URL  mock upstream /v1 base (default http://127.0.0.1:18081/v1)

set -eu

repo_dir=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
router_name=tiller-browser-router
mock_name=tiller-browser-mock
password=browser-test-password
router_port=${TILLER_BROWSER_ROUTER_PORT:-18080}
mock_port=${TILLER_BROWSER_MOCK_PORT:-18081}

stop_containers() {
    docker rm -f "$router_name" >/dev/null 2>&1 || true
    docker rm -f "$mock_name"   >/dev/null 2>&1 || true
}

cleanup() {
    stop_containers
}
trap cleanup EXIT INT TERM
stop_containers

# BuildKit is required for the RUN cache mounts in the test Dockerfiles.
export DOCKER_BUILDKIT=1

echo "==> Building tiller-router:dev (cached)"
# --pull=false: never re-download the base image / layer blobs we already have;
# --build-arg BUILDKIT_INLINE_CACHE is not needed — the docker driver keeps a
# local build cache, so repeat builds reuse unchanged layers.
docker build --pull=false -t tiller-router:dev "$repo_dir"

echo "==> Building tiller-router-browser-tests:dev (cached)"
docker build --pull=false -t tiller-router-browser-tests:dev "$repo_dir/tests/browser"

echo "==> Starting mock upstream on 127.0.0.1:$mock_port"
docker run --rm -d --name "$mock_name" --network host \
    -v "$repo_dir/tests/compatibility/mock_upstream.py:/mock_upstream.py:ro" \
    python:3.13-alpine python /mock_upstream.py >/dev/null

echo "==> Starting router on 127.0.0.1:$router_port (fresh ephemeral /data)"
# No bind mount: the router image runs as non-root UID 65532 and chmods its
# data dir to 0700, which it cannot do to a host dir owned by the invoking
# user. Using the image's internal /data gives a clean, empty, writable DB for
# this container — a fresh router means a fresh, isolated dataset per run.
docker run --rm -d --name "$router_name" --network host \
    -e TILLER_LISTEN_ADDR="127.0.0.1:$router_port" \
    -e TILLER_ADMIN_USERNAME=admin \
    -e TILLER_ADMIN_PASSWORD="$password" \
    tiller-router:dev >/dev/null
ready=0
for _ in $(seq 1 40); do
    if curl -fsS "http://127.0.0.1:$router_port/health/ready" >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    echo "FAIL: router never became ready" >&2
    docker logs "$router_name" >&2 || true
    exit 1
fi

echo "==> Running Playwright browser suite"
router_log="/tmp/router.log"
docker run --rm --network host \
    -e TILLER_BROWSER_BASE_URL="http://127.0.0.1:$router_port" \
    -e TILLER_BROWSER_MOCK_BASE_URL="http://127.0.0.1:$mock_port/v1" \
    -e TILLER_BROWSER_ADMIN_USERNAME=admin \
    -e TILLER_BROWSER_ADMIN_PASSWORD="$password" \
    tiller-router-browser-tests:dev
playwright_status=$?
echo "==> Capturing router logs for debugging"
docker logs "$router_name" > "$router_log" 2>&1 || true
router_lines=$(wc -l < "$router_log" 2>/dev/null || echo 0)
echo "    router log: $router_log ($router_lines lines)"
# Always upload router log as artifact for post-mortem, even on success
mkdir -p ./test-debug
cp "$router_log" ./test-debug/router.log 2>/dev/null || true
# Surface the last 200 lines of router logs on test failure for fast diagnosis
if [ "$playwright_status" -ne 0 ]; then
    echo "==> Playwright failed — last 200 lines of router log:"
    tail -n 200 "$router_log" >&2 || true
    echo "==> Searching router log for known failure patterns:"
    grep -iE "discover|refresh|models.dev|http" "$router_log" | tail -30 >&2 || true
fi
exit $playwright_status
