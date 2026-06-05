---
title: ADR-0034 Zero-Allocation Operation Telemetry Storage
description: Store operation telemetry in preallocated commons-owned operation slots so command goroutines record without heap allocation
status: proposed
date: 2026-06-03
deciders: [witherxse]
related:
  - 0022-modular-performance-budget
  - 0029-operationtracker-sidecar-low-overhead-telemetry
  - 0031-operation-identity-export-contract
  - 0032-telemetry-context-version-ownership
  - 0033-common-operationtracker-sharding-contract
  - projects/gocache/research/gocache-zero-allocation-telemetry-sidecar-implementation-brief.md
---

# ADR-0034: Zero-Allocation Operation Telemetry Storage

## Context

ADR-0029 accepts the low-overhead OperationTracker direction: command goroutines submit compact telemetry records while worker goroutines perform context folding, typed event/log projection, GCPC/protobuf materialization, and plugin fanout. ADR-0033 then places reusable sharded tracker primitives in `commons/observability`, and the latest design review tightens the performance requirement: the goroutine performing the actual Redis command must not allocate for operation telemetry, and telemetry storage must use the minimum practical number of mutexes.

Log records are part of operation telemetry, not a parallel side channel. This includes server startup logs after environment loading: the telemetry manager must be initialized early, a startup operation identity must be created, and startup logs/events must be recorded through the operation API before they are projected. If command or server startup code writes directly to zerolog, GoCache loses a single ordered telemetry source for command records, events, context mutations, GCPC observability, plugin replay, runtime logs, and startup diagnostics. Direct zerolog calls also put formatting, sink I/O, batching, and sink backpressure back onto the goroutine performing the work.

Startup may request immediate local log materialization before the server is ready, but that is an OperationTracker materialization mode over accepted log-request records, not a direct zerolog bypass. The immediate mode is limited to tightly scoped startup/bootstrap stages before connection acceptance begins. It must never be used from connection goroutines, command goroutines, plugin hook execution, cache hot paths, or any path that can run after the server is marked ready. Therefore the tracker must exist before the log-pipeline rewire: startup, command, and server code record log-request intent into the OperationTracker, and materializers decide how to project it to zerolog, runtime log batches, GCPC, event streams, and plugin consumers.

Log-request records are bytes-first. The message is copied into fixed inline name storage from `[]byte` on hot paths, with string helpers reserved for boundaries that already hold strings. Structured key/value log fields are encoded into the fixed inline payload with bounded `AddFieldBytes` helpers, not with variadic logger-style arguments. If fields do not fit, the record increments a dropped-field counter and remains best-effort. A local-materialized flag marks startup records that already produced immediate zerolog output so background processing can still export telemetry without double-writing the local log.

Research across OpenTelemetry, Prometheus remote write, NATS, etcd watch, and LMAX Disruptor shows the same tradeoff pattern: high-throughput systems use bounded, preallocated queues/rings or per-object bounded state with explicit overflow policy, not unbounded linked lists on producer paths. OpenTelemetry span events are bounded per span and then handed to a shared batch processor queue; Prometheus remote write uses shard queues and batching; NATS and etcd enforce bounded pending state with explicit full/retry behavior. This supports a GoCache-specific hybrid: operation-local records for flush-on-finish semantics, plus shard-owned bounded completion queues for background processing.

The existing legacy `pkg/operations.Tracker` cannot remain the canonical operation lifecycle store. It formats ids, keeps active operation objects, and feeds replay/crashdump/projection consumers synchronously enough that it preserves the old lifecycle model. The new storage contract must make the commons tracker slot table the source of truth for active and completed operation telemetry while preserving API/plugin boundaries from ADR-0031 through ADR-0033.

## Decision

GoCache stores operation telemetry in preallocated, shard-owned operation slots owned by the reusable `commons/observability` tracker implementation. `api/observability` defines the plugin-safe identities, records, and small contracts; `commons/observability` owns the generic tracking mechanism that the server and plugins can use directly. `pkg/` packages may wire server-specific projection, GCPC/log/event materialization, or temporary legacy adapters, but they must not own a separate operation tracking mechanism or introduce a `pkg/observability` layer. Each tracker shard starts with bounded capacity, but capacity is allowed to scale and shrink under pressure through background-owned segment management. A shard owns one or more preallocated slot segments, fixed free-lists, bounded completed-operation rings, and any lookup table needed for non-hot-path resolution. A command goroutine receives an internal tracker handle that identifies `{shard, segment, slot, generation}` and records telemetry by writing values into that already allocated slot.

