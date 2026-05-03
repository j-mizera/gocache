package persistence

import (
	"context"
	"errors"
	"sync"
	"time"

	"gocache/api/logger"
	apipersistence "gocache/api/persistence"
)

// Group-commit triggers for the per-sink flush loop. Defaults match the
// values cited in ADR-0003 — small enough to keep tail latency bounded,
// large enough to amortise per-flush overhead across batched writes.
const (
	defaultBatchInterval = 1 * time.Millisecond
	defaultBatchBytes    = 64 * 1024
	defaultBufferSize    = 16 * 1024 // mutations per sink before backpressure
	highWaterPercent     = 80        // emergency flush + warn at 80% buffer fill
)

// sinkChannel pairs a Sink with the goroutine driving its group-commit
// flush loop. One per registered Sink. The Coordinator's Emit fans out
// to each sinkChannel in registration order (no broadcast amplification —
// each sink gets its own copy of the mutation reference, but the Mutation
// struct is value-copied so nothing aliases).
type sinkChannel struct {
	sink     apipersistence.Sink
	incoming chan apipersistence.Mutation
	wakeup   chan struct{} // size-trigger / shutdown nudge to break the timer wait
	wg       sync.WaitGroup

	// onQuarantine fires when the Sink returns an ErrSinkFatal-wrapped
	// error from Apply. The Coordinator passes a callback that
	// decrements activeSinks so HasSinks reflects the quarantine state.
	// Once quarantined, the run loop drains incoming without applying
	// (producers don't block) and never calls Apply again.
	onQuarantine func()

	// Capacity values resolved at construction. Kept on the channel so
	// the flush loop can compute high-water without re-reading config.
	bufferSize int
	highWater  int
}

func newSinkChannel(s apipersistence.Sink, bufSize int, onQuarantine func()) *sinkChannel {
	if bufSize <= 0 {
		bufSize = defaultBufferSize
	}
	return &sinkChannel{
		sink:         s,
		incoming:     make(chan apipersistence.Mutation, bufSize),
		wakeup:       make(chan struct{}, 1),
		onQuarantine: onQuarantine,
		bufferSize:   bufSize,
		highWater:    bufSize * highWaterPercent / 100,
	}
}

// startSinkLoop runs the per-sink group-commit flush loop. The loop
// terminates when stop is closed AND the incoming buffer is drained.
//
// Triggers in priority order:
//  1. stop closed AND buffer empty -> drain remaining inflight, Apply,
//     Sink.Close, return.
//  2. buffer at or above high-water -> emergency flush + warn (so users
//     can tune buffer size before hitting the blocking edge).
//  3. accumulated batch bytes >= defaultBatchBytes -> flush.
//  4. defaultBatchInterval elapsed since last flush -> flush.
//
// Apply errors are logged. Errors that wrap api/persistence.ErrSinkFatal
// quarantine the sink (loop exits, future Emit drops on the floor for
// this sink). Transient errors keep the loop alive — the next batch
// retries. (Backoff scheduling is a TODO; today retry is "the next batch
// implicitly".)
func (sc *sinkChannel) startSinkLoop(ctx context.Context, stop <-chan struct{}) {
	sc.wg.Add(1)
	go sc.run(ctx, stop)
}

