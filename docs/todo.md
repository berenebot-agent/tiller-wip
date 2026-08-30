# TODO — Roadmap-Aligned Low-Hanging Work

**Status:** Items 1–6 complete and verified; Item 7 final regression passed  
**Prepared:** 2026-08-30  
**Source:** Review of the current repository against `tiller-router-roadmap-v2-core.md` and `tiller-router-roadmap-saas-multiuser.md`  
**Rule:** Complete and verify each numbered item before starting the next one.

## Completion status

Items 1–6 are implemented and verified. Item 7 (final regression and handoff)
passed on 2026-08-30:

- `go test ./...` — PASS
- `go vet ./...` — PASS
- `./tests/compatibility/run.sh` — PASS (official SDK, Codex CLI, OpenCode,
  Claude Code, and Hermes chat/responses/messages probes)
- Playwright browser suite (`tiller-router-browser-tests:dev`) — 4/4 PASS
- V1 acceptance test `TestV1VirtualRoutingRemapIsolationRotationAndBackup`
  (covers §28.3 catalogue isolation, §28.4 hidden real target, §28.5 immediate
  remap, §28.6 streaming, §28.7 broken mappings, §28.8 provider outage,
  §28.9 backup/restore) — PASS
- No test or implementation logging of prompt/response bodies, tool
  arguments/results, reasoning, credentials, authorization headers, or
  plaintext client keys — CONFIRMED
- `git diff --check` — PASS

Note: Item 7 verification found and fixed a regression in the Item 4 test
harness `tests/compatibility/mock_upstream.py`: the `handle_responses` method
definition had been accidentally replaced by `handle_model_control`, orphaning
the Responses handler body and breaking the compatibility Responses probe
(502). Restored the `def handle_responses(self, request):` method; the
compatibility suite then passed. No production code was changed.

This checklist covers the small correctness fixes and contained roadmap increments identified in the review. It does not authorize unrelated roadmap work. The frozen V1 specification and `AGENTS.md` remain controlling.

## 0. Mandatory preflight for every item

Before editing:

1. Read `AGENTS.md`, the relevant roadmap section, and the files named by the item.
2. Confirm `git status --short` and preserve all pre-existing user changes.
3. Do not rename provider names, real model IDs, virtual group names, or virtual model names.
4. Do not add dependencies, services, ports, named volumes, fallback, retry, or health-based routing.
5. Never log or persist prompts, responses, tool arguments/results, reasoning, credentials, authorization headers, or plaintext client keys.
6. Run all Go commands in `golang:1.26.7-alpine`; never install or invoke Go on the host.

Standard verification after every Go change:

```sh
docker run --rm -v "$PWD:/src" -w /src golang:1.26.7-alpine go test ./...
docker run --rm -v "$PWD:/src" -w /src golang:1.26.7-alpine go vet ./...
```

If the image's login shell resets `PATH`, invoke `go` directly as shown above or use `/usr/local/go/bin/go`.

---

## 1. Correct virtual-model usage classification

**Roadmap:** Phase H, requests by virtual model  
**Size:** Small  
**Reason for first position:** The current usage API labels direct real-model requests as virtual-model usage.

### Required behavior

1. `virtual_models` in `GET /api/admin/usage` must contain only requests whose `requested_model` matches a currently defined virtual model canonical ID.
2. Direct real-model requests must continue to contribute to:
   - the relevant `client_keys` bucket; and
   - the relevant resolved `real_models` bucket.
3. A virtual-model request must contribute to all three applicable views:
   - client key;
   - requested virtual model; and
   - resolved real provider/model.
4. Keep the existing 1-hour, 24-hour, and 7-day windows and name-based historical aggregation.
5. Do not add a migration for this correction. Filter/join against the current virtual group/model catalogue in the aggregation query.

### Implementation notes

- Update `usageByVirtual` in `internal/server/admin_usage.go`.
- Join `request_logs.requested_model` to `virtual_provider_groups.name || '/' || virtual_models.name` and group by that canonical ID.
- Retain the existing lower-bound `created_at >= seven-day-cutoff` restriction.
- Do not change `usageByClient` or `usageByReal` except for a necessary shared refactor.

