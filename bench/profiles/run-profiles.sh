#!/usr/bin/env bash
# Captures CPU + alloc + block + mutex profiles for each diagnosis workload
# in a single go test invocation per workload. Trace captures are separate
# runs (the runtime tracer interferes with -cpuprofile sampling).
#
# Sampling rates are chosen so the profiles still attribute the top-N
# contenders correctly while keeping per-iteration overhead manageable:
#   memprofilerate = 4096 — one sample per 4 KiB allocated (Go default 524288)
#   blockprofilerate = 10000 — one sample per 10 us of blocking
#   mutexprofilefraction = 10 — 1 in 10 contention events
# The defaults at rate=1 produce 24x slowdown on contended workloads.
#
# Output layout under bench/profiles/pre-implementation/.

set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
OUT="$REPO/bench/profiles/${OUT_LABEL:-diagnosis-baseline}"
DUR="${DUR:-10s}"
CPU="${CPU:-4}"

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

# 5 workloads × in-process / TCP variants where applicable
run_one inproc-hset   BenchmarkInProc_HSET
run_one tcp-hset-std  BenchmarkTCP_HSET_Standard
run_one inproc-get    BenchmarkInProc_GET
run_one tcp-get-std   BenchmarkTCP_GET_Standard
run_one inproc-set    BenchmarkInProc_SET
run_one tcp-set-std   BenchmarkTCP_SET_Standard
run_one tcp-get-pipe  BenchmarkTCP_GET_Pipelined
run_one tcp-hset-pipe BenchmarkTCP_HSET_Pipelined

# Trace captures only for the two pipelined workloads
run_trace tcp-get-pipe  BenchmarkTCP_GET_Pipelined  10s
run_trace tcp-hset-pipe BenchmarkTCP_HSET_Pipelined 10s

echo
echo "All profile captures complete."
ls -la "$OUT/cpu" "$OUT/alloc" "$OUT/block" "$OUT/mutex" "$OUT/trace" "$OUT/gctrace"
