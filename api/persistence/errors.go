package persistence

import "errors"

// ErrSinkFatal wraps Sink.Apply errors that the sink considers
// non-recoverable. The coordinator quarantines fatal sinks (stops
// dispatching to them, surfaces the failure, continues serving traffic
// against the remaining healthy sinks). Transient errors that don't wrap
// ErrSinkFatal are retried under a backoff schedule.
//
// Use with errors.Is / errors.As at the boundary:
//
//	if err := sink.Apply(ctx, batch); err != nil {
//	    if errors.Is(err, persistence.ErrSinkFatal) {
//	        // quarantine
//	    }
//	}
var ErrSinkFatal = errors.New("persistence: sink fatal — quarantine")

// ErrInvalidBootMode is returned by the coordinator when a Source returns
// a BootResult with an unrecognised Mode. Closed enums are checked at
// boot time so unknown values fail fast rather than getting silently
// dropped.
var ErrInvalidBootMode = errors.New("persistence: source returned invalid BootMode")

// ErrNoSnapshotter is returned by Coordinator.Snapshot when no Snapshotter
// has been registered. Distinct from "snapshot succeeded with zero entries"
// — calling code (SAVE handler, scheduled worker, shutdown path) needs to
// distinguish "configured but nothing to save" from "not configured at all"
// because the second is a config bug, not a steady-state outcome.
var ErrNoSnapshotter = errors.New("persistence: no snapshotter registered")
