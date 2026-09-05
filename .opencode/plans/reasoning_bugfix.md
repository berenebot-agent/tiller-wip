# Reasoning/Thinking — TODO

> Post-PR audit of `beta.1-reasoning`. 5 bugs fixed & committed (B1–B4, T7).

---

## Active (8 items)

### T1 — `warning_code` end-to-end test
**Severity:** Critical | **Files:** `internal/server/admin_activity_export_test.go`, `insertLogRow` helper

The `reasoning_selector_omitted` warning flows proxy → DB → CSV/JSON export. No test verifies it lands.

1. Extend `insertLogRow` to accept optional `warning_code`.
2. Insert a row with `warning_code='reasoning_selector_omitted'`.
3. Verify CSV: header has `"warning_code"`, row cell has value.
4. Verify JSON: row object has `"warning_code": "reasoning_selector_omitted"`.

---

### T2 — `aggregateVirtualReasoningCapabilities` tests
**Severity:** High | **File:** `internal/server/client.go:204–219`

Zero direct tests for the only path populating reasoning for virtual models. Test valid JSON, NULL, invalid JSON targets; verify merged superset + graceful bad-data handling.

---

### T3 — Budget-mapping application tests
**Severity:** High | **File:** `internal/server/translate_reasoning_test.go`

Budget parsing tested (models.dev), but applying budget to target body is not. Test: budget emitted when supported, warning when unsupported, out-of-bounds, combined effort+budget.

---

### T4 — `mergeReasoningCapabilities` comprehensive tests
**Severity:** High | **File:** `internal/server/translate_reasoning_test.go` or `virtual_capabilities_test.go`

1 test covers 1 of ~12 branches. Test: nil inputs, effort union (finite + unrestricted), toggle, budget range merge, parameter dedup, thinking-mode ordering, default effort fallback, mandatory/default-enabled merge.

---

### T5 — `ExtractReasoningOptions` unit tests
**Severity:** High | **File:** `internal/providers/registry_test.go`

Bridge between stored metadata and mapping decision. Zero direct tests. Test: effort-only, toggle-only, budget-only, combined, unrestricted effort, nil caps.

---

### T6 — `selector.Present == false` early-return
**Severity:** Medium | **File:** `internal/server/translate_reasoning_test.go`

The `if !selector.Present { return body, "" }` branch is never tested. Send body with no reasoning fields; verify unchanged, no warning.

---

### T8 — `decodeReasoningCapabilities` with invalid JSON
**Severity:** Medium | **File:** `internal/server/admin_providers_test.go`

No test verifies graceful handling of malformed JSON. Insert bad JSON; verify returns nil without panicking.

---

### T9 — `nullableReasoningCapabilities` marshaling round-trip
**Severity:** Medium | **File:** `internal/providers/registry_test.go` or `manager_test.go`

No round-trip test for storage marshaler. Test marshal → unmarshal equality; test nil → NULL → nil.

---

## Deferred — HOLD (4 items)

Gated on product decision about admin reasoning surface. Router is pass-through by design.

| Item | Description | Why deferred |
|---|---|---|
| **F2** | `.warning-code` class emitted but not styled | Needs design decision (badge? color?) |
| **F3** | Warning cell has no tooltip/help text | Needs copy/content from product |
| **F4** | `capFlags` discards full `ReasoningCapabilities` shape | Would only matter if reasoning were admin-configurable |
| **F5** | Stale `#capabilities-refresh-error` on dialog reopen | Small bug; fix independently when touching that dialog |

---

## Suggested Priority

T1 → T2 → T3 → T4 → T5 → T6, T8, T9

## Verification

After all fixes: `./tiller-go.sh test ./...` + `./tiller-go.sh vet ./...` + `./tests/browser/run.sh`. Skip compatibility & runtime-readonly suites.

---

## Open Questions

1. **T1 stretch goal:** Seed DB row only (recommended), or also drive a real proxy request?
2. **T2 location:** Add to `client_test.go` or create `virtual_capabilities_test.go`?
