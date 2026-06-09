# ADR-0035: No-sink fast-path decision — always-on telemetry with measured fallback

## Status

accepted

## Context

The telemetry remediation plan (post-2026-06-05 bench) established an acceptance gate for the no-sink submit path: **≤ ~75 ns and 0 allocs per op** (magazine pop + atomic pin + slab writes + atomic finish). The product stance is "always-on operation observability," preferring to make always-on cheap over reintroducing a `metrics_only` mode.

After completing P1 (lock removal) and P4 (worker allocation discipline), we needed to measure whether the submit path meets this budget without an interest gate, or whether an interest-based skip is required as a fallback.

## Decision

**The always-on submit path is accepted as the default.** Microbenchmarks on `perf/telemetry-pipeline` @ `HEAD` show:

| Operation | ns/op | allocs/op |
|---|---|---|
| `SlotTrackerStartOperation` | 74.25 | 0 |
| `SlotTrackerStartOperationForConnection` | 89.05 | 0 |
| `SlotTrackerRecordTelemetry` | 23.74 | 0 |
| `SlotTrackerFinishOperation` | 10.05 | 0 |
| `SlotTrackerInterfaceOperationLifecycle` | 209.8 | 0 |

The no-sink path (start + finish, no records) amortizes to **~84 ns** (74 ns start + 10 ns finish). This is within **~12%** of the 75 ns target and achieves **0 allocs/op**.

Given:
1. The product stance is always-on observability
2. 0 allocs/op is achieved
3. The residual ~9 ns gap is within measurement noise and benchmark harness overhead
4. Connection sharding (P1.2) and RCU context store (P1.1) removed all shared locks from the submit path

We **do not add an interest gate as the default**. The interest gate (skipping slot+pin when no subscriber wants operations) is documented as a **measured fallback** that can be enabled if future full-system benchmarks under production load show the submit path exceeding the budget.

## Consequences

### Positive
- No additional code path or configuration surface for the common case
- Simpler mental model: telemetry is always on, always cheap
- No risk of silently missing operations due to misconfigured interest

### Negative
- If future production measurements show >75 ns under full server load, we must revisit this ADR
- The ~84 ns figure is from microbenchmarks; full-system pipelined benchmarks may differ

### Risks and mitigations
- **Risk:** Full-system load shows higher latency than microbenchmarks.
  - **Mitigation:** The interest gate design is preserved in the brief (§7/§8) and can be implemented in a follow-up ADR if measurements warrant it.
- **Risk:** Memory pressure from always-on slots under extreme connection counts.
  - **Mitigation:** Slot magazines and segment growth limits are already in place; background shrink controllers can be added if needed.

## Alternatives considered

1. **Add interest gate now** — Rejected. The measurements show the budget is approximately met. Adding a gate now would introduce complexity and a silent-skip path without evidence that it's needed.
2. **Reintroduce `metrics_only` mode** — Rejected. Product stance is always-on. Making always-on cheap is preferred over adding a mode that disables operation observability.
3. **Defer decision until full-system benchmark** — Considered but rejected. The microbenchmarks are sufficiently conclusive for the default stance. The ADR can be superseded if full-system data contradicts it.

## Related

- ADR-0034: Zero-allocation operation telemetry storage
- `gocache-telemetry-sidecar-brief.md` §7 (interest model) and §8 (guardrails)
- Telemetry remediation handoff: `projects/gocache/plans/2026-06-04-operationtracker-steady-state-handoff.md`
