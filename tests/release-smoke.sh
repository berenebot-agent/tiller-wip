#!/bin/sh
# Clean-install/restart smoke test for the release container.
#
# This intentionally uses a temporary bind mount and a loopback-only port. It
# does not touch the repository's data directory or require provider
# credentials, so it is safe to run from a clean checkout or in CI.
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
router_name="tiller-release-smoke-$$"
image="tiller-router:release-smoke-$$"
password="release-smoke-password-$$"
data_dir=$(mktemp -d)
cookie_jar=$(mktemp)
router_port=${TILLER_RELEASE_SMOKE_PORT:-$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')}
base="http://127.0.0.1:$router_port"
host_uid=$(id -u)
host_gid=$(id -g)

remove_router() {
	docker rm -f "$router_name" >/dev/null 2>&1 || true
	while docker ps -a --format '{{.Names}}' | grep -qx "$router_name"; do
		sleep 1
	done
}

cleanup() {
	remove_router
	# The runtime user tightens /data to 0700. Restore ownership before the
	# shell removes the temporary bind-mount source. Teardown must not fail
	# the run: if this chown-back helper fails the dir stays owned by 65532
	# and rm below would EPERM under set -e, turning a green run red.
	docker run --rm -v "$data_dir:/d" --user root alpine chown -R "$host_uid:$host_gid" /d >/dev/null 2>&1 || true
	rm -rf "$data_dir" "$cookie_jar" || true
	docker image rm "$image" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

remove_router

echo "==> Building release image"
docker build --pull=false -t "$image" "$repo_dir"

# The image runs as UID/GID 65532 and database.Open chmods the bind mount.
docker run --rm -v "$data_dir:/d" --user root alpine chown 65532:65532 /d

start_router() {
	docker run --rm -d --name "$router_name" --network host \
		-v "$data_dir:/data" \
		-e TILLER_LISTEN_ADDR="127.0.0.1:$router_port" \
		-e TILLER_DATA_DIR=/data \
		-e TILLER_ADMIN_USERNAME=admin \
		-e TILLER_ADMIN_PASSWORD="$password" \
		-e TILLER_TRUST_PROXY_HEADERS=false \
		-e TILLER_MODELS_DEV_ENABLED=false \
		"$image" >/dev/null

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
		docker logs "$router_name" >&2 || true
		echo "FAIL: container never became ready" >&2
		exit 1
	fi
}

echo "==> Starting clean container"
start_router
docker run --rm -v "$data_dir:/d:ro" alpine test -f /d/tiller-router.db || {
	echo "FAIL: migrations did not create the database" >&2
	exit 1
}
db_mode=$(docker run --rm -v "$data_dir:/d:ro" alpine stat -c '%a' /d/tiller-router.db)
[ "$db_mode" = 600 ] || { echo "FAIL: database mode is $db_mode, want 600" >&2; exit 1; }

echo "==> Checking unauthenticated admin rejection"
unauthenticated=$(curl -s -o /dev/null -w '%{http_code}' "$base/api/admin/providers")
[ "$unauthenticated" = 401 ] || { echo "FAIL: unauthenticated admin request returned $unauthenticated" >&2; exit 1; }

echo "==> Logging in and checking an authenticated admin endpoint"
login=$(curl -fsS -c "$cookie_jar" -H 'Content-Type: application/json' \
	-d "{\"username\":\"admin\",\"password\":\"$password\"}" \
	"$base/api/admin/session")
csrf=$(printf '%s' "$login" | sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p')
[ -n "$csrf" ] || { echo "FAIL: login response did not contain a CSRF token" >&2; exit 1; }
curl -fsS -b "$cookie_jar" "$base/api/admin/settings" | grep -q 'notifications_enabled' || {
	echo "FAIL: authenticated settings request failed" >&2
	exit 1
}

logs=$(docker logs "$router_name" 2>&1 || true)
case "$logs" in
	*"$password"*) echo "FAIL: admin password appeared in container logs" >&2; exit 1 ;;
esac

echo "==> Restarting with the same persistent data"
docker stop "$router_name" >/dev/null
while docker ps -a --format '{{.Names}}' | grep -qx "$router_name"; do
	sleep 1
done
start_router
curl -fsS -b "$cookie_jar" "$base/api/admin/session" | grep -q '"authenticated":true' || {
	echo "FAIL: admin session did not survive restart" >&2
	exit 1
}
curl -fsS "$base/health/live" >/dev/null

logs=$(docker logs "$router_name" 2>&1 || true)
case "$logs" in
	*"$password"*) echo "FAIL: admin password appeared in restart logs" >&2; exit 1 ;;
esac

echo "PASS: clean install, migrations, authenticated session, restart persistence, and log hygiene"
