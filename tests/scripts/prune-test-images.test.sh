#!/bin/bash
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export PRUNE_TEST_TMP="$tmp"
old=$(date -u -d '2 hours ago' '+%F %T +0000 UTC')
new=$(date -u -d '1 hour ago' '+%F %T +0000 UTC')
cat > "$tmp/images" <<IMAGES
tiller-router|dev-newest|111111111111|$new
tiller-router|dev-newest-alias|111111111111|$old
tiller-router|dev-keep-alias|222222222222|$old
external|keep|222222222222|$old
tiller-router|dev-busy|333333333333|$old
tiller-router|dev-race|444444444444|$old
tiller-router|dev-remove|555555555555|$old
tiller-router|dev-untag|666666666666|$old
IMAGES
cat > "$tmp/docker" <<'DOCKER'
#!/bin/bash
case "$1" in
    info) exit 0 ;;
    ps) echo container ;;
    inspect) echo sha256:333333333333aaaaaaaa ;;
    images) cat "$PRUNE_TEST_TMP/images" ;;
    image)
        printf '%s\n' "$*" >> "$PRUNE_TEST_TMP/calls"
        case "$*" in
            'image rm tiller-router:dev-remove') echo 'Deleted: sha256:555555555555' ;;
            'image rm tiller-router:dev-untag') echo 'Untagged: tiller-router:dev-untag' ;;
            *) exit 1 ;;
        esac
        ;;
    *) exit 1 ;;
esac
DOCKER
chmod +x "$tmp/docker"
output=$(PATH="$tmp:$PATH" KEEP_IMAGES=1 KEEP_MINUTES=15 PRUNE_KEEP=external:keep bash "$here/prune-test-images.sh")
diff -u <(printf '%s\n' 'image rm tiller-router:dev-race' 'image rm tiller-router:dev-remove' 'image rm tiller-router:dev-untag') <(sort "$tmp/calls")
[[ $output == *'removed tiller-router:dev-remove'* ]]
[[ $output == *'untagged tiller-router:dev-untag'* ]]
[[ $output == *'processed 2 image ref(s)'* ]]
echo 'Image pruning regression passed'
