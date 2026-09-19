# Phase A — Memory Observability (admin-gated MemStats + pprof)

**Goal:** Replace the estimates in `docs/roadmap_memory.md` with real, attributable
memory telemetry so subsequent tuning phases are driven by data. Measure first;
no KDF/DB/transport/limit changes in this phase.

**Scope:** `TILLER_DEBUG_PPROF` (default false, env-required), an admin-gated
`GET /api/admin/debug/memory`, and an admin-gated `GET /api/admin/debug/pprof/*`.
No new dependencies (`net/http/pprof` and `runtime` are stdlib).

**Out of scope (deferred pending measurement):** `GOMEMLIMIT`/`GOGC`, SQLite
`cache_size`/idle-conn tuning, transport `MaxIdleConns`, proxy byte caps, argon2
singleflight/semaphore changes, models.dev pruning, cgo sqlite swap.

---

## Tasks

### Task 1 — `TILLER_DEBUG_PPROF` config flag
**File:** `internal/config/config.go`
- Add `DebugPprof bool` to `Config`.
- Parse `TILLER_DEBUG_PPROF` with `strconv.ParseBool`, mirroring
  `TILLER_MODELS_DEV_ENABLED`. Default `false`; any value other than an explicit
  `true` leaves telemetry off.
- Committed default stays `false` (env-required enable).

### Task 2 — `internal/server/debug.go` (new)
- Blank import `_ "net/http/pprof"` (registers handlers on `http.DefaultServeMux`;
  that mux is never served directly — only through the gated wrapper below).
- `debugMemory`: `runtime.ReadMemStats` summary + `runtime.NumGoroutine()` +
  configured `GOMEMLIMIT`/`GOGC` env strings. Note: there is no non-mutating getter
  for the live soft limit (`debug.SetMemoryLimit(-1)` *sets* it), so report env.
- `debugPprof`: clone the request, rewrite
  `/api/admin/debug/pprof/<x>` → `/debug/pprof/<x>`, delegate to
  `http.DefaultServeMux.ServeHTTP`.

### Task 3 — Routes
**File:** `internal/server/server.go` (`Handler()`)
- `GET /api/admin/debug/memory` → `s.requireAdmin`.
- `GET /api/admin/debug/pprof/` → `s.requireAdmin`, registered **only when**
  `s.config.DebugPprof` (disabled ⇒ 404). GET is exempt from the CSRF check, as
  intended.

### Task 4 — Deployment plumbing
- `docker-compose.yml`: `TILLER_DEBUG_PPROF: ${TILLER_DEBUG_PPROF:-false}`.
- `.env.example`: commented `# TILLER_DEBUG_PPROF=false`.

### Task 5 — Verification (authorised: unit + vet)
- `./tests/scripts/check-fmt.sh`
- `./tiller-go.sh vet ./...`
- `./tiller-go.sh test ./...`
- Manual: pprof 404 when flag off; 401 without session; 200 with session.

### Task 6 — Rebuild + measurement (authorised)
- `docker compose down && docker compose up --build`.
- Baseline (idle) vs load (5 concurrent non-stream + 5 concurrent stream against
  `tiller/free`), 5 distinct test keys to force concurrent argon2.
- Heap profiles idle + under load → `./tiller-go.sh tool pprof -top`.
- Cleanup test keys; report before/after + ranked drivers.

### Task 7 — Commit
Commit in place on `feat/activity-graph-codex-fallback-usage` once Phase A works and
telemetry is verified. No push, no PR.

---

## Risks
- pprof blank import registers on `DefaultServeMux` process-wide; acceptable because
  that mux is only reachable via the `requireAdmin`-gated wrapper.
- Free-tier upstream may return `free_tier_rejected`; measure the paths that work and
  report, never substitute silently.
- No memory reduction expected this phase — the deliverable is attribution.