The internal tracker handle is not the public `api/observability.OperationHandle` boundary. The public handle remains an API/plugin-facing operation identity and parent/correlation reference. The slot handle is a server/common implementation detail used only for command-side routing, generation validation, and slot lookup; it must not cross GCPC, SDK, plugin, event, or log boundaries.

The command-executing goroutine must perform zero heap allocations for telemetry start, record, finish, and failure paths. It must not allocate linked-list nodes, grow slices, insert into Go maps, format strings, build protobufs, materialize GCPC/event/log records, run subscriber checks, or call `sync.Pool.Get` as part of the normal telemetry record path. Pools may be used only for startup prewarming, background processing, or slow-path recovery where an allocation cannot appear in the command benchmark.

Operation records are stored as fixed-size `api/observability.TelemetryRecord` values inside the slot. The first implementation uses a bounded operation-local record array/ring per slot. If the array is full, the tracker increments per-operation and per-shard dropped-record counters and keeps command execution non-blocking. Operation start stores raw exported identity bytes or strategy-owned identity material without rendering strings; string rendering happens only at materialization boundaries.

Shard mutexes are allowed only for slot acquisition, completion/release state transitions, context-version refcount updates that must be serialized, maintenance of shard-owned free/completed queues, and installation or retirement of capacity segments by a background controller. The steady command-body record path uses the handle-indexed slot and must be mutex-free when the command goroutine is the operation owner. If a future feature needs multi-producer writes into one operation, it must use an explicit API that preserves ordering and benchmarks its synchronization cost separately.

On normal finish, failure, timeout, or abandon, the tracker marks the slot terminal and pushes the slot index into the shard's bounded completed-operation ring. Background processors drain completed slots, resolve and copy the tracker-owned operation-start context version from ADR-0032, fold accepted telemetry records in order, materialize GCPC/events/logs, release the context reference, reset the slot, and return it to the shard free-list. If the completed-operation ring is full, telemetry remains best-effort: the implementation may drop the completed operation payload, increment visible counters, release context references, and recycle the slot rather than blocking the command goroutine.

The slot table replaces the legacy map-backed lifecycle registry as the operation telemetry source of truth. Active operation snapshots for crashdump, plugin queries, replay, and health surfaces are projections over tracker shard slots and background-owned materialized state, not a separate canonical registry. Public contracts remain in `api/observability`, reusable tracking/storage primitives live in `commons/observability` so plugins can use them directly, and server-specific context mapping, redaction, projection, GCPC/log emission, and plugin fanout remain in `pkg/`.

## Contract Shape

### Hot command-goroutine contract

For telemetry purposes, the command-executing goroutine may only:

1. acquire or receive an internal tracker handle backed by a preallocated slot;
2. submit operation lifecycle, command, event, log request, and context mutation intent as scalar fields and fixed-size `TelemetryRecord` values into that slot, including bytes-first log messages and bounded inline key/value fields;
3. increment bounded drop/skipped counters;
4. transition the slot to terminal state and enqueue its index for background processing.

After this migration, operation lifecycle, command telemetry, runtime event intent, log request intent, startup log/event intent, and operation-context mutation intent must enter the OperationTracker mechanism before any GCPC, event-bus, zerolog, or plugin-facing materialization. Command/server code must not call zerolog directly for operation-correlated logs; it must submit a log-request telemetry record to the observability tracker/component. Normal operation materialization happens in background workers. Startup/bootstrap code may request immediate zerolog output only through the observability materializer before the server is ready, so startup diagnostics are visible immediately while still remaining telemetry-first. Direct command-goroutine emission paths are allowed only for non-telemetry control flow such as cancellation, RESP replies, cache mutation, or explicit legacy adapters being removed in the same migration phase.

The command telemetry path must not:

