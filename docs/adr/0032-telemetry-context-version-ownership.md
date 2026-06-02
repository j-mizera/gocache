---
title: ADR-0032 Telemetry Context Version Ownership
description: Keep server context versions internal while exposing event-time serialized context buckets to plugins
status: proposed
date: 2026-06-01
deciders: [witherxse]
related:
  - 0028-operation-observability-and-log-records
  - 0029-operationtracker-sidecar-low-overhead-telemetry
  - 0030-gcpc-v1-observability-context-contract
  - 0031-operation-identity-export-contract
  - 0033-common-operationtracker-sharding-contract
  - projects/gocache/research/gocache-zero-allocation-telemetry-sidecar-implementation-brief.md
---

# ADR-0032: Telemetry Context Version Ownership

## Context

GoCache currently stores mutable per-connection state in `pkg/clientctx.ClientContext` and uses operation context maps for plugin hooks, command requests, events, logs, and runtime instrumentation. This works for the prototype but forces the command path to snapshot or enrich maps before the operation returns. It also risks processing-time context if a sidecar lazily reads live connection state after the client context has changed. The updated sidecar brief requires event-time correctness: telemetry must describe the connection state that existed when the operation started, not the state observed later by the worker.

Current code also explains why the earlier operations model became confusing: command operations are parented to the connection operation id in tests such as `conn_1`, while `ClientContext.ConnectionID` is a separate stable client id. That historical behavior is not the target contract. The current conclusion is that connection identity and connection context are telemetry context, not operation parentage.

This ADR covers telemetry context ownership only. It does not redefine Go `context.Context`, connection id formats, or event-bus coupling.

## Decision

Server-side `ClientContext` state is converted into a telemetry-relevant base context map at operation start. The operation start record pins the server-only base context version current for that operation. `OperationTracker` workers resolve that version, fold operation-level context update/remove records on top of it in submit order, and project serialized filtered context only when records cross an API, SDK, GCPC, event, or log boundary.

Connection context version ids are server/worker-local handles. They never cross GCPC as common fields, typed event fields, metadata, or SDK-visible ids. Plugins receive serialized filtered context or typed context buckets, never references to the server version table. Plugin-originated background operations own their own telemetry context and may parent to a server operation by exported operation id when there is real operation causality, but they do not mutate or borrow server connection context versions.

GoCache operation `parent_id` represents GoCache operation causality only. A normal client command is a root operation with empty `parent_id`; connection identity, connection context ancestry, context version ancestry, and command sequencing must not be interpreted as GoCache operation parentage. REX `traceparent` may be mapped by an exporter plugin into external trace/span parentage, but core and `OperationTracker` must not rewrite GoCache `parent_id` from REX metadata.

Go `context.Context` remains for cancellation, deadlines, and request lifetime. It is not the telemetry context store and must not be used as a mutable data bus.

## Contract Shape

### Server base context

The server owns telemetry-relevant connection state such as connection identity, remote address, authentication state, protocol mode, selected metadata, tenant/user attributes, and other filtered values. At operation start, the server converts the relevant `ClientContext` fields into a base telemetry context map and records the base context version id in the operation start record. Read-only commands reuse existing immutable base context versions when connection-scoped telemetry state has not changed.

The exact set of telemetry-relevant keys is implementation work, but the conversion point is explicit: operation start captures the event-time base context. Worker projection must not read live mutable `ClientContext` state later and treat it as operation-start context.

### EventContext buckets

GCPC and SDK-facing observability records should expose context as a typed common shape rather than one flat ambiguous map. The first shape is:

```text
EventContext {
  rexConnectionContext map<string,string>
  rexCommandContext    map<string,string>
  additionalContext    map<string,string>
}
```

`rexConnectionContext` contains sticky connection-scoped `REX.META` defaults captured from the base connection context version. `rexCommandContext` contains one-shot command `META` values that apply only to the current command and override connection REX keys during materialization. `additionalContext` contains server, hook, plugin, and operation annotations that are not REX metadata.

