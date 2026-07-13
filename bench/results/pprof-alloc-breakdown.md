# Alloc-Site Breakdown: BenchmarkPluginCommandRTT_NetPipe

## Metadata

- Benchmark: `BenchmarkPluginCommandRTT_NetPipe` (`pkg/plugin/router/router_bench_test.go:86`)
- Profile method: `-memprofile` with `-memprofilerate=1`, `-benchtime=10000x`
- Date: 2026-07-07
- Go version: `go version go1.26.4-X:nodwarf5 linux/amd64`
- Total allocs/op: 36 allocs/op
- Total B/op: 3278 B/op
- CPU: AMD Ryzen 9 7900X 12-Core Processor

Exact command requested by FR-009:

```bash
go test -bench=BenchmarkPluginCommandRTT_NetPipe -benchmem -memprofile=/tmp/alloc-breakdown.prof -memprofilerate=1 -benchtime=10000x ./pkg/plugin/router/
go tool pprof -alloc_objects -top /tmp/alloc-breakdown.prof
go tool pprof -alloc_space -top /tmp/alloc-breakdown.prof
```

The exact command produced:

```text
BenchmarkPluginCommandRTT_NetPipe-24    10000    92169 ns/op    3280 B/op    36 allocs/op
```

Because the exact command also runs package tests before the benchmark, the profile contained a small amount of non-benchmark setup/test allocation. For cleaner attribution, I also ran the same benchmark with tests disabled and used that profile for the site breakdown below:

```bash
go test '-run=^$' -bench=BenchmarkPluginCommandRTT_NetPipe -benchmem -memprofile=/tmp/alloc-breakdown-benchonly.prof -memprofilerate=1 -benchtime=10000x ./pkg/plugin/router/
go tool pprof -alloc_objects -top /tmp/alloc-breakdown-benchonly.prof
go tool pprof -alloc_space -top /tmp/alloc-breakdown-benchonly.prof
```

The bench-only command produced:

```text
BenchmarkPluginCommandRTT_NetPipe-24    10000    91898 ns/op    3278 B/op    36 allocs/op
```

Notes on attribution:

- `pprof` merged the 36 reported allocs/op into 14 benchmark-relevant top-level flat allocation sites plus smaller dropped/runtime nodes. The top object profile showed `323998` objects, 99.32% of `326217` profile objects, not 36 separately named source lines.
- Per-site `Allocs/op` and `Bytes/op` below are normalized from the bench-only profile over 10,000 iterations and rounded. The classification summary uses the benchmark's 36 allocs/op and 3278 B/op as the total.
- Optimization targets are sorted by bytes, not object count.

## Classification Summary

| Category | Sites | Status |
|----------|-------|--------|
| Irreducible | 10 | Unchanged (channels, protobuf decode objects) |
| Reducible (Phase 1) | 0 | ELIMINATED (collectBatchWithDelay batch slice) |
| Poolable (Phase 2) | 0 | ELIMINATED (SendBatch frame buffer + Recv payload buffer) |
| Poolable DEFERRED | 1 | Still allocates ~75 B/op (MarshalVT, generated code) |

## Post-Optimization Results

The WriteLoop buffer reuse optimization in `pkg/plugin/router/router.go` reuses `batchBuf []outboundEnvelope` and `envsBuf []*gcpc.EnvelopeV1` with `clear()` plus `[:0]` reset after each `writeOutboundBatch` call. The buffers are struct fields on the heap-allocated `PluginConn`, but they remain single-owner state inside the write-loop goroutine, so reuse does not add a new heap allocation path.

Measured on 2026-07-10 (`go1.26.4`, AMD Ryzen 9 7900X), the bench-only rerun of `BenchmarkPluginCommandRTT_NetPipe` improved from:

```text
3278 B/op, 36 allocs/op
```

to:

```text
1487 B/op, 35 allocs/op
```

Delta: `1791 B/op` eliminated (`54.7%` reduction), `1 alloc/op` eliminated.

Escape analysis proof: `go build -gcflags="-m" ./pkg/plugin/router/` confirms the `batchBuf` / `envsBuf` `[:0]` resets are self-assignments, not fresh allocations.

### Phase 2 transport buffer pooling follow-up