### Tests

Update `internal/server/usage_test.go` so it inserts distinct direct-real and virtual requests and asserts:

1. the direct real ID is absent from `virtual_models`;
2. the virtual ID has only the virtual request's totals;
3. the resolved real target totals include both direct and virtual traffic;
4. client totals include both request types;
5. the empty response remains three non-null empty maps.

### Done when

- The endpoint no longer misclassifies direct traffic.
- Focused usage tests, `go test ./...`, and `go vet ./...` pass.

---

## 2. Make request-log primary keys unambiguously request-unique

**Roadmap:** Phase A correctness hardening  
**Size:** Small  
**Reason:** `writeLog` currently uses `database.Now()` as the `request_logs.id` primary key, so a timestamp collision can silently discard a best-effort log row.

### Required behavior

1. Use the already generated router request ID (`client_request_id`) as `request_logs.id`.
2. Preserve `client_request_id` as its existing API field and `X-Tiller-Request-Id` response header.
3. Keep logging synchronous and best-effort: logging failure must never fail inference.
4. Do not expose a new public identifier or change the request-log schema.

### Implementation notes

- Change the insert in `internal/server/logging.go` to pass `row.clientRequestID` for `id`.
- Do not generate a second ID inside `writeLog`.
- Keep `created_at` sourced from the request-start metadata as it is now.

### Tests

Extend `internal/server/logging_test.go` to assert:

1. each stored row has `id = client_request_id`;
2. the stored ID matches the `X-Tiller-Request-Id` returned for that request;
3. multiple requests produce distinct stored IDs and no rows are lost;
4. logging-disabled clients still produce no rows;
5. a logging insert failure remains invisible to the inference response, if this can be induced without weakening production code.

### Done when

- Timestamp values are no longer used as request-log primary keys.
- Logging/privacy tests, V1 acceptance tests §28.4 and §28.5, `go test ./...`, and `go vet ./...` pass.

---

## 3. Preserve unsaved permission edits across filtering

**Roadmap:** Permission UX prerequisite  
**Size:** Small  
**Reason:** Re-rendering the permission dialog from the original API payload can discard unsaved checkbox changes when the search filter changes.

### Required behavior

1. Toggling a model permission updates the in-memory permission state immediately.
2. Toggling a group's `New models default` updates only that group's in-memory feeder state.
3. Typing, clearing, or changing the filter must not reset either kind of unsaved change.
4. Cancel/close must discard unsaved changes naturally; reopening reloads from the API.
5. Save must persist exactly the current in-memory state.
6. Preserve the feeder invariant: changing `New models default` never changes existing model checkboxes.

### Implementation notes

- Update permission rendering/state handling in `internal/web/assets/app.js`.
- Treat `state.permissionData` as the single source of truth while the dialog is open.
- Attach model and feeder checkbox change handlers that update the matching state object before any re-render.
- The search term controls visibility only; it must never mutate permissions.

### Tests

Extend `tests/browser/admin.spec.js` with a flow that:

1. opens a client's permissions;
2. changes a model and feeder toggle;
3. filters so the changed model disappears;
4. clears the filter and verifies both unsaved changes remain;
5. cancels and reopens to verify cancellation discarded them;
6. repeats, saves, reopens, and verifies persistence.

### Done when

- Filtering cannot lose unsaved edits.
- Browser tests and the standard Go verification pass.

---

## 4. Add permission bulk enable/disable for current models

**Roadmap:** §16 Permission UX Improvements  
**Dependency:** Item 3  
**Size:** Small

### Locked semantics

1. Add two dialog actions: `Enable current` and `Disable current`.
2. With an empty search, an action applies to all **available** real and virtual models in the dialog.
3. With a non-empty search, an action applies only to **available models matching the current filter**.
4. Retired/unavailable models are never changed by a bulk action.
5. Bulk actions update only the unsaved in-memory model checkboxes; the user must still press `Save permissions` to persist.
6. Bulk actions never alter any `New models default` feeder.
7. `Cancel` discards bulk changes.
8. Do not add a new API endpoint or database migration; use the existing complete permissions payload and save endpoint.

### Implementation notes

