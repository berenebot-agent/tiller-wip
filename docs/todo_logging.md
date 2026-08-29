# TODO — Per-Client Token-Level Request Logging

**Status:** Planned (grilled & scoped, not yet implemented)
**Date:** 2026-08-29
**Feature:** Roadmap v2-core Phase A (§4) — Metadata-only request audit, surfaced as a per-client "Activity" view.

---

## 1. Scope & Guardrails

- This is **roadmap v2-core Phase A (metadata-only request audit)** — a post-V1 feature, explicitly authorized by a human.
- **Privacy boundary (non-negotiable):** log metadata only. Never persist, log, or expose prompt/response bodies, tool arguments, or reasoning content. Response bodies are read transiently in-memory solely to extract `usage` token counts, then discarded.
- **No new dependencies, no external services, no named volumes.** Everything lives in the existing SQLite DB under `./data`.
- **Synchronous logging** (per-request insert). At this project's scale (single-digit to low-tens of req/s) SQLite WAL comfortably handles it (~1% of write budget). No async buffer — it would add a mutex, goroutine, drain path, and overflow policy for no measurable benefit. Migration path to async later is trivial (row-building code and insert SQL are identical).

---

## 2. Decisions Locked (from grilling)

| # | Decision |
|---|----------|
| Q1 | Log a full metadata row per request (roadmap v2-core Phase A §4.2 field list). No bodies. |
| Q2 | Parse `usage` from response body in-memory, log numbers only, never persist body. Streaming is best-effort (parse final SSE chunk; `NULL` if absent). |
| Q3 | Time-based retention, configurable. |
| Q4 | Per-client `logging_enabled` (on/off) **and** per-client `retention_days`. |
| Q5 | Global default for new client keys. |
| Q6 | **Copy-at-creation, no live link.** Each client key has concrete non-null values copied from the system default at creation; never auto-updated afterward. No `COALESCE`, no inherit state, no effective-vs-configured UI. |
| Q7 | Settings stored in a **settings table**, runtime-editable via the Settings tab. |
| Q8 | Settings table is **plain key-value** + typed Go accessor layer. |
| Q9 | Log table: `ON DELETE CASCADE` on `client_key_id` (delete key = delete logs). `resolved_provider`/`resolved_model` stored as **names** (self-describing, survive provider deletion/rename). |
| Q10 | Activity view: read-only per-client dialog + search filter + pagination + "Clear logs" action. No export. Global view deferred to future Settings tab. |
| Q11 | Pruner: hourly, in the existing scheduler, also runs at startup. |
| Q12 | Log **every request that reaches `proxy` with a valid client + model, including failures** — `http_status` reflects outcome; `resolved_provider`/`resolved_model` NULL when resolution failed. |
| Q13 | **Synchronous** log write (best-effort: failed insert logs nothing, never fails the request). |
| Q14 | Activity shows provider error text (nullable `error_text` column) in addition to HTTP status. |
| Q15 | Request logs include a client-visible request ID (`client_request_id`, nullable) when one exists. |
| — | Rename the "System" tab to **"Settings"** (UI label change only; will hold more settings in future). |

---

## 3. Migrations

### 3.1 `internal/database/migrations/002_request_logs.sql`

```sql
CREATE TABLE request_logs (
    id                 TEXT PRIMARY KEY,
    client_key_id      TEXT NOT NULL REFERENCES client_keys(id) ON DELETE CASCADE,
    requested_model    TEXT NOT NULL,
    resolved_provider  TEXT,
    resolved_model     TEXT,
    protocol           TEXT NOT NULL,
    streaming          INTEGER NOT NULL CHECK (streaming IN (0,1)),
    http_status        INTEGER NOT NULL,
    latency_ms         INTEGER NOT NULL,
    input_tokens       INTEGER,
    output_tokens      INTEGER,
    provider_request_id TEXT,
    client_request_id  TEXT,
    error_text         TEXT,
    created_at         TEXT NOT NULL
) STRICT;
CREATE INDEX request_logs_client_created ON request_logs(client_key_id, created_at DESC);
CREATE INDEX request_logs_created ON request_logs(created_at);
```

- `resolved_provider`/`resolved_model` are **names**, not IDs.
- `ON DELETE CASCADE` — deleting a client key deletes its logs.
- `input_tokens`/`output_tokens`/`provider_request_id`/`client_request_id`/`error_text` nullable (unknown for streaming best-effort / early failures).

### 3.2 `internal/database/migrations/003_settings.sql`

```sql
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
```

Seeded with two keys:
- `default_logging_enabled` = `"1"`
- `default_retention_days` = `"30"`

### 3.3 `internal/database/migrations/004_client_log_config.sql`

```sql
ALTER TABLE client_keys ADD COLUMN logging_enabled INTEGER NOT NULL DEFAULT 1 CHECK (logging_enabled IN (0,1));
ALTER TABLE client_keys ADD COLUMN retention_days INTEGER NOT NULL DEFAULT 30;
```