REX keys stay bare inside the REX buckets. Projection to legacy flat keys such as `shared.rex.*`, hook metadata, or exporter-specific attribute names happens only at materialization boundaries that require that shape. A key must not be duplicated as both REX data and `additionalContext`. When materializing a flat context, command REX values override connection REX values for the same key.

### Operation context replay

Operation-level context updates are submitted as sidecar records and folded by the worker in FIFO order for that operation. Each log or event observes the context after all prior mutations and before later mutations. Later context updates must not retroactively change earlier logs or events.

Context remove records are idempotent. If a remove can mask an inherited base, REX, or earlier context value, the worker must represent it as a tombstone so the removed value does not reappear during materialization. A context key added and then removed should appear only on logs/events emitted while it was active.

Open/user-defined context keys remain bytes or strings at the boundary. They must not require hot-path interning or map lookup. Worker-side materialization may use maps, pooled state, or packed representations as long as plugin-visible output remains serialized filtered context or the typed `EventContext` buckets above.

### Parent and trace correlation

Command-sourced operations do not parent to connection operation ids by default. They should have an empty GoCache `parent_id` unless the command is truly caused by another GoCache operation. Connection ids and connection operation ids remain context/correlation data, not parent operations.

REX and command metadata can carry external trace context. `rexCommandContext["traceparent"]`, `rexConnectionContext["traceparent"]`, `shared.rex.traceparent`, or `shared.traceparent` may be interpreted by an instrumentation/exporter plugin to create external span parentage. That mapping belongs to exporter/plugin behavior; the server-side operation model remains GoCache operation causality.

### Plugin context

For server-caused work, plugins receive filtered serialized context materialized from the server operation. If they react by creating child operations, they use the server exported operation id as parent only when that child operation is causally created by the server operation and maintain plugin-owned telemetry context for their work.

For plugin background tasks, the plugin initializes its own telemetry context through SDK/API helpers. It may include values derived from earlier server context, but it owns that copy and cannot observe future server connection context mutations unless a new server record is delivered.

### Version lifetime

Operation start pins the referenced base context version. Operation finish, abandon, replay-window expiry, or reaper release that reference. Any async component that retains a base version beyond normal operation processing must acquire and release its own reference. Context versions must be reclaimable even when operations are abandoned or telemetry workers lag.

Version lifetime is an implementation detail local to server/worker code. GCPC carries serialized filtered context after dereferencing; it never carries the version id, refcount, or reaper state.

### GCPC boundary and redaction

GCPC carries serialized filtered telemetry context or typed `EventContext` buckets. It never carries server context version ids. Any GCPC field or SDK type that appears to expose a version id must be treated as a bug in the contract.

Redaction and visibility filtering happen before context crosses the plugin/GCPC boundary. Redaction tests must cover sensitive keys in base context, REX connection context, REX command context, and additional context. Context replay is best-effort telemetry; dropped context records may make reconstructed telemetry context incomplete and must not be treated as audit-durable truth.

## Priority Review Gates Before Implementation

This ADR remains `proposed` until these context-lifecycle gates are resolved or explicitly deferred:

1. **Version lifetime**: define finish, abandon, replay-window, async-retainer, and reaper release rules for pinned base context versions.
2. **Mutation inputs**: enumerate which `ClientContext`, `RexMeta`, `CmdMeta`, hook, plugin, and operation fields become base context, `rexConnectionContext`, `rexCommandContext`, or `additionalContext`.
3. **Parent rewrite**: replace current command-parent-to-connection-operation behavior with empty command parents by default and update legacy `conn_1` parent tests/docs accordingly.
4. **Replay tests**: require delayed-worker, concurrent-mutation, update/remove ordering, and tombstone tests proving event-time context correctness.
5. **Redaction/filtering tests**: verify sensitive keys are removed before plugin/GCPC materialization across all `EventContext` buckets.
6. **REX/exporter integration**: prove REX traceparent stays context for exporter mapping and does not rewrite core GoCache `parent_id`.

