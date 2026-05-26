---
title: ADR-0021 Introduce commons/ Package Layer
description: Add a shared implementation layer between api/ (contracts) and pkg/ (server internals)
status: accepted
date: 2026-05-26
deciders: [witherxse]
related:
  - 0020-client-push-via-gcpc
---

# ADR-0021: Introduce commons/ Package Layer

## Context

The `api/` directory was intended for interfaces, constants, and structs — the contract surface between the server and plugins. Over time it accumulated concrete implementations: a zerolog logger wrapper (191 lines), crash dump disk I/O (214 lines), Unix socket transport with protobuf framing (143 lines), and RESP encoding helpers. These are not contracts — they are shared utilities that both `pkg/` and plugins need.

Meanwhile, `pkg/resp/` holds the RESP `Value` type, command name constants, and response constructors that plugins would benefit from but cannot import (CI enforces `plugins/ → api/` only). The RESP type constants are duplicated between `api/resp/encode.go` and `pkg/resp/resp.go`.

The embedded plugin system (`api/embedded/`) contains the `Plugin` interface, a global registry, and lifecycle orchestration. This is plugin integration plumbing, not a server-facing contract like `Sink` or `Emitter`.

## Decision

We introduce a top-level `commons/` directory for shared implementations importable by all layers. We also move embedded plugin integration to `sdk/` and test utilities to `testkit/`.

New import hierarchy:

```
plugins/  → api/, sdk/, commons/
sdk/      → api/, commons/
commons/  → api/, stdlib, external libs
api/      → stdlib only (+ protobuf for gcpc)
pkg/      → api/, commons/
cmd/      → anything
testkit/  → api/, commons/ (test-only)
```

Migrations:

| From | To | Reason |
|------|----|--------|
| `api/logger/` | `commons/logger/` | Zerolog implementation, not interface |
| `api/crashdump/` | `commons/crashdump/` | Disk I/O implementation |
| `api/transport/` | `commons/transport/` | Unix socket + protobuf framing impl |
| `api/resp/` + `pkg/resp/{resp,commands,helpers,pool}.go` | `commons/resp/` | Consolidate RESP, eliminate duplication |
| `api/embedded/` | `sdk/embedded/` | Plugin-author-facing contract + registry |
| `api/persistence/memstore.go` | `testkit/memstore/` | Test-only CacheStore impl |
| `api/config/testhelper.go` | `testkit/config/` | Test-only config double |

CI enforcement (`scripts/check-plugin-isolation.sh`) is extended to cover all layer boundaries.

## Alternatives Considered

### Alternative 1: Flatten into api/ with sub-packages

Keep everything in `api/` but add `api/internal/` for implementations.

- **Pros**: No new top-level directory, minimal import changes
- **Cons**: `api/internal/` is invisible to plugins/sdk — defeats the purpose. Also `internal/` has Go-enforced access control that's too restrictive here.
- **Why not**: The whole point is making shared implementations importable by both pkg/ and plugins.

### Alternative 2: Move implementations to pkg/ with interfaces in api/

Extract interfaces (`Logger`, `Transport`) to `api/`, move implementations to `pkg/`.

- **Pros**: Clean interface/implementation split, api/ becomes pure contracts
- **Cons**: Plugins can't import `pkg/` implementations, so they'd need the interfaces but couldn't construct concrete instances. Requires dependency injection plumbing everywhere.
- **Why not**: Impractical — plugins need to call `logger.New()`, not receive a pre-built logger through DI. This is a follow-up concern (extract interfaces later without moving impls back).

### Alternative 3: Put shared code in sdk/

Expand `sdk/` to hold all shared implementations.

- **Pros**: No new directory; sdk already exists
- **Cons**: `sdk/` semantically means "plugin development kit." Logger and RESP encoding are not plugin SDK concerns — the server uses them too. Muddies the sdk/ purpose.
- **Why not**: `commons/` is semantically accurate — shared utilities used by multiple layers.

## Consequences

### Positive

- `api/` becomes what it was meant to be: interfaces, constants, structs
- RESP type constants exist in one place (no duplication)
- Command names and Value constructors become available to plugins
- CI enforces the full import hierarchy, not just one rule
- Test utilities have a proper home outside the public API

### Negative

- ~80 files need import path updates (mechanical but noisy diff)
- One more top-level directory to understand
- `pkg/resp/` becomes a directory with no .go files (only `handler/` sub-package)

### Risks

- **Risk**: Future contributors put new shared code in api/ out of habit. **Mitigation**: CI script rejects api/ importing commons/pkg/sdk/plugins; CLAUDE.md documents the hierarchy.
- **Risk**: commons/ grows into a dumping ground. **Mitigation**: Each sub-package must be a coherent utility (logger, resp, transport, crashdump). Review new additions against "is this a shared implementation or does it belong in one layer?"
