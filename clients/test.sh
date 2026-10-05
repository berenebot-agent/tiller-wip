#!/bin/bash
# Container-only gateway client verification; live calls are explicitly opt-in.
set -euo pipefail
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
mkdir -p tests/logs/clients
log="tests/logs/clients/$(date -u +%Y%m%dT%H%M%S).log"
start=$SECONDS
run() {
    local label=$1
    shift
    local code=0
    "$@" >>"$log" 2>&1 || code=$?
    printf '[gateway clients] %s %s\n' "$label" "$([ "$code" -eq 0 ] && printf PASS || printf FAIL)"
    if [ "$code" -ne 0 ]; then
        printf 'detail: %s\n' "$root/$log"
        exit "$code"
    fi
}
run go-contract ./tiller-go.sh test -count=1 ./clients/go ./clients/examples/go
run tiller-http ./tiller-go.sh test -count=1 ./internal/server -run TestGatewayClient
run go-vet ./tiller-go.sh vet ./clients/go ./clients/examples/go
run python-build docker build --pull=false -f clients/python/Dockerfile.test -t tiller-gateway-python-tests:dev clients
run python-wheel docker run --rm --memory=512m --memory-swap=512m tiller-gateway-python-tests:dev
if [ "${1:-}" = "--live" ]; then
    config=${2:?Usage: bash clients/test.sh --live tests/logs/protected-config.json}
    if [[ "$config" = /* || "$config" = *..* || ! -f "$config" ]]; then
        printf 'Live config must be a protected file relative to the repo root.\n' >&2
        exit 2
    fi
    run go-live ./tiller-go.sh run ./clients/examples/go --mode live --config "/src/$config" --features tools,structured,image
    run python-live docker run --rm --memory=512m --memory-swap=512m \
        --user "$(id -u):$(id -g)" -v "$root/$config:/config.json:ro" \
        tiller-gateway-python-tests:dev python /work/clients/examples/python/live_acceptance.py /config.json --features
elif [ "$#" -ne 0 ]; then
    printf 'Usage: bash clients/test.sh [--live tests/logs/protected-config.json]\n' >&2
    exit 2
fi
printf '[gateway clients] overall PASS %ss; detail: %s\n' "$((SECONDS - start))" "$root/$log"
