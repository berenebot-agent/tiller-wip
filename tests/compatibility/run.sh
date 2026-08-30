#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
router_name=tiller-compat-router
mock_name=tiller-compat-mock
password=compatibility-test-password
data_dir=$(mktemp -d)
chmod 0777 "$data_dir"
router_port=${TILLER_COMPAT_ROUTER_PORT:-$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')}

stop_containers() {
    docker stop "$router_name" >/dev/null 2>&1 || true
    docker stop "$mock_name" >/dev/null 2>&1 || true
}

cleanup() {
    stop_containers
    chmod -R ugo+rwx "$data_dir" >/dev/null 2>&1 || true
    rm -rf "$data_dir"
}
trap cleanup EXIT INT TERM
stop_containers

docker build -t tiller-router:dev "$repo_dir"
docker build -t tiller-router-sdk-probes:dev "$repo_dir/tests/compatibility"
docker build -f "$repo_dir/tests/compatibility/hermes.Dockerfile" -t tiller-router-hermes-probe:dev "$repo_dir/tests/compatibility"

docker run --rm -d --name "$mock_name" --network host \
    -v "$repo_dir/tests/compatibility/mock_upstream.py:/mock_upstream.py:ro" \
    python:3.13-alpine python /mock_upstream.py >/dev/null

start_router() {
    docker run --rm -d --name "$router_name" --network host \
        -v "$data_dir:/data" \
        -e TILLER_LISTEN_ADDR=127.0.0.1:$router_port \
        -e TILLER_ADMIN_USERNAME=admin \
        -e TILLER_ADMIN_PASSWORD="$password" \
        tiller-router:dev >/dev/null
    for _ in $(seq 1 30); do
        if docker exec "$router_name" /tiller-router healthcheck >/dev/null 2>&1; then
            return
        fi
        sleep 1
    done
    docker logs "$router_name" >&2 || true
    return 1
}

start_router
docker run --rm --network host \
    -e TILLER_COMPAT_BASE_URL=http://127.0.0.1:$router_port \
    -e TILLER_COMPAT_ADMIN_PASSWORD="$password" \
    tiller-router-sdk-probes:dev

docker stop "$router_name" >/dev/null
start_router
docker run --rm --network host \
    -e TILLER_COMPAT_BASE_URL=http://127.0.0.1:$router_port \
    -e TILLER_COMPAT_ADMIN_PASSWORD="$password" \
    tiller-router-hermes-probe:dev