func (sc *sinkChannel) run(ctx context.Context, stop <-chan struct{}) {
	defer sc.wg.Done()

	ticker := time.NewTicker(defaultBatchInterval)
	defer ticker.Stop()

	batch := make([]apipersistence.Mutation, 0, 64)
	batchBytes := 0
	warnedHighWater := false
	quarantined := false

	flush := func(reason string) {
		if len(batch) == 0 {
			return
		}
		if quarantined {
			// Drop the buffered batch — the sink is dead.
			batch = batch[:0]
			batchBytes = 0
			return
		}
		err := sc.sink.Apply(ctx, batch)
		batch = batch[:0]
		batchBytes = 0
		warnedHighWater = false
		if err == nil {
			return
		}
		logger.Error(ctx).
			Err(err).
			Str("sink", sc.sink.Name()).
			Str("reason", reason).
			Msg("persistence: sink Apply failed")
		if errors.Is(err, apipersistence.ErrSinkFatal) {
			logger.Error(ctx).
				Str("sink", sc.sink.Name()).
				Msg("persistence: sink quarantined (fatal error)")
			quarantined = true
			if sc.onQuarantine != nil {
				sc.onQuarantine()
			}
		}
	}

	consume := func(m apipersistence.Mutation) {
		if quarantined {
			// Sink is dead — drop new mutations on the floor. Producers
			// already gate on HasSinks; a small race window can still
			// land a few stragglers here. Dropping is correct: there's
			// nowhere durable for them to go.
			return
		}
		batch = append(batch, m)
		batchBytes += sizeOfMutation(m)
		if batchBytes >= defaultBatchBytes {
			flush("size-trigger")
			return
		}
		if !warnedHighWater && len(sc.incoming) >= sc.highWater {
			logger.Warn(ctx).
				Str("sink", sc.sink.Name()).
				Int("buffer", sc.bufferSize).
				Int("watermark", sc.highWater).
				Int("len", len(sc.incoming)).
				Msg("persistence: sink buffer at high-water — flushing early; consider increasing buffer size")
			warnedHighWater = true
			flush("high-water")
		}
	}

	for {
		select {
		case m := <-sc.incoming:
			consume(m)
		case <-ticker.C:
			flush("time-trigger")
		case <-sc.wakeup:
			// Size-trigger or stop nudge. The actual decision (drain or
			// shutdown) is in the next loop iteration's select arms.
		case <-stop:
			// Drain remaining inflight, then close.
			for {
				select {
				case m := <-sc.incoming:
					consume(m)
				default:
					flush("shutdown")
					if err := sc.sink.Close(ctx); err != nil {
						logger.Warn(ctx).Err(err).Str("sink", sc.sink.Name()).Msg("persistence: sink Close error")
					}
					return
				}
			}
		}
	}
}

// sizeOfMutation returns an approximate byte cost for group-commit batching
// purposes. The exact wire-format size depends on the sink's encoding;
// what matters here is rough proportionality to keep the 64 KB trigger
// firing at predictable intervals.
func sizeOfMutation(m apipersistence.Mutation) int {
	n := 8 + len(m.Op) + len(m.Key) // LSN (8 bytes) + op + key strings
	for _, a := range m.Args {
		n += len(a)
	}
	return n
}

// Emit pushes m to every registered Sink. Non-blocking when buffers have
// room; blocks the caller when any buffer is full (intentional
// backpressure — see ADR-0003 risks).
//
// Hot-path callers MUST gate this behind HasSinks() to avoid the
// allocation + atomic load on the no-subscriber path. See ADR-0003 and
// the `feedback-keep-work-out-of-locks` design rule.
func (c *Coordinator) Emit(m apipersistence.Mutation) {
	for _, sc := range c.feed {
		sc.incoming <- m
	}
}

// AllocateAndEmit allocates a fresh LSN and emits one mutation. Hot-path
// convenience for the dispatcher — the caller doesn't have to know about
// LSN management. Same backpressure semantics as Emit.
//
// Returns the allocated LSN so callers that want to log it can.
func (c *Coordinator) AllocateAndEmit(op, key string, args [][]byte) apipersistence.LSN {
	lsn := c.AllocateLSN()
	c.Emit(apipersistence.Mutation{LSN: lsn, Op: op, Key: key, Args: args})
	return lsn
}

// HasSinks reports whether any sink is currently registered with this
// coordinator. The check is an atomic load of an int32 sink-count, which
// is the same shape as the existing evaluator.hasAnySink fast-path gate.
//
// Callers in the cache write path use HasSinks as the first check before
// allocating LSN / building Mutation structs / pushing to channels. When
// no sink is registered, the dispatcher's overhead is one nil-check + one
// atomic load — matching pre-feature baseline.
func (c *Coordinator) HasSinks() bool {
	return c.activeSinks.Load() > 0
}

// recordSinkActive increments activeSinks. Called from Start, once per
// registered sink. The producer-side hot path reads activeSinks via
// HasSinks to short-circuit emission when there are no consumers.
// Decrement is exclusively the quarantine path (sinkChannel.onQuarantine).
func (c *Coordinator) recordSinkActive() {
	c.activeSinks.Add(1)
}
