package persistence

import "context"

// Sink consumes the ongoing mutation feed during steady-state operation.
// Sinks see writes after the cache has committed them — they cannot veto
// or modify the write. The coordinator group-commits mutations (1ms ‖ 64KB
// triggers, see ADR-0003) before calling Apply, so each Apply receives a
// batch the Sink can amortise its per-flush cost over.
//
// Sinks are NOT wired in this PR (feat/persistence-contract). The interface
// is defined so the contract is stable; the coordinator's Sink dispatch
// path lands in feat/persistence-mutation-feed.
type Sink interface {
	// Name identifies the sink for logging, error attribution, and
	// command-routing decisions (which Sink answers BGREWRITEAOF, etc.).
	Name() string

	// FsyncPolicy returns the durability policy this sink runs under.
	// The coordinator uses this to dispatch fsyncs after Apply; the sink
	// should not call fsync itself unless it has a sink-specific reason
	// to (e.g. a file rotation point). See ADR-0003.
	FsyncPolicy() FsyncPolicy

	// Apply receives a batch of mutations in LSN order. The batch is
	// non-empty. The coordinator calls Apply serially per Sink (no
	// concurrent Apply calls on the same Sink instance), so Sinks need
	// not be internally synchronised across Apply calls.
	//
	// Errors are surfaced via the coordinator's health channel. Sinks
	// that cannot recover from a write failure should return an error
	// that wraps a sentinel (see ErrSinkFatal); the coordinator may then
	// quarantine the sink. Transient errors (e.g. disk full) are
	// retried by the coordinator under a backoff schedule.
	Apply(ctx context.Context, batch []Mutation) error

	// Close signals shutdown. The coordinator drains any in-flight batch
	// to this sink and calls Close exactly once. Sinks should flush any
	// internal buffers, close file handles, and release network
	// connections. Returning an error from Close is logged but does not
	// abort shutdown — by Close time the cache state is no longer being
	// modified.
	Close(ctx context.Context) error
}
