#!/bin/sh
# TLS reverse-proxy smoke for trusted headers, Secure cookies, CSRF, and SSE.
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
network_name="tiller-proxy-smoke-$$"
router_name="tiller-proxy-router-$$"
proxy_name="tiller-proxy-nginx-$$"
password="proxy-smoke-password-$$"
state_dir=$(mktemp -d)
data_dir="$state_dir/data"
cert_dir="$state_dir/certs"
cookie_jar="$state_dir/cookies"
headers_file="$state_dir/headers"
body_file="$state_dir/body"
proxy_port=${TILLER_PROXY_SMOKE_PORT:-$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')}
base="https://127.0.0.1:$proxy_port"
host_uid=$(id -u)
host_gid=$(id -g)

cleanup() {
	docker rm -f "$proxy_name" "$router_name" >/dev/null 2>&1 || true
	docker network rm "$network_name" >/dev/null 2>&1 || true
	docker run --rm -v "$data_dir:/d" --user root alpine chown -R "$host_uid:$host_gid" /d >/dev/null 2>&1 || true
	rm -rf "$state_dir"
}
trap cleanup EXIT INT TERM

mkdir -p "$data_dir" "$cert_dir"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=localhost' \
	-keyout "$cert_dir/key.pem" -out "$cert_dir/cert.pem" >/dev/null 2>&1

docker network create "$network_name" >/dev/null
trusted_proxy=$(docker network inspect -f '{{(index .IPAM.Config 0).Subnet}}' "$network_name")
docker run --rm -v "$data_dir:/d" --user root alpine chown 65532:65532 /d

docker run --rm -d --name "$router_name" --network "$network_name" --network-alias router \
	-v "$data_dir:/data" \
	-e TILLER_ADMIN_USERNAME=admin \
	-e TILLER_ADMIN_PASSWORD="$password" \
	-e TILLER_DATA_DIR=/data \
	-e TILLER_LISTEN_ADDR=:8080 \
	-e TILLER_TRUST_PROXY_HEADERS=true \
	-e TILLER_TRUSTED_PROXY="$trusted_proxy" \
	-e TILLER_MODELS_DEV_ENABLED=false \
	tiller-router:dev >/dev/null

docker run --rm -d --name "$proxy_name" --network "$network_name" --network-alias proxy \
	-p "127.0.0.1:$proxy_port:8443" \
	-v "$repo_dir/tests/reverse-proxy-nginx.conf:/etc/nginx/nginx.conf:ro" \
	-v "$cert_dir:/certs:ro" \
	nginx:alpine >/dev/null

ready=0
i=0
while [ "$i" -lt 60 ]; do
	if curl -kfsS "$base/health/ready" >/dev/null 2>&1; then
		ready=1
		break
	fi
	i=$((i + 1))
	sleep 1
done
[ "$ready" -eq 1 ] || { docker logs "$router_name" >&2 || true; docker logs "$proxy_name" >&2 || true; echo 'FAIL: HTTPS proxy never became ready' >&2; exit 1; }

curl -kfsS -D "$headers_file" -o "$body_file" -c "$cookie_jar" \
	-H 'Content-Type: application/json' \
	-d "{\"username\":\"admin\",\"password\":\"$password\"}" \
	"$base/api/admin/session"
grep -i '^set-cookie:' "$headers_file" | grep -q '; Secure' || { echo 'FAIL: trusted HTTPS proxy cookie is not Secure' >&2; exit 1; }
csrf=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$body_file")
[ -n "$csrf" ] || { echo 'FAIL: login response has no CSRF token' >&2; exit 1; }

settings_status=$(curl -ksS -o /dev/null -w '%{http_code}' -b "$cookie_jar" \
	-H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
	-X PUT -d '{"default_retention_days":30}' "$base/api/admin/settings")
[ "$settings_status" = 204 ] || { echo "FAIL: proxied CSRF mutation returned $settings_status" >&2; exit 1; }

provider=$(curl -kfsS -b "$cookie_jar" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
	-d '{"name":"proxy-upstream","type":"generic-openai","base_url":"http://proxy:8081/v1"}' \
	"$base/api/admin/providers")
provider_id=$(printf '%s' "$provider" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
models=$(curl -kfsS -b "$cookie_jar" "$base/api/admin/providers/$provider_id/models")
model_id=$(printf '%s' "$models" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"][0]["id"])')

client=$(curl -kfsS -b "$cookie_jar" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
	-d '{"name":"proxy-smoke-client"}' "$base/api/admin/client-keys")
client_id=$(printf '%s' "$client" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
client_secret=$(printf '%s' "$client" | python3 -c 'import json,sys; print(json.load(sys.stdin)["secret"])')
permission_status=$(curl -ksS -o /dev/null -w '%{http_code}' -b "$cookie_jar" \
	-H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' -X PUT \
	-d "{\"defaults\":[],\"permissions\":[{\"kind\":\"real\",\"model_id\":\"$model_id\",\"enabled\":true}]}" \
	"$base/api/admin/client-keys/$client_id/permissions")
[ "$permission_status" = 204 ] || { echo "FAIL: permission update returned $permission_status" >&2; exit 1; }

stream=$(curl -kfsSN --max-time 10 -H "Authorization: Bearer $client_secret" -H 'Content-Type: application/json' \
	-d '{"model":"proxy-upstream/mock-model","stream":true,"messages":[{"role":"user","content":"hello"}]}' \
	"$base/v1/chat/completions")
printf '%s' "$stream" | grep -q 'data: ' || { echo 'FAIL: proxied SSE returned no data event' >&2; exit 1; }
printf '%s' "$stream" | grep -q '\[DONE\]' || { echo 'FAIL: proxied SSE returned no completion marker' >&2; exit 1; }

[ -z "$(docker port "$router_name")" ] || { echo 'FAIL: router unexpectedly published a direct host port' >&2; exit 1; }
echo 'PASS: TLS proxy, Secure cookie, CSRF, private router port, and client SSE'
