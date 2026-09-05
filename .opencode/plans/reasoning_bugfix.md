# Reasoning/Thinking Feature — Bugfix & Test Plan

> **Scope:** Post-PR audit of `beta.1-reasoning` (8 commits, `c70d53c..HEAD`, +2517/-56 lines).
> **Goal:** Fix confirmed bugs, fill critical/high test gaps, document deferred UI items.
> **Status:** 4 bugs fixed & committed. 9 active items remain, 4 deferred.

---

## Completed (committed)

| Item | What was wrong | What we did |
|---|---|---|
| **B1** | `extractMessagesReasoning` marked a selector as "present" even when `thinking.type` was an empty string — silent no-op, inconsistent with the Chat path. | Added `&& t != ""` guard to `translate.go:156`, matching `extractChatReasoning`. |
| **B2** | `stripMessagesOutputConfigEffort` was defined but never called — fully duplicated by `stripReasoningSelector`. | Deleted the dead function (`translate.go:408–422`). |
| **B3** | `mergeReasoningCapabilities` had an unreachable branch that reassigned `""` to `""` — copy-paste artifact. | Deleted the dead branch (`virtual_capabilities.go:87–89`). |
| **B4** | Anthropic catalogue code silently dropped `budget_tokens` — nobody knew if it was a bug or a feature. | Added a code comment documenting the intentional drop (`client.go:126–129`). |

Verification: `vet` and `test ./internal/server/ ./internal/providers/` both pass.

---

## All Remaining Items — ELI5 Summary

| Item | Severity | ELI5 Problem | ELI5 Solution |
|---|---|---|---|
| **T7** | High | If a client says "disable reasoning but also use high effort," BOTH are silently dropped. The upstream provider uses its default (often reasoning ENABLED), contradicting the client's explicit `enabled=false`. | Fix the logic so `enabled=false` is honored even when effort is also set. Add a test documenting the corrected behavior. |
| **T1** | Critical | The `reasoning_selector_omitted` warning goes through proxy → DB → CSV/JSON export, but no test checks if it actually lands. A bug in any step silently breaks the only admin-visible signal. | Seed a DB row with a warning, then verify it appears in both CSV and JSON export. Now we trust the pipeline. |
| **T2** | High | Virtual models aggregate reasoning from multiple targets. No test checks this — a merge bug could expose reasoning a target doesn't support (or hide reasoning all targets support). | Test with valid JSON, NULL, and invalid JSON targets; verify the merge produces the correct combined superset. |
| **T3** | High | The code that writes `budget_tokens` into the upstream request body is never exercised. A bug here means budget silently never reaches the provider. | Test that a budget value appears in the output body when the target supports it, and triggers a warning when it doesn't. |
| **T4** | High | The function combining two models' reasoning capabilities has ~12 code paths (nil handling, effort union, budget range, modes, etc.). Only ONE is tested. | Add tests for every branch: nil inputs, effort unions, toggle, budget ranges, parameter dedup, thinking-mode ordering. |
| **T5** | High | `ExtractReasoningOptions` is the bridge between stored metadata and the mapping decision. If it's wrong, every downstream mapping is wrong. Zero direct tests. | Test each option type (effort, toggle, budget, adaptive) converts correctly, including edge cases like unrestricted effort. |
| **T6** | Medium | When a client sends no reasoning at all, the body should pass through untouched. No test confirms this — a regression could inject reasoning where none was requested. | Send a body with no reasoning fields; verify it's unchanged and no warning fires. |
| **T8** | Medium | If `reasoning_capabilities` contains bad JSON (migration gone wrong, manual edit), `decodeReasoningCapabilities` should return nil gracefully. No test verifies — could crash the admin UI. | Insert malformed JSON; verify the function returns nil without panicking. |
| **T9** | Medium | The JSON marshaler for stored capabilities has no round-trip test. A serialization bug could corrupt data on disk silently. | Test marshal → unmarshal produces equal output; test nil → NULL → nil. Now we trust the storage layer. |
| **F2** | Medium | The UI renders `<code class="warning-code">reasoning_selector_omitted</code>` but no CSS styles it. It looks like plain monospace text, and empty rows collapse the column width. | Add CSS (badge, color, padding) so warning codes stand out from rows with no warning. |
| **F3** | Medium | Operators see `reasoning_selector_omitted` as raw text with no idea what it means or whether it matters. | Add a `title=` tooltip or click-to-detail dialog explaining "the requested reasoning level was dropped because the target doesn't support it." |
| **F4** | Low | The backend sends the full reasoning shape (effort list, budget range, modes, etc.) but the admin UI reduces it to a single `R ✓/✗/—` pill. All the useful info is discarded. | Expand the Capabilities dialog to show the actual options (effort values, toggle, budget, modes) instead of one bit. |
| **F5** | Low | If you open a Capabilities dialog, hit "Refresh," see an error, close it, then reopen for a *different* model, the old error text is still there until the next refresh. | Clear `#capabilities-refresh-error` at the top of `openCapabilities`/`openRealModelCapabilities` before populating. |

