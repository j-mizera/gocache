#!/usr/bin/env bash
# Captures CPU + alloc + block + mutex profiles for the mixed-workload
# benchmarks added for the per-shard locking diagnosis (issue #34).
#
# These mixed benchmarks expose the cross-path interaction cost that
# single-workload benchmarks (run-profiles.sh) miss. Half the goroutines
# read while half write, on a shared server.
#
# Same sampling rates as run-profiles.sh; outputs into a sibling label
# directory so single-workload and mixed captures can coexist.

set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
BRANCH=$(git -C "$REPO" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)
BRANCH_SAFE="${BRANCH//\//-}"
OUT="${OUT:-$REPO/bench/results/$BRANCH_SAFE/profiles/${OUT_LABEL:-diagnosis-mixed}}"
DUR="${DUR:-10s}"
CPU="${CPU:-4}"
mkdir -p "$OUT"/{cpu,alloc,block,mutex,gctrace,trace}

run_one() {
    local label=$1 bench=$2
    echo
    echo ">>> $label  (bench=$bench  duration=$DUR  cpu=$CPU)"

    GODEBUG=gctrace=1 \
    go test -run=NONE -bench="^$bench$" \
        -cpu="$CPU" -benchtime="$DUR" \
        -cpuprofile="$OUT/cpu/$label.prof" \
        -memprofile="$OUT/alloc/$label.mem" -memprofilerate=4096 \
        -blockprofile="$OUT/block/$label.prof" -blockprofilerate=10000 \
        -mutexprofile="$OUT/mutex/$label.prof" -mutexprofilefraction=10 \
        ./pkg/server/ \
        > "$OUT/$label.bench.txt" \
        2> "$OUT/gctrace/$label.gctrace"

    grep -E "^Bench|allocs/op|ns/op" "$OUT/$label.bench.txt" | tail -1
}

run_trace() {
    local label=$1 bench=$2 dur=${3:-10s}
    echo
    echo ">>> trace $label  (bench=$bench  duration=$dur)"
    go test -run=NONE -bench="^$bench$" \
        -cpu="$CPU" -benchtime="$dur" \
        -trace="$OUT/trace/$label.trace" \
        ./pkg/server/ \
        > "$OUT/trace/$label.bench.txt" 2>&1
    grep -E "^Bench|allocs/op|ns/op" "$OUT/trace/$label.bench.txt" | tail -1
}

# 3 mixed workloads: pipelined string read+write, pipelined collection
# write + read, and standard string read+write for the cross-comparison.
run_one tcp-mixed-getset-pipe   BenchmarkTCP_Mixed_GetSet_Pipelined
run_one tcp-mixed-gethset-pipe  BenchmarkTCP_Mixed_GetHset_Pipelined
run_one tcp-mixed-getset-std    BenchmarkTCP_Mixed_GetSet_Standard

# Trace captures only for the two pipelined mixed workloads (the standard
# variant won't show channel-hop park/unpark cycles as cleanly).
run_trace tcp-mixed-getset-pipe   BenchmarkTCP_Mixed_GetSet_Pipelined  10s
run_trace tcp-mixed-gethset-pipe  BenchmarkTCP_Mixed_GetHset_Pipelined 10s

echo
echo "All mixed-workload profile captures complete."
ls -la "$OUT/cpu" "$OUT/alloc" "$OUT/block" "$OUT/mutex" "$OUT/trace" "$OUT/gctrace"
