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
#   TILLER_BROWSER_WORKERS       admin shards (default 3); one activity lane is added
#   TILLER_BROWSER_BASE_URL  base URL for playwright (default http://127.0.0.1:18080)
#   TILLER_BROWSER_MOCK_BASE_URL  mock upstream /v1 base (default http://127.0.0.1:18081/v1)

set -eu

repo_dir=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
password=browser-test-password
workers=${TILLER_BROWSER_WORKERS:-${PLAYWRIGHT_WORKERS:-3}}
run_id=tiller-browser-$$
ports_file=$(mktemp)

case "$workers" in
    ''|*[!0-9]*|0) echo "TILLER_BROWSER_WORKERS must be a positive integer" >&2; exit 2 ;;
esac

probe_port() {
    python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

stop_containers() {
    containers=$(docker ps -aq --filter "name=$run_id-router-" --filter "name=$run_id-mock-")
    if [ -n "$containers" ]; then docker rm -f $containers >/dev/null 2>&1 || true; fi
}

cleanup() {
    stop_containers
    rm -f "$ports_file"
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

echo "==> Starting $workers isolated mock/router pairs"
for i in $(seq 0 $((workers - 1))); do
    if [ "$workers" = 1 ] && [ -n "${TILLER_BROWSER_MOCK_PORT:-}" ]; then mock_port=$TILLER_BROWSER_MOCK_PORT; else mock_port=$(probe_port); fi
    if [ "$workers" = 1 ] && [ -n "${TILLER_BROWSER_ROUTER_PORT:-}" ]; then router_port=$TILLER_BROWSER_ROUTER_PORT; else router_port=$(probe_port); fi
    mock_name="$run_id-mock-$i"
    router_name="$run_id-router-$i"
    echo "$i $router_port $mock_port" >> "$ports_file"
    docker run --rm -d --name "$mock_name" --network host \
        -v "$repo_dir/tests/compatibility/mock_upstream.py:/mock_upstream.py:ro" \
        -e TILLER_MOCK_PORT="$mock_port" \
        python:3.13-alpine python /mock_upstream.py >/dev/null
    docker run --rm -d --name "$router_name" --network host \
        -e TILLER_LISTEN_ADDR="127.0.0.1:$router_port" \
        -e TILLER_ADMIN_USERNAME=admin \
        -e TILLER_ADMIN_PASSWORD="$password" \
        tiller-router:dev >/dev/null
done

activity_mock_port=$(probe_port)
activity_router_port=$(probe_port)
docker run --rm -d --name "$run_id-mock-activity" --network host \
    -v "$repo_dir/tests/compatibility/mock_upstream.py:/mock_upstream.py:ro" \
    -e TILLER_MOCK_PORT="$activity_mock_port" \
    python:3.13-alpine python /mock_upstream.py >/dev/null
docker run --rm -d --name "$run_id-router-activity" --network host \
    -e TILLER_LISTEN_ADDR="127.0.0.1:$activity_router_port" \
    -e TILLER_ADMIN_USERNAME=admin \
    -e TILLER_ADMIN_PASSWORD="$password" \
    tiller-router:dev >/dev/null

while read -r i router_port mock_port; do
    echo "==> Waiting for worker $i mock/router readiness"
    mock_ready=0
    for _ in $(seq 1 60); do
        if curl -fsS "http://127.0.0.1:$mock_port/v1/models" >/dev/null 2>&1; then mock_ready=1; break; fi
        sleep 1
    done
    if [ "$mock_ready" -ne 1 ]; then echo "FAIL: worker $i mock never became ready" >&2; exit 1; fi
    ready=0
    for _ in $(seq 1 40); do
        if curl -fsS "http://127.0.0.1:$router_port/health/ready" >/dev/null 2>&1; then ready=1; break; fi
        sleep 1
    done
    if [ "$ready" -ne 1 ]; then echo "FAIL: worker $i router never became ready" >&2; exit 1; fi
done < "$ports_file"

echo "==> Waiting for dedicated activity lane readiness"
activity_mock_ready=0
for _ in $(seq 1 60); do
    if curl -fsS "http://127.0.0.1:$activity_mock_port/v1/models" >/dev/null 2>&1; then activity_mock_ready=1; break; fi
    sleep 1
done
if [ "$activity_mock_ready" -ne 1 ]; then echo "FAIL: activity mock never became ready" >&2; exit 1; fi
activity_router_ready=0
for _ in $(seq 1 40); do
    if curl -fsS "http://127.0.0.1:$activity_router_port/health/ready" >/dev/null 2>&1; then activity_router_ready=1; break; fi
    sleep 1
done
if [ "$activity_router_ready" -ne 1 ]; then echo "FAIL: activity router never became ready" >&2; exit 1; fi

echo "==> Running Playwright browser suite"
# Run one Playwright shard per isolated router. Each process has a normal,
# fixed base URL, avoiding shared process environment or unsupported fixture
# overrides while still allowing the shard count to match machine capacity.
pids_file=$(mktemp)
while read -r i router_port mock_port; do
    docker run --rm --network host \
        -e TILLER_BROWSER_BASE_URL="http://127.0.0.1:$router_port" \
        -e TILLER_BROWSER_MOCK_BASE_URL="http://127.0.0.1:$mock_port/v1" \
        -e PLAYWRIGHT_WORKERS=1 \
        -e TILLER_BROWSER_ADMIN_USERNAME=admin \
        -e TILLER_BROWSER_ADMIN_PASSWORD="$password" \
        tiller-router-browser-tests:dev npx playwright test admin.spec.js --shard="$((i + 1))/$workers" &
    echo "$i $!" >> "$pids_file"
done < "$ports_file"

docker run --rm --network host \
    -e TILLER_BROWSER_BASE_URL="http://127.0.0.1:$activity_router_port" \
    -e TILLER_BROWSER_MOCK_BASE_URL="http://127.0.0.1:$activity_mock_port/v1" \
    -e PLAYWRIGHT_WORKERS=1 \
    -e TILLER_BROWSER_ADMIN_USERNAME=admin \
    -e TILLER_BROWSER_ADMIN_PASSWORD="$password" \
    tiller-router-browser-tests:dev npx playwright test activity.spec.js &
activity_pid=$!

playwright_status=0
while read -r i pid; do
    if wait "$pid"; then :; else playwright_status=$?; fi
done < "$pids_file"

if wait "$activity_pid"; then :; else playwright_status=$?; fi
rm -f "$pids_file"
if [ "$playwright_status" -ne 0 ]; then
    echo "==> Playwright failed — router logs:" >&2
    for log_name in $(docker ps -a --format '{{.Names}}' | grep "^$run_id-router-"); do
        echo "--- $log_name ---" >&2
        docker logs "$log_name" 2>&1 | tail -n 100 >&2 || true
    done
    exit "$playwright_status"
fi
