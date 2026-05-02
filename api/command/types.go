// Package command provides shared types for command handling in GoCache.
//
// This package lives in api/ and has zero dependencies on server internals.
// Both the plugin SDK and server-side code import these types.
package command

// Result holds the return value or error from a command handler.
type Result struct {
	Value any
	Err   error
}

// Spec defines the minimum and maximum number of arguments a command
// accepts (not counting the command name itself). Max == -1 means unlimited.
//
// ReadOnly marks commands whose handlers do not mutate cache state.
// The evaluator routes such commands through a read-lock-only path that
// bypasses the engine queue, gaining significant pipelined-read
// throughput. Commands that look read-like but have side effects
// (GETSET, GETDEL, GETEX, BLPOP/BRPOP, LPOP/RPOP, SPOP) must NOT be
// marked ReadOnly. When in doubt: mark false; the worst case is the
// command takes the engine path and runs slightly slower.
//
// KeyArgIndex and MultiKey describe how a command's keys map onto the
// engine's keyspace. The sharded engine uses these to route commands to
// the shard owning the key (single-key) or to the cross-shard
// coordination path (multi-key). Today the production engine is a
// single goroutine and ignores both fields; the metadata is in place
// so a future sharded engine can route without per-handler reflection.
//
//   KeyArgIndex >= 0 — a single-key command; the key is at Args[KeyArgIndex].
//                      Default 0 (the most common case: first arg is key).
//   KeyArgIndex == -1 — a keyless command (PING, AUTH, HELLO, MULTI,
//                       DISCARD, INFO, etc.). Has no cache key.
//   MultiKey == true — a command that touches multiple keys or every key
//                      (MGET, MSET, RENAME, KEYS, SCAN, SINTER, EXEC,
//                      WATCH, UNWATCH, DBSIZE, RANDOMKEY, FLUSHDB,
//                      DELETE-with-N-args, BLPOP-with-N-keys, snapshot
//                      save/load). KeyArgIndex is ignored when MultiKey
//                      is true.
type Spec struct {
	Min         int
	Max         int
	ReadOnly    bool
	KeyArgIndex int
	MultiKey    bool
}
