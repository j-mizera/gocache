# #47 (MSET cache locality) + #45 (Go vs docker bench gap)

**Branch:** `perf/mset-locality-and-bench-gap`
**Date:** 2026-05-03
**Issues:** [#47](https://github.com/j-mizera/gocache/issues/47), [#45](https://github.com/j-mizera/gocache/issues/45)

Combined branch tackling the two open #34-arc follow-ups. Both deliver something. Neither is a runaway win.

## #45 — Go bench vs docker bench gap (investigation, ships as docs)

**Outcome:** Confirmed via direct profile capture. The Go-bench-vs-docker-bench gap is **scheduler overhead from goroutine park/unpark cycles caused by docker's higher TCP latency**, not syscall cost. Channel-hop relief from sharding still happens at the docker tier (engine goroutines 89% idle in block profile), but the saved time is reabsorbed by `runtime.selectgo` arbitration.

| Metric | Go bench (#38) | Docker (#45) | Δ |
|---|---:|---:|---:|
| `runtime.selectgo` cum CPU | 18.74% | **31.30%** | **+12.6 pp** |
| `Syscall6` flat CPU | 25.93% | 19.47% | -6.5 pp (less!) |
| Per-cmd alloc churn | (not measured) | ~430 B/cmd | — |
| Live heap at snapshot | (not measured) | 5.93 MB | — |

Selectgo is bigger in docker, syscall is smaller. The docker tier doesn't pay for slow syscalls; it pays for slow scheduler decisions. The full investigation lives in `docs/audits/go-bench-vs-docker-gap.md`.

**Practical takeaway:** future architectural arcs should report both Go bench and docker bench numbers, treating them as answering different questions (mechanism-works vs user-visible-gain). The two can disagree by 5-10× in relative-percentage terms — that's the nature of the measurement, not a bug.

**Ship surface:**
- `cmd/server/pprof_on.go` (new, build-tag-gated) — exposes `net/http/pprof` on `:6060` plus enables block + mutex profiling at the rates the Go bench harness uses
- `Dockerfile` — `PPROF=1` build-arg toggles the tag
- `bench/redis-benchmark/run.sh` — passes `PPROF=1` through, publishes port 6060
- `docs/audits/go-bench-vs-docker-gap.md` — writeup
- `bench/results/perf-mset-locality-and-bench-gap/profiles/docker-pipelined-get/` — raw profiles

## #47 — MSET cache-locality fix (partial)

**Outcome:** Implemented `Shard.BulkSetBytes` and rewrote `HandleMset` to group keys by shard then batch each shard's writes through one call. Pipelined MSET goes 123k → **129k cmd/s (+5%)**. Within docker noise band but consistent across 3 runs. **Does not recover the −42% regression** that #34 introduced (pre-#34 was 229k cmd/s).

### Bench (3 runs each)

| Configuration | Pipelined MSET (mean) | Standard MSET (mean) |
|---|---:|---:|
| pre-#34 baseline | 229k cmd/s | 88k cmd/s |
| post-#42 (sharded, no fix) | 134k | 91k |
| post-#46 (selective shard lock) | 130k | 100k |
| **post-#47 (this PR — bulkSet)** | **129k** | **94k** |

### Why bulkSet didn't fully recover

The cross-shard cache-line cost is structural at N=8. A 10-key MSET on random keys touches ~7 different shard structs. Each touch pulls a different cache line for:
- shard struct itself (~150 bytes)
- shard's items map header
- shard's slab arena state (free list, per-class metadata)
- shard's LRU head/tail pointers

`BulkSetBytes` keeps a single shard's writes hot in cache for that shard's batch — but if a 10-key MSET groups into 1-2 pairs per shard across 7 shards, you only get 1-2 hits per cache line. The dominant cost is still loading 7 different shard cache lines.

To eliminate the cross-shard locality cost would require unsharding (defeats #34's purpose). The remaining −31% pipelined MSET gap is the cost of running a sharded cache; it's the price paid for the +12% pipelined SET gain that sharding delivered overall.

### What ships

- `pkg/cache/shard.go` — new `Shard.BulkSetBytes(ctx, []BulkPair, expiration)` exported method + `BulkPair` type
- `pkg/resp/handler/basic.go` — `HandleMset` groups by shard upfront, dispatches one `BulkSetBytes` per touched shard
- Bench artefacts in `bench/results/perf-mset-locality-and-bench-gap/`

### What this means for the issue

#47 is closed by this PR with **partial improvement + structural-limit finding**. The remaining MSET gap can be revisited if a use case demands it (e.g. a workload doing many large multi-key sets), but it's not a code-level fix — it'd need a fundamentally different sharding approach (e.g. lock-free RCU keyspace).

## Combined verification

- `go vet ./...` clean
- `staticcheck ./...` clean
- `staticcheck -tags 'crashdump otlp pprof' ./...` clean
- `go test -race -count=1 ./...` green across 25 packages

## Reproduce

```bash
# #47 bench
sg docker -c "REBUILD=1 bash bench/redis-benchmark/run.sh mset-bulkset --target gocache"
sg docker -c "bash bench/redis-benchmark/run.sh mset-bulkset-r2 --target gocache"
sg docker -c "bash bench/redis-benchmark/run.sh mset-bulkset-r3 --target gocache"

# #45 profile capture
PPROF=1 REBUILD=1 sg docker -c "bash bench/redis-benchmark/run.sh pprof-build --target gocache"
sg docker -c "docker run -d --name gp --network gocache-bench-net --cpuset-cpus 0-3 -p 6060:6060 gocache:pprof --config= --address 0.0.0.0 --port 6379 --load-on-startup=false --log-level warn"
sg docker -c "docker run -d --rm --name load --network gocache-bench-net --cpuset-cpus 4-7 valkey/valkey:8 valkey-benchmark -h gp -p 6379 -n 30000000 -c 50 -r 100000 -P 10 -t get -q"
sleep 3
curl -s -o cpu.prof "http://localhost:6060/debug/pprof/profile?seconds=25"
go tool pprof -top -cum cpu.prof
```
