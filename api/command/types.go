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
type Spec struct {
	Min      int
	Max      int
	ReadOnly bool
}
