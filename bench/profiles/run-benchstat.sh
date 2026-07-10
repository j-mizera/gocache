#!/usr/bin/env bash
# Purpose: run benchmarks with -count=10 and analyze via benchstat (FR-006).
#
# WHY -count=10: single-run b.N is an anecdote, not a measurement.
# benchstat needs multiple runs to compute confidence intervals.
#
# Any thesis evidence claim MUST cite benchstat confidence intervals, not raw deltas.
#
# Note: benchstat compares old vs new. For single-config analysis (no comparison),
# use `benchstat -geomean` or just review the raw output.

set -euo pipefail

BENCH="${1:-}"
COUNT="${2:-10}"
PACKAGE="${3:-./pkg/plugin/benchsuite/}"

if [[ -z "$BENCH" ]]; then
    echo "Usage: $0 <bench-pattern> [count] [package]" >&2
    echo "  bench-pattern: benchmark name regex (required, e.g. BenchmarkRawSyscallPingPong)" >&2
    echo "  count: number of runs (default: 10, per FR-006 mandate)" >&2
    echo "  package: Go package path (default: ./pkg/plugin/benchsuite/)" >&2
    exit 2
fi

if ! command -v benchstat >/dev/null 2>&1; then
    echo "Install: go install golang.org/x/perf/cmd/benchstat@latest" >&2
    exit 1
fi

REPO=$(git rev-parse --show-toplevel)
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
OUT="$REPO/bench/results/benchstat/$TIMESTAMP"

mkdir -p "$OUT"

go test -bench="$BENCH" -count="$COUNT" -benchmem "$PACKAGE" > "$OUT/raw.txt" 2>&1
benchstat "$OUT/raw.txt" > "$OUT/analysis.txt" 2>&1 || true

cat "$OUT/analysis.txt"

echo
echo "Raw output: $OUT/raw.txt"
echo "Benchstat analysis: $OUT/analysis.txt"
