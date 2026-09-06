#!/bin/bash
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
run_start=$(date +%s)
note_phase() {
    echo "==> $1"
}
# Per-run scratch lives under tests/logs/ (gitignored). Holds the activity
# router's host-mounted data dir, the extracted fixturectl binary, the run
# log, and the Playwright artifacts (preserved on failure, auto-removed on
# success). The old /tmp mktemp path had external-permission issues.
# Prune stale runs first so interrupted runs don't accumulate on disk.
. "$repo_dir/tests/scripts/prune-test-logs.sh"
mkdir -p tests/logs
run_dir="$(pwd)/tests/logs/$run_id"
mkdir -p "$run_dir/playwright-results" "$run_dir/activity-data"
# Capture the full run output to a per-run log file. The summary block at the
# end prints this path; on failure the first error is also inlined. Every
# line of the run is prefixed with the elapsed time since the run started
# (via tests/scripts/ts-filter.py) so a single log scan tells you exactly
# when each step happened.
RUN_LOG="$run_dir/run.log"
exec > >(python3 -u tests/scripts/ts-filter.py | tee "$RUN_LOG") 2>&1

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
    if [ -n "${run_dir:-}" ] && [ -d "$run_dir" ]; then
        # Preserve the run on failure (artifacts are still in $run_dir for
        # inspection); clean up on success. The activity router chowns its
        # data dir to uid 65532, so a plain rm may fail; fall back to a root
        # throwaway container to remove the tree.
        if [ "${playwright_status:-0}" -eq 0 ]; then
            if ! rm -rf "$run_dir" 2>/dev/null; then
                # The activity router chowns its data dir to uid 65532, so a
                # plain rm fails. Remove the tree's contents as root (the
                # mount root itself can't be removed from inside the
                # container), then let the host rm the now-empty dir.
                docker run --rm -v "$run_dir:/cleanup:rw" python:3.13-alpine python -c "import shutil,os; [shutil.rmtree(os.path.join('/cleanup',d)) if os.path.isdir(os.path.join('/cleanup',d)) else os.remove(os.path.join('/cleanup',d)) for d in os.listdir('/cleanup')]" >/dev/null 2>&1 || true
                rm -rf "$run_dir" 2>/dev/null || true
            fi
        else
            echo "==> preserving failed-run artifacts at $run_dir" >&2
        fi
    fi
}
trap cleanup EXIT INT TERM
stop_containers

# BuildKit is required for the RUN cache mounts in the test Dockerfiles.
export DOCKER_BUILDKIT=1

. "$repo_dir/tests/scripts/build-router.sh"

echo "==> Building tiller-router-browser-tests:dev (cached)"
docker build --pull=false -t tiller-router-browser-tests:dev "$repo_dir/tests/browser"

echo "==> Building fixturectl (cached) and extracting the binary"
docker build --pull=false -q -t tiller-router-fixturectl:dev -f "$repo_dir/tests/fixturectl/Dockerfile" "$repo_dir" >/dev/null
fixture_cid=$(docker create tiller-router-fixturectl:dev /fixturectl)
docker cp "$fixture_cid:/fixturectl" "$run_dir/fixturectl"
docker rm "$fixture_cid" >/dev/null
chmod +x "$run_dir/fixturectl"
note_phase "images built and fixturectl extracted"

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
        -e TILLER_LOG_LEVEL=warn \
        tiller-router:dev >/dev/null
done

activity_mock_port=$(probe_port)
activity_router_port=$(probe_port)
docker run --rm -d --name "$run_id-mock-activity" --network host \
    -v "$repo_dir/tests/compatibility/mock_upstream.py:/mock_upstream.py:ro" \
    -e TILLER_MOCK_PORT="$activity_mock_port" \
    python:3.13-alpine python /mock_upstream.py >/dev/null
# The activity router's /data is a host mount so fixturectl (invoked by the
# activity spec, against the same mount inside the browser container) can seed
# rows directly. Started as root so the router's own privdrop chowns the fresh
# mount to its runtime user, exactly like the documented compose posture.
docker run --rm -d --name "$run_id-router-activity" --network host \
    --user 0:0 \
    -v "$run_dir/activity-data:/data" \
    -e TILLER_LISTEN_ADDR="127.0.0.1:$activity_router_port" \
    -e TILLER_ADMIN_USERNAME=admin \
    -e TILLER_ADMIN_PASSWORD="$password" \
    -e TILLER_LOG_LEVEL=warn \
    tiller-router:dev >/dev/null
note_phase "containers started"

# Wait for every mock/router to become ready CONCURRENTLY. All containers were
# started above, so polling them one at a time only serializes a wait that can
# run in parallel. wait_url polls a URL every 0.25s and records OK/FAIL to a
# result file; the script then waits for all probes and reports any failure.
wait_url() {
    name=$1 url=$2 limit=$3 out=$4
    tries=0
    while [ "$tries" -lt "$((limit * 4))" ]; do
        if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
            echo "OK $name" > "$out"
            return 0
        fi
        tries=$((tries + 1))
        sleep 0.25
    done
    echo "FAIL $name" > "$out"
    return 1
}