- allocate heap objects;
- grow slices with `append`;
- allocate or mutate linked-list nodes;
- insert into or resize Go maps;
- render UUIDv7/W3C/string ids;
- convert `[]byte` to `string` or `string` to `[]byte`;
- allocate closures, interface boxes, protobufs, event structs, log field slices, or variadic helper backing arrays;
- call callback-style helpers that make records escape unless benchmarks prove otherwise;
- inspect subscribers or plugin queues before recording telemetry;
- call zerolog directly for operation-correlated logs instead of submitting a log-request record through the tracker, including startup logs after the telemetry manager is initialized;
- emit operation events, runtime logs, GCPC observability payloads, or plugin fanout directly instead of recording intent through the tracker;
- block on telemetry workers or plugin delivery.

Allocation-sensitive benchmarks must cover at least operation start, command record submission, completion/failure, per-operation record overflow, and skipped-operation/no-slot paths. They must cover both concrete calls and the intended interface-dispatched calls so escape behavior is visible. The acceptance target is `0 B/op` and `0 allocs/op` for the command-side telemetry path, backed by `-benchmem` and escape-analysis review of the hot APIs.

### Shard storage

Each shard owns fixed-capacity storage sized from configuration and measurements:

```text
operationShard {
  segments: fixed/preallocated []operationSegment references
  free: fixed stack/ring of segment+slot indexes
  completed: fixed ring of terminal segment+slot indexes
  lookup: fixed/open-addressed table for non-hot-path id resolution when needed
}

operationSegment {
  slots: fixed []operationSlot
  generation/epoch
  active count
  retire state
}

operationSlot {
  generation
  internal operation handle
  exported operation id bytes/material
  parent exported/internal references as needed
  pinned context version
  timestamps/status/kind
  fixed telemetry record array
  counters for accepted/dropped records
}
```

The exact Go type names are implementation details. The storage invariants are fixed: bounded memory, preallocated command-side storage, deterministic handle validation through generation checks, no per-record heap allocation, and visible overflow counters. Scaling adds or removes whole segments; it does not resize active slot arrays in place or move active operations.

### Slot lifecycle

Each slot follows one lifecycle state machine:

```text
free -> active -> terminal/enqueued -> worker-owned -> reset/free
```

Failure and timeout paths enter the same terminal/enqueued phase as normal finish, with status fields recording the terminal reason. A no-slot start returns a no-op/skipped handle and must not pin context. If context was pinned before a later start failure, that failure path must release it before returning. A stale handle whose generation does not match the slot generation is ignored and counted as invalid/stale telemetry rather than writing into a reused slot.

Exactly one terminal transition is allowed for each active slot. Double finish/fail/timeout attempts must not enqueue the slot twice, release context twice, or mutate a worker-owned slot. Reset is legal only after worker ownership has completed context release, projection/drop accounting, and slot cleanup.

When an operation starts for a connection, the tracker pins the current connection context version once and stores that immutable version on the operation slot. Later updates to the same connection context create newer versions, but completed-operation drain and projection must continue to resolve the start-time version recorded on the slot. If slot acquisition fails after pinning, the tracker releases the pinned version before returning a skipped/no-slot result.

### Elastic pressure control

The manager may run a background capacity controller that watches cheap shard counters and changes capacity outside the command path. Growth is triggered by sustained free-slot pressure, no-slot skips, completed-ring pressure, record drops, or measured drain lag. Shrink is triggered only after sustained low utilization and only retires fully free segments after a grace period. Active slots are never relocated; handles stay valid until the operation reaches a terminal state and the worker releases the slot.

Elasticity has explicit bounds: minimum segments per shard, maximum segments per shard, growth step, shrink grace period, and pressure windows are configuration/measurement knobs. Segment allocation, zeroing, and release happen in manager/background goroutines. A command goroutine may observe newly installed capacity through the shard free-list, but it must not allocate, resize, or wait for scaling work.

Segment publication must be race-free. Implementations may use a max-size stable segment directory, atomic pointer publication, or shard-lock-only segment lookup during operation start, but the command record path must use a stable slot pointer/index captured in the internal tracker handle. Background shrink marks a segment retiring, removes its free slots from future allocation, waits until active count reaches zero, and then retires the whole segment. It must never mutate or replace storage that an active handle can still address.

If pressure exceeds the configured maximum capacity, telemetry follows the best-effort overflow policy in this ADR rather than blocking command execution. Scaling is a way to reduce drops under sustained pressure, not a guarantee of complete telemetry.

