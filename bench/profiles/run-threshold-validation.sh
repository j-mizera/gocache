#!/usr/bin/env bash
# Purpose: validate FR-007 tiered reproducibility thresholds across benchmark classes.
#
# Each class is run with -count=10 and -benchmem. The script computes coefficient
# of variation for ns/op and requires allocs/op to be exact across all runs.

set -euo pipefail

REPO=$(git rev-parse --show-toplevel)
OUT="$REPO/bench/results/threshold-validation.md"
TIMESTAMP=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
GO_VERSION=$(go version)
KERNEL=$(uname -r)
CPU=$(grep 'model name' /proc/cpuinfo | head -1 | cut -d: -f2 | xargs || true)
if [[ -z "$CPU" ]]; then
    CPU="unknown"
fi
GOMAXPROCS=$(nproc)
BENCH_TIMEOUT=${BENCH_TIMEOUT:-180s}

mkdir -p "$(dirname "$OUT")"

declare -a BENCHMARK_IDS=(
    "command_netpipe"
    "command_afunix"
    "hook_nohook"
    "hook_prehook_16"
    "telemetry_local"
    "telemetry_tmpfs"
    "telemetry_fanout_100"
    "concurrent_shared_goroutines_4"
    "publish_subscribers_10"
    "raw_syscall_afunix"
)

declare -A BENCHMARK_NAMES=(
    [command_netpipe]="CommandRTT_NetPipe"
    [command_afunix]="CommandRTT_AFUnix"
    [hook_nohook]="HookDispatchRTT_NoHook"
    [hook_prehook_16]="HookDispatchRTT_PreHook_16"
    [telemetry_local]="TelemetryDeliveryLatency_Local"
    [telemetry_tmpfs]="TelemetryDeliveryLatency_Tmpfs"
    [telemetry_fanout_100]="TelemetryDeliveryFanout_Subscribers_100"
    [concurrent_shared_goroutines_4]="ConcurrentCommandThroughput_Shared_Goroutines_4"
    [publish_subscribers_10]="PluginCommandPUBLISH_Subscribers_10"
    [raw_syscall_afunix]="RawSyscallPingPong_AFUnix"
)

declare -A BENCHMARK_PATTERNS=(
    [command_netpipe]="^BenchmarkPluginCommandRTT_NetPipe$"
    [command_afunix]="^BenchmarkPluginCommandRTT_AFUnix$"
    [hook_nohook]="^BenchmarkHookDispatchRTT$/^NoHook$"
    [hook_prehook_16]="^BenchmarkHookDispatchRTT$/^PreHook_16$"
    [telemetry_local]="^BenchmarkTelemetryDeliveryLatency_Local$"
    [telemetry_tmpfs]="^BenchmarkTelemetryDeliveryLatency_Tmpfs$"
    [telemetry_fanout_100]="^BenchmarkTelemetryDeliveryFanout$/^Subscribers_100$"
    [concurrent_shared_goroutines_4]="^BenchmarkConcurrentCommandThroughput_Shared$/^Goroutines_4$"
    [publish_subscribers_10]="^BenchmarkPluginCommandPUBLISH$/^Subscribers_10$"
    [raw_syscall_afunix]="^BenchmarkRawSyscallPingPong_AFUnix$"
)

declare -A BENCHMARK_PACKAGES=(
    [command_netpipe]="./pkg/plugin/router/"
    [command_afunix]="./pkg/plugin/router/"
    [hook_nohook]="./pkg/pipeline/"
    [hook_prehook_16]="./pkg/pipeline/"
    [telemetry_local]="./pkg/events/"
    [telemetry_tmpfs]="./commons/observability/"
    [telemetry_fanout_100]="./pkg/events/"
    [concurrent_shared_goroutines_4]="./pkg/plugin/router/"
    [publish_subscribers_10]="./plugins/pubsub/"
    [raw_syscall_afunix]="./pkg/plugin/benchsuite/"
)

declare -A BENCHMARK_THRESHOLDS=(
    [command_netpipe]="5"
    [command_afunix]="10"
    [hook_nohook]="15"
    [hook_prehook_16]="15"
    [telemetry_local]="15"
    [telemetry_tmpfs]="15"
    [telemetry_fanout_100]="15"
    [concurrent_shared_goroutines_4]="15"
    [publish_subscribers_10]="15"
    [raw_syscall_afunix]="10"
)

cat > "$OUT" <<REPORT
# Tiered Reproducibility Threshold Validation (FR-007)

## Metadata
- Date: $TIMESTAMP
- Go version: $GO_VERSION
- Kernel: $KERNEL
- CPU: $CPU
- GOMAXPROCS: $GOMAXPROCS
- Benchtime: 500ms per run, 10 runs per benchmark
- Benchmark timeout: $BENCH_TIMEOUT per class

## Results

| Benchmark | Threshold | CV (ns/op) | Status | Allocs/op Variance |
|-----------|-----------|------------|--------|-------------------|
REPORT

passed=0
total=0
failed_classes=()

cd "$REPO"

