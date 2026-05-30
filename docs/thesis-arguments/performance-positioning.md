# Performance positioning versus Valkey

## Purpose

This note captures thesis-safe wording for the benchmark interpretation after ADR-0028 / Phase 2A observability optimization. The format intentionally records both the author's original claim and a refined academic claim.

## Evidence context

Benchmark evidence comes from `bench/results/heavy-event-hotpath-20260530/summary.md`.

Measured workload:

- Dockerized `bench/redis-benchmark` harness.
- `BENCH_N=100000`, `BENCH_CLIENTS=50`, `BENCH_KEYSPACE=100000`, `BENCH_PIPELINE=10`.
- Command suite: `PING_INLINE`, `PING_MBULK`, `SET`, `GET`, `INCR`, `LPUSH`, `RPUSH`, `LPOP`, `RPOP`, `SADD`, `HSET`, `SPOP`, `LRANGE_100`, `MSET`.
- Compared GoCache implementation branch against Valkey 8 and against the previous GoCache baseline.

Key Valkey-relative results:

- GoCache core, non-pipelined: `+4.41%` geometric mean throughput versus Valkey, `-19.63%` p99 latency, 14/15 commands above Valkey in throughput.
- GoCache core, pipelined: `-9.97%` geometric mean throughput versus Valkey, `+87.94%` p99 latency, 6/15 commands above Valkey.
- GoCache IPC Prometheus, non-pipelined: `-6.76%` geometric mean throughput versus Valkey, `+132.03%` p99 latency.
- GoCache IPC Prometheus, pipelined: `-59.63%` geometric mean throughput versus Valkey, `+530.0%` p99 latency.
- GoCache memory footprint was substantially higher than Valkey in the measured Docker RSS samples.

## Thesis claim pairs

### 1. Overall competitiveness

**Original claim (mine):**

> GoCache can be overall competitive with Valkey, but on pipelined commands it would still require great improvements.

**Refined claim (thesis-safe):**

> The benchmark results indicate that GoCache is competitive with Valkey in non-pipelined core workloads, where it achieved slightly higher geometric mean throughput and lower p99 latency in this benchmark suite. However, the results also show that GoCache is not yet consistently competitive in pipelined workloads, especially when IPC-based observability plugins are enabled. This identifies pipelined command execution and plugin-observability delivery as the main areas requiring further optimization.

### 2. Core standard mode

**Original claim (mine):**

> Core can compete on standard mode.

**Refined claim (thesis-safe):**

> In the non-pipelined core configuration, GoCache can be considered throughput-competitive with Valkey for the measured command suite. The implementation exceeded Valkey's geometric mean throughput by `4.41%` and had better p99 latency in the aggregate, while also outperforming Valkey on 14 of 15 measured commands.

### 3. Core pipelined mode

**Original claim (mine):**

> Core can compete on pipelined mode without IPC plugins, but not as clearly.

**Refined claim (thesis-safe):**

> In the pipelined core configuration, GoCache remains partially competitive but does not match Valkey overall. Some commands, such as `SPOP`, `SET`, `PING_MBULK`, `GET`, `PING_INLINE`, and `INCR`, exceeded Valkey throughput, but the geometric mean throughput was `9.97%` lower and p99 latency was substantially worse. This suggests that the core execution path is viable, but pipelined batching, serialization, and collection-command paths need further optimization.

### 4. Standard IPC observability

**Original claim (mine):**

> Standard with IPC is at least close on throughput levels.

**Refined claim (thesis-safe):**

> With IPC Prometheus observability enabled, GoCache's non-pipelined throughput remains within a comparable range but is no longer ahead of Valkey. The measured geometric mean throughput was `6.76%` below Valkey, while p99 latency was worse. This indicates that IPC observability is viable for non-pipelined workloads from a throughput perspective, but it still introduces measurable latency and memory overhead.

### 5. Pipelined IPC observability

**Original claim (mine):**

> Where there is no discussion at all is pipelined with IPC, and especially this part would require further optimizations.

**Refined claim (thesis-safe):**

> The pipelined IPC observability configuration is the clearest remaining performance gap. Compared with Valkey, GoCache with the IPC Prometheus plugin was `59.63%` lower in geometric mean throughput and had much worse p99 latency. This result shows that per-command IPC observability is not yet suitable for high-throughput pipelined workloads without additional batching, aggregation, or lower-cost event delivery mechanisms.

### 6. Memory efficiency

**Original claim (mine):**

> Memory efficiency will naturally be worse because Go is garbage collected, Valkey uses manual memory management, and Valkey does not emit events or create all this internal observability. Competing on memory would probably require unsafe Go.

**Refined claim (thesis-safe):**

> GoCache's higher memory footprint is expected and should be interpreted in the context of its implementation language and feature scope. Unlike Valkey, which is implemented in C and manually manages memory, GoCache uses Go's garbage-collected runtime and maintains additional internal observability structures for operations, events, hooks, and plugin integration. Therefore, RSS parity with Valkey is not the primary success criterion of this thesis. A substantial reduction of the memory gap would likely require lower-level memory-layout work, object pooling, reduced allocation frequency, and possibly unsafe or manual-memory techniques, which would trade off against the project's safety and maintainability goals.

### 7. Safety/extensibility trade-off

**Original claim (mine):**

> The point is not that GoCache beats Valkey everywhere; it shows safe extensibility can still be competitive.

**Refined claim (thesis-safe):**

> The benchmark results should be interpreted as evidence for a trade-off rather than as a claim of universal superiority. GoCache demonstrates that a Redis-compatible cache written in a memory-safe language and extended through an isolated plugin architecture can remain competitive in non-pipelined core workloads. At the same time, the results expose the cost of richer observability and IPC-based extensibility, especially under pipelined workloads.

## Recommended thesis wording

> The experimental results show that GoCache is competitive with Valkey in non-pipelined core workloads, despite using Go's garbage-collected runtime and maintaining additional observability and plugin-extension structures. This supports the thesis that safe extensibility can coexist with competitive throughput for common request/response workloads. However, the pipelined results, especially with IPC observability enabled, reveal a significant remaining performance gap. The main direction for future work is therefore not basic command execution, but reducing the cost of high-frequency observability through batching, aggregation, delayed serialization, and lower-allocation event delivery.

## Caveats

- The benchmark was a single run per target; small single-digit differences should be treated as directional until repeated median runs are collected.
- Docker bridge networking, container CPU pinning, and the closed-loop benchmark model affect absolute numbers.
- The Valkey comparison is useful as an external reference point, but Valkey does not perform equivalent GoCache plugin/event/operation-observability work in this benchmark.
- Memory conclusions are based on container RSS samples from the benchmark harness, not a full heap profile.