### Sizing and tuning

Sizing uses separate knobs because operation concurrency and records-per-operation are different pressures:

- `trackerShards`: tuned from lock contention, active command concurrency, and shard distribution.
- `minSlotsPerShard`: baseline preallocated slots retained even at low pressure.
- `maxSlotsPerShard`: hard memory bound where telemetry starts dropping/skipping instead of growing further.
- `segmentSize`: number of operation slots added or retired as one elastic unit.
- `recordsPerOperation`: tuned from records-per-operation histograms and expected command/log/context mutation volume.
- `completedRingPerShard`: tuned from finish burst rate and background drain latency.
- `growthPressureWindow` and `shrinkGraceWindow`: tuned so burst traffic does not cause capacity thrashing.

Raw operations per second is not enough by itself. A 400 ns operation needs roughly 40 million operations/second to keep 16 operations concurrently active, while slower TCP/plugin paths can require tens or hundreds of active slots at much lower throughput. The implementation must therefore measure current GoCache command paths and tune the minimum, maximum, and elastic segment size rather than hard-coding a universal small number.

### Overflow and visibility

Telemetry remains best-effort, not durable audit storage. Overflow policy prioritizes command latency:

- no free operation slot: return a no-op/skipped telemetry handle and increment skipped-operation counters;
- per-operation record array full: drop later telemetry records for that operation or use the selected bounded overwrite policy, incrementing dropped-record counters;
- completed queue full: drop the completed telemetry payload or mark it lost with visible counters instead of blocking command execution;
- background projection/GCPC/plugin delivery overload: expose queue drop/gap/health state without making producers inspect subscribers.

Dropped or skipped telemetry must be visible through cheap counters, health/query surfaces, replay-gap metadata, or logs emitted off the command goroutine. Telemetry consumers must not interpret a stream with overflow counters as complete.

### Context release ownership

Pinned context versions from ADR-0032 are released exactly once on every terminal path. Normal finish, failure, timeout, abandon, completed-ring overflow, worker-side drop, and delayed worker drain all share the same ownership rule: either the active slot transfers the context reference to worker-owned terminal processing, or the terminal path releases it before recycling the slot. No path may recycle a slot while a context reference, active snapshot reference, or worker projection reference can still observe its old operation.

### Legacy tracker removal

The legacy map-backed `pkg/operations.Tracker` stops being the canonical lifecycle owner. Existing consumers that need active operations, skipped counts, crashdump snapshots, operation hooks, replay, or plugin query projections must move to `commons/observability` tracker snapshots or worker-materialized views. Transitional adapters are allowed only as temporary server-specific compatibility wiring inside `pkg/`; they must not preserve a second source of truth, create a new `pkg/observability` mechanism, or keep the old context/log-event hot path alive.

## Alternatives Considered

### Alternative 1: Mixed per-shard record ring only

- **Pros**: Closest to the existing `commons/observability` ring shape, simple bounded FIFO draining, and low fixed memory.
- **Cons**: Conflates active operation lifecycle with raw record volume. A noisy operation can evict unrelated operation records, and ring overflow can force early flush/reap before operation finish.
- **Why not**: GoCache needs flush-on-finish/fail/timeout semantics with event-time context ownership. A pure mixed ring makes context lifetime and per-operation completeness harder than necessary.

### Alternative 2: One heap-backed ring per operation

- **Pros**: Natural per-operation ordering and flush-on-finish behavior.
- **Cons**: Allocates or rents a ring for every operation, increasing memory pressure at high concurrency and making pool misses visible on command goroutines.
- **Why not**: The user requirement is zero allocation on the command-executing goroutine. Per-operation storage must already exist inside preallocated slots, not be allocated on demand.

### Alternative 3: Linked list per operation

- **Pros**: Flexible length and simple append semantics.
- **Cons**: Requires per-record node allocation or complex node pooling, pointer chasing, poorer cache locality, and higher GC/escape risk.
- **Why not**: It is the worst fit for GoCache's performance goal. The hot path needs contiguous value writes, not linked node management.

### Alternative 4: Go map of active operations plus `sync.Pool` buffers

