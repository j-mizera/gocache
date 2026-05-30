# Heavy benchmark summary — event hotpath observability

Date: 2026-05-30 UTC

## Scope

Compared clean baseline checkout `/home/witherxse/IdeaProjects/gocache` at `main` commit `4b62f81` against implementation worktree `/home/witherxse/IdeaProjects/gocache-event-contract` branch `impl/event-hotpath-observability`, same base commit plus uncommitted ADR-0028/Phase 2A changes.

Harness: `bench/redis-benchmark` Docker/Valkey benchmark scripts.

Workload parameters:

- `BENCH_N=100000`
- `BENCH_CLIENTS=50`
- `BENCH_KEYSPACE=100000`
- `BENCH_PIPELINE=10`
- target CPUs `0-3`, client CPUs `4-7`
- target memory limit `2g`
- command suite: `ping_inline,ping_mbulk,set,get,incr,lpush,rpush,lpop,rpop,sadd,hset,spop,lrange_100,mset`

Commands run:

```bash
RESULTS_DIR=/home/witherxse/IdeaProjects/gocache-event-contract/bench/results/heavy-event-hotpath-20260530 REBUILD=1 ./bench/redis-benchmark/run.sh baseline --target gocache
RESULTS_DIR=/home/witherxse/IdeaProjects/gocache-event-contract/bench/results/heavy-event-hotpath-20260530 REBUILD=1 ./bench/redis-benchmark/run.sh implementation --target gocache
RESULTS_DIR=/home/witherxse/IdeaProjects/gocache-event-contract/bench/results/heavy-event-hotpath-20260530 REBUILD=1 ./bench/redis-benchmark/run-ipc.sh baseline --target gocache-ipc
RESULTS_DIR=/home/witherxse/IdeaProjects/gocache-event-contract/bench/results/heavy-event-hotpath-20260530 REBUILD=1 ./bench/redis-benchmark/run-ipc.sh implementation --target gocache-ipc
RESULTS_DIR=/home/witherxse/IdeaProjects/gocache-event-contract/bench/results/heavy-event-hotpath-20260530 ./bench/redis-benchmark/compare.sh baseline-gocache implementation-gocache
RESULTS_DIR=/home/witherxse/IdeaProjects/gocache-event-contract/bench/results/heavy-event-hotpath-20260530 ./bench/redis-benchmark/compare.sh baseline-gocache-ipc implementation-gocache-ipc
RESULTS_DIR=/home/witherxse/IdeaProjects/gocache-event-contract/bench/results/heavy-event-hotpath-20260530 ./bench/redis-benchmark/compare.sh implementation-gocache implementation-gocache-ipc
```

Docker emitted a missing `docker-buildx` plugin warning and fell back to the legacy builder. Benchmark scripts emitted `WARNING: Could not fetch server CONFIG`; runs still completed and produced CSV/memory files.

## Aggregate results

### Core, no IPC plugin: baseline → implementation

| Mode | Throughput result | p99 result | Memory result |
|---|---:|---:|---:|
| Standard/non-pipelined | geometric mean RPS `+5.11%`; 11/15 commands improved | geometric mean p99 `-18.65%`; 12/15 improved, 0 worsened | final RSS `+69.3 MB` / `+50.08%`; delta RSS `+43.8 MB` / `+34.81%` |
| Pipelined | geometric mean RPS `+6.07%`; 12/15 commands improved | geometric mean p99 `-22.92%`; 12/15 improved, 3 worsened | same memory file as core run |

Largest core throughput gains:

- Standard: LRANGE setup LPUSH `+16.0%`, RPOP `+14.9%`, MSET `+9.2%`, HSET `+8.7%`, LRANGE_100 `+7.9%`.
- Pipelined: LPUSH `+20.5%`, SADD `+14.6%`, MSET `+14.0%`, INCR `+13.6%`, LRANGE_100 `+12.4%`.

Core regressions:

- Standard RPS worsened on PING_INLINE `-2.1%`, PING_MBULK `-2.0%`, INCR `-1.2%`, SET `-0.8%`.
- Pipelined RPS worsened on SET `-7.3%` and LPOP `-5.8%`; LPOP p99 worsened `+90.4%`, SET p99 worsened `+15.9%`, RPOP p99 worsened `+9.4%`.

### IPC Prometheus plugin: baseline → implementation

| Mode | Throughput result | p99 result | Memory result |
|---|---:|---:|---:|
| Standard/non-pipelined | geometric mean RPS `+0.84%`; 9/15 commands improved | geometric mean p99 `-13.07%`; 10/15 improved | final RSS `+8.4 MB` / `+4.65%`; delta RSS `-17.0 MB` / `-10.49%` |
| Pipelined | geometric mean RPS `+33.27%`; 15/15 commands improved | geometric mean p99 `-15.91%`; 12/15 improved | same memory file as IPC run |

IPC pipelined largest gains:

- SET `+51.8%`
- GET `+49.5%`
- LPOP/RPOP `+42.7%`
- SPOP `+41.3%`
- SADD `+38.5%`
- HSET `+37.6%`

IPC standard regressions:

- RPOP `-5.7%`
- LPUSH `-5.5%`
- SPOP `-2.2%`
- MSET `-2.1%`
- PING_INLINE `-0.7%`

### Implementation IPC cost versus implementation core

