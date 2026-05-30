---
title: ADR-0026 Event Traffic Classes and Backpressure
description: Require research and measurement before choosing plugin traffic-class queueing and backpressure design
status: proposed
date: 2026-05-29
deciders: [witherxse]
related:
  - 0024-async-event-delivery-and-command-reaction-points
  - 0025-connection-evaluators-and-connection-events
  - 0022-modular-performance-budget
  - 0020-client-push-via-gcpc
---

# ADR-0026: Event Traffic Classes and Backpressure

## Context

ADR-0024 makes runtime events asynchronous and observational, while command hooks remain explicit synchronous reaction points. ADR-0025 separates synchronous connection evaluators from asynchronous connection events. The current per-plugin writer loop serializes outbound messages through one FIFO queue. That protects writes to a plugin connection, but it can also let best-effort telemetry backlog sit ahead of critical command RPCs, blocking hooks, or future connection evaluators.

The modular performance plan requires telemetry overload to avoid blocking normal command responses, but it also requires no silent telemetry loss. GoCache therefore needs explicit traffic semantics, queue/backpressure research, and measured evidence before accepting any implementation.

## Decision

GoCache will evaluate plugin traffic-class and backpressure designs before accepting an implementation. This ADR does **not** select a queue topology, stream count, transport shape, or overflow policy. It defines the candidate traffic semantics, candidate mechanisms, required overload behaviors, and measurement criteria that any future design must satisfy.

Candidate traffic classes are:

1. Critical command RPCs: request/response traffic governed by command timeout and plugin failure policy.
2. Blocking command hooks: synchronous reaction traffic that may affect command admission, response, or ordering.
3. Connection evaluators: synchronous reaction traffic that may affect connection/session admission or forced close/drop.
4. Lifecycle/control events: low-volume observational events that may be replayable and may require stronger retention than runtime telemetry.
5. Runtime telemetry events: high-volume observational events that may be bounded best-effort with explicit drop/lag/gap accounting.
6. Client push traffic: client-facing plugin output governed by client connection semantics and ADR-0020; it must be measured separately from server-to-plugin event delivery.

Any accepted design must preserve plugin isolation, avoid Prometheus-specific core behavior, make telemetry loss visible, and prove through benchmarks that best-effort telemetry does not block critical traffic without starving observational traffic indefinitely. External libraries and frameworks are first-class candidates: if a maintained, battle-tested dependency solves queueing, streaming, flow control, or backpressure better than custom code, it must be compared with evidence rather than dismissed because it is a dependency.

## Candidate Mechanisms to Research

### Separate bounded queues per traffic class

- **What to test**: One writer goroutine schedules between per-class queues.
- **Potential upside**: Clear semantic separation and simple queue-depth metrics per class.
- **Potential risk**: One socket writer can still create head-of-line blocking if scheduling is naive.

### Strict priority queue

- **What to test**: Critical traffic always drains before best-effort traffic.
- **Potential upside**: Strong protection for command RPCs, blocking hooks, and evaluators.
- **Potential risk**: Runtime telemetry can starve forever under sustained critical load.

### Weighted fair queue or deficit round-robin

- **What to test**: Critical traffic gets bounded preference while best-effort traffic receives guaranteed drain opportunities.
- **Potential upside**: Reduces starvation while still protecting reaction paths.
- **Potential risk**: Weight tuning can leak telemetry latency into critical paths or create complex behavior.

### Multiple logical GCPC streams over one plugin connection

- **What to test**: Protocol-level stream IDs/classes with one underlying UDS connection.
- **Potential upside**: Makes class semantics explicit without multiplying plugin processes or sockets.
- **Potential risk**: Still shares one physical write path and requires protocol/SDK complexity.

### Multiple Unix socket connections per plugin

- **What to test**: Separate sockets for critical RPC/hooks/evaluators and best-effort telemetry.
- **Potential upside**: Stronger head-of-line isolation between traffic classes.
- **Potential risk**: More handshake, lifecycle, failure, shutdown, and compatibility surface.

### External RPC, stream, queue, or messaging libraries

- **What to test**: gRPC/Connect/Twirp-style RPC, stream multiplexers such as yamux/smux/HTTP2/QUIC streams, NATS/Watermill-style messaging, and focused queue/scheduler libraries.
- **Potential upside**: Mature flow-control, streaming, health, metrics, reconnect, and testing patterns may reduce custom code and risk.
- **Potential risk**: Dependency footprint, migration cost, less protocol control, higher local hot-path overhead, operational complexity, or mismatch with Unix-socket plugin lifecycle.