Copy-at-creation, non-null, no inherit state.

---

## 4. Settings Accessor Layer

New `internal/database/settings.go`:
- `GetSetting(ctx, key) (string, error)`
- `SetSetting(ctx, key, value) error`
- `GetBool(ctx, key) (bool, error)`
- `GetInt(ctx, key) (int, error)`
- `GetLoggingDefaults(ctx) (enabled bool, retentionDays int, error)` — reads both, sane fallbacks if unset.

---

## 5. Client Key Creation — Copy Defaults

In `createClientKey` (`internal/server/admin_clients.go:46`):
- Read `default_logging_enabled` / `default_retention_days` from settings.
- Include `logging_enabled`, `retention_days` in the `INSERT INTO client_keys`.

---

## 6. Client Key View + Update

- `clientKeyView` (`admin_clients.go:12`): add `LoggingEnabled bool` and `RetentionDays int`; include in the `SELECT` in `listClientKeys`.
- `updateClientKey` (`admin_clients.go:96`): accept optional `logging_enabled` and `retention_days` in the input struct; update the columns.

---

## 7. Settings Admin Endpoints + UI

New `internal/server/admin_settings.go`:
- `GET /api/admin/settings` → `{ default_logging_enabled, default_retention_days }`
- `PUT /api/admin/settings` → updates both (validated: retention ≥ 1)

Register both under `requireAdmin` in `server.go`.

**Tab rename** in `internal/web/assets/app.js`:
- `VIEWS`: `'system'` → `'settings'`
- `navigate()`: `loadSystem` → `loadSettings`
- Nav label + panel id `#view-system` → `#view-settings`
- Keep health rendering; add the two editable settings fields to the Settings view.

---

## 8. Activity Admin Endpoint

New `internal/server/admin_activity.go`:
- `GET /api/admin/client-keys/{id}/activity?limit&offset&search` → paginated rows from `request_logs` for that client, ordered `created_at DESC`. Search filters on `requested_model` / `resolved_provider` / `http_status` / `error_text`.
- `DELETE /api/admin/client-keys/{id}/activity` → clear that client's logs.

Register both under `requireAdmin`.

---

## 9. Proxy Write Path (Synchronous)

In `proxy` (`internal/server/client.go:78`):
- Build a `logRow` struct: client id, requested model, resolved provider/model (names), protocol, streaming, http_status, latency_ms, input/output tokens, provider_request_id, client_request_id, error_text, created_at.
- **Log every request that reaches `proxy` with a valid client + model, including failures.** `http_status` reflects outcome; `resolved_provider`/`resolved_model` NULL when resolution failed.
- **Error text:** capture a short provider/upstream error message into `error_text` on failure (never a prompt/response body, never credentials). NULL on success.
- **Client-visible request ID:** capture the client-supplied request ID (e.g. OpenAI `request_id` / Anthropic `x-request-id`) into `client_request_id` when one exists; NULL otherwise.
- Write once before returning — refactor the many early returns to funnel through a single deferred/end write.
- **Best-effort:** a failed insert logs nothing and does not fail the request.
- **Token extraction:** parse `usage` from the response body in-memory, discard the body, log only the numbers. For streaming, best-effort parse of the final SSE chunk's `usage`; `NULL` if absent. Touches `rewriteSSE` / `streamReplace` (`client.go:216` / `client.go:272`) to capture usage before it is rewritten/streamed.

---

## 10. Pruner

- Function: `DELETE FROM request_logs WHERE created_at < ?` per distinct `retention_days` (join `client_keys`).
- Hook into the existing scheduler pattern (`providers.Manager.StartScheduler`, `manager.go:126`): add a parallel hourly ticker in `Server.StartBackground` (`server.go:56`), also run once at startup.

---

## 11. UI — Activity Dialog

- In `renderClients` (`app.js:132`): add an **Activity** button per client row.
- New dialog (mirroring the permissions dialog): read-only table of that client's log rows (timestamp, requested model, resolved provider/model, protocol, streaming, status, latency, tokens, provider request id, client request id, error text), with a search filter and pagination, plus a **Clear logs** action (confirm dialog).
- Client edit dialog: add `logging_enabled` toggle + `retention_days` number field.

---

## 12. Tests

- Unit: settings accessors; client-key creation copies defaults; activity endpoint pagination/search; pruner deletes by retention.
- Re-verify **§28.4** (virtual routing hides real target) and **§28.5** (immediate remap) — the proxy refactor for logging must not change routing behavior.
- Assert no prompt/response body ever lands in `request_logs` (only metadata columns populated).
- Assert `error_text` is populated on failure and NULL on success; assert `client_request_id` is captured when the client supplies one.

---

## 13. Open Items (need sign-off before coding)

- **`synchronous=NORMAL` tuning** for the DSN (optional, ~10x faster WAL writes). Flagged; not changing without explicit approval.
- **Settings tab rename** touches the nav/view id — a UI label change, not a model/provider rename, so within bounds. Confirmed desired.