The transport layer in `commons/transport/transport.go` now reuses per-connection `writeBuf []byte` and `readBuf []byte`. `SendBatch` marshals directly into the reusable write buffer under `c.mu.Lock`, removing the intermediate frames slice and the per-call frame buffer. `Recv` grows a reusable read buffer by capacity and resets it with `clear()` + `[:0]` so the payload path no longer allocates a fresh `[]byte` on each call. The 4-byte header buffer remains a `make()` allocation because it is too small to pool, and `MarshalVT` stays deferred to generated-code regeneration.

Measured on 2026-07-10 (`go1.26.4`, AMD Ryzen 9 7900X), the bench-only rerun of `BenchmarkPluginCommandRTT_NetPipe` improved from:

```text
1487 B/op, 35 allocs/op
```

to:

```text
1330 B/op, 31 allocs/op
```

Delta: `157 B/op` eliminated and `4 alloc/op` eliminated. Cumulative reduction versus the original baseline is `1948 B/op` (`59.4%`) and `5 allocs/op`.

### Reclassified alloc sites

The former `collectBatchWithDelay` site is completely eliminated from the post-optimization profile. Phase 2 eliminated the transport-side `SendBatch` frame buffer and `Recv` payload buffer, leaving only irreducible sites plus the deferred protobuf serializer buffer:

1. `(*PluginConn).Send` — 2471 kB (channels — irreducible, pooling rejected as unsafe)
2. `(*Conn).Recv` — 2352 kB (envelope decode — irreducible)
3. `(*EnvelopeV1).UnmarshalVT` — 1912 kB (protobuf decode — irreducible)
4. `NewCommandRequest` — 1833 kB (wire protocol — irreducible)
5. `(*EnvelopeV1).MarshalVT` — 733 kB (serialization buffer — DEFERRED, generated code)

| Category | Sites (before) | Sites (after) | Allocs/op (after) | Bytes/op (after) |
|----------|----------------|---------------|-------------------|------------------|
| Irreducible | 10 | 10 | ~30 | ~1215 B |
| Reducible (eliminated) | 1 | 0 | 0 | 0 B |
| Poolable (Phase 2 eliminated) | 3 | 0 | 0 | 0 B |
| Poolable deferred | 0 | 1 | ~1 | ~75 B |
| **Total** | **14** | **11** | **31** | **~1330 B** |

Deferred optimizations remain the same:

1. `MarshalVT` serialization buffer (~75 B/op) — generated protobuf code, regeneration risk
2. Channel pooling — permanently rejected (channels escape to callers, unsafe)

## Alloc Sites (sorted by bytes)

### 1. REDUCIBLE `(*PluginConn).collectBatchWithDelay`

- Location: `pkg/plugin/router/router.go:197`
- Allocs/op: ~1.02
- Bytes/op: ~1828 B
- Evidence: `pprof -alloc_space` shows `17851.75kB` flat in `collectBatchWithDelay`, all at `batch := make([]outboundEnvelope, 0, pluginOutboundBatchMax)`; `pprof -alloc_objects` shows `10201` flat objects at the same line.
- Classification reason: This is an internal writer-loop batch buffer. It does not escape through the public API, and the batch capacity is a stable constant (`pluginOutboundBatchMax = 32`). It was eliminated by reusing the internal writer-loop buffers (`batchBuf` and `envsBuf`) without changing the `Send` API or GCPC wire contract.

### 2. IRREDUCIBLE `(*PluginConn).Send` response channel

- Location: `pkg/plugin/router/router.go:358`
- Allocs/op: ~2.04
- Bytes/op: ~123 B
- Evidence: `pprof -alloc_objects` shows `20402` objects at `ch := make(chan *gcpc.EnvelopeV1, 1)`; `pprof -alloc_space` shows `1.17MB` at that line.
- Classification reason: `Send` returns a response channel to the caller. Pooling or reusing this channel is rejected as unsafe because ownership escapes to callers and the receive lifecycle is caller-controlled.

### 3. IRREDUCIBLE `(*PluginConn).Send` write-ack channel

- Location: `pkg/plugin/router/router.go:361`
- Allocs/op: ~2.04
- Bytes/op: ~128 B
- Evidence: `pprof -alloc_objects` shows `20402` objects at `errCh := make(chan error, 1)`; `pprof -alloc_space` shows `1.25MB` at that line.
- Classification reason: This channel is the per-send writer acceptance/error synchronization path. Removing it would change the current `Send` semantics, and pooling channel instances would be unsafe because the write loop and caller-side send path have distinct ownership windows.

### 4. IRREDUCIBLE `(*Conn).Recv` envelope object

