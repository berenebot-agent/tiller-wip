#!/bin/bash
# tests/run.sh — unified test runner for tiller-router.
#
# Runs the selected test tiers, writes ALL detail to
# tests/logs/runs/<UTC-ts>-<tiers>/, and prints only a one-line-per-tier
# summary to stdout with a pointer to that folder. The full output is
# ALWAYS on disk — never re-run just to see more.
#
# Usage:
#   ./tests/run.sh                      # default preset: unit + vet + runtime
#   ./tests/run.sh --unit --vet         # pick specific tiers
#   ./tests/run.sh --all                # every tier
#   ./tests/run.sh --last               # print the newest run's summary + paths (no run)
#   ./tests/run.sh --prune [--keep N]   # prune old runs (default: keep all, no-op)
#   ./tests/run.sh --list               # list tiers and exit
#
# Tiers:
#   unit     Go unit/integration tests (./tiller-go.sh test)
#   vet      Go static analysis (./tiller-go.sh vet)
#   browser  Playwright admin UI tests (tests/browser/run.sh)
#   compat   Real SDK/CLI compatibility probes (tests/compatibility/run.sh)
#   runtime  Read-only rootfs / caps / backup checks (tests/runtime-readonly.sh)
#
# Output levels (all written to the run folder every time):
#   L1  summary.txt   per-tier pass/fail + elapsed + first error + folder path
#   L2  timings.txt   per-tier elapsed + per-test breakdown + browser phases
#   L3  full.log      concat of every tier's complete output (tier-tagged)
#   meta.txt          run id, tiers, git SHA/branch/dirty, toolchain image
#       <tier>/out.log  each tier's raw output
#
# Retention: runs are kept in full (no automatic pruning) so an agent can
# always dig into an earlier run. Reclaim space explicitly with:
#   ./tests/run.sh --prune --keep 20
# tests/logs/history.tsv is an append-only ledger that survives pruning.
#
# The stdout block is L1 only — context-cheap for agents. Detail lives in
# the folder; the summary's `detail:` line points straight at it.

set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$REPO_ROOT"

RUNS_DIR="tests/logs/runs"
LATEST_LINK="tests/logs/latest"
HISTORY="tests/logs/history.tsv"

# --- mode flags -----------------------------------------------------------
SHOW_LAST=0
DO_PRUNE=0
PRUNE_KEEP=""

# --- tier selection -------------------------------------------------------
declare -A TIER_SELECTED=()
ALL_TIERS=(unit vet browser compat runtime)

args=("$@")
i=0
while [ "$i" -lt "${#args[@]}" ]; do
    arg="${args[$i]}"
    case "$arg" in
        --unit)     TIER_SELECTED[unit]=1 ;;
        --vet)      TIER_SELECTED[vet]=1 ;;
        --browser)  TIER_SELECTED[browser]=1 ;;
        --compat)   TIER_SELECTED[compat]=1 ;;
        --runtime)  TIER_SELECTED[runtime]=1 ;;
        --all)      for t in "${ALL_TIERS[@]}"; do TIER_SELECTED[$t]=1; done ;;
        --last)     SHOW_LAST=1 ;;
        --prune)    DO_PRUNE=1 ;;
        --keep)
            i=$((i + 1))
            if [ "$i" -ge "${#args[@]}" ]; then
                echo "tests/run.sh: --keep requires a number" >&2
                exit 2
            fi
            PRUNE_KEEP="${args[$i]}"
            ;;
        --list)
            echo "Tiers: ${ALL_TIERS[*]}"
            echo "Flags:  --unit --vet --browser --compat --runtime --all"
            echo "Modes:  --last | --prune [--keep N] | --list"
            exit 0
            ;;
        -h|--help)
            sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "tests/run.sh: unknown flag '$arg' (try --help)" >&2
            exit 2
            ;;
    esac
    i=$((i + 1))
done