**Summary counts:** 9 active (1 bug fix + 8 tests), 4 deferred (frontend polish).

**Suggested priority:** T7 → T1 → T2 → T3 → T4 → T5 → T6–T9. Defer F2–F5 until product decision on admin reasoning surface.

---

## Active Items — Detailed

### T7 — `enabled=false + effort=high` silently drops both (BUG)

**File:** `internal/server/translate.go:292–319` (Chat), `:333–355` (Responses), `:367–397` (Messages)
**Severity:** High

**The bug:** When a client sends `{"reasoning":{"enabled":false,"effort":"high"}}`, the selector is extracted as `{Present:true, Enabled:&false, Effort:"high"}`. In `applyChatReasoning`:
- Line 292: `(mode=="disabled") && selector.Effort==""` → **FALSE** (effort is "high") → skip mode block
- Line 313: `selector.Effort!="" && mode!="disabled"` → **FALSE** (mode is "disabled") → skip effort block
- **Result: body unchanged, no warning** — upstream uses its default (often reasoning ENABLED), contradicting the client's explicit `enabled=false`.

Same bug exists in `applyResponsesReasoning` and `applyMessagesReasoning` (when the provider supports effort but not the `disabled` type).

**Approach:** Restructure the logic so the mode (enabled/disabled) and effort are handled independently:
1. First apply the mode (enabled/disabled) if specified — this controls whether reasoning is on.
2. Then apply the effort if specified AND mode is not "disabled" — this controls the level when reasoning is on.
3. If mode is "disabled" AND effort is set, emit a warning (the combination is contradictory — the client should pick one).

**Tests:** Add cases for:
- `enabled=false, effort=high` → mode applied (disabled), effort dropped, warning emitted
- `enabled=true, effort=high` → both applied
- `enabled=false` (no effort) → mode applied, no warning
- `effort=high` (no enabled) → effort applied, no warning

---

### T1 — `warning_code` end-to-end test

**Files:** `internal/server/admin_activity_export_test.go`, test helper `insertLogRow`
**Severity:** Critical

Migration 023 added `warning_code TEXT` to `request_logs`. The proxy sets `row.warningCode` (client.go:696). But no test inserts a row with `warning_code != NULL` and verifies it in output.

**Approach:**
1. Extend `insertLogRow` to accept an optional `warning_code`.
2. Insert a row with `warning_code='reasoning_selector_omitted'`.
3. Verify CSV export: header contains `"warning_code"`, the row's cell contains the value.
4. Verify JSON export: the row object has `"warning_code": "reasoning_selector_omitted"`.

---

### T2 — `aggregateVirtualReasoningCapabilities` tests

**File:** `internal/server/client.go:204–219`
**Severity:** High

Zero direct tests for the only path populating reasoning for virtual models.

**Approach:** Test with valid JSON, NULL, and invalid JSON targets; verify the merged superset and graceful handling of bad data.

---

### T3 — Budget-mapping application tests

**File:** `internal/server/translate_reasoning_test.go`
**Severity:** High

Budget parsing is tested (models.dev), but applying a budget selector to a target body is not.

**Approach:** Test budget emitted when supported, warning when unsupported, out-of-bounds budget, combined effort+budget.

---

### T4 — `mergeReasoningCapabilities` comprehensive tests

**File:** `internal/server/translate_reasoning_test.go` or `virtual_capabilities_test.go`
**Severity:** High

1 test covers 1 of ~12 branches.

