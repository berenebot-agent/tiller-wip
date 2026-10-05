# Testing

How tests are run, how output is captured, and where to find logs. The short
version: **run a known set of tiers through `./tests/run.sh`, and never re-run a
test just to see the full output — it is always on disk.**

For the tier-by-tier guidance (which tier catches which mistake, and the
run-twice rule for the browser suite), see `AGENTS.md` § "How to test". This
document covers the runner, the log layout, and retention.

## The unified runner

`./tests/run.sh` runs the selected tiers, writes a complete run folder under
`tests/logs/runs/<run-id>/`, and prints only a one-line-per-tier summary to
stdout (cheap for agents and CI).

```bash
./tests/run.sh                      # default preset: unit + vet + runtime
./tests/run.sh --unit --vet         # pick specific tiers
./tests/run.sh --all                # every tier
./tests/run.sh --last               # print the newest run's summary + paths (no run)
./tests/run.sh --prune --keep 20    # reclaim space (runs are kept in full by default)
./tests/run.sh --list               # list tiers, flags and modes
```

Flags select tiers: `--unit --vet --browser --compat --runtime`, or `--all`.
There are **no verbosity flags** — all output levels are always written to disk.

The older per-tier commands (`./tiller-go.sh`, `tests/browser/run.sh`,
`tests/compatibility/run.sh`, `tests/runtime-readonly.sh`) still work and remain
the right tool for a tight edit loop (Tier A: one package or one spec). Use the
runner when you want a known set of tiers with a single summary and guaranteed
detail on disk.

## Tiers

| Tier | What it runs | Typical |
|---|---|---|
| `unit` | `./tiller-go.sh test -count=1 -v ./...` | ~1–3 min |
| `vet` | `./tiller-go.sh vet ./...` | ~2 s |
| `browser` | `tests/browser/run.sh` (Playwright admin UI, incl. first-run lane) | ~2–3 min |
| `compat` | `tests/compatibility/run.sh` (real SDK/CLI probes) | ~2–4 min |
| `runtime` | `tests/runtime-readonly.sh` (read-only rootfs / caps / backup) | ~15 s |

Default preset is `unit + vet + runtime`. Use `--all` for the full sweep.

## Run folder layout

Every run produces a folder named `<UTC timestamp>-<pid>-<tiers>`:

```
tests/logs/runs/20261004T061115-1688700-unit,vet/
  summary.txt      L1: per-tier PASS/FAIL + elapsed + first error + detail path
  timings.txt      L2: per-tier elapsed + per-test breakdown + browser phases
  full.log         L3: every tier's complete output, concatenated
  meta.txt         run id, tiers, git SHA/branch/dirty, toolchain image
  unit/out.log     raw output for the unit tier
  vet/out.log
  browser/out.log  (+ playwright-results/ preserved on failure)
```

- **L1** is what reaches stdout. Deliberately terse; on failure it inlines the
  failing tier's first error.
- **L2** ranks tests by elapsed time so the slowest are obvious, and lists the
  browser tier's phase timings.
- **L3** is the full captured stream — the thing to read instead of re-running.

## Reading logs without re-running

`tests/logs/latest` is a symlink to the newest run folder:

```bash
cat  tests/logs/latest/summary.txt           # pass/fail overview
cat  tests/logs/latest/timings.txt           # slowest tests first
cat  tests/logs/latest/meta.txt              # which commit was tested
tail -n 200 tests/logs/latest/unit/out.log   # raw tail of the unit tier
grep -n '--- FAIL' tests/logs/latest/full.log
```

`./tests/run.sh --last` prints the newest summary, the exact paths, and the most
recent ledger entries in one shot.

For a failed browser run, the full output is `browser/out.log` and the Playwright
artifacts (`trace.zip`, `error-context.md`, screenshots) are under
`browser/playwright-results/` — preserved on failure, auto-removed on success.

## Retention

Runs are **retained in full** — `tests/run.sh` does not prune automatically, so
an earlier run is always available to dig into. Reclaim space explicitly:

```bash
./tests/run.sh --prune --keep 20
```

The browser and compatibility runners still prune their *own* transient
artifacts (`tiller-browser-*`, `compat/*-compat.log`, `tiller-go/*.log`) via
`tests/scripts/prune-test-logs.sh` with their own caps.

`tests/logs/history.tsv` is an append-only ledger (timestamp, SHA, branch, dirty
state, tiers, result, elapsed, run dir) that survives pruning — use it for
trends and regression hunting:

```bash
column -t tests/logs/history.tsv | tail -20
```

The prune script never removes the ledger or the `latest` symlink.

## CI

CI (`.github/workflows/ci.yml`) runs on native Go, not the Docker runner, for
speed. Each step in the `go` job tees its full output to `ci-logs/` and the
folder is uploaded as the `ci-logs` artifact with `if: always()`, so a failed
run's full log is downloadable without re-running.

## Writing tests

- Go tests are **in-package**: `internal/<pkg>/<name>_test.go`. There is no
  `tests/unit/` tree in this repo.
- Keep tests deterministic and use local mock upstreams (`httptest` servers, or
  the Python mock in `tests/compatibility/mock_upstream.py`). Never call a real
  provider or the network.
- Browser coverage is a spec in `tests/browser/*.spec.js`, added to the matching
  lane in `tests/browser/run.sh`: the sharded list, the activity lane, or the
  env-gated first-run lane (`TILLER_BROWSER_FIRST_RUN=1`, a router with no admin
  credentials that the spec claims through the setup page).
- Never include provider credentials, client keys, session cookies, prompts,
  response bodies, or private deployment data in tests or logs. Detailed error
  logging stays disabled by default in tests.
- Prefer the smallest tier that catches the regression (see AGENTS.md § "Test
  tiers").