- **Pros**: Idiomatic and easier to grow dynamically.
- **Cons**: Map insert/resize and pool misses can allocate on operation start. `sync.Pool` is a cache, not a capacity guarantee, and GC may clear it.
- **Why not**: The command-side allocation contract requires fixed capacity and deterministic memory ownership. Maps and pools may still be useful off the hot path, but not as the core command-path storage.

### Alternative 5: Keep legacy `pkg/operations.Tracker` as lifecycle registry

- **Pros**: Preserves existing crashdump, plugin query, hook replay, and logging integrations during transition.
- **Cons**: Keeps the old source of truth, operation object model, context/log-event coupling, and synchronous projection assumptions.
- **Why not**: The accepted OperationTracker direction requires `pkg/operations/tracker.go` to stop existing as the canonical lifecycle path. Keeping it would make the new storage model an extra telemetry sink instead of the operation lifecycle owner.

### Alternative 6: Block command goroutines when telemetry storage is full

- **Pros**: Reduces telemetry loss and simplifies consumer reasoning.
- **Cons**: Violates the modular performance budget and lets telemetry overload become command latency.
- **Why not**: GoCache telemetry is best-effort. Backpressure belongs to future durable/audit systems, not the command-side telemetry path.

## Consequences

### Positive

- Makes the command-side telemetry path measurable as `0 B/op` and `0 allocs/op`.
- Keeps per-operation ordering and flush-on-finish semantics without linked-list or per-operation ring allocation.
- Gives GoCache explicit capacity knobs for shards, active slots, record volume, and completed-operation backlog.
- Replaces the legacy active-operation tracker with the commons OperationTracker slot table as the single source of truth.
- Keeps expensive projection, GCPC/protobuf construction, event/log materialization, and subscriber/plugin fanout off command goroutines.
- Preserves plugin boundaries from ADR-0033: reusable primitives in `commons/observability`, server projection in `pkg/`, public contracts in `api/observability`.

### Negative

- Elastic capacity requires measurement, configuration, and overload visibility; incorrect bounds can still drop telemetry or retain too much memory.
- Preallocated minimum slots increase baseline memory usage even when telemetry volume is low.
- Active operation snapshot/projection code must be rewritten around shard slots and worker-owned materialized views.
- The implementation is more specialized than idiomatic map/slice storage and therefore needs stronger tests and benchmarks.
- Completion queue overflow requires careful context-version release so dropped completed telemetry does not leak references.
- Segment publication, retirement, and active snapshot projection require explicit race tests because background controllers can otherwise invalidate command-side handles.

### Risks

- **Risk**: Hidden allocations enter through identity rendering, interfaces, variadics, map growth, or escaping records. **Mitigation**: Add allocation benchmarks for start/record/finish/no-slot/overflow paths and treat non-zero command-side allocation as a regression.
- **Risk**: Shard mutex contention still appears in command latency at operation start or finish. **Mitigation**: Benchmark lock profiles under realistic concurrency, tune `trackerShards`, and only consider lock-free free-list/completion indices if measurements justify the complexity.
- **Risk**: Elastic scaling thrashes under bursty traffic or accidentally moves active operations. **Mitigation**: Grow and shrink whole segments from a background controller, never relocate active slots, use grace windows, and benchmark pressure scenarios before enabling aggressive shrink.
- **Risk**: Slot reuse corrupts background processing if records or context references are reset before workers finish. **Mitigation**: Use explicit terminal states, generation checks, worker ownership rules, and tests that delay worker drains while new operations reuse slots.
- **Risk**: Interface-dispatched APIs or helper callbacks look value-only but still allocate through escaping records or variadic arguments. **Mitigation**: Benchmark both concrete and interface paths and inspect escape analysis for hot APIs before claiming zero allocation.
- **Risk**: Context versions leak when operations are dropped, abandoned, or completion queues overflow. **Mitigation**: Every terminal path must release or transfer the pinned context reference exactly once; tests must cover normal finish, fail, timeout, no-slot, and completed-queue-full paths.
- **Risk**: Telemetry drops are mistaken for complete observability. **Mitigation**: Expose skipped-operation, dropped-record, dropped-completed-operation, and projection queue counters through health/query surfaces and gap metadata.
- **Risk**: Transitional code preserves `pkg/operations.Tracker` as a second lifecycle owner. **Mitigation**: Limit adapters to temporary `pkg/` wiring and remove legacy tracker responsibilities as part of the implementation gate.