- Location: `commons/transport/transport.go:108`
- Allocs/op: ~2.04
- Bytes/op: ~160 B
- Evidence: `pprof -alloc_objects` shows `20402` objects at `env := &gcpc.EnvelopeV1{}`; `pprof -alloc_space` shows `1.56MB` at that line.
- Classification reason: `Recv` returns the decoded `*gcpc.EnvelopeV1` to callers. The object escapes by design, so it cannot be safely reused by the transport without introducing ownership or lifetime hazards.

### 5. IRREDUCIBLE `(*EnvelopeV1).UnmarshalVT` command-request payload struct

- Location: `api/gcpc/v1/gcpc_vtproto.pb.go:6347` and `api/gcpc/v1/gcpc_vtproto.pb.go:6351`
- Allocs/op: ~2.04
- Bytes/op: ~106 B
- Evidence: `pprof -alloc_space` shows `958.59kB` for `v := &CommandRequestV1{}` and `79.88kB` for `m.Payload = &EnvelopeV1_CommandRequest{CommandRequest: v}`.
- Classification reason: These are generated protobuf oneof/message objects required to materialize the incoming GCPC command-request envelope.

### 6. IRREDUCIBLE `NewCommandRequest`

- Location: `api/gcpc/v1/helpers.go:142`, `api/gcpc/v1/helpers.go:145`, `api/gcpc/v1/helpers.go:146`
- Allocs/op: ~3.06
- Bytes/op: ~188 B
- Evidence: `pprof -alloc_objects` shows `30603` flat objects in `NewCommandRequest`; `pprof -alloc_space` shows `796.95kB` for `&EnvelopeV1`, `79.70kB` for `&EnvelopeV1_CommandRequest`, and `956.34kB` for `&CommandRequestV1`.
- Classification reason: The request envelope and protobuf oneof/message structs are required for the GCPC command request wire contract.

### 7. IRREDUCIBLE `(*CommandRequestV1).UnmarshalVT`

- Location: `api/gcpc/v1/gcpc_vtproto.pb.go:8522`, `8554`, `8590`
- Allocs/op: ~2.91
- Bytes/op: ~177 B
- Evidence: `pprof -alloc_objects` shows `29061` flat objects in `CommandRequestV1.UnmarshalVT`; line attribution includes `RequestId` string copy plus nested `CommandInfoV1` and `ConnectionInfoV1` message allocation.
- Classification reason: The generated protobuf decoder must materialize strings and nested messages for the received command request. Eliminating these would require changing decode semantics or generated-code strategy.

### 8. IRREDUCIBLE `NewCommandResponse` and responder result construction

- Location: `api/gcpc/v1/helpers.go:161`, `api/gcpc/v1/helpers.go:162`, and `pkg/plugin/router/router_bench_test.go:72`
- Allocs/op: ~4.08
- Bytes/op: ~172 B
- Evidence: `pprof -alloc_space` shows `876.65kB` flat in `NewCommandResponse`; the benchmark responder closure also contributes `796.95kB` flat at `startMockPluginResponder.func1`, where it constructs the `ResultV1` response payload.
- Classification reason: The benchmark simulates a real plugin returning a GCPC command response. The response envelope, oneof/message wrappers, and `ResultV1` payload are part of the wire protocol being measured.

### 9. ELIMINATED `(*Conn).SendBatch` frame buffer

- Location: `commons/transport/transport.go:66`
- Allocs/op: ~2.04 before Phase 2; 0 after Phase 2
- Bytes/op: ~82 B before Phase 2; 0 after Phase 2
- Evidence: Phase 2 moved frame assembly into reusable per-connection `writeBuf`, so the per-call `make([]byte, total)` allocation disappeared from the profile.
- Classification reason: The frame buffer was safe to reuse inside the owning connection once `SendBatch` stopped building a separate intermediate frames slice.

### 10. IRREDUCIBLE `(*CommandResponseV1).UnmarshalVT`

- Location: `api/gcpc/v1/gcpc_vtproto.pb.go:8931`, `8963`, `8965`
- Allocs/op: ~2.03
- Bytes/op: ~81 B
- Evidence: `pprof -alloc_objects` shows `20274` flat objects in `CommandResponseV1.UnmarshalVT`; `pprof -alloc_space` shows `796.95kB` flat with nested `ResultV1` decode contributing to the cumulative total.
- Classification reason: The response decoder must materialize the request ID string and nested result message for the response envelope.

### 11. POOLABLE `(*EnvelopeV1).MarshalVT` serialization buffer

