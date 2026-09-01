#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
router_name=tiller-compat-router
mock_name=tiller-compat-mock
password=compatibility-test-password
data_dir=$(mktemp -d)
# The router image runs as non-root UID 65532 and chmods its data dir (0700)
# on startup. A host dir owned by us (UID 1000) cannot be chmodded by the
# router, so prime its ownership to 65532 through a short-lived root helper
# container (writing through the bind mount affects the host dir). Compat
# needs the bind mount because it restarts the router mid-test, and the data
# must persist across the restart.
docker run --rm -v "$data_dir:/d" --user root alpine chown 65532:65532 /d
router_port=${TILLER_COMPAT_ROUTER_PORT:-$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')}

stop_containers() {
    docker rm -f "$router_name" >/dev/null 2>&1 || true
    docker rm -f "$mock_name" >/dev/null 2>&1 || true
}

cleanup() {
    stop_containers
    # The router chowns/chmods our temp dir as UID 65532; chown it back to our
    # UID first so rm can remove it (only possible as root via a helper).
    docker run --rm -v "$data_dir:/d" --user root alpine chown -R 1000:1000 /d >/dev/null 2>&1 || true
    # Teardown must not fail the run: if the chown-back helper failed (e.g. the
    # Docker daemon hiccup) the temp dir stays owned by 65532 and this rm would
    # EPERM under set -e, turning a green run red. Best-effort cleanup only.
    rm -rf "$data_dir" || true
}
trap cleanup EXIT INT TERM
stop_containers

# BuildKit is required for the cache mounts in the test Dockerfiles.
export DOCKER_BUILDKIT=1
# --pull=false: reuse the base layers/blobs we already have locally instead of
# re-downloading them each run (the BuildKit RUN cache mounts in the
# Dockerfiles keep the apk/npm/pip package downloads durable across rebuilds).
docker build --pull=false -t tiller-router:dev "$repo_dir"
docker build --pull=false -t tiller-router-sdk-probes:dev "$repo_dir/tests/compatibility"
docker build --pull=false -f "$repo_dir/tests/compatibility/hermes.Dockerfile" -t tiller-router-hermes-probe:dev "$repo_dir/tests/compatibility"

docker run --rm -d --name "$mock_name" --network host \
    -v "$repo_dir/tests/compatibility/mock_upstream.py:/mock_upstream.py:ro" \
    python:3.13-alpine python /mock_upstream.py >/dev/null

start_router() {
    # Make the restart idempotent. `docker stop` on a --rm container leaves it
    # "Dead" while Docker's auto-remove goroutine releases the name asynchronously;
    # rm -f can briefly race that and leave the name held. Wait until the name is
    # actually free before re-creating so `docker run` can't hit a name conflict.
    docker rm -f "$router_name" >/dev/null 2>&1 || true
    while docker ps -a --format '{{.Names}}' | grep -qx "$router_name"; do
        sleep 1
    done
    docker run --rm -d --name "$router_name" --network host \
        -v "$data_dir:/data" \
        -e TILLER_LISTEN_ADDR=127.0.0.1:$router_port \
        -e TILLER_ADMIN_USERNAME=admin \
        -e TILLER_ADMIN_PASSWORD="$password" \
        tiller-router:dev >/dev/null
    # HTTP readiness catches a container that failed to start (e.g. the UID
    # 65532 /data chmod issue) rather than waiting on a dead container.
    ready=0
    for _ in $(seq 1 30); do
        if curl -fsS "http://127.0.0.1:$router_port/health/ready" >/dev/null 2>&1; then
            ready=1
            return
        fi
        sleep 1
    done
    if [ "$ready" -ne 1 ]; then
        docker logs "$router_name" >&2 || true
        return 1
    fi
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
