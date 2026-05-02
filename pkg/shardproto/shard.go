// Package shardproto is a throwaway prototype that validates per-shard
// locking against the diagnosis baseline captured in PR #38. The
// production cache stays untouched; this package mirrors the minimum
// surface needed to run pipelined GET/SET/HSET over TCP and compare
// throughput at varying shard counts.
//
// Constraints (deliberate; do not extend without reason):
//   - Single-key path only — GET, SET, HSET. No multi-key commands.
//   - No transactions, no WATCH, no persistence, no eviction.
//   - Plain map[string]any values (string for SET, map[string]string for HSET).
//     Slab allocator integration is a production-implementation concern.
//   - Engine ownership: each shard has its own goroutine and command channel,
//     and the shard's mutex is acquired only inside that goroutine. This
//     mirrors the production engine's "single writer per cache" invariant
//     applied per shard.
//
// The package name has a "proto" suffix so future production code never
// imports it accidentally — this is measurement scaffolding, not an API.
package shardproto

import "sync"

// Shard owns one slice of the keyspace. The mutex guards items; only the
// shard's own engine goroutine acquires it on the write path. Reads on the
// fast path also acquire it (RLock) but stay inside the engine goroutine.
type Shard struct {
	mu    sync.RWMutex
	items map[string]any
}

func newShard() *Shard {
	return &Shard{items: make(map[string]any)}
}