### Hybrid designs

- **What to test**: For example, one critical stream plus one best-effort stream, each with bounded queues and weighted scheduling inside the best-effort stream.
- **Potential upside**: Can keep the common case simple while isolating worst traffic interactions.
- **Potential risk**: Easy to overfit to one benchmark or hide semantics in implementation detail.

## Evidence Matrix from Initial Research

The following matrix captures research inputs only. It does **not** select an implementation.

| Candidate family | What it could replace | Evidence from research | Fit to GoCache | Main risks to measure |
|---|---|---|---|---|
| Current single FIFO writer | Nothing; baseline only | GoCache currently uses one bounded per-plugin outbound FIFO in `pkg/plugin/router/router.go`; `SendFireAndForget` drops when full while blocking request paths share the same queue. | Best baseline and lowest migration cost. | Telemetry floods delaying critical sends, silent fire-and-forget drops, queue age, write-loop stalls. |
| Separate bounded queues per traffic class | Internal `PluginConn` outbound queue shape | OSS patterns from NATS and Gorilla WebSocket use one socket writer with bounded outbound queues, explicit slow-consumer handling, and clear close semantics. | Strong first custom candidate because it preserves GCPC and plugin process isolation. | Shared socket head-of-line blocking, cross-class ordering, shutdown behavior, metrics correctness. |
| Strict priority queue | Internal queue scheduler | Envoy uses separate `DEFAULT` and `HIGH` priority resource managers and tracks overflow per priority. | Useful baseline for protecting critical RPCs/hooks/evaluators. | Runtime telemetry starvation, priority inversion, hidden loss under sustained critical load. |
| Weighted fair / deficit round-robin scheduler | Internal queue scheduler | HTTP/2 priority scheduling and `x/sync/semaphore` show mature fairness/cancellation patterns, but existing Go WFQ/DRR libraries are not compelling dependencies. | Candidate if multiple best-effort classes must all make progress. | Weight tuning, fairness correctness, p99 leakage into critical traffic, implementation complexity. |
| Drop-newest bounded best-effort queue | Runtime telemetry overflow policy | OpenTelemetry Collector uses bounded queues that can fail fast on overflow and exposes queue size/capacity/enqueue-failure metrics. | Good when older queued telemetry should be preserved. | Freshness loss, repeated enqueue failures, operator visibility. |
| Drop-oldest / overwrite-oldest ring | Runtime telemetry overflow policy | OpenTelemetry Go log/span queues and Linux perf/BPF rings use overwrite/lost-sample patterns with explicit loss accounting. | Good for latest-state telemetry or high-rate observations where freshness matters more than history. | Misleading partial history, cursor/gap semantics, replay interaction. |
| Coalesced latest-value buffer | State-like best-effort telemetry overflow policy | Redpanda Connect and OTel patterns favor bounded buffers, batching, retry/WAL, and visible metrics over unbounded FIFO growth. | Good for state-like metrics where only the latest value matters. | Requires keyed semantics and gap/drop counters rather than plain FIFO delivery. |
| Multiple logical GCPC streams over one connection | GCPC envelope/protocol shape | yamux/smux demonstrate stream multiplexing and per-stream flow/backpressure over one underlying connection; gRPC/HTTP2 use separate control/quota/wakeup channels internally. | Surgical if GoCache wants class semantics while keeping one plugin socket. | Protocol/SDK churn, shared physical writer, stream-fairness testing, backwards compatibility. |
| Multiple Unix sockets per plugin | Plugin connection lifecycle | Go `UnixConn`/`UnixListener` make multi-UDS lanes straightforward; separate sockets give stronger kernel-level isolation than one FIFO. | Strong isolation candidate when traffic classes are few and stable. | More file descriptors, handshakes, accept loops, health checks, reconnect/shutdown complexity. |
| yamux / smux | GCPC transport multiplexing layer | yamux supports TCP or Unix domain sockets and per-stream flow control; smux provides token-bucket receive, fair queue shaping, sliding windows, and small frame headers. | Real candidate for multiplexing GCPC without adopting full RPC framework semantics. | Dependency behavior under slow streams, memory caps, frame overhead, interaction with current protobuf framing. |
| gRPC-Go / Connect-Go | Custom GCPC framing and RPC codegen path | gRPC gives HTTP/2 streaming, flow control, health/reflection/tooling, and UDS-style targets; Connect offers slimmer `net/http` RPC with gRPC compatibility and h2c. | Legitimate standard-RPC candidates if GCPC framing/tooling becomes the bottleneck. | Hot-path overhead, migration cost, codegen/API churn, whether HTTP semantics help or hurt local plugins. |
| Twirp / msgpack-RPC | Unary RPC codec/framing | Twirp and msgpack-RPC are simpler unary request/response options but lack strong streaming/backpressure semantics. | Mostly useful as simplicity references, not clear traffic-class solutions. | No native stream fairness, no built-in backpressure, custom tooling still required. |
| Cap'n Proto RPC | Schema and RPC model | Cap'n Proto offers promise pipelining, object capabilities, and flow-control concepts, but Go RPC support remains a larger model shift. | Radical redesign candidate only if schema/RPC model changes are in scope. | Migration complexity, protobuf incompatibility, unfamiliar programming model. |
| FlatBuffers | Serialization only | FlatBuffers targets low-copy serialization and can pair with gRPC, but does not itself solve queueing or traffic classes. | Candidate only if profiles show protobuf allocation/copy cost dominates after queue fixes. | Solves the wrong layer if bottleneck is scheduling/backpressure. |
| NATS JetStream / Watermill | Queueing, fanout, ack/retry/backpressure plumbing | NATS JetStream provides pull consumers, `MaxAckPending`, ack/redelivery, backoff, monitoring, and slow-consumer behavior; Watermill supplies router/middleware abstractions over brokers and in-process GoChannel. | Strongest battle-tested queue candidate if a separate daemon/broker is acceptable. | Operational footprint, TCP-first local fit, auth/config complexity, changed delivery semantics. |
| Redpanda Connect / OTel Collector patterns | Full sidecar pipeline or design pattern source | Both provide proven bounded buffer, batching, retry, WAL, and metrics patterns. | Better as design references unless GoCache intentionally adopts a sidecar pipeline model. | Overbuilt for direct plugin IPC, more processes/config, unclear thesis fit. |

