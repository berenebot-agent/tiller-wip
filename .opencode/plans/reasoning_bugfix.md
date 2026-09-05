# Reasoning/Thinking — Closeout Plan

> Post-PR audit of `beta.1-reasoning`. The implementation direction is now
> capability-aware pass-through with silent omission of selector parts that a
> known target cannot represent.

## Product and implementation decisions

- Unsupported reasoning selector parts are omitted silently. There is no
  `warning_code` activity signal, warning column, or warning UI to maintain.
- Migration 023 is still unreleased: it adds only
  `provider_models.reasoning_capabilities`; the `request_logs.warning_code`
  addition is removed. A pre-release development database may retain that
  unused column; it is harmless and does not need a cleanup migration.
- The T7 production fix is in scope: explicit disable wins over a combined
  effort selector, and regression coverage exercises Chat, Responses, and
  Messages mappings. The no-selector early return and silent unsupported-part
  behavior are covered too.

## Current scope

### Active — capability API and UI

Keep the normalized reasoning capability metadata available through the client
catalogue and admin provider/virtual-model APIs. The admin UI exposes the
capabilities entry points and dialog for real and virtual models, including
eligible target aggregation. This surface remains active product work; do not
reintroduce activity warnings as a substitute for capability visibility.

### Completed coverage

- **T2:** Virtual capability aggregation is covered through valid, NULL, and
  malformed stored JSON, including admin API exposure and eligible-target
  merging.
- **T3:** Budget application covers supported in-range values and omission of
  unsupported or out-of-range values.
- **T5:** `ExtractReasoningOptions` covers effort, toggle, budget, combined,
  unrestricted, and nil capabilities.
- **T6:** A selector-absent request is verified to pass through byte-for-byte
  without a warning.
- **T7:** Production fix and cross-protocol regression tests are complete.
- **T8:** Malformed stored capability JSON is verified to fail closed without
  panicking.
- **T9:** Capability storage marshaling covers round-trip equality and
  nil-to-SQL-NULL behavior.

### Planned coverage

- **T4:** Expand `mergeReasoningCapabilities` tests to cover all meaningful
  branches: nil inputs, effort unions (finite and unrestricted), toggle,
  budget-range merging, parameter de-duplication, thinking-mode ordering,
  default effort, and mandatory/default-enabled merging.

## Obsolete or invalid audit items

- **T1 — obsolete:** warning persistence and CSV/JSON export coverage is no
  longer applicable because the silent-default policy removes warning-code
  production and storage.
- **F2 — obsolete:** there is no warning-code cell to style.
- **F3 — obsolete:** there is no warning-code message requiring tooltip copy.
- **F5 — invalid:** the stale refresh-error scenario is not a valid remaining
  issue for the current capability dialog implementation.

## Verification

Run only the applicable closeout checks:

- `./tiller-go.sh test ./...`
- `./tiller-go.sh vet ./...`
- `./tests/browser/run.sh`

Compatibility and runtime-readonly suites are out of scope for this change.
