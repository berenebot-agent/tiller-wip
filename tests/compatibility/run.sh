#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
router_name=tiller-compat-router
mock_name=tiller-compat-mock
password=compatibility-test-password

cleanup() {
    docker stop "$router_name" >/dev/null 2>&1 || true
    docker stop "$mock_name" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM
cleanup

docker build -t tiller-router:dev "$repo_dir"
docker build -t tiller-router-sdk-probes:dev "$repo_dir/tests/compatibility"
docker build -f "$repo_dir/tests/compatibility/hermes.Dockerfile" -t tiller-router-hermes-probe:dev "$repo_dir/tests/compatibility"

docker run --rm -d --name "$mock_name" --network host \
    -v "$repo_dir/tests/compatibility/mock_upstream.py:/mock_upstream.py:ro" \
    python:3.13-alpine python /mock_upstream.py >/dev/null

start_router() {
    docker run --rm -d --name "$router_name" --network host \
        -e TILLER_LISTEN_ADDR=127.0.0.1:18080 \
        -e TILLER_ADMIN_USERNAME=admin \
        -e TILLER_ADMIN_PASSWORD="$password" \
        tiller-router:dev >/dev/null
    docker exec "$router_name" /tiller-router healthcheck
}

start_router
docker run --rm --network host \
    -e TILLER_COMPAT_BASE_URL=http://127.0.0.1:18080 \
    -e TILLER_COMPAT_ADMIN_PASSWORD="$password" \
    tiller-router-sdk-probes:dev

docker stop "$router_name" >/dev/null
start_router
docker run --rm --network host \
    -e TILLER_COMPAT_BASE_URL=http://127.0.0.1:18080 \
    -e TILLER_COMPAT_ADMIN_PASSWORD="$password" \
    tiller-router-hermes-probe:dev