Research also found a practical internal building-block path if GoCache keeps GCPC custom: `x/sync/semaphore` for admission/backpressure, `gammazero/deque` for per-class FIFO buckets, and `container/heap` or a small custom heap for arbitration. Existing WFQ/DRR Go libraries were not strong enough to adopt blindly; they are references, not obvious dependencies.

## Pragmatic Benchmark and Spike Order

Because this work supports a thesis timeline, candidates should be ordered by expected learning and improvement per implementation cost. The goal is not “custom first” or “framework first”; it is **cheapest credible reversible learning first**. A well-documented, battle-tested dependency can be an early spike if it may avoid a larger custom refactor and can be rolled back cleanly.

1. **Current FIFO baseline with added measurement hooks**: queue depth, drop count, enqueue latency, queue age, critical-send latency under telemetry flood, goroutine count, and write-loop stall timing. This is mandatory because every other candidate needs the same baseline.
2. **yamux logical-stream spike**: test whether a small stream-multiplexing dependency can isolate critical/control and best-effort telemetry lanes over the existing local connection model with less work than writing custom scheduling. Rollback should be limited to one adapter and one experiment path.
3. **smux logical-stream spike**: test if yamux is insufficient or if explicit receive buffering, token-bucket behavior, and per-stream windows look better for GoCache’s traffic shape. This is still a small dependency spike, but has more tuning surface.
4. **Connect-Go typed-RPC spike**: test early if the problem appears to be broader RPC/framing/tooling rather than only queue scheduling. Keep it narrow: one adapter, one path, no full GCPC migration.
5. **Custom GCPC separate bounded queues**: benchmark if the multiplexing/RPC spikes are not cheaper or do not fit. This preserves the existing protobuf envelopes, plugin lifecycle, and SDK assumptions while separating critical/control traffic from best-effort runtime telemetry.
6. **Custom GCPC strict-priority scheduler**: benchmark as a simple protection baseline, not as the presumed answer, because it may starve telemetry.
7. **Custom GCPC weighted/DRR-style scheduler**: benchmark only if strict priority protects critical paths but causes unacceptable telemetry starvation, or if multiple best-effort event classes need progress guarantees.
8. **Multiple Unix sockets per plugin**: benchmark if logical streams cannot isolate traffic enough or if the fixed traffic-class count makes extra sockets manageable.
9. **gRPC-Go, NATS JetStream, or Watermill prototypes**: keep as first-class candidates, but treat them as heavier spikes. gRPC-Go is battle-tested but has a larger HTTP/2/codegen surface than Connect. NATS JetStream and Watermill are credible if GoCache intentionally moves toward broker/message-stream semantics, but their ack/retry/state/config model is broader than direct plugin IPC.
10. **OTel Collector / Redpanda Connect patterns**: use as design references for bounded queues, batching, retry, WAL, and metrics unless GoCache deliberately chooses a sidecar pipeline architecture.

