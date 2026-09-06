#!/bin/bash
set -euo pipefail

minutes="${1:-10}"
mode="${2:-all}"

case "$mode" in
	all|errors) ;;
	*)
		echo "usage: $(basename "$0") [minutes] [all|errors]" >&2
		echo "  minutes: window in minutes (default 10)" >&2
		echo "  mode:    all (default) or errors" >&2
		exit 2
		;;
esac

case "${minutes}" in
	''|*[!0-9]*)
		echo "error: minutes must be a positive integer, got: ${minutes}" >&2
		exit 2
		;;
esac

repo_root="$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

container="$(
	docker compose -f "$repo_root/docker-compose.yml" ps --services --filter 'status=running' 2>/dev/null \
		| head -n1
)"

if [ -z "$container" ]; then
	echo "error: no running container from $repo_root/docker-compose.yml" >&2
	exit 1
fi

label="last ${minutes} min (${mode})"
echo "--- ${container}: ${label} ---"

if [ "$mode" = "errors" ]; then
	if ! command -v jq >/dev/null 2>&1; then
		echo "warning: jq not installed — falling back to raw JSON" >&2
		docker logs --since "${minutes}m" "$container"
	else
		docker logs --since "${minutes}m" "$container" 2>&1 \
			| jq -r 'select(.level == "ERROR" or .level == "WARN") | "\(.time) [\(.level)] \(.msg)"'
	fi
else
	docker logs --since "${minutes}m" "$container"
fi
