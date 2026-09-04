#!/bin/bash
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
password=compatibility-test-password
run_id=tiller-compat-$$
. "$repo_dir/tests/scripts/prune-test-logs.sh"
mkdir -p tests/logs/compat
sdk_data_dir="$(pwd)/tests/logs/compat/sdk-$run_id"
hermes_data_dir="$(pwd)/tests/logs/compat/hermes-$run_id"
mkdir -p "$sdk_data_dir" "$hermes_data_dir"

# Log capture: always write full output to a per-run log file. Every line is
# prefixed with the elapsed time since the run started (via
# tests/scripts/ts-filter.py, which captures its own start at process spawn)
# so a single log scan tells you exactly when each step happened.
LOG_FILE="tests/logs/compat/$(date -u +%Y%m%dT%H%M%S)-compat.log"
ts=$(date +%s)
exec > >(python3 -u tests/scripts/ts-filter.py | tee "$LOG_FILE") 2>&1

probe_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'; }
sdk_router_port=${TILLER_COMPAT_ROUTER_PORT:-$(probe_port)}
sdk_mock_port=$(probe_port)
hermes_router_port=$(probe_port)
hermes_mock_port=$(probe_port)

prime_data_dir() {
    docker run --rm -v "$1:/d" --user root alpine chown 65532:65532 /d
}
prime_data_dir "$sdk_data_dir"
prime_data_dir "$hermes_data_dir"

stop_containers() {
    containers=$(docker ps -aq --filter "name=$run_id-router-" --filter "name=$run_id-mock-")
    if [ -n "$containers" ]; then docker rm -f $containers >/dev/null 2>&1 || true; fi
}

cleanup() {
    stop_containers
    docker run --rm -v "$sdk_data_dir:/d" --user root alpine chown -R 1000:1000 /d >/dev/null 2>&1 || true
    docker run --rm -v "$hermes_data_dir:/d" --user root alpine chown -R 1000:1000 /d >/dev/null 2>&1 || true
    rm -rf "$sdk_data_dir" "$hermes_data_dir" || true
    # Always print where the log lives, even on early failure (e.g. when the
    # build stage aborts before sdk/hermes statuses are assigned). Lets the
    # reader find the captured detail without re-running.
    rc=${sdk_status:-0}
    [ -n "${hermes_status:-}" ] && [ "$hermes_status" -ne 0 ] && rc="$hermes_status"
    echo "==> compatibility suite: rc=$rc, log: $LOG_FILE" >&2
}
trap cleanup EXIT INT TERM
stop_containers

export DOCKER_BUILDKIT=1
. "$repo_dir/tests/scripts/build-router.sh"
docker build --pull=false -t tiller-router-sdk-probes:dev "$repo_dir/tests/compatibility"
if ! docker image inspect tiller-router-hermes-probe:dev >/dev/null 2>&1; then
    docker build --pull=false -f "$repo_dir/tests/compatibility/hermes.Dockerfile" -t tiller-router-hermes-probe:dev "$repo_dir/tests/compatibility"
else
    echo "==> tiller-router-hermes-probe:dev (cached)"
fi

start_mock() {
    docker run --rm -d --name "$1" --network host \
        -v "$repo_dir/tests/compatibility/mock_upstream.py:/mock_upstream.py:ro" \
        -e TILLER_MOCK_PORT="$2" \
        python:3.13-alpine python /mock_upstream.py >/dev/null
}

start_router() {
    name=$1
    port=$2
    data_dir=$3
    docker rm -f "$name" >/dev/null 2>&1 || true
    while docker ps -a --format '{{.Names}}' | grep -qx "$name"; do sleep 1; done
    docker run --rm -d --name "$name" --network host \
        -v "$data_dir:/data" \
        -e TILLER_LISTEN_ADDR="127.0.0.1:$port" \
        -e TILLER_ADMIN_USERNAME=admin \
        -e TILLER_ADMIN_PASSWORD="$password" \
        -e TILLER_LOG_LEVEL=warn \
        tiller-router:dev >/dev/null
    for _ in $(seq 1 10); do
        if curl -fsS "http://127.0.0.1:$port/health/ready" >/dev/null 2>&1; then return; fi
        sleep 1
    done
    docker logs "$name" >&2 || true
    return 1
}

start_mock "$run_id-mock-sdk" "$sdk_mock_port"
start_mock "$run_id-mock-hermes" "$hermes_mock_port"
start_router "$run_id-router-sdk" "$sdk_router_port" "$sdk_data_dir"
start_router "$run_id-router-hermes" "$hermes_router_port" "$hermes_data_dir"

docker run --rm --network host \
    -e TILLER_COMPAT_BASE_URL="http://127.0.0.1:$sdk_router_port" \
    -e TILLER_COMPAT_MOCK_BASE_URL="http://127.0.0.1:$sdk_mock_port/v1" \
    -e TILLER_COMPAT_ADMIN_PASSWORD="$password" \
    tiller-router-sdk-probes:dev &
sdk_pid=$!

# Preserve the existing restart coverage for the Hermes environment.
docker stop "$run_id-router-hermes" >/dev/null
start_router "$run_id-router-hermes" "$hermes_router_port" "$hermes_data_dir"
docker run --rm --network host \
    -e TILLER_COMPAT_BASE_URL="http://127.0.0.1:$hermes_router_port" \
    -e TILLER_COMPAT_MOCK_BASE_URL="http://127.0.0.1:$hermes_mock_port/v1" \
    -e TILLER_COMPAT_ADMIN_PASSWORD="$password" \
    tiller-router-hermes-probe:dev &
hermes_pid=$!
sdk_status=0
hermes_status=0
wait "$sdk_pid" || sdk_status=$?
wait "$hermes_pid" || hermes_status=$?

te=$(date +%s)
echo
echo "==> compatibility suite summary"
echo "    rc:      sdk=$sdk_status hermes=$hermes_status"
echo "    elapsed: $((te - ts))s"
echo "    log:     $LOG_FILE"
if [ "$sdk_status" -ne 0 ] || [ "$hermes_status" -ne 0 ]; then
    first_error=$(grep -m1 -E 'ERROR|FAIL|Error:' "$LOG_FILE" 2>/dev/null | head -c 400 || true)
    [ -n "$first_error" ] && echo "    first error: $first_error"
    exit 1
fi
