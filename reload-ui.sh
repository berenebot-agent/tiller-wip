#!/usr/bin/env bash
# Rebuild and restart the tiller-router container so the embedded admin UI is
# refreshed. The web assets (app.js, style.css, index.html) are compiled into
# the server binary at build time (//go:embed assets/*), so there is no live
# reload: any change under internal/web/assets requires a rebuild + restart.
set -euo pipefail
cd "$(dirname "$0")"
docker compose up -d --build tiller-router
