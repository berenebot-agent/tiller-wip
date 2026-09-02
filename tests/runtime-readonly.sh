#!/bin/sh
# Runtime read-only root filesystem test for tiller-router.
#
# Verifies the production image runs correctly under the deployment runtime
# settings from docker-compose.yml: read-only root, /tmp tmpfs, all caps
# dropped, no-new-privileges, non-root UID/GID 65532, and a bind-mounted
# /data. Also exercises the authenticated SQLite backup export.
#
# Modeled on tests/compatibility/run.sh (shell, set -eu, cleanup trap,
# host networking, env-var admin credentials, builds tiller-router:dev).
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
name=tiller-runtime-readonly
password=runtime-readonly-test-password
port=18081
base="http://127.0.0.1:$port"

cleanup() {
    docker rm -f "$name" >/dev/null 2>&1 || true
    if [ -n "${data_dir:-}" ]; then
        docker run --rm -v "$data_dir:/d" --user root alpine chown -R "${host_uid:-0}:${host_gid:-0}" /d >/dev/null 2>&1 || true
        # Teardown must not fail the run (the chown-back helper above can fail
        # transiently, leaving the dir owned by 65532 and rm would EPERM).
        rm -rf "$data_dir" || true
    fi
    [ -z "${cookie_jar:-}" ] || rm -f "$cookie_jar"
    [ -z "${backup_file:-}" ] || rm -f "$backup_file"
}
trap cleanup EXIT INT TERM
cleanup

data_dir=$(mktemp -d)
cookie_jar=$(mktemp)
backup_file=$(mktemp)
host_uid=$(id -u)
host_gid=$(id -g)

# Host dir for the /data bind mount, owned by the non-root runtime user.
if ! chmod 700 "$data_dir"; then
    echo "FAIL: unable to set /data test mount mode to 0700" >&2
    exit 1
fi
if ! chown 65532:65532 "$data_dir" 2>/dev/null &&
    ! sudo -n chown 65532:65532 "$data_dir" 2>/dev/null &&
    ! docker run --rm --user 0:0 -v "$data_dir:/data" alpine:3.20 \
        chown 65532:65532 /data >/dev/null 2>&1; then
    echo "FAIL: unable to chown /data test mount to UID/GID 65532" >&2
    exit 1
fi

echo "==> Building tiller-router:dev"
docker build -t tiller-router:dev "$repo_dir"

echo "==> Starting container with deployment runtime settings"
docker run --rm -d --name "$name" --network host \
    --read-only \
    --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777 \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    --user 65532:65532 \
    -v "$data_dir:/data" \
    -e TILLER_ADMIN_USERNAME=admin \
    -e TILLER_ADMIN_PASSWORD="$password" \
    -e TILLER_LISTEN_ADDR="127.0.0.1:$port" \
    -e TILLER_DATA_DIR=/data \
    -e TILLER_TRUSTED_PROXY=127.0.0.0/8 \
    tiller-router:dev >/dev/null

echo "==> Waiting for readiness"
ready=0
i=0
while [ "$i" -lt 60 ]; do
    if curl -fsS "$base/health/ready" >/dev/null 2>&1; then
        ready=1
        break
    fi
    i=$((i + 1))
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    echo "FAIL: container never became ready" >&2
    exit 1
fi
echo "    ready after ${i}s"

echo "==> Health endpoints"
live_code=$(curl -s -o /dev/null -w '%{http_code}' "$base/health/live")
ready_code=$(curl -s -o /dev/null -w '%{http_code}' "$base/health/ready")
echo "    /health/live  -> $live_code"
echo "    /health/ready -> $ready_code"
[ "$live_code" = "200" ] || { echo "FAIL: /health/live returned $live_code" >&2; exit 1; }
[ "$ready_code" = "200" ] || { echo "FAIL: /health/ready returned $ready_code" >&2; exit 1; }

echo "==> Migrations/startup wrote to /data bind mount"
if [ ! -f "$data_dir/tiller-router.db" ] && ! docker run --rm --user 0:0 \
    -v "$data_dir:/data" alpine:3.20 test -f /data/tiller-router.db; then
    echo "FAIL: no tiller-router.db under /data" >&2
    exit 1
fi
echo "    $data_dir/tiller-router.db present"

echo "==> docker inspect runtime settings"
ro=$(docker inspect -f '{{.HostConfig.ReadonlyRootfs}}' "$name")
capdrop=$(docker inspect -f '{{.HostConfig.CapDrop}}' "$name")
secopt=$(docker inspect -f '{{.HostConfig.SecurityOpt}}' "$name")
echo "    ReadonlyRootfs=$ro"
echo "    CapDrop=$capdrop"
echo "    SecurityOpt=$secopt"
[ "$ro" = "true" ] || { echo "FAIL: ReadonlyRootfs is not true" >&2; exit 1; }
echo "$capdrop" | grep -q 'ALL' || { echo "FAIL: CapDrop does not include ALL" >&2; exit 1; }
echo "$secopt" | grep -q 'no-new-privileges' || { echo "FAIL: no-new-privileges missing" >&2; exit 1; }

echo "==> Authenticated backup export"
login=$(curl -s -c "$cookie_jar" -H 'Content-Type: application/json' \
    -d "{\"username\":\"admin\",\"password\":\"$password\"}" \
    "$base/api/admin/session")
csrf=$(printf '%s' "$login" | sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p')
[ -n "$csrf" ] || { echo "FAIL: no csrf_token in login response" >&2; exit 1; }
backup_code=$(curl -s -b "$cookie_jar" -o "$backup_file" -w '%{http_code}' \
    "$base/api/admin/backup/export")
echo "    backup export -> $backup_code"
[ "$backup_code" = "200" ] || { echo "FAIL: backup export returned $backup_code" >&2; exit 1; }
header=$(head -c 15 "$backup_file")
echo "    backup header: $header"
[ "$header" = "SQLite format 3" ] || { echo "FAIL: backup is not a valid SQLite file" >&2; exit 1; }
if [ ! -d "$data_dir/backups" ] && ! docker run --rm --user 0:0 \
    -v "$data_dir:/data" alpine:3.20 test -d /data/backups; then
    echo "FAIL: backup did not write under /data/backups" >&2
    exit 1
fi

echo "==> Write behavior demonstration (read-only shell container, identical flags)"
# The scratch image has no shell, so the authoritative read-only check is
# docker inspect (ReadonlyRootfs=true) above. This one-off alpine container
# with identical flags demonstrates the flags themselves: /tmp writable,
# root filesystem not.
if docker run --rm --read-only \
    --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777 \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    --user 65532:65532 \
    --entrypoint /bin/sh \
    alpine:3.20 -c 'touch /tmp/x && ! touch /root-test'; then
    echo "    /tmp writable, /root-test rejected (read-only root in effect)"
else
    echo "FAIL: read-only write demonstration failed" >&2
    exit 1
fi

echo "PASS: all runtime read-only checks passed"