for benchmark_id in "${BENCHMARK_IDS[@]}"; do
    benchmark_name=${BENCHMARK_NAMES[$benchmark_id]}
    pattern=${BENCHMARK_PATTERNS[$benchmark_id]}
    package=${BENCHMARK_PACKAGES[$benchmark_id]}
    threshold=${BENCHMARK_THRESHOLDS[$benchmark_id]}

    total=$((total + 1))
    echo ">>> Validating $benchmark_name (threshold: $threshold%)"

    run_output=""
    set +e
    run_output=$(timeout "$BENCH_TIMEOUT" go test -bench="$pattern" -count=10 -benchmem -benchtime=500ms "$package" 2>&1)
    run_status=$?
    set -e

    if [[ $run_status -ne 0 ]]; then
        reason=$(printf '%s\n' "$run_output" | tr '\n' ' ' | cut -c1-160)
        echo "    SKIP: go test failed or timed out"
        printf '| %s | %s%% | n/a | SKIP | go test failed or timed out: `%s` |\n' "$benchmark_name" "$threshold" "$reason" >> "$OUT"
        failed_classes+=("$benchmark_name")
        continue
    fi

    mapfile -t ns_values < <(printf '%s\n' "$run_output" | awk '
        /^Benchmark/ {
            pending_benchmark = $1
            for (field_index = 1; field_index <= NF; field_index++) {
                if ($field_index == "ns/op") { print $(field_index - 1); pending_benchmark = "" }
            }
            next
        }
        /^[[:space:]]*[0-9]+[[:space:]]+/ && pending_benchmark != "" {
            for (field_index = 1; field_index <= NF; field_index++) {
                if ($field_index == "ns/op") { print $(field_index - 1); pending_benchmark = "" }
            }
        }
    ')
    mapfile -t alloc_values < <(printf '%s\n' "$run_output" | awk '
        /^Benchmark/ {
            pending_benchmark = $1
            for (field_index = 1; field_index <= NF; field_index++) {
                if ($field_index == "allocs/op") { print $(field_index - 1); pending_benchmark = "" }
            }
            next
        }
        /^[[:space:]]*[0-9]+[[:space:]]+/ && pending_benchmark != "" {
            for (field_index = 1; field_index <= NF; field_index++) {
                if ($field_index == "allocs/op") { print $(field_index - 1); pending_benchmark = "" }
            }
        }
    ')

    if [[ ${#ns_values[@]} -ne 10 ]]; then
        reason="expected 10 ns/op samples, found ${#ns_values[@]}"
        echo "    SKIP: $reason"
        printf '| %s | %s%% | n/a | SKIP | %s |\n' "$benchmark_name" "$threshold" "$reason" >> "$OUT"
        failed_classes+=("$benchmark_name")
        continue
    fi

    stats=$(printf '%s\n' "${ns_values[@]}" | awk '
        { ns_samples[NR] = $1 + 0; sum += ns_samples[NR]; count++ }
        END {
            mean = sum / count
            for (sample_index = 1; sample_index <= count; sample_index++) {
                delta = ns_samples[sample_index] - mean
                variance += delta * delta
            }
            variance = variance / count
            stddev = sqrt(variance)
            cv = mean == 0 ? 0 : (stddev / mean) * 100
            printf "%.2f %.2f %.2f %d", mean, stddev, cv, count
        }
    ')
    read -r mean_ns stddev_ns cv_percent sample_count <<< "$stats"

    if [[ ${#alloc_values[@]} -ne 10 ]]; then
        alloc_variance="missing alloc samples (${#alloc_values[@]}/10)"
        alloc_exact=0
    else
        unique_alloc_count=$(printf '%s\n' "${alloc_values[@]}" | awk '!seen[$0]++ { count++ } END { print count + 0 }')
        if [[ "$unique_alloc_count" == "1" ]]; then
            alloc_variance="0% (exact: ${alloc_values[0]})"
            alloc_exact=1
        else
            alloc_variance="NONZERO (${unique_alloc_count} distinct values)"
            alloc_exact=0
        fi
    fi

    cv_pass=$(awk -v cv="$cv_percent" -v limit="$threshold" 'BEGIN { print (cv <= limit) ? "yes" : "no" }')
    status="FAIL"
    if [[ "$cv_pass" == "yes" && "$alloc_exact" -eq 1 ]]; then
        status="PASS"
        passed=$((passed + 1))
    else
        failed_classes+=("$benchmark_name")
    fi

    echo "    $status: cv=${cv_percent}% mean=${mean_ns}ns stddev=${stddev_ns}ns samples=${sample_count} allocs=${alloc_variance}"
    printf '| %s | %s%% | %s%% | %s | %s |\n' "$benchmark_name" "$threshold" "$cv_percent" "$status" "$alloc_variance" >> "$OUT"
done

if [[ ${#failed_classes[@]} -eq 0 ]]; then
    isolation_list="none"
else
    isolation_list=$(printf '%s, ' "${failed_classes[@]}")
    isolation_list=${isolation_list%, }
fi

cat >> "$OUT" <<REPORT

## Summary
- $passed/$total classes passed their tiered threshold
- Classes requiring hardware isolation: $isolation_list

## Notes
- Allocs/op variance is expected to be 0% (deterministic). Any non-zero variance indicates a real change in allocation behavior.
- CV is computed as (stddev/mean × 100) across 10 independent runs.
- This is a preliminary validation on the development host. Final thesis evidence should be run on a dedicated benchmark host with CPU governor=performance and Turbo Boost disabled.
REPORT

echo "Report written to $OUT"