This order is not a final architecture decision. It is a time-aware measurement plan. The first pass should keep each spike to one adapter, one representative path, explicit rollback, and the same baseline metrics. If a library spike fails to show fit quickly, discard it and move on rather than turning it into a thesis-consuming migration.

## Overflow Policies to Measure

Overflow policy is part of the design and must be measured per traffic class:

- Block until enqueue or context deadline.
- Fail explicitly according to failure policy.
- Drop newest.
- Drop oldest.
- Coalesce events into a gap marker.
- Sample high-volume events.
- Disconnect or quarantine a plugin that cannot drain.
- Shed by subscriber or event type.

Critical traffic cannot disappear silently and cannot be unbounded. Runtime telemetry may drop only with visible counters/gaps. Security/audit events must be classified separately before implementation if they require stronger delivery than metrics telemetry.

## Required Measurements

Each candidate design must be compared with the current FIFO baseline using measurements that include:

- Command throughput for standard and pipelined workloads.
- p50, p95, p99, and p999 latency where the harness can capture them.
- Critical send latency while telemetry is flooding.
- Same-class FIFO correctness.
- Queue depth and queue age/lag per class.
- Drop counts and gap counts.
- RSS and allocation rate.
- Goroutine count and blocked goroutines.
- CPU, allocation, mutex, and block profiles.
- Write-loop stalls and socket write latency.
- Plugin disconnect/reconnect and shutdown behavior.
- Plugin process CPU/RSS where practical.

## Alternatives Considered

### Alternative 1: Keep one FIFO for all plugin traffic

- **Pros**: Simple and preserves total per-plugin send order.
- **Cons**: Telemetry floods can delay critical requests and synchronous hooks.
- **Why not**: Different traffic classes have different failure semantics; one FIFO hides those differences. It remains the baseline to measure against, not the accepted final design.

### Alternative 2: Choose strict priority immediately

- **Pros**: Protects critical traffic in the obvious failure mode.
- **Cons**: Can starve telemetry indefinitely and mask loss under sustained critical load.
- **Why not**: Starvation and p99 behavior must be measured before accepting strict priority.

### Alternative 3: Split every traffic class into separate Unix sockets immediately

- **Pros**: Strong head-of-line isolation.
- **Cons**: More handshake, lifecycle, failure, shutdown, and compatibility complexity before measurement proves it is needed.
- **Why not**: Multiple sockets are a candidate, not the starting assumption.

### Alternative 4: Drop all async events silently on overload

- **Pros**: Protects command throughput and is easy to implement.
- **Cons**: Operators cannot know telemetry is incomplete.
- **Why not**: Best-effort does not mean invisible loss. Drops must be counted and surfaced.

### Alternative 5: Reject frameworks because they are dependencies

- **Pros**: Preserves maximum control and avoids migration/dependency risk.
- **Cons**: Can waste time rebuilding mature flow-control, streaming, health, queue, metrics, and reconnect behavior that maintained libraries already solve.
- **Why not**: Dependency avoidance is not an engineering argument by itself. Frameworks and libraries must be researched, benchmarked, and compared against custom GCPC before being accepted or rejected.

## Consequences

### Positive

- Prevents premature commitment to one queueing design.
- Makes overload behavior and measurement criteria explicit before code.
- Keeps plugin isolation and avoids Prometheus-specific core behavior.
- Creates a shared benchmark vocabulary for comparing queue/backpressure options.

### Negative

- Adds a research/measurement phase before implementation.
- Requires additional harness work for traffic floods, slow plugins, and critical-send latency.
- Delays queue implementation until the options are compared.

### Risks

- **Risk**: Measurement matrix becomes too broad to run repeatedly. **Mitigation**: Use smoke tests for every candidate and full docker matrix only for shortlisted designs.
- **Risk**: A design protects critical traffic but starves telemetry forever. **Mitigation**: Include telemetry starvation/lag as an acceptance metric.
- **Risk**: Drop counters are emitted over the same saturated path and disappear. **Mitigation**: Expose counters through a separate local metrics/server-query surface, not only as events.
- **Risk**: Client push gets mixed into server-to-plugin event policy. **Mitigation**: Measure plugin-to-server `ClientPushV1` separately because it has client connection semantics under ADR-0020.