- Add the two controls beside the permission search field in `internal/web/assets/index.html`.
- Implement the bulk state update and re-render in `internal/web/assets/app.js`.
- Keep button labels explicit about “current” models so they cannot be confused with the future-model feeder.
- Preserve keyboard accessibility and visible focus behavior.

### Tests

Extend the browser suite to cover:

1. enable all available models with no filter;
2. disable only matching available models with a filter;
3. retired models remain unchanged;
4. feeder toggles remain unchanged;
5. cancel discards bulk changes;
6. save persists bulk changes;
7. newly discovered models still follow the feeder rather than any previous bulk action.

Re-run the relevant V1 catalogue-isolation and feeder acceptance tests.

### Done when

- Administrators can safely bulk-toggle current permissions without blurring the feeder boundary.
- Browser, routing/permission, `go test ./...`, and `go vet ./...` checks pass.

---

## 5. Add the workspace-free Global Activity view

**Roadmap:** Phase H and §15.1 Global Activity  
**Dependency:** Items 1 and 2  
**Size:** Small-to-medium

### API

Add an admin-authenticated endpoint:

```text
GET /api/admin/activity?limit=50&offset=0&search=
```

Response:

```json
{
  "data": [
    {
      "id": "...",
      "client_key_id": "...",
      "client_name": "...",
      "requested_model": "...",
      "resolved_provider": "...",
      "resolved_model": "...",
      "protocol": "chat",
      "streaming": false,
      "http_status": 200,
      "latency_ms": 123,
      "input_tokens": 10,
      "output_tokens": 4,
      "provider_request_id": null,
      "client_request_id": "...",
      "error_text": null,
      "created_at": "..."
    }
  ],
  "limit": 50,
  "offset": 0
}
```

### Locked behavior

1. Sort newest first by `created_at`, with `id` as a deterministic secondary sort key.
2. Reuse the existing pagination limits: default 50, maximum 200, non-negative offset.
3. Search case-insensitively using SQLite's existing `LIKE` behavior across:
   - client name;
   - requested model;
   - resolved provider;
   - resolved model;
   - HTTP status text;
   - client request ID;
   - provider request ID; and
   - error text.
4. Keep the endpoint read-only. Do not add global clear or export.
5. Return metadata only; never add body-related fields.
6. Register the endpoint under `requireAdmin`.

### UI

1. Add a `Global activity` section to the existing Settings view, matching the earlier Phase A decision to place it there.
2. Display:
   - time;
   - client;
   - requested model;
   - resolved target;
   - protocol/streaming;
   - status;
   - latency;
   - input/output tokens; and
   - router request ID.
3. Provide search plus `Newer`/`Older` pagination controls.
4. Show a clear empty state and an inline load error.
5. Do not add charts, cost calculations, CSV export, or body inspection.
6. Keep the existing per-client Activity dialog and per-client clear action unchanged.

### Implementation notes

- Reuse or carefully extract the row-scanning logic in `internal/server/admin_activity.go` rather than duplicating incompatible field definitions.
- Join `request_logs.client_key_id` to `client_keys.id` to obtain the client name.
- Because logs cascade when a client is deleted, no deleted-client fallback label is required.
- Add the route in `internal/server/server.go` and UI markup/behavior in the existing embedded assets.

### Tests

Add server tests for:

1. admin authentication is required;
2. rows from multiple clients are returned newest first;
3. client identity is included;
4. every documented search field works;
5. limit/offset pagination works;
6. no prompt, response, tool, reasoning, credential, authorization, or plaintext key material is present.

Add a browser test that creates activity for two clients, searches, pages, and verifies the Settings view rendering.

### Done when

- Administrators can inspect recent metadata activity across all client keys from Settings.
- Existing per-client activity behavior remains unchanged.
- Server/browser tests, §28.4, §28.5, `go test ./...`, and `go vet ./...` pass.

---

## 6. Make the container root filesystem read-only

**Roadmap:** Phase G / §19 Deployment Hardening  
**Dependency:** None, but perform after application changes to isolate deployment troubleshooting  
**Size:** Small

### Required Compose changes

