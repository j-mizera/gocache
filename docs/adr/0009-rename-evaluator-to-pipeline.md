---
title: ADR-0009 Rename evaluator to pipeline
description: Rename pkg/evaluator to pkg/pipeline to reflect its actual responsibilities
status: proposed
date: 2026-05-19
deciders: [witherxse]
related:
  - Server-Architecture
  - ADR-0010-direct-shard-mutex
---

# ADR-0009: Rename evaluator to pipeline

## Context

The `pkg/evaluator` package was created when the component only evaluated commands against the cache. It has since grown into the central command processing hub with 8+ distinct responsibilities: command lookup, argument validation, transaction queueing, fast/slow instrumentation path routing, pre/post hook execution, plugin command routing, command context population, and dispatch coordination. The name "evaluator" describes at most one of these; new contributors consistently misunderstand its scope.

The type `BaseEvaluator` compounds the problem -- it is the only evaluator, there is no derived type, and "base" suggests an inheritance hierarchy that does not exist in Go.

## Decision

We rename `pkg/evaluator` to `pkg/pipeline` and `BaseEvaluator` to `Pipeline`. The package name reflects that commands flow through a staged processing pipeline: lookup, validate, queue, instrument, hook, dispatch. All imports, struct fields, and design documents are updated to match.

## Alternatives Considered

### Alternative 1: dispatcher

- **Pros**: Emphasizes the routing/dispatch role, shorter name
- **Cons**: Conflicts conceptually with the engine's dispatch layer (`Engine.Dispatch*` methods), which handles shard locking -- a different kind of dispatch
- **Why not**: Two "dispatcher" concepts in the same codebase invites confusion about which layer is responsible for what

### Alternative 2: processor

- **Pros**: Accurate -- it processes commands
- **Cons**: Too generic; `CommandProcessor` doesn't convey the staged nature of the work or distinguish it from the handlers that do the actual command logic
- **Why not**: "Processor" could describe almost any component; "pipeline" is more specific about the shape of the processing

## Consequences

### Positive

- Package name matches what the code actually does -- new contributors can navigate by name
- `Pipeline` as a type name is shorter and more descriptive than `BaseEvaluator`
- Design docs and architecture diagrams become more accurate

### Negative

- Every import of `pkg/evaluator` (3 production files, 8 test files) must be updated
- 23 design documents (PlantUML + markdown) reference "Evaluator" and need renaming
- Git blame history becomes harder to follow across the rename commit

### Risks

- **Risk**: External references (wiki links, bookmarks) to `pkg/evaluator` break. **Mitigation**: The old directory is deleted in a single commit; `git log --follow` tracks the rename. No external API surface is affected since `pkg/evaluator` is internal.
