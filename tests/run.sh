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
#   L2  timings.txt   per-tier elapsed + Go per-test breakdown + browser phases
#   L3  full.log      concat of every tier's complete output (tier-tagged)
#       <tier>/out.log  each tier's raw output
#
# The stdout block is L1 only — context-cheap for agents. Detail lives in
# the folder; the summary's `detail:` line points straight at it.

set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$REPO_ROOT"

# --- tier selection -------------------------------------------------------
declare -A TIER_SELECTED=()
ALL_TIERS=(unit vet browser compat runtime)

for arg in "$@"; do
    case "$arg" in
        --unit)     TIER_SELECTED[unit]=1 ;;
        --vet)      TIER_SELECTED[vet]=1 ;;
        --browser)  TIER_SELECTED[browser]=1 ;;
        --compat)   TIER_SELECTED[compat]=1 ;;
        --runtime)  TIER_SELECTED[runtime]=1 ;;
        --all)      for t in "${ALL_TIERS[@]}"; do TIER_SELECTED[$t]=1; done ;;
        --list)
            echo "Tiers: ${ALL_TIERS[*]}"
            echo "Flags:  --unit --vet --browser --compat --runtime --all"
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
done

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

# bound the runs/ tree (LRU, same policy as the other test-log dirs)
. tests/scripts/prune-test-logs.sh

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

    # Go per-test breakdown (parsed from -v output)
    if [ -n "${TIER_SELECTED[unit]:-}" ] && [ -f "$RUN_DIR/unit/out.log" ]; then
        echo
        echo "Unit per-test (sorted by elapsed, descending):"
        grep -E '^\s*--- (PASS|FAIL):' "$RUN_DIR/unit/out.log" 2>/dev/null | \
            sed -E 's/^[[:space:]]*--- (PASS|FAIL):[[:space:]]+([^[:space:]]+)[[:space:]]+\(([0-9.]+)s\)[[:space:]]*$/\3 \1 \2/' | \
            sort -t' ' -k1 -rn | \
            awk '{printf "  %7.3fs  %-6s  %s\n", $1, $2, $3}' || true
    fi

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

exit "$overall_rc"