# --- --last: print the newest run, no execution ---------------------------
newest_run() {
    ls -1dt "$RUNS_DIR"/*/ 2>/dev/null | head -1 | sed 's:/$::'
}

if [ "$SHOW_LAST" -eq 1 ]; then
    latest=$(newest_run)
    if [ -z "${latest:-}" ]; then
        echo "tests/run.sh: no runs yet under $RUNS_DIR/" >&2
        exit 1
    fi
    echo "run:     $latest/"
    echo "summary: $latest/summary.txt"
    echo "timings: $latest/timings.txt"
    echo "full:    $latest/full.log"
    [ -f "$latest/meta.txt" ] && echo "meta:    $latest/meta.txt"
    echo
    cat "$latest/summary.txt" 2>/dev/null || echo "(no summary.txt)"
    echo
    if [ -f "$HISTORY" ]; then
        echo "recent history ($HISTORY):"
        tail -n 5 "$HISTORY"
    fi
    exit 0
fi

# --- --prune: optional, explicit LRU pruning ------------------------------
if [ "$DO_PRUNE" -eq 1 ]; then
    if [ -z "$PRUNE_KEEP" ]; then
        echo "tests/run.sh: --prune requires --keep N (runs are kept in full by default)" >&2
        exit 2
    fi
    KEEP_RUNS="$PRUNE_KEEP" KEEP_BROWSER="$PRUNE_KEEP" KEEP_COMPAT="$PRUNE_KEEP" KEEP_GO="$PRUNE_KEEP" \
        . tests/scripts/prune-test-logs.sh
    echo "pruned $RUNS_DIR/ to the newest $PRUNE_KEEP runs" >&2
    exit 0
fi

# default preset: fast, high-signal. Use --all for everything.
if [ "${#TIER_SELECTED[@]}" -eq 0 ]; then
    TIER_SELECTED[unit]=1
    TIER_SELECTED[vet]=1
    TIER_SELECTED[runtime]=1
fi

SELECTED=()
for t in "${ALL_TIERS[@]}"; do
    [ -n "${TIER_SELECTED[$t]:-}" ] && SELECTED+=("$t")
done

# --- run id + folder ------------------------------------------------------
TS=$(date -u +%Y%m%dT%H%M%S)-$$
TIER_LABEL=$(IFS=,; echo "${SELECTED[*]}")
RUN_DIR="tests/logs/runs/${TS}-${TIER_LABEL}"

for t in "${SELECTED[@]}"; do
    mkdir -p "$RUN_DIR/$t"
done

# --- run metadata ---------------------------------------------------------
git_sha="unknown"
git_branch="unknown"
git_dirty="unknown"
if git rev-parse --git-dir >/dev/null 2>&1; then
    git_sha=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
    git_branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)
    if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
        git_dirty="dirty"
    else
        git_dirty="clean"
    fi
fi
# Single source of truth for the Go image is ./tiller-go.sh.
image=$(grep -m1 '^GO_IMAGE=' tiller-go.sh 2>/dev/null | cut -d'"' -f2 || echo unknown)

{
    echo "run_id:    ${TS}-${TIER_LABEL}"
    echo "started:   $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "host:      $(hostname 2>/dev/null || echo unknown)"
    echo "tiers:     ${TIER_LABEL}"
    echo "git_sha:   ${git_sha}"
    echo "git_br:    ${git_branch}"
    echo "git_state: ${git_dirty}"
    echo "image:     ${image}"
} > "$RUN_DIR/meta.txt"

# --- run tiers ------------------------------------------------------------
declare -A TIER_RC=()
declare -A TIER_ELAPSED=()
declare -A TIER_FIRST_ERROR=()

run_start=$(date +%s.%N)

for t in "${SELECTED[@]}"; do
    echo "  running $t ..." >&2
    tier_start=$(date +%s.%N)
    rc=0
    case "$t" in
        unit)
            # -v so per-test timing is available in the output for L2
            TILLER_TEST_DIR="$RUN_DIR/unit" \
                ./tiller-go.sh test -count=1 -v ./... >/dev/null 2>&1 || rc=$?
            ;;
        vet)
            TILLER_TEST_DIR="$RUN_DIR/vet" \
                ./tiller-go.sh vet ./... >/dev/null 2>&1 || rc=$?
            ;;
        browser)
            TILLER_TEST_DIR="$RUN_DIR/browser" \
                ./tests/browser/run.sh >/dev/null 2>&1 || rc=$?
            ;;
        compat)
            TILLER_TEST_DIR="$RUN_DIR/compat" \
                ./tests/compatibility/run.sh >/dev/null 2>&1 || rc=$?
            ;;
        runtime)
            TILLER_TEST_DIR="$RUN_DIR/runtime" \
                ./tests/runtime-readonly.sh >/dev/null 2>&1 || rc=$?
            ;;
    esac
    tier_end=$(date +%s.%N)
    elapsed=$(awk "BEGIN {printf \"%.1f\", $tier_end - $tier_start}")
    TIER_RC[$t]=$rc
    TIER_ELAPSED[$t]=$elapsed

    # let any tee'd process substitution flush before we read the file
    wait 2>/dev/null || true

    if [ "$rc" -ne 0 ]; then
        first_err=$(grep -m1 -E '^(--- FAIL|FAIL\b|Error:|FAIL:)' "$RUN_DIR/$t/out.log" 2>/dev/null | head -c 300 || true)
        [ -n "$first_err" ] && TIER_FIRST_ERROR[$t]="$first_err"
    fi
done

run_end=$(date +%s.%N)
total_elapsed=$(awk "BEGIN {printf \"%.1f\", $run_end - $run_start}")

# --- L1 summary -----------------------------------------------------------
overall_rc=0
for t in "${SELECTED[@]}"; do
    [ "${TIER_RC[$t]}" -ne 0 ] && overall_rc=1
done
overall_str=$([ "$overall_rc" -eq 0 ] && echo "PASS" || echo "FAIL")

{
    echo "[tiller-router tests] ${TS} — ${TIER_LABEL}"
    for t in "${SELECTED[@]}"; do
        rc=${TIER_RC[$t]}
        elapsed=${TIER_ELAPSED[$t]}
        if [ "$rc" -eq 0 ]; then
            printf "  %-8s PASS   %ss\n" "$t" "$elapsed"
        else
            printf "  %-8s FAIL   %ss" "$t" "$elapsed"
            [ -n "${TIER_FIRST_ERROR[$t]:-}" ] && printf "  %s" "${TIER_FIRST_ERROR[$t]}"
            printf "\n"
        fi
    done
    printf "  overall  %s     %ss\n" "$overall_str" "$total_elapsed"
    echo "  detail: $RUN_DIR/"
} | tee "$RUN_DIR/summary.txt"

# --- L2 timings -----------------------------------------------------------
{
    echo "Timings — ${TS} ${TIER_LABEL}"
    echo "--------------------------------"
    for t in "${SELECTED[@]}"; do
        printf "%-8s %ss\n" "$t" "${TIER_ELAPSED[$t]}"
    done
    printf "total    %ss\n" "$total_elapsed"

    # Go per-test breakdown (parsed from -v output) for any selected tier that
    # produced it, not just unit — compat and runtime also run Go tests.
    for t in "${SELECTED[@]}"; do
        out="$RUN_DIR/$t/out.log"
        [ -f "$out" ] || continue
        if ! grep -qE '^\s*--- (PASS|FAIL):' "$out" 2>/dev/null; then
            continue
        fi
        echo
        echo "${t^} per-test (sorted by elapsed, descending):"
        grep -E '^\s*--- (PASS|FAIL):' "$out" 2>/dev/null | \
            sed -E 's/^[[:space:]]*--- (PASS|FAIL):[[:space:]]+([^[:space:]]+)[[:space:]]+\(([0-9.]+)s\)[[:space:]]*$/\3 \1 \2/' | \
            sort -t' ' -k1 -rn | \
            awk '{printf "  %7.3fs  %-6s  %s\n", $1, $2, $3}' || true
    done

    # Browser per-phase lines
    if [ -n "${TIER_SELECTED[browser]:-}" ] && [ -f "$RUN_DIR/browser/out.log" ]; then
        echo
        echo "Browser phases:"
        grep -E '^\s*phase:' "$RUN_DIR/browser/out.log" 2>/dev/null | \
            sed -E 's/^\s*/  /' || true
    fi
} > "$RUN_DIR/timings.txt"

# --- L3 full log ----------------------------------------------------------
{
    echo "===== tiller-router tests: ${TS} ${TIER_LABEL} ====="
    echo "overall: $overall_str  total: ${total_elapsed}s"
    echo
    for t in "${SELECTED[@]}"; do
        echo "===== $t (${TIER_ELAPSED[$t]}s, rc=${TIER_RC[$t]}) ====="
        if [ -f "$RUN_DIR/$t/out.log" ]; then
            cat "$RUN_DIR/$t/out.log"
        else
            echo "  (no output captured)"
        fi
        echo
    done
} > "$RUN_DIR/full.log"

# --- latest pointer + durable history -------------------------------------
ln -sfn "runs/${TS}-${TIER_LABEL}" "$LATEST_LINK"

if [ ! -f "$HISTORY" ]; then
    printf 'timestamp\tsha\tbranch\tstate\ttiers\toverall\telapsed_s\trun_dir\n' > "$HISTORY"
fi
printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$TS" "$git_sha" "$git_branch" "$git_dirty" "$TIER_LABEL" "$overall_str" "$total_elapsed" "$RUN_DIR" \
    >> "$HISTORY"

exit "$overall_rc"
