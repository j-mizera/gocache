---
title: Go-vs-docker bench gap
description: pprof-attributed audit explaining why Go bench shows +18% while docker shows +3% on the same code — runtime.selectgo park/unpark, not syscall cost
status: stable
last_updated: 2026-05-03
related:
  - Performance
  - Audit-per-shard-arc-summary
---

# Go bench vs docker bench gap — investigation

**Issue:** [#45](https://github.com/j-mizera/gocache/issues/45)
**Date:** 2026-05-03
**Outcome:** The gap between Go bench's +18% pipelined-GET gain (from #34's per-shard locking) and docker bench's +3% on the same workload is **mostly scheduler overhead, not syscall cost**. Channel-hop relief from sharding was happening (engine goroutines were 89% idle in docker), but the time saved was reabsorbed by goroutine park/unpark cycles caused by docker's higher TCP latency. **The gap is not fixable in code** — it's the structural cost of running through veth + iptables NAT vs Go's loopback. **Note:** The channel dispatch analyzed here was subsequently replaced by direct shard mutex (ADR-0010), eliminating the `sendAndWait` / `selectgo` overhead entirely.

## Method

1. Added `pprof` build tag to `cmd/server` (compiled in via `--build-arg PPROF=1` on `Dockerfile`) that exposes `net/http/pprof` on `:6060` plus enables block + mutex profiling at the same rates the Go bench harness uses (`SetBlockProfileRate(10000)`, `SetMutexProfileFraction(10)`).
2. Ran a 30M-iteration pipelined GET workload (50 connections, P=10) against the pprof-enabled gocache image at the same `--cpuset-cpus 0-3` / `--memory 2g` limits as the regular bench.
3. Captured CPU (25s), block, mutex, and heap profiles via `curl localhost:6060/debug/pprof/*` mid-workload.
4. Compared attribution to the Go-bench profile from #38 (mixed pipelined GetSet at N=16).

## CPU attribution — direct comparison

