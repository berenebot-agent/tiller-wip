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
. "$repo_dir/tests/scripts/build-router.sh"

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

echo "==> Default (adoption-first) posture: root-at-boot self-heals a fresh ./data"
# The default image has no baked-in `user:` (root at boot); the new defaults
# in docker-compose.yml enable read-only rootfs, the /tmp tmpfs, and
# no-new-privileges. Those are all compatible with the in-process Go
# privdrop (chown lives on the bind mount, setuid happens before no-new-
# privileges is enforced for the runtime, and /tmp is a writable tmpfs).
# A fresh bind mount created by rootful Docker is root-owned; the drop
# must chown it to 65532:65532 and the running process must be non-root
# after boot.
default_dir=$(mktemp -d)
dc=dropped-$$
docker run --rm -d --name "$dc" --network host \
    --read-only \
    --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777 \
    --security-opt no-new-privileges:true \
    -v "$default_dir:/data" \
    -e TILLER_ADMIN_USERNAME=admin \
    -e TILLER_ADMIN_PASSWORD="$password" \
    -e TILLER_LISTEN_ADDR="127.0.0.1:18082" \
    -e TILLER_DATA_DIR=/data \
    tiller-router:dev >/dev/null
ready=0
i=0
while [ "$i" -lt 60 ]; do
    if curl -fsS "http://127.0.0.1:18082/health/ready" >/dev/null 2>&1; then
        ready=1
        break
    fi
    i=$((i + 1))
    sleep 1
done
docker rm -f "$dc" >/dev/null 2>&1 || true
if [ "$ready" -ne 1 ]; then
    echo "FAIL: default posture never became ready" >&2
    rm -rf "$default_dir" || true
    exit 1
fi
owner=$(docker run --rm -v "$default_dir:/d:ro" --user root alpine stat -c '%u:%g' /d/tiller-router.db)
echo "    fresh ./data -> $owner (owner of tiller-router.db)"
[ "$owner" = "65532:65532" ] || {
    echo "FAIL: default posture left ./data owned by $owner, want 65532:65532" >&2
    rm -rf "$default_dir" || true
    exit 1
}
# Verify the running process is non-root after the drop. The image has no
# shell, so the standard `ps`/`id` route is unavailable; instead, attach
# a sidecar container to the same network namespace and use Alpine's
# /proc/1/status parser. (We can't `docker exec` into the scratch image
# because there's no shell.)
probe_dir=$(mktemp -d)
pc=probe-$$
docker run --rm -d --name "$pc" --network host \
    --read-only \
    --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777 \
    --security-opt no-new-privileges:true \
    -v "$probe_dir:/data" \
    -e TILLER_ADMIN_USERNAME=admin \
    -e TILLER_ADMIN_PASSWORD="$password" \
    -e TILLER_LISTEN_ADDR="127.0.0.1:18083" \
    -e TILLER_DATA_DIR=/data \
    tiller-router:dev >/dev/null
probe_ready=0
i=0
while [ "$i" -lt 60 ]; do
    if curl -fsS "http://127.0.0.1:18083/health/ready" >/dev/null 2>&1; then
        probe_ready=1
        break
    fi
    i=$((i + 1))
    sleep 1
done
if [ "$probe_ready" -ne 1 ]; then
    echo "FAIL: probe container never became ready" >&2
    docker rm -f "$pc" >/dev/null 2>&1 || true
    rm -rf "$default_dir" "$probe_dir" || true
    exit 1
fi
# The tiller-router process is the only one inside the container, so
# its PID 1 /proc/<pid>/status is what we want. We can't list PIDs from
# outside the container, but the entrypoint command is /tiller-router
# (ENTRYPOINT in the Dockerfile), so PID 1 is the router. Read
# /proc/1/status from a sidecar that shares the pid namespace.
spc=sidecar-$$
docker run --rm -d --name "$spc" \
    --pid=container:"$pc" \
    --network container:"$pc" \
    alpine:3.20 sleep 30 >/dev/null
proc_uid=$(docker exec "$spc" awk '/^Uid:/{print $2; exit}' /proc/1/status 2>/dev/null || true)
docker rm -f "$spc" >/dev/null 2>&1 || true
docker rm -f "$pc" >/dev/null 2>&1 || true
docker run --rm -v "$probe_dir:/d" --user root alpine chown -R "$host_uid:$host_gid" /d >/dev/null 2>&1 || true
rm -rf "$probe_dir" || true
echo "    /proc/1 Uid -> $proc_uid"
[ "$proc_uid" = "65532" ] || {
    echo "FAIL: default posture process is uid $proc_uid, want 65532 (root should have dropped)" >&2
    docker run --rm -v "$default_dir:/d" --user root alpine chown -R "$host_uid:$host_gid" /d >/dev/null 2>&1 || true
    rm -rf "$default_dir" || true
    exit 1
}
docker run --rm -v "$default_dir:/d" --user root alpine chown -R "$host_uid:$host_gid" /d >/dev/null 2>&1 || true
rm -rf "$default_dir" || true

echo "PASS: all runtime read-only checks passed"