- Location: `api/gcpc/v1/gcpc_vtproto.pb.go:28`
- Allocs/op: ~2.04
- Bytes/op: ~75 B
- Evidence: `pprof -alloc_objects` shows `20402` objects at `dAtA = make([]byte, size)`; `pprof -alloc_space` shows `732.96kB` at that line.
- Classification reason: The serialized protobuf byte slice is short-lived and immediately copied into the transport frame buffer. A transport/protobuf buffer-pool design could reduce this, but it must preserve generated-code ownership expectations.

### 12. POOLABLE `(*Conn).Recv` payload buffer

- Location: `commons/transport/transport.go:103`
- Allocs/op: ~2.04
- Bytes/op: ~75 B
- Evidence: `pprof -alloc_objects` shows `20402` objects at `data := make([]byte, size)`; `pprof -alloc_space` shows `732.96kB` at that line. The same function also shows a much smaller header allocation at line 90.
- Classification reason: The payload byte slice is a short-lived decode buffer. It is a plausible pool candidate if the decoder does not retain references into the slice after unmarshal.

### 13. IRREDUCIBLE `NextRequestID`

- Location: `pkg/plugin/router/router.go:41`
- Allocs/op: ~0.89
- Bytes/op: ~14 B
- Evidence: `pprof -alloc_objects` shows `8897` objects at `return string(b)`; `pprof -alloc_space` shows `139.02kB` at that line.
- Classification reason: The public request correlation path currently uses string IDs in both `Send` and GCPC request/response messages. Avoiding this allocation would require changing the request-ID representation or broader correlation contract.

### 14. IRREDUCIBLE `(*ConnectionInfoV1).UnmarshalVT` string copy

- Location: `api/gcpc/v1/gcpc_vtproto.pb.go:8292`
- Allocs/op: ~0.87
- Bytes/op: ~14 B
- Evidence: `pprof -alloc_objects` shows `8737` flat objects in `ConnectionInfoV1.UnmarshalVT`; `pprof -alloc_space` shows `136.52kB` at `m.Id = string(dAtA[iNdEx:postIndex])`.
- Classification reason: The generated decoder copies wire bytes into an owned Go string for the decoded connection ID.

## Optimization Targets (by bytes saved, descending)

1. `(*EnvelopeV1).MarshalVT` serialization buffer — potential savings: ~75 B/op if marshaling can target pooled/reused buffers safely. DEFERRED: this is generated protobuf code; pooling requires modifying the generation contract or wrapping MarshalVT with a pool-managed scratch buffer.

The following sites were eliminated by the WriteLoop buffer reuse and transport buffer pooling optimizations:
- `(*PluginConn).collectBatchWithDelay` batch slice (~1828 B/op) — eliminated by Phase 1 (reusable batchBuf)
- `(*PluginConn).writeOutboundBatch` envs slice (~32 B/op) — eliminated by Phase 1 (reusable envsBuf)
- `(*Conn).SendBatch` frame buffer (~82 B/op) — eliminated by Phase 2 (reusable writeBuf)
- `(*Conn).SendBatch` frames intermediate slice — eliminated by Phase 2 (single-pass marshal)
- `(*Conn).Recv` payload buffer (~75 B/op) — eliminated by Phase 2 (reusable readBuf)

Cumulative result: 3278 B/op → 1330 B/op (59.4% reduction), 36 → 31 allocs/op.

Channel count reductions are intentionally excluded: channel pooling is unsafe under the current API (channels escape to callers).

## Channel Allocs (MUST be irreducible)

- `pkg/plugin/router/router.go:358` — `ch := make(chan *gcpc.EnvelopeV1, 1)`: response channel returned by `Send`; pooling rejected as unsafe because `Send` returns channels to callers.
- `pkg/plugin/router/router.go:361` — `errCh := make(chan error, 1)`: write-ack channel used to synchronize accepted/failed writes for each blocking send; pooling rejected as unsafe because lifecycle overlaps the per-send writer path and would risk stale sends/receives.

## Bottom Line

The two largest byte targets — the `collectBatchWithDelay` batch slice (~1.8 KiB/op) and the transport frame/payload buffers (~157 B/op combined) — are now eliminated by the WriteLoop buffer reuse and transport buffer pooling optimizations. The cumulative result is 3278 B/op → 1330 B/op (59.4% reduction). The channel allocations remain irreducible under the current `Send` contract. The only remaining poolable site is `MarshalVT` (~75 B/op), which is deferred due to generated-code regeneration risk.
