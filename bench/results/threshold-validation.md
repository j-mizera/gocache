# Tiered Reproducibility Threshold Validation (FR-007)

## Metadata
- Date: 2026-07-07T20:16:03Z
- Go version: go version go1.26.4-X:nodwarf5 linux/amd64
- Kernel: 7.0.10-arch1-1
- CPU: AMD Ryzen 9 7900X 12-Core Processor
- GOMAXPROCS: 24
- Benchtime: 500ms per run, 10 runs per benchmark
- Benchmark timeout: 180s per class

## Results

| Benchmark | Threshold | CV (ns/op) | Status | Allocs/op Variance |
|-----------|-----------|------------|--------|-------------------|
| CommandRTT_NetPipe | 5% | 1.95% | PASS | 0% (exact: 36) |
| CommandRTT_AFUnix | 10% | 26.49% | FAIL | 0% (exact: 36) |
| HookDispatchRTT_NoHook | 15% | 2.17% | PASS | 0% (exact: 6) |
| HookDispatchRTT_PreHook_16 | 15% | 2.56% | PASS | 0% (exact: 22) |
| TelemetryDeliveryLatency_Local | 15% | 0.73% | PASS | 0% (exact: 0) |
| TelemetryDeliveryLatency_Tmpfs | 15% | 4.24% | PASS | 0% (exact: 0) |
| TelemetryDeliveryFanout_Subscribers_100 | 15% | 3.01% | PASS | 0% (exact: 1) |
| ConcurrentCommandThroughput_Shared_Goroutines_4 | 15% | 1.91% | PASS | 0% (exact: 36) |
| PluginCommandPUBLISH_Subscribers_10 | 15% | 41.04% | FAIL | 0% (exact: 149) |
| RawSyscallPingPong_AFUnix | 10% | 1.34% | PASS | 0% (exact: 0) |

## Summary
- 8/10 classes passed their tiered threshold
- Classes requiring hardware isolation: CommandRTT_AFUnix, PluginCommandPUBLISH_Subscribers_10

## Notes
- Allocs/op variance is expected to be 0% (deterministic). Any non-zero variance indicates a real change in allocation behavior.
- CV is computed as (stddev/mean × 100) across 10 independent runs.
- This is a preliminary validation on the development host. Final thesis evidence should be run on a dedicated benchmark host with CPU governor=performance and Turbo Boost disabled.
