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

# The mock image may still be pulling on a cold CI runner. The router below
# becomes ready in seconds while the mock's Python server is not yet listening
# on 18081; the first provider creates would then race connection-refused from
# discoverPaged, yield an empty catalogue, and flake the suite. So wait for the
# mock's /v1/models to answer BEFORE running any tests. Give it a generous
# window to cover the cold-image pull on a fresh runner.
echo "==> Waiting for mock upstream to be ready on 127.0.0.1:$mock_port"
mock_ready=0
for _ in $(seq 1 60); do
    if curl -fsS "http://127.0.0.1:$mock_port/v1/models" >/dev/null 2>&1; then
        mock_ready=1
        break
    fi
    sleep 1
done
if [ "$mock_ready" -ne 1 ]; then
    echo "FAIL: mock upstream never became ready" >&2
    docker logs "$mock_name" >&2 || true
    exit 1
fi

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
# Capture the playwright exit status explicitly (`set -e` would abort here if
# we let the container's non-zero exit propagate) so we can surface the router
# log on failure and still exit non-zero.
if ! docker run --rm --network host \
    -e TILLER_BROWSER_BASE_URL="http://127.0.0.1:$router_port" \
    -e TILLER_BROWSER_MOCK_BASE_URL="http://127.0.0.1:$mock_port/v1" \
    -e TILLER_BROWSER_ADMIN_USERNAME=admin \
    -e TILLER_BROWSER_ADMIN_PASSWORD="$password" \
    tiller-router-browser-tests:dev; then
    playwright_status=$?
    echo "==> Playwright failed — last 100 lines of router log:" >&2
    docker logs "$router_name" 2>&1 | tail -n 100 >&2 || true
    exit $playwright_status
fi