## Alternatives Considered

### Alternative 1: Read live `ClientContext` lazily in the worker

- **Pros**: Avoids building context versions up front.
- **Cons**: Emits processing-time state instead of event-time state and risks cross-goroutine races or stale pointers.
- **Why not**: Telemetry must reflect the connection state at operation start.

### Alternative 2: Copy the full connection context per operation on the command path

- **Pros**: Simple event-time snapshot semantics.
- **Cons**: Copies rarely changing state for every command and recreates the allocation pressure this sidecar work is meant to remove.
- **Why not**: Versioning copies on mutation or operation-start conversion, not by copying a full map for every record.

### Alternative 3: Use Go `context.Context` values as telemetry storage

- **Pros**: Familiar propagation API and already used in some logging/tracing libraries.
- **Cons**: `context.Context` value chains are not a mutable map, can allocate per wrapper, and are intended for request-scoped data rather than telemetry state storage.
- **Why not**: Go context remains for cancellation/deadlines; telemetry context needs an explicit versioned data model.

### Alternative 4: Expose server context version ids to plugins

- **Pros**: Avoids materializing maps before IPC and preserves exact server version identity.
- **Cons**: Leaks server lifetime management and connection ownership to plugins, which cannot safely resolve or retain those ids.
- **Why not**: Plugins need filtered telemetry values, not server memory/version table handles.

### Alternative 5: Treat connection operation id as the default parent for command operations

- **Pros**: Preserves existing `conn_1`-style operation trees and makes commands appear under a connection node.
- **Cons**: Confuses connection lifetime/correlation with operation causality and creates misleading parentage for every normal client command.
- **Why not**: The accepted model keeps command operations root-level by default. Connection identity belongs in context; external trace parentage belongs to exporter interpretation of trace metadata.

### Alternative 6: Flatten all REX and context data into one map immediately

- **Pros**: Smallest protobuf/context surface and close to the current `shared.rex.*` projection.
- **Cons**: Loses the distinction between sticky connection REX, one-shot command REX, and non-REX annotations; makes precedence and redaction harder to test.
- **Why not**: Typed `EventContext` buckets make the boundary explicit while still allowing flat projection at materialization points that need it.

## Consequences

### Positive

- Preserves event-time correctness even when connection context mutates while sidecar workers lag.
- Removes live mutable `ClientContext` reads and per-record connection-context snapshots from the command goroutine.
- Keeps server-only connection context lifetimes out of plugin and GCPC contracts.
- Gives plugins a clearer model: receive serialized typed context buckets or own their own context.
- Separates GoCache operation causality from connection correlation and external trace parentage.

### Negative

- Requires a server-side context-version store, lifetime/refcount strategy, and cleanup path.
- Requires explicit mapping from current `ClientContext`, `RexMeta`, `CmdMeta`, hooks, and plugin annotations into `EventContext` buckets.
- Existing tests that expect `conn_1` command parents or flat `shared.rex.*` context everywhere must be updated once implementation follows this ADR.
- Exporter plugins must intentionally map REX traceparent into external tracing rather than relying on core parent ids.

### Risks

- **Risk**: Context versions leak if operations never finish. **Mitigation**: Tie version references to OperationTracker finish/abandon/reaper paths and require async retainers to acquire/release references explicitly.
- **Risk**: Sensitive values leak into materialized plugin maps. **Mitigation**: Reuse and test `api/context` filtering/redaction before GCPC emission for every context bucket.
- **Risk**: Plugin authors confuse telemetry context with Go `context.Context`. **Mitigation**: Document the split in SDK and GCPC docs and expose helpers with distinct names.
- **Risk**: Dropped context update/remove records make later event context incomplete. **Mitigation**: Treat telemetry as best-effort, expose drop/health signals where practical, and do not represent telemetry context as audit-durable truth.
- **Risk**: REX traceparent is accidentally converted into GoCache operation parentage. **Mitigation**: Add tests that command operations have empty parent ids by default and that traceparent is visible only as context for exporter mapping.