| Mode | Throughput cost | p99 cost | Memory cost |
|---|---:|---:|---:|
| Standard/non-pipelined | geometric mean RPS `-10.7%`; IPC slower on 15/15 commands | geometric mean p99 `+188.68%`; IPC worse on 15/15 commands | post-standard RSS `+35.5 MB`; baseline RSS `+5.3 MB`; final RSS was `18.9 MB` lower in this run |
| Pipelined | geometric mean RPS `-55.15%`; IPC slower on 15/15 commands | geometric mean p99 `+235.21%`; IPC worse on 15/15 commands | same memory comparison |

## Interpretation

The changes appear to improve the no-plugin core path modestly and the IPC observability path substantially under pipelining. The strongest evidence for the Phase 2A goal is the IPC pipelined result: all 15 commands improved, with a `+33.27%` geometric mean RPS gain and better p99 on 12/15 commands.

Costs remain significant:

1. Core RSS increased materially in this run: final RSS `+69.3 MB`, delta RSS `+43.8 MB`. This is the main negative result and needs follow-up before claiming the change is unambiguously cheaper.
2. IPC observability is still expensive versus no-plugin core: implementation IPC is `-10.7%` geometric mean RPS in standard mode and `-55.15%` in pipelined mode, with p99 latency much worse in both modes.
3. Some command-specific regressions remain: core pipelined SET and LPOP regressed in throughput, and LPOP p99 worsened strongly.
4. The benchmark is one run per target. Treat small single-digit deltas as directional rather than definitive until repeated runs or medians are collected.

## Raw files

- `baseline-gocache.csv`
- `baseline-gocache-pipelined.csv`
- `baseline-gocache-memory.txt`
- `implementation-gocache.csv`
- `implementation-gocache-pipelined.csv`
- `implementation-gocache-memory.txt`
- `baseline-gocache-ipc.csv`
- `baseline-gocache-ipc-pipelined.csv`
- `baseline-gocache-ipc-memory.txt`
- `implementation-gocache-ipc.csv`
- `implementation-gocache-ipc-pipelined.csv`
- `implementation-gocache-ipc-memory.txt`


## Valkey reference comparison

Additional command run after the baseline/implementation comparison:

```bash
RESULTS_DIR=/home/witherxse/IdeaProjects/gocache-event-contract/bench/results/heavy-event-hotpath-20260530 ./bench/redis-benchmark/run.sh reference --target valkey
RESULTS_DIR=/home/witherxse/IdeaProjects/gocache-event-contract/bench/results/heavy-event-hotpath-20260530 ./bench/redis-benchmark/compare.sh reference-valkey implementation-gocache
RESULTS_DIR=/home/witherxse/IdeaProjects/gocache-event-contract/bench/results/heavy-event-hotpath-20260530 ./bench/redis-benchmark/compare.sh reference-valkey implementation-gocache-ipc
```

Valkey memory sample:

- baseline RSS: `18,025,021` bytes
- post-standard RSS: `32,359,055` bytes
- final RSS: `36,270,243` bytes
- delta RSS: `18,245,222` bytes

### Implementation core versus Valkey

| Mode | Throughput position | p99 position | Memory position |
|---|---:|---:|---:|
| Standard/non-pipelined | geometric mean RPS `+4.41%`; 14/15 commands above Valkey | geometric mean p99 `-19.63%`; 14/15 better, 0 worse | final RSS `+171.5 MB` (`+472.71%`); delta RSS `+151.2 MB` (`+828.85%`) |
| Pipelined | geometric mean RPS `-9.97%`; 6/15 commands above Valkey, 9/15 below | geometric mean p99 `+87.94%`; 4/15 better, 11/15 worse | same memory file as core run |

Core standard strengths versus Valkey:

- HSET `+12.5%`
- LRANGE setup LPUSH `+9.6%`
- RPUSH `+7.5%`
- LPUSH `+7.1%`
- LRANGE_100 `+7.0%`

Core standard weakness versus Valkey:

- LPOP `-1.1%` RPS; p99 equal in the compare output.

Core pipelined strengths versus Valkey:

- SPOP `+30.6%`
- SET `+24.5%`
- PING_MBULK `+10.7%`
- GET `+5.6%`
- PING_INLINE `+2.0%`
- INCR `+1.0%`

Core pipelined weaknesses versus Valkey:

- HSET `-32.1%`
- SADD `-29.2%`
- RPUSH `-29.1%`
- LRANGE setup LPUSH `-26.7%`
- LPOP `-22.6%`
- p99 is worse on 11/15 pipelined commands.

### Implementation IPC Prometheus plugin versus Valkey

| Mode | Throughput position | p99 position | Memory position |
|---|---:|---:|---:|
| Standard/non-pipelined | geometric mean RPS `-6.76%`; 0/15 commands above Valkey | geometric mean p99 `+132.03%`; 15/15 worse | final RSS `+152.6 MB` (`+420.67%`); delta RSS `+127.0 MB` (`+696.26%`) |
| Pipelined | geometric mean RPS `-59.63%`; 0/15 commands above Valkey | geometric mean p99 `+530.0%`; 15/15 worse | same memory file as IPC run |

Interpretation versus Valkey:

- GoCache core is competitive or better than Valkey in non-pipelined request/response throughput for this suite, but it pays much higher RSS.
- GoCache core is not yet competitive with Valkey under pipelining overall; a few commands win, but most list/hash/set-heavy commands lose and p99 is usually worse.
- GoCache IPC observability mode remains materially slower and higher-latency than Valkey across both standard and pipelined workloads, even though it improved versus GoCache's own previous IPC baseline.
