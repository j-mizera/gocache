#!/usr/bin/env bash
# Purpose: capture pprof profiles in isolation for FR-008 compliance.
#
# WHY separate invocations: Go profiling tools interfere with each other;
# memory profiling skews CPU profiles, and block profiling affects scheduler
# trace/parking behavior. Each profile type below therefore runs in a separate
# go test invocation.
#
# Contrast with run-profiles.sh: that script combines profiles for quick
# diagnosis; this script NEVER combines profiles because thesis-grade evidence
# requires isolated captures.
#
# Sampling rates:
#   memprofilerate=1 — every byte, for allocation-site analysis
#   block rate=1 — every blocking event
#   mutex fraction=1 — every contention event
# These settings add heavy overhead, so timing from profiled runs MUST NOT be
# cited as canonical. Canonical timing numbers come from unprofiled -bench runs.

set -euo pipefail

PROFILE_TYPE="${1:-all}"
BENCH="${2:-BenchmarkRawSyscallPingPong}"
DUR="${3:-3s}"

REPO=$(git rev-parse --show-toplevel)
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
OUT="$REPO/bench/results/benchsuite-profiles/$TIMESTAMP"
PROFILE_TYPES=(cpu heap block mutex goroutine)
CAPTURED_PROFILES=()

mkdir -p "$OUT"/{cpu,heap,block,mutex,goroutine}

run_profile() {
    local profile_type=$1

    echo
    echo ">>> $profile_type profile  (bench=$BENCH  duration=$DUR)"

    case "$profile_type" in
        cpu)
            go test -bench="$BENCH" -benchmem -benchtime="$DUR" \
                -cpuprofile="$OUT/cpu/profile.prof" \
                ./pkg/plugin/benchsuite/
            CAPTURED_PROFILES+=("cpu:$OUT/cpu/profile.prof")
            ;;
        heap)
            go test -bench="$BENCH" -benchmem -benchtime="$DUR" \
                -memprofile="$OUT/heap/profile.prof" -memprofilerate=1 \
                ./pkg/plugin/benchsuite/
            CAPTURED_PROFILES+=("heap:$OUT/heap/profile.prof")
            ;;
        block)
            GOCACHE_BENCH_BLOCK_RATE=1 \
            go test -bench="$BENCH" -benchmem -benchtime="$DUR" \
                -blockprofile="$OUT/block/profile.prof" \
                ./pkg/plugin/benchsuite/
            CAPTURED_PROFILES+=("block:$OUT/block/profile.prof")
            ;;
        mutex)
            GOCACHE_BENCH_MUTEX_FRACTION=1 \
            go test -bench="$BENCH" -benchmem -benchtime="$DUR" \
                -mutexprofile="$OUT/mutex/profile.prof" \
                ./pkg/plugin/benchsuite/
            CAPTURED_PROFILES+=("mutex:$OUT/mutex/profile.prof")
            ;;
        goroutine)
            GOCACHE_BENCH_GOROUTINE_PROFILE="$OUT/goroutine/profile.prof" \
            go test -bench="$BENCH" -benchmem -benchtime="$DUR" \
                ./pkg/plugin/benchsuite/
            CAPTURED_PROFILES+=("goroutine:$OUT/goroutine/profile.prof")
            ;;
        *)
            echo "Unknown profile type: $profile_type" >&2
            echo "Usage: $0 [cpu|heap|block|mutex|goroutine|all] [bench-name] [duration]" >&2
            exit 2
            ;;
    esac
}

case "$PROFILE_TYPE" in
    all)
        for profile_type in "${PROFILE_TYPES[@]}"; do
            run_profile "$profile_type"
        done
        ;;
    cpu|heap|block|mutex|goroutine)
        run_profile "$PROFILE_TYPE"
        ;;
    *)
        echo "Unknown profile type: $PROFILE_TYPE" >&2
        echo "Usage: $0 [cpu|heap|block|mutex|goroutine|all] [bench-name] [duration]" >&2
        exit 2
        ;;
esac

echo
echo "Profile captures complete."
echo "Output directory: $OUT"
echo "Captured profiles:"
for captured_profile in "${CAPTURED_PROFILES[@]}"; do
    echo "  ${captured_profile%%:*}: ${captured_profile#*:}"
done
