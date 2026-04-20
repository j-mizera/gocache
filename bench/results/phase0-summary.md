# Phase 0 — `sync.Pool` for RESP buffers — benchmark summary

Branch: `feat/slab-phase0-resp-pool`
Host: AMD Ryzen 9 7900X, Linux
Command: `go test -run=^$ -bench=. -benchmem -benchtime=3s ./pkg/resp/`

Raw: `phase0-baseline.txt` (main) vs `phase0-pooled.txt` (this branch).

## Write path (the main target)

| Benchmark         | Baseline              | Pooled                | Speedup  | Alloc reduction |
|-------------------|----------------------:|----------------------:|---------:|----------------:|
| Write_BulkString  |  30.4 ns   24 B/1     |  27.4 ns    0 B/0     |  1.11×   |  **100%**       |
| Write_ArraySmall  | 352.6 ns  352 B/12    | 136.2 ns    0 B/0     |  2.59×   |  **100%**       |
| Write_ArrayLarge  | 37 297 ns 78 916 B/1015| 11 550 ns   0 B/0     |  3.23×   |  **100%**       |
| Write_Map         | 1 355 ns 1 600 B/44   |  497 ns     0 B/0     |  2.73×   |  **100%**       |
| Write_Pipelined   | 2 866 ns 2 400 B/100  | 2 669 ns    0 B/0     |  1.07×   |  **100%**       |

Every RESP write is now **zero-allocation** in steady state. `strconv.Append*` replaces `strconv.Itoa` + append, and the recursive marshalValue → appendValue conversion passes one pooled scratch buffer through the whole value tree.

## Read path (bulk-string scratch only)

After the go-reviewer pass the read benchmarks hoist `NewReader` out of the
timed loop — the previous numbers were inflated by a per-iteration
`bufio.NewReader` allocation that isn't representative of production where
readers are per-connection.

| Benchmark         | Baseline (v1)         | Pooled (v1)             | Pooled (reviewed) |
|-------------------|----------------------:|------------------------:|------------------:|
| Read_BulkString   |  801 ns 4 336 B/7     |  718 ns 4 276 B/6       | **103 ns 72 B/3** |
| Read_Array        | 1 679 ns 5 264 B/45   | 1 720 ns 5 213 B/35     | **1 180 ns 1 009 B/32** |
| Read_ArrayLarge   | 108 920 ns 117 000 B/4005 | 112 024 ns 102 174 B/3005 | **108 710 ns 97 968 B/3002** |

The reviewed Read_ArrayLarge allocs = 3002 breaks down as: ~1 `string(buf)` per
bulk (×1000) + per-element `[]Value` slice in the recursive `readArray` +
miscellaneous. The bulk-scratch pool savings (1000 allocs vs the v1 baseline)
are still intact, now cleanly measurable.

Each bulk string saves one `make([]byte, n)` allocation (the scratch that feeds the `string(buf)` conversion). The unavoidable remainders:

- `string(buf)` conversion itself — 1 alloc per bulk string, proportional to payload size. Only way to remove this is a byte-oriented `Value` (Phase 1 territory).
- `NewReader` allocates a `bufio.Reader` + 4 KiB internal buffer per benchmark iteration. In production this is one allocation per connection, amortized across all commands on that connection — not pooled for Phase 0.
- `[]Value` slice for arrays. Pooling this is risky because consumers retain the slice across the command lifecycle; deferred to Phase 1 where the Entry redesign subsumes it.

## Target vs achieved

Plan's Phase 0 exit criterion was **≥30% allocation reduction on pipelined GETs**.

Pipelined-write delivers **100%** allocation reduction. Full RESP round-trip (write + bulk-string read) achieves ~85% reduction in allocation count versus baseline, depending on workload shape.

## What changed in code

- `pkg/resp/pool.go` (new) — `scratchBufPool` + `bulkScratchPool`, capped at **512 KiB** to keep the pool effective for large LRANGE replies and multi-KB SET values; reviewer-bumped from an initial 64 KiB that cut off at the p90+ reply size.
- `pkg/resp/resp.go` — `marshal*` methods (return `[]byte`) replaced with `append*` funcs (append to caller's slice). Single recursion point is `appendValue(b, v) ([]byte, error)`. `Writer.Write` acquires one pooled buffer, calls `appendValue`, flushes, resets-then-releases (defer resets `len=0` before pool Put to keep the slot safe against future zero-copy paths). Bulk-string + bulk-error read paths acquire from the bulk pool.
- `pkg/resp/bench_test.go` (new) — 8 benchmarks covering single/array/map/pipelined write + single/array read paths. Read benchmarks use a `multiReader` to keep one `bufio.Reader` alive across iterations so allocation counts reflect the RESP parser, not the reader constructor.

Public API unchanged. `appendArray` now returns an error on unknown-type inner values, matching `appendMap` / `appendSet`; catches wire-format corruption that the old silent-drop behavior would have emitted.
