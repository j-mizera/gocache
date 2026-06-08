# GoCache Detailed Bottleneck Analysis

## Methodology

This document identifies every measured performance bottleneck in GoCache with exact file paths, line numbers, and root causes. Evidence comes from:
- **pprof mutex profiles** — `sync.(*Mutex).Unlock` attribution
- **pprof alloc profiles** — `alloc_objects` and `alloc_space` attribution
- **Benchstats counters** — `operation_tracker.skipped_operations`, `instrumentation.send_queue_full`, etc.
- **Code inspection** — direct reading of hot-path source files

All line numbers reference commit `37240842e4b8bac063c4f826a9dcb240605f8a24` on branch `zero-allocation-telemetry-subplan-b`.

---

## 1. Core Cache Shard Mutex (Dominant Bottleneck)

### 1.1 Per-Shard Lock Contention

**Location:** `pkg/cache/shard.go:25-59`
```go
type Shard struct {
    mu              sync.RWMutex
    // ...
}
func (s *Shard) Lock()    { s.mu.Lock() }
func (s *Shard) Unlock()  { s.mu.Unlock() }
```

**Evidence:**
- **Prom mutex pprof**: `sync.(*Mutex).Unlock` flat = 299.41s (7392.47% of total)
- **OTEL mutex pprof**: `sync.(*Mutex).Unlock` flat = 322.98s (569.05% of total)
- **Attribution chain**: `pkg/cache.(*Shard).Unlock` → `pkg/server.(*Server).runBatch` → `pkg/engine.(*Engine).DispatchToShards`

**Root Cause:** Every single-key command acquires the target shard's write lock for the entire duration of the handler. On pipelined workloads, this serializes all commands targeting the same shard. The FNV-1a hash function `shardIndexOf(key) = fnv1a(key) & (N-1)` with N=8 means ~12.5% of keys hash to each shard. With 50 pipelined connections, each shard sees contention from ~6 connections on average.

**Impact:**
- G1: Prom mutex wait 116.5s / 1.5M ops (target: <=5s)
- G2: OTEL mutex wait 127.6s / 1.5M ops (target: <=20s)
- G12: Core pipelined -27.3% vs Valkey (target: <=-5%)

**Why it's structural:** The per-shard mutex is the fundamental concurrency model. Removing it would require:
- Lock-free data structures (risky, complex)
- RCU (read-copy-update) pattern (high memory overhead)
- Transactional memory (not available in Go)
- COW (copy-on-write) snapshots (would double memory)

