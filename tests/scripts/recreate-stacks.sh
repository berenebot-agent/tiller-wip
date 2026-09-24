#!/usr/bin/env bash
# One-shot recreate of both Compose stacks after pinning their project names.
#
# WHY THIS IS A SCRIPT AND NOT TWO COMMANDS IN A CHAT SESSION:
# Hermes (this agent) runs through Tiller Router as its LLM provider. Stopping
# the Tiller container kills the agent mid-command, so the "start it again" half
# would never run. Run this DETACHED so it survives the provider outage.
#
#   terminal(command="<this script>", background=true, notify_on_complete=true)
#
# WHY CONTAINERS MUST BE REMOVED FIRST:
# Both stacks previously ran under the Compose project name "repo" (derived from
# the directory basename). The compose files now pin `name: tiller-router` and
# `name: mailmoose`. `up --force-recreate` does NOT remove containers carrying
# the old project label — it tries to create NEW ones alongside, which then
# collide on container name and published port:
#   "Conflict. The container name /tiller-router is already in use"
#   "Bind for 0.0.0.0:25 failed: port is already allocated"
# So the stale containers are removed explicitly here.
#
# DATA SAFETY: both stacks use bind mounts only (verified: no named volumes).
# Removing the container does not touch ./data. The images are built before any
# container is removed, so nothing is destroyed on a build failure.
#
# Ordering: mailmoose is recreated FIRST (while Tiller still serves, so the
# agent is unaffected), then Tiller LAST, minimising the provider outage.

set -uo pipefail

TILLER_DIR=/home/ben/projects/tiller-router/repo
MAILMOOSE_DIR=/home/ben/projects/gatehouse-mail/repo
STALE_TILLER=tiller-router          # old project=repo container
STALE_MAILMOOSE=repo-mailmoose-1    # old project=repo container
STALE_PARTIAL=mailmoose             # half-created container from the failed run

log() { echo "[recreate-stacks] $(date -Is) $*"; }

# ---------------------------------------------------------------------------
# 1. Build both images FIRST, so any build failure aborts before anything is
#    removed and both stacks keep running untouched.
log "building mailmoose image"
(cd "$MAILMOOSE_DIR" && docker compose build) || { log "FATAL: mailmoose build failed — aborting, nothing removed"; exit 1; }

log "building tiller image"
(cd "$TILLER_DIR" && docker compose build) || { log "FATAL: tiller build failed — aborting, nothing removed"; exit 1; }
log "both images built"

# ---------------------------------------------------------------------------
# 2. Remove stale containers so names + ports free up. Bind mounts only, so
#    data is untouched.
for c in "$STALE_PARTIAL" "$STALE_MAILMOOSE" "$STALE_TILLER"; do
  if docker inspect "$c" >/dev/null 2>&1; then
    log "removing stale container $c"
    docker rm -f "$c" >/dev/null 2>&1 || log "  WARN: could not remove $c"
  else
    log "no stale container named $c"
  fi
done

# ---------------------------------------------------------------------------
# 3. mailmoose up first — Tiller is still serving during this phase.
log "starting mailmoose (project: mailmoose)"
(cd "$MAILMOOSE_DIR" && docker compose up -d) || log "ERROR: mailmoose up failed"

# ---------------------------------------------------------------------------
# 4. Tiller last. THIS is where the agent's provider goes offline.
log "starting tiller (project: tiller-router) — agent provider down from here"
(cd "$TILLER_DIR" && docker compose up -d) || log "ERROR: tiller up failed"

# ---------------------------------------------------------------------------
# 5. Wait for Tiller to serve again.
log "waiting for tiller to respond"
ok=0
for i in $(seq 1 90); do
  if curl -fsS --max-time 3 http://127.0.0.1:8080/api/runtime >/dev/null 2>&1; then
    ok=1; log "tiller responding after ${i}s"; break
  fi
  sleep 1
done
[ "$ok" = 1 ] || log "ERROR: tiller did not respond within 90s"

# ---------------------------------------------------------------------------
# 6. Report.
log "--- final state ---"
docker ps --format '{{.Names}}\t{{.Status}}\t{{.Ports}}' | grep -E '^(tiller-router|mailmoose)\b' || log "WARNING: expected containers missing"

log "project labels (must be distinct):"
for c in tiller-router mailmoose; do
  p=$(docker inspect "$c" --format '{{index .Config.Labels "com.docker.compose.project"}}' 2>/dev/null)
  log "  $c -> project=${p:-<missing>}"
done

log "orphan check — each dir should list ONLY its own container:"
log "  tiller dir sees:    $(cd "$TILLER_DIR" && docker compose ps -a --format '{{.Name}}' 2>/dev/null | tr '\n' ' ')"
log "  mailmoose dir sees: $(cd "$MAILMOOSE_DIR" && docker compose ps -a --format '{{.Name}}' 2>/dev/null | tr '\n' ' ')"

log "done"