1. Add `read_only: true` to the single `tiller-router` service.
2. Add a writable `/tmp` tmpfs with restrictive options suitable for SQLite/runtime temporary files:

   ```yaml
   tmpfs:
     - /tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777
   ```

3. Drop all Linux capabilities:

   ```yaml
   cap_drop:
     - ALL
   ```

4. Retain `security_opt: no-new-privileges:true`.
5. Retain the `./data:/data` bind mount. Do not introduce a named volume.
6. Retain the approved `0.0.0.0:${TILLER_TEST_PORT:-8080}:8080` test/LAN port exactly as-is.
7. Do not add privileged mode, Docker socket access, another service, or another port.

### Verification

1. Run `docker compose config` with temporary non-secret test credentials and confirm the rendered configuration.
2. Build the production image.
3. Start it with:
   - a temporary host directory bind-mounted to `/data`;
   - `--read-only`;
   - the `/tmp` tmpfs; and
   - all capabilities dropped.
4. Verify:
   - migrations and startup succeed;
   - `/health/live` and `/health/ready` return 200;
   - configuration persists under the temporary `/data` directory across restart;
   - catalogue refresh can write its state;
   - authenticated SQLite backup export still works; and
   - the container cannot create a file under `/` outside `/data` or `/tmp`.
5. Remove only the temporary test container/image/data created for this verification; never touch the repository's real `./data` directory.
6. Run the standard Go test/vet commands and containerized compatibility tests if deployment behavior changed beyond Compose declarations.

### Documentation

- Update the README deployment/security description to say the root filesystem is read-only and `/tmp` is ephemeral.
- Keep the canonical `/opt/tiller-router`, one-service, bind-mount-only deployment contract unchanged.

### Done when

- The normal one-service Compose deployment starts and operates with a read-only root filesystem.
- Persistent writes occur only beneath `/data`; temporary writes occur only beneath `/tmp`.

---

## 7. Final regression and handoff

After Items 1–6 are complete:

1. Run `go test ./...` and `go vet ./...` in the mandated Go container.
2. Run `./tests/compatibility/run.sh`.
3. Build and run the Playwright browser test image according to the README.
4. Re-verify V1 acceptance tests:
   - §28.3 client catalogue isolation;
   - §28.4 hidden real target through virtual routing;
   - §28.5 immediate remap without restart;
   - §28.6 streaming;
   - §28.7 broken mappings;
   - §28.8 provider outage; and
   - §28.9 backup/restore.
5. Confirm no test or implementation logs prompt/response bodies, tool arguments/results, reasoning, credentials, authorization headers, or plaintext client keys.
6. Confirm `git diff --check` passes.
7. Update this file's status with completed item numbers and verification results; do not mark an item complete if any required check is skipped or failing.

---

## Explicitly not in this implementation sequence

The following roadmap areas are not low-hanging or are not decision-complete. A junior agent must stop rather than expanding an item into them:

1. **Provider health state:** Current code preserves catalogue data on refresh failure and shows refresh errors, but passive/active health semantics, classification thresholds, polling, recovery, and `Test Provider` behavior still require human decisions.
2. **Catalogue lifecycle history:** Current code preserves retired models and restores reappearing models, but a durable appeared/disappeared/reappeared event history needs retention and purge decisions before implementation.
3. **Fallbacks and retries:** Explicitly excluded until their roadmap decision gates are resolved. Never introduce silent rerouting or retry.
4. **Capability metadata and compatibility warnings:** Requires source-of-truth and unknown-capability decisions.
5. **Aliases:** Client-facing naming and collision semantics remain unresolved and renames are breaking.
6. **Credential encryption, scheduled backups, and graceful overlapping key rotation:** Security-sensitive and separately scoped Phase G work.
7. **Additional provider/protocol work:** Add only for a demonstrated compatibility requirement; generic OpenAI-compatible support remains preferred.
8. **SaaS/multi-user roadmap:** Entirely deferred. Do not add users, workspaces, tenancy, signup, RBAC, billing, quotas, hosted infrastructure, PostgreSQL, Redis, or Kubernetes.

The existing Phase B subset must remain intact throughout this work: a failed discovery keeps the last-known catalogue, retired models and permissions are preserved, broken virtual targets remain visible, and no health condition changes routing.