| Metric | Go bench (#38) | Docker (this) | Δ |
|---|---:|---:|---:|
| Total CPU samples / wall-time | 54.85s / 20.6s ≈ 2.66 cores | 74.61s / 25.1s ≈ 2.97 cores | docker uses slightly more |
| `handleConnection` cum | 55.68% | 78.27% | +22.6 pp |
| `Syscall6` flat | 25.93% | 19.47% | -6.5 pp (less syscall %!) |
| `bufio.Writer.Flush` cum | 19.05% | 14.64% | -4.4 pp |
| `Reader.Read` cum | 18.76% | 18.75% | flat |
| **`runtime.selectgo` cum** | **18.74%** | **31.30%** | **+12.6 pp (more!)** |

The cleanest signal: **`runtime.selectgo` consumes 12.6 percentage points MORE in docker than in Go bench** — the opposite of what a "syscall is slower in docker" hypothesis would predict. Syscall percentage is actually *smaller* in docker.

## What `runtime.selectgo` is doing

`runtime.selectgo` is the Go runtime's select-statement arbitration. Three select sites in the gocache hot path (per pipelined batch of 10 commands):

1. **Connection goroutine ↔ shard engine goroutine** — `sendAndWait` selected on `cmdChan <- Command` then `<-resChan`. Two select calls per dispatched command. *(Since removed by ADR-0010 — replaced with direct shard mutex.)*
2. **Engine goroutine** — `shardEngine.run` selected on `<-cmdChan / <-stopChan` per command processed. *(Since removed — no per-shard goroutines.)*
3. **Connection acceptor + various server-internal select sites** — accept loop, shutdown signaling, worker tickers.

In Go bench (loopback): pipelined batches arrived back-to-back. Connection goroutines stayed active, engine goroutines drained `cmdChan` continuously. Few park/unpark cycles per second.

In docker (veth + iptables NAT): pipelined batches took longer to round-trip end-to-end. Connection goroutines parked between batches more often. Engine goroutines parked between batches more often. **Each park/unpark involved runtime.selectgo arbitration.**

## Block profile — engines are mostly idle

| Source | Block time | % of total |
|---|---:|---:|
| `shardEngine.run` (engine goroutines parked on empty cmdChan) *(since removed)* | 600.58s | 88.89% |
| `CleanupWorker.Start.func1` (worker tick) | 60.06s | 8.89% |
| **Connection-side `Engine.DispatchToShard` wait** | **15s** | **2.22%** |

Connection-side wait is **only 2.2% of total block time**. The architectural relief from per-shard locking (#34) IS happening — connections are not blocked waiting for engines. **The bottleneck is not the engine queue.**

## Mutex profile — channel-hop still dominant share of contention

| Source | Mutex time | % of total |
|---|---:|---:|
| `Engine.DispatchToShard` cum | 6.53s | 44% |
| Other (cleanup, shard lock, ...) | 8.31s | 56% |

Mutex contention from the channel-hop pattern (`runtime.unlock` reached via `runtime.selectgo` → `selunlock`) was 44%. Not as dominant as in pre-#34 (where it was 55-67%), but still the largest single contributor. *(This contention source was eliminated by ADR-0010's direct shard mutex.)*

## Heap attribution — workload hot allocations

`alloc_space` over 25 seconds of capture, ~17M commands processed:

| Source | Cumulative alloc | Per-cmd estimate |
|---|---:|---:|
| `gocache/api/operations.New` + `nextID` + `WithContext` | 3.97 GB | ~233 B/cmd |
| `resp.(*Reader).Read` (parsing + array slice) | 2.09 GB | ~123 B/cmd |
| `context.WithValue` | 486 MB | ~28 B/cmd |
| `resp.(*Reader).readBulkString` | 397 MB | ~23 B/cmd |
| `HandleGet` | 356 MB | ~21 B/cmd |
| **Total** | **~7.3 GB** | **~430 B/cmd** |

Each pipelined GET command churns roughly **430 bytes of heap allocation**. The `*ops.Operation` per command is ~163 bytes and is the single biggest contributor (38% of allocation pressure on the hot path).

In-use heap at snapshot time: only **2.26 MB live data** (per-shard slab arenas) + 1.16 MB of pprof itself = ~3.4 MB. The rest of the 220 MB RSS is GC overhead from the alloc churn.

## Why this explains the gap

The Go-bench gain on `BenchmarkTCP_GET_Pipelined` was +18% from N=1 → N=16 sharding. The relief targets `runtime.selectgo` (channel arbitration). In Go-bench numbers:

- N=1 has high channel contention because all 52 connections push through 1 cmdChan
- N=16 partitions across 16 cmdChans → less arbitration per cmdChan
- Saving = X ns per command

In docker:

- N=1 → N=16 saves the same X ns of arbitration per command (same code path)
- But per-command total time is HIGHER in docker because of veth round-trip
- Plus selectgo grew its share (18.74% → 31.30%) because of MORE park/unpark from inter-batch gaps
- Net relative gain: smaller percentage of a bigger denominator

**The architectural relief is real and measurable in both tiers.** The docker bench underrepresents it because:
1. The denominator (total per-command time) is larger
2. The numerator (saved arbitration time) is the same absolute value, smaller as a percentage

## Conclusions

1. **The Go-bench-vs-docker-bench gap is structural, not fixable in our code.** Veth + iptables NAT adds per-pipeline-batch latency that isn't a syscall cost (Linux `Syscall6` is cheap on both tiers) but a scheduler-park-frequency cost.

2. **Future architectural arcs should report both Go bench AND docker bench numbers, with the understanding that they answer different questions:**
   - Go bench measures "did the architectural change reduce the bottleneck?" — useful for proving a mechanism works
   - Docker bench measures "what gain do users see in production?" — useful for sizing user-visible improvements
   - The two can disagree by 5-10× in relative-percentage terms; this is the nature of the measurement, not a bug

3. **Acceptance criteria denominated in docker rps numbers must be measured in docker.** #34's "≥ 900k pipelined GET" target was set against pre-#34 docker baselines; only docker numbers can certify against it. Go bench can't substitute.

4. **Allocation churn is a separate concern that affects both tiers.** Each GET command churns ~430 bytes of heap (mostly `*ops.Operation` + RESP parsing). In a tight inner loop this contributes meaningfully to GC pressure. Reducing per-command allocation is a separate lever that would benefit both tiers; not investigated here.

## Files

- `cmd/server/pprof_on.go` (new, 38 lines) — build-tag-gated pprof endpoint + runtime profile rates
- `Dockerfile` (4 lines added) — `PPROF` build-arg → adds `pprof` build tag
- `bench/redis-benchmark/run.sh` (~10 lines) — pass `PPROF=1` through to docker build, expose port 6060
- `bench/results/perf-mset-locality-and-bench-gap/profiles/docker-pipelined-get/` — captured profiles (cpu, block, mutex, heap)
- `docs/audits/go-bench-vs-docker-gap.md` (this file)

## Reproduce

```bash
PPROF=1 REBUILD=1 sg docker -c "bash bench/redis-benchmark/run.sh pprof-test --target gocache"
sg docker -c "docker run -d --name gocache-pprof --network gocache-bench-net --cpuset-cpus 0-3 --memory 2g -p 6060:6060 gocache:pprof --config= --address 0.0.0.0 --port 6379 --load-on-startup=false --log-level warn"
sg docker -c "docker run -d --rm --name load --network gocache-bench-net --cpuset-cpus 4-7 valkey/valkey:8 valkey-benchmark -h gocache-pprof -p 6379 -n 30000000 -c 50 -r 100000 -P 10 -t get -q"
sleep 3
curl -s -o cpu.prof "http://localhost:6060/debug/pprof/profile?seconds=25"
curl -s -o block.prof "http://localhost:6060/debug/pprof/block"
curl -s -o mutex.prof "http://localhost:6060/debug/pprof/mutex"
go tool pprof -top -cum cpu.prof
```