ready_dir=$(mktemp -d)
ready_pids=""
probe_endpoint() {
    name=$1 url=$2 limit=$3
    wait_url "$name" "$url" "$limit" "$ready_dir/$name" &
    ready_pids="$ready_pids $!"
}
echo "==> Waiting for mock/router readiness (all endpoints in parallel)"
while read -r i router_port mock_port; do
    probe_endpoint "worker$i-mock" "http://127.0.0.1:$mock_port/v1/models" 60
    probe_endpoint "worker$i-router" "http://127.0.0.1:$router_port/health/ready" 40
done < "$ports_file"
probe_endpoint "activity-mock" "http://127.0.0.1:$activity_mock_port/v1/models" 60
probe_endpoint "activity-router" "http://127.0.0.1:$activity_router_port/health/ready" 40
for p in $ready_pids; do
    wait "$p" || true
done
failed=""
for f in "$ready_dir"/*; do
    [ -f "$f" ] || continue
    read -r status name < "$f"
    if [ "$status" != "OK" ]; then
        failed="$failed $name"
    fi
done
rm -rf "$ready_dir"
if [ -n "$failed" ]; then
    echo "FAIL: never became ready:$failed" >&2
    exit 1
fi
note_phase "all endpoints ready"

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
        -v "$run_dir/playwright-results:/tests/test-results" \
        tiller-router-browser-tests:dev npx playwright test admin.spec.js live.spec.js capabilities.spec.js --shard="$((i + 1))/$workers" &
    echo "$i $!" >> "$pids_file"
done < "$ports_file"

docker run --rm --network host \
    -e TILLER_BROWSER_BASE_URL="http://127.0.0.1:$activity_router_port" \
    -e TILLER_BROWSER_MOCK_BASE_URL="http://127.0.0.1:$activity_mock_port/v1" \
    -e PLAYWRIGHT_WORKERS=1 \
    -e TILLER_BROWSER_ADMIN_USERNAME=admin \
    -e TILLER_BROWSER_ADMIN_PASSWORD="$password" \
    -e TILLER_FIXTURE_BIN=/usr/local/bin/fixturectl \
    -e TILLER_FIXTURE_DB=/fixture-data/tiller-router.db \
    -v "$run_dir/fixturectl:/usr/local/bin/fixturectl:ro" \
    -v "$run_dir/activity-data:/fixture-data:rw" \
    -v "$run_dir/playwright-results:/tests/test-results" \
    tiller-router-browser-tests:dev npx playwright test activity.spec.js &
activity_pid=$!

playwright_status=0
shard_rcs=()
failing_shards=()
while read -r i pid; do
    if wait "$pid"; then
        shard_rcs+=(0)
    else
        rc=$?
        shard_rcs+=("$rc")
        failing_shards+=("$i")
    fi
done < "$pids_file"
if wait "$activity_pid"; then
    shard_rcs+=(0)
else
    rc=$?
    shard_rcs+=("$rc")
    failing_shards+=("activity")
fi
for rc in "${shard_rcs[@]}"; do
    [ "$rc" -gt "$playwright_status" ] && playwright_status=$rc
done
rm -f "$pids_file"
echo
echo "==> browser suite summary"
echo "    rc:         $playwright_status"
echo "    run log:    $RUN_LOG"
if [ "$playwright_status" -ne 0 ]; then
    echo "    artifacts:  $run_dir/playwright-results/  (Playwright traces, error-context.md, screenshots)"
    # Playwright only writes per-test error-context.md / trace.zip for failing
    # tests, so listing the directory enumerates exactly the failing tests
    # without needing to grep run.log for the ✘ lines and correlate their
    # slugs. One bullet per failing test, paths absolute so the agent can
    # read them without joining.
    err_contexts=()
    while IFS= read -r ctx; do
        err_contexts+=("$ctx")
    done < <(find "$run_dir/playwright-results" -mindepth 2 -maxdepth 2 -name 'error-context.md' 2>/dev/null | sort)
    if [ "${#err_contexts[@]}" -gt 0 ]; then
        echo "    per-test error contexts (one per failing test):"
        for ctx in "${err_contexts[@]}"; do
            slug_dir="$(dirname "$ctx")"
            trace="$slug_dir/trace.zip"
            echo "      - $ctx"
            echo "          trace: $trace"
        done
    fi
    first_error=$(grep -m1 -E 'Error: ' "$RUN_LOG" 2>/dev/null | head -c 400 || true)
    [ -n "$first_error" ] && echo "    first error: $first_error"
    if [ "${#failing_shards[@]}" -gt 0 ]; then
        echo
        echo "==> Playwright failed — router logs (failing shards only: ${failing_shards[*]}):" >&2
        for idx in "${failing_shards[@]}"; do
            if [ "$idx" = "activity" ]; then
                log_name="$run_id-router-activity"
            else
                log_name="$run_id-router-$idx"
            fi
            echo "--- $log_name ---" >&2
            docker logs "$log_name" 2>&1 | tail -n 100 >&2 || true
        done
    else
        echo
        echo "==> Playwright failed — no shard rc captured (router logs unavailable):" >&2
    fi
    exit "$playwright_status"
fi
note_phase "browser suite complete"
