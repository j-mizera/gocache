---
title: ADR-0025 Connection Evaluators and Connection Events
description: Separate synchronous connection admission/evaluation hooks from asynchronous connection observation events
status: proposed
date: 2026-05-29
deciders: [witherxse]
related:
  - 0024-async-event-delivery-and-command-reaction-points
  - 0022-modular-performance-budget
  - Plugins
  - REX
---

# ADR-0025: Connection Evaluators and Connection Events

## Context

Future auth/OAuth work needs plugins to participate in connection/session admission decisions without making OAuth a core concept. At the same time, connection lifecycle notifications such as connection opened, closed, dropped, or authenticated are useful telemetry and audit events. These are different integration surfaces: an admission decision must be synchronous, while observation should be asynchronous.

The async event delivery contract in ADR-0024 says events cannot affect command or connection outcomes. Therefore GoCache needs a separate connection evaluator concept for auth, IP allow/deny, session viability, protocol admission, and forced close/drop decisions.

## Decision

Connection events are asynchronous observational notifications. They report facts such as connection open, connection close, authentication failure, forced drop, or session expiry after the relevant state transition has occurred.

Connection evaluators are synchronous reaction points. They may allow, deny, enrich, expire, or close a connection according to an explicit failure policy. Candidate evaluator phases are `connection.start`, `connection.evaluate`, and `connection.close` / `connection.drop`; exact API names remain part of the auth/session design, but the architectural boundary is fixed: admission uses evaluators, observation uses events.

Core remains OAuth/OIDC-agnostic. OAuth plugins implement a generic auth/session evaluator contract and return generic principal, scope, expiry, and metadata state for core to enforce locally.

## Alternatives Considered

### Alternative 1: Use connection.open events for auth admission

- **Pros**: Reuses the event bus and avoids a new evaluator API.
- **Cons**: Events are asynchronous and cannot safely block or deny connection use. Race conditions appear between event delivery and command processing.
- **Why not**: Auth/admission is a reaction point. It must be explicit and synchronous when configured.

### Alternative 2: Put OAuth/OIDC directly in core

- **Pros**: Simplest hot path once implemented; no IPC decision latency for auth.
- **Cons**: Imports one auth technology into the server and violates plugin isolation.
- **Why not**: Core should own generic session state and local authorization enforcement, not OAuth-specific validation.

### Alternative 3: Require per-command IPC authorization

- **Pros**: Plugins get full control over every decision.
- **Cons**: Adds IPC latency to every command and makes auth plugins part of the hot path by default.
- **Why not**: The target model is validate credentials at explicit admission/refresh points, install generic session state, then enforce scopes locally in core.

### Alternative 4: Treat all connection policies as command hooks

- **Pros**: Avoids another plugin integration surface.
- **Cons**: Some decisions must happen before the first command or outside command execution entirely, such as IP allow/deny or forced disconnect.
- **Why not**: Connection lifecycle and command lifecycle have different timing and failure semantics.

## Consequences

### Positive

- Keeps auth/OAuth work aligned with the async-by-default event contract.
- Gives future plugins a clear synchronous surface for connection/session decisions.
- Keeps core independent of OAuth/OIDC while still allowing local scope enforcement after validation.
- Prevents telemetry events from being overloaded as policy hooks.

### Negative

- Adds another plugin extension surface that must be designed, documented, and tested.
- Requires careful failure-policy semantics for evaluator timeouts, plugin crashes, and conflicting plugin decisions.
- May require GCPC protocol additions once the concrete auth/session API is designed.

### Risks

- **Risk**: Multiple evaluators disagree. **Mitigation**: The evaluator design must define ordering, aggregation, priority, and failure behavior before implementation.
- **Risk**: Evaluator latency affects connection setup or refresh. **Mitigation**: Evaluators need deadlines, observability, and configurable failure policy.
- **Risk**: Plugins assume connection events are admission hooks. **Mitigation**: SDK docs must explicitly separate connection events from connection evaluators.