**Mitigation already applied:**
- Sharding reduced from single global mutex to 8 shards (PR #34)
- Default lowered from N=16 to N=8 (PR #50) — reduced memory without throughput loss
- Selective shard locking for multi-key commands (PR #44)

---

### 1.2 Multi-Shard Lock Ordering (MSET Penalty)

**Location:** `pkg/engine/engine.go:121-134`
```go
func (e *Engine) DispatchToShards(ctx context.Context, shardIDs []int, fn func() any) (any, error) {
    release := e.cache.LockShards(shardIDs, true)
    defer release()
    return fn(), nil
}
```

**Root Cause:** MSET commands touch multiple shards. Each shard touched adds:
1. Lock acquisition latency (mutex contention)
2. Cache-line bounce (shard metadata across CPU cores)
3. Per-shard LRU update + eviction check

**Evidence:** Pipelined MSET is ~0.55× Valkey (per-shard arc summary), -31% vs single-mutex baseline.

**Why it's structural:** The break-even depends on key distribution. Single-key handlers win (K=1 < N=8); multi-key handlers lose when K > 1.

---

## 2. Telemetry Drain Path (Secondary Bottleneck)

### 2.1 Drain Worker Lock Serialization

**Location:** `pkg/server/operation_tracker_drain_worker.go:300-306`
```go
func (w *OperationTrackerDrainWorker) DrainOnce() int {
    w.drainMu.Lock()
    defer w.drainMu.Unlock()
    drained := 0
    for shard := 0; shard < w.manager.ShardCount(); shard++ {
        drained += w.manager.DrainCompletedShard(shard, w.projectCompletedOperation)
    }
    return drained
}
```

**Root Cause:** The drain worker holds a single `drainMu` mutex while iterating ALL shards. This serializes:
- `DrainCompletedShard()` for each shard
- `projectCompletedOperation()` callback for each completed operation

**Evidence:**
- **Prom alloc pprof**: `DrainCompletedShard` = 21.6M objects (16.90% of total)
- **OTEL alloc pprof**: `DrainCompletedShard` = 140.4M objects (22.66% of total)

**Why it's a bottleneck:** Under high load, the drain worker cannot keep up with the completion rate. The 1ms drain interval (`defaultOperationTrackerDrainInterval`) is a safety timeout; the real drain is event-driven via `CompletedNotify()` channel. But when completions arrive faster than drain speed, operations pile up.

---

### 2.2 Projection/Materialization Allocations

**Location:** `pkg/server/operation_tracker_drain_worker.go:331-367`
```go
func (w *OperationTrackerDrainWorker) projectCompletedOperation(operation commonobs.CompletedOperation) {
    if !w.hasAnyEventInterest(operation) {
        return
    }
    operationContext := w.copyOperationContext(operation)
    for i := range operation.Records {
        record := operation.Records[i]
        switch record.Kind {
        case apiobs.TelemetryRecordOperationStart:
            w.materializeOperationStartedRecord(operation, record, operationContext)
        case apiobs.TelemetryRecordOperationFinish:
            w.materializeOperationFinishedRecord(operation, record, operationContext)
        // ... 10 more cases
        }
    }
}
```

**Root Cause:** Every completed operation with event interest goes through:
1. `copyOperationContext()` — clones context map
2. `recordPairs()` — parses payload bytes into `[]kvPair`
3. `materialize*Record()` — constructs event objects, calls `cloneStringMap()`
4. `emitter.Emit()` — fanout to subscribers

**Evidence:**
- **Prom alloc pprof**: `projectCompletedOperation` = 12.9M objects (10.14%)
- **OTEL alloc pprof**: `projectCompletedOperation` = 137.2M objects (22.14%)
- **OTEL alloc pprof**: `recordPairs` = 54.5M objects (8.80%)
- **OTEL alloc pprof**: `materializeOperationFinishedRecord` = 56.4M objects (9.10%)
- **OTEL alloc pprof**: `materializeOperationStartedRecord` = 49.9M objects (8.05%)

**Why it's worse with OTEL:** OTEL has event subscribers (`HasSubscribersFor` returns true), so `hasAnyEventInterest` passes and materialization runs. Prometheus-only has no runtime event subscribers, so many operations skip materialization.

---

### 2.3 String Interning Table

**Location:** `pkg/server/operation_tracker_drain_worker.go:170-185`
```go
func (s *operationTrackerDrainScratch) intern(value string) string {
    if s.interned == nil {
        s.interned = make(map[string]string, 64)
    }
    if interned, ok := s.interned[value]; ok {
        return interned
    }
    if len(s.interned) >= 512 {
        return value
    }
    s.interned[value] = value
    return value
}
```

**Root Cause:** The intern table has a hard cap of 512 entries. Beyond that, every string is a new allocation. The `internKeyBytes` function converts `[]byte` → `string` (allocation), then does a map lookup.

**Evidence:**
- **OTEL alloc pprof**: `internKeyBytes` = 36.7M objects (5.92%)

**Why it's a bottleneck:** Map lookups on the hot path. The intern table is meant to reduce allocations, but at high throughput it becomes a cache with a low hit rate.

---

## 3. Pipeline Evaluation (Tertiary Bottleneck)

### 3.1 Hook String Conversion

**Location:** `pkg/pipeline/pipeline.go` (implied by pprof, not exact line)
```go
func resultToHookStrings(result apicommand.Result) []string {
    // Uses fmt.Sprintf + reflect.Value.Interface
}
```

**Evidence:**
- **Prom alloc pprof**: `fmt.Sprintf` = 31.8M objects (24.91%)
- **Prom alloc pprof**: `reflect.packEface` = 30.2M objects (23.63%)
- **Attribution chain**: `resultToHookStrings` → `fmt.Sprintf` → `reflect.Value.Interface`

**Root Cause:** Hook strings are materialized via reflection and `fmt.Sprintf` for every command, even when no hooks are registered. The `cmdCtxPool` recycle helps, but the string formatting is unavoidable on the current path.

---

### 3.2 Command Context Pool

**Location:** `pkg/pipeline/pipeline.go:46-58`
```go
var cmdCtxPool = sync.Pool{
    New: func() any { return &command.Context{} },
}
func putCmdCtx(c *command.Context) {
    c.Reset()
    cmdCtxPool.Put(c)
}
```

**Root Cause:** `sync.Pool` has known issues with high churn — objects can be dropped during GC, causing new allocations. The `Reset()` call zeroes all fields, which touches memory.

**Evidence:** `cmdCtxPool` is present but not in top 20 alloc pprof; it's likely amortized well.

---

## 4. RESP Protocol Parsing

### 4.1 Array/Bulk String Parsing

**Location:** `commons/resp/reader.go` (implied by pprof)

**Evidence:**
- **Prom alloc pprof**: `resp.(*Reader).Read` = 19.2M objects (15.01%)
- **Prom alloc pprof**: `resp.(*Reader).readArray` = 4.4M objects (3.46%)
- **Prom alloc pprof**: `resp.(*Reader).readBulkString` = 6.8M objects (5.36%)

**Root Cause:** Every pipelined command requires RESP array parsing. The `readBulkString` allocates a new string for each argument. The `readArray` allocates a slice for the array elements.

**Why it's hard to fix:** RESP is a text protocol. Parsing inherently requires string allocations. The bufio.Reader underlying the connection may have buffered data, but each command is still parsed independently.

---

## 5. OTEL Plugin IPC Path (OTEL-Specific Bottleneck)

### 5.1 Plugin Write Loop

**Location:** `pkg/plugin/router/router.go` (implied by pprof)

**Evidence:**
- **OTEL alloc pprof**: `plugin/router.(*PluginConn).writeLoop` = 66.2M objects (10.69%)
- **OTEL alloc pprof**: `plugin/router.(*PluginConn).writeOutboundBatch` = 66.2M objects (10.68%)

**Root Cause:** The plugin write loop serializes all outbound events through a single goroutine per plugin. Each write involves:
1. `proto.Marshal` — protobuf serialization
2. `commons/transport.(*Conn).SendBatch` — framing + socket write
3. `reflect.copyVal` — protobuf reflection copies

**Evidence:**
- **OTEL alloc pprof**: `proto.Marshal` = 62.4M objects (10.07%)
- **OTEL alloc pprof**: `reflect.copyVal` = 60.4M objects (9.74%)

**Why it's devastating:** IPC serialization is ~10% of all allocations. The protobuf marshal uses reflection, which is allocation-heavy. The GC pressure from these allocations triggers more frequent GC cycles, which pause goroutines.

---

### 5.2 Event Bus Fanout

**Location:** `pkg/events/bus.go` (implied by pprof)

**Evidence:**
- **OTEL alloc pprof**: `events.(*Bus).Emit` = 41.3M objects (6.67%)

**Root Cause:** The event bus fanout creates event copies for each subscriber. With OTEL + Prometheus, there are multiple subscribers, so each event is cloned.

**Why it's worse with OTEL:** OTEL adds more subscribers than Prometheus-only. The event bus does not use a zero-copy broadcast mechanism.

---

### 5.3 IPC Queue Pressure

**Evidence:**
- `instrumentation.send_queue_full=4,607,647` (OTEL benchstats)
- `send_accepted=9,788,450`
- `write_batches=306,649`
- Write latency total: 59.667s

**Root Cause:** The OTEL plugin's IPC queue is bounded. When the queue fills, events are dropped (fire-and-forget). The `send_queue_full` counter indicates 4.6M drops. The queue pressure also backpressures into the event bus, which backpressures into the drain worker.

---

## 6. Operation Tracker Admission Pressure

### 6.1 Slot Magazine Exhaustion

**Location:** `commons/observability/manager.go` (implied by benchstats)

**Evidence:**
- Prom: `skipped_operations=52,782`
- OTEL: `skipped_operations=1,811,152`

**Root Cause:** The `SlotOperationTrackerManager` has a finite number of operation slots (magazine capacity). When all slots are in use, new operations are skipped. The telemetry submit path is fast (92ns), but the drain path is slow. This creates a production-consumption mismatch.

**Why it's worse with OTEL:** OTEL's drain is slower due to materialization + IPC, so slots stay occupied longer, causing more skips.

---

### 6.2 Connection Context Version Pinning

**Location:** `commons/observability/manager.go` (implied by `PinOwnedConnectionContextVersion`)

**Root Cause:** Every operation start pins the connection context version. This requires atomic operations on the context registry. Under high concurrency, the atomic operations on shared state create cache-line contention.

**Evidence:** Not directly visible in pprof (too small individually), but contributes to the overall mutex wait time.

---

## 7. Server Batch Processing

### 7.1 Batch Pre-Sorting

**Location:** `pkg/server/server.go:752-765`
```go
shardMode := make(map[int]bool, len(batch))
for _, e := range batch {
    if prev, ok := shardMode[e.shard]; ok {
        shardMode[e.shard] = prev && e.readOnly
    } else {
        shardMode[e.shard] = e.readOnly
    }
}
shardIDs := make([]int, 0, len(shardMode))
for id := range shardMode {
    shardIDs = append(shardIDs, id)
}
sort.Ints(shardIDs)
```

**Root Cause:** Every batch pre-computes the shard set and sorts it. For a batch of 10 commands, this is negligible. For a batch of 128 commands (maxBatchSize), the sort is O(K log K) where K is the number of unique shards.

**Evidence:** `runBatch` is 247.24s cum (Prom) / 215.94s cum (OTEL) in mutex pprof. This includes all shard lock acquisitions.

---

### 7.2 Response Serialization

**Location:** `pkg/server/server.go:800-888`
```go
func (srv *Server) mapValueToResp(ctx *clientctx.ClientContext, val any) resp.Value {
    switch v := val.(type) {
    case []byte:
        return resp.MarshalBulkString(string(v)) // copies!
    case []any:
        respArray := make([]resp.Value, len(v))
        for i, item := range v {
            respArray[i] = srv.mapValueToResp(ctx, item)
        }
        return resp.ValueArray(respArray...)
    }
}
```

**Root Cause:** Response serialization allocates for every element. The `[]byte` → `string` conversion in `MarshalBulkString` copies. The `[]any` → `[]resp.Value` conversion allocates a new slice.

**Evidence:** Present in heap pprof but not in top alloc_objects (because it's per-response, not per-command in the same way).

---

## 8. Unmeasured/Structural Overheads

### 8.1 Go Runtime Scheduler

**Evidence:** `docs/audits/go-bench-vs-docker-gap.md` — `runtime.selectgo` consumes 18.74% (Go bench) → 31.30% (docker bench) of CPU.

**Root Cause:** Go's scheduler has overhead for goroutine park/unpark. In docker, the veth + iptables NAT increases TCP latency, causing more inter-batch gaps, which causes more goroutine park/unpark cycles.

**Why it's not fixable in code:** It's a runtime behavior. The only fix is reducing the number of goroutine context switches, which means reducing the number of goroutines or batching more work per goroutine.

---

### 8.2 Docker Network Stack

**Evidence:** `docs/audits/go-bench-vs-docker-gap.md` — docker bench is 5-10× lower in relative-percentage terms than Go bench.

**Root Cause:** Docker's veth + iptables NAT adds ~10-20µs per RTT. This is not a code issue.

**Why it's not fixable in code:** Infrastructure issue. The benchmark harness is dockerized for reproducibility.

---

### 8.3 Memory Overhead (RSS)

**Evidence:** `docs/performance/README.md` — RSS at N=8 is ~10× Valkey's at 1M keys.

**Root Cause:**
- Per-shard maps, slab metadata, LRU lists
- Persistence layer (gob snapshot worker)
- Telemetry infrastructure (slot magazines, connection context registry)

**Why it's structural:** The per-shard architecture inherently uses more memory than a single-shard design. The persistence layer is scheduled for replacement as a plugin.

---

## 9. Summary Table

| Rank | Bottleneck | Location | Evidence | Impact | Fixable? |
|------|-----------|----------|----------|--------|----------|
| 1 | Per-shard mutex contention | `pkg/cache/shard.go:25-59` | Mutex pprof: 299-323s | G1/G2/G12 fail | No (structural) |
| 2 | Drain worker serialization | `pkg/server/operation_tracker_drain_worker.go:300-306` | Alloc pprof: 21-140M objects | G3/G4 fail | Yes (parallel drain) |
| 3 | Projection/materialization | `pkg/server/operation_tracker_drain_worker.go:331-367` | Alloc pprof: 13-137M objects | G3/G4 fail | Yes (lazy projection) |
| 4 | RESP parsing | `commons/resp/reader.go` | Alloc pprof: 19-30M objects | Moderate | Partial (buffer reuse) |
| 5 | IPC protobuf serialization | `pkg/plugin/router/router.go` | Alloc pprof: 62-66M objects | G8/G9 fail | Yes (zero-copy) |
| 6 | Event bus fanout | `pkg/events/bus.go` | Alloc pprof: 41M objects | Moderate | Yes (shared broadcast) |
| 7 | Hook string conversion | `pkg/pipeline/pipeline.go` | Alloc pprof: 31M objects | Moderate | Yes (skip if no hooks) |
| 8 | String interning | `pkg/server/operation_tracker_drain_worker.go:170-185` | Alloc pprof: 37M objects | Low | Yes (better cache) |
| 9 | Slot magazine exhaustion | `commons/observability/manager.go` | Benchstats: 1.8M skips | G5 fail | Yes (increase capacity) |
| 10 | Batch pre-sorting | `pkg/server/server.go:752-765` | Mutex pprof: 215-247s cum | Low | Yes (radix sort) |
| 11 | Response serialization | `pkg/server/server.go:800-888` | Heap pprof | Low | Partial (buffer pool) |
| 12 | Go scheduler overhead | `runtime.selectgo` | CPU pprof: 18-31% | Structural | No |
| 13 | Docker network stack | veth + iptables | Bench gap audit | Structural | No |
| 14 | Memory overhead (RSS) | `pkg/cache/shard.go` | RSS ~10× Valkey | Structural | No |

---

## 10. Recommended Fix Priority

### High Impact, Low Effort (Fix First)
1. **Lazy projection gate** — Skip `projectCompletedOperation` if no subscribers (already partially done, but OTEL path still runs)
2. **Increase slot magazine capacity** — Reduce `skipped_operations`
3. **Skip hook string conversion if no hooks** — `resultToHookStrings` should be conditional

### High Impact, Medium Effort
4. **Parallel drain worker** — One drain goroutine per shard instead of one global drain
5. **Zero-copy IPC serialization** — Avoid protobuf reflection, use flatbuffers or manual marshaling
6. **Shared event broadcast** — Event bus should use a single shared event with reference counting instead of cloning

### High Impact, High Effort
7. **Lock-free shard data structures** — Replace per-shard mutex with lock-free skiplist or hash map
8. **Batch coalescing optimization** — Pre-compute shard set without sorting (radix sort or bitset)

### Low Impact, Any Effort
9. **String interning cache** — Increase from 512 to 4096, or use LRU
10. **Response buffer pool** — Reuse `[]resp.Value` slices

---

## Evidence References

- `bench/results/telemetry-remediation-p21-prom/*-profiles/` — Prom pprof artifacts
- `bench/results/telemetry-remediation-p21-otel/*-profiles/` — OTEL pprof artifacts
- `.omo/evidence/telemetry-remediation/task-21-failure-attribution.md` — Gate failure analysis
- `docs/audits/go-bench-vs-docker-gap.md` — Scheduler overhead analysis
- `docs/performance/README.md` — Per-shard arc summary
- `docs/performance/modular-overhead-optimization-plan.md` — Planned fixes

---

*Commit: `37240842e4b8bac063c4f826a9dcb240605f8a24` | Branch: `zero-allocation-telemetry-subplan-b`*