**Approach:** Test nil inputs, effort union (finite + unrestricted), toggle, budget range merge, parameter dedup, thinking-mode ordering, default effort fallback, mandatory/default-enabled merge.

---

### T5 — `ExtractReasoningOptions` unit tests

**File:** `internal/providers/registry_test.go`
**Severity:** High

No direct tests for the bridge between parsing and mapping.

**Approach:** Test effort-only, toggle-only, budget-only, combined, unrestricted effort, nil caps.

---

### T6 — `selector.Present == false` early-return

**File:** `internal/server/translate_reasoning_test.go`
**Severity:** Medium

The `if !selector.Present { return body, "" }` branch is never tested.

**Approach:** Send a body with no reasoning fields; verify unchanged, no warning.

---

### T8 — `decodeReasoningCapabilities` with invalid JSON

**File:** `internal/server/admin_providers_test.go`
**Severity:** Medium

No test verifies graceful handling of malformed JSON.

**Approach:** Insert malformed JSON; verify returns nil without panicking.

---

### T9 — `nullableReasoningCapabilities` marshaling round-trip

**File:** `internal/providers/registry_test.go` or `manager_test.go`
**Severity:** Medium

No round-trip test for the storage marshaler.

**Approach:** Test marshal → unmarshal equality; test nil → NULL → nil.

---

## Deferred (F2–F5) — HOLD

Gated on product decision about how much reasoning detail the admin UI should surface. The router is pass-through by design (see F1 decision in audit notes). Fix independently when touching shared dialog code.

| Item | Description | Why deferred |
|---|---|---|
| **F2** | `.warning-code` class emitted but not styled | Needs design decision (badge? color?) |
| **F3** | Warning cell has no tooltip/help text | Needs copy/content from product |
| **F4** | `capFlags` discards full `ReasoningCapabilities` shape | Would only matter if reasoning were admin-configurable |
| **F5** | Stale `#capabilities-refresh-error` on dialog reopen | Small bug; fix independently when touching that dialog |

**Tracking:** `docs/todo_minor_updates.md`.

---

## Implementation Order

| Step | Item | Est. risk |
|---|---|---|
| 1 | T7 (fix `enabled=false + effort=high` bug) | Medium — logic change across 3 functions |
| 2 | T1 (warning_code e2e) | Medium — touches test helper + export assertions |
| 3 | T2 (`aggregateVirtualReasoningCapabilities` tests) | Medium — needs DB seeding |
| 4 | T3 (budget application tests) | Low — additive cases |
| 5 | T4 (`mergeReasoningCapabilities` comprehensive) | Low — additive cases |
| 6 | T5 (`ExtractReasoningOptions` unit tests) | Low — additive cases |
| 7 | T6–T9 (medium test gaps) | Low — additive cases |

---

## Verification Strategy

After all fixes:
- Run `./tiller-go.sh test ./...` — all existing + new tests pass.
- Run `./tiller-go.sh vet ./...` — no new static analysis issues.
- Run `./tests/browser/run.sh` — admin UI still renders.
- Do **not** run compatibility or runtime-readonly suites — changes don't touch provider protocols, client-facing catalogues beyond the budget-doc fix, or deployment/security settings.

---

## Files Touched (planned)

| File | Change type |
|---|---|
| `internal/server/translate.go` | Fix T7 logic (Chat, Responses, Messages appliers) |
| `internal/server/translate_reasoning_test.go` | Tests for T3, T4, T6, T7 |
| `internal/server/admin_activity_export_test.go` | Tests for T1 |
| `internal/server/client_test.go` (or new) | Tests for T2 |
| `internal/providers/registry_test.go` | Tests for T5, T9 |
| `internal/server/admin_providers_test.go` | Tests for T8 |

---

## Open Questions for the Human

1. **T7 fix direction:** Should `enabled=false + effort=high` (a) honor disabled and drop effort with a warning, or (b) honor effort and ignore disabled? Recommend (a) — explicit disable should win, and the contradictory combination deserves a warning.
2. **T1 stretch goal:** Drive a real proxy request to trigger the warning, or just seed the DB row? Recommend seeding only; defer integration e2e to follow-up.
3. **T2 location:** Add to existing `client_test.go` or create `virtual_capabilities_test.go`?
