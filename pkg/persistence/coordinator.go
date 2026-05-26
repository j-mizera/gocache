package persistence

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"gocache/commons/logger"
	apipersistence "gocache/api/persistence"
)

// Coordinator orchestrates the persistence Source and Sinks. On boot it
// asks the registered Source for recovery state and applies it to the
// cache; during runtime it allocates LSNs and group-commit dispatches
// mutations to Sinks via per-sink flush loops.
//
// Lifecycle:
//  1. New(source, sinks...) — register the contract participants.
//  2. BootInto(ctx, cache)  — recover state from Source.
//  3. Start(ctx)            — spawn the per-sink flush loops.
//  4. Emit / AllocateAndEmit — runtime mutation feed.
//  5. Stop(ctx)             — drain inflight, Close each Sink, exit.
type Coordinator struct {
	source apipersistence.Source
	sinks  []apipersistence.Sink

	// store is the coordinator's abstract view of the cache. Set via
	// SetStore before BootInto / Snapshot. The coordinator never imports
	// pkg/cache — all cache access flows through this interface.
	store apipersistence.CacheStore

	// snapshotter is the registered point-in-time snapshot writer.
	// Optional — Coordinator.Snapshot returns ErrNoSnapshotter when nil.
	// Guarded by snapshotterMu so RegisterSnapshotter can race with
	// in-flight Snapshot calls during config reload without tearing.
	snapshotterMu sync.RWMutex
	snapshotter   apipersistence.Snapshotter

	// lsn is the most recently allocated LSN. AllocateLSN increments it;
	// boot reads it from the snapshot and seeds the runtime allocator.
	lsn atomic.Uint64

	// activeSinks counts healthy Sinks. Read by HasSinks (atomic load)
	// on the cache write hot path; written on Start (per-sink increment)
	// and on quarantine (decrement). Initial value zero — HasSinks is
	// false until Start runs.
	activeSinks atomic.Int32

	// feed holds one sinkChannel per registered Sink. Populated in Start.
	feed []*sinkChannel

	// lastSaveUnix records the Unix timestamp (seconds) of the most recent
	// successful Snapshot() call. Exposed via LastSaveUnix() for LASTSAVE.
	lastSaveUnix atomic.Int64

	// droppedMutations counts mutations dropped because a sink buffer was full.
	droppedMutations atomic.Uint64

	// stop is closed by Stop to signal every per-sink flush loop to exit.
	stop      chan struct{}
	stopOnce  sync.Once
	startOnce sync.Once
	started   atomic.Bool
}

// New returns a coordinator. source may be nil — in that case Boot returns
// BootModeInitial without error and the coordinator runs in pass-through
// mode (no recovery, LSN starts at zero). Sinks may be empty; HasSinks
// returns false and the dispatcher's emission fast-path skips entirely.
func New(source apipersistence.Source, sinks ...apipersistence.Sink) *Coordinator {
	return &Coordinator{
		source: source,
		sinks:  sinks,
		stop:   make(chan struct{}),
	}
}

// Start spawns one flush goroutine per registered Sink and arms HasSinks
// so the cache write path begins emitting mutations. Idempotent — a
// second Start call is a no-op. Must be called after BootInto so the
// runtime resumes from the recovered LSN cursor.
//
// The provided ctx is used as the parent for all Sink Apply / Close
// invocations; it should outlive the coordinator (typically a request-
// scoped server-lifetime context). Stop signals shutdown via the
// coordinator's internal channel — ctx cancellation is observed by
// Apply / Close calls but does not by itself stop the flush loops.
func (c *Coordinator) Start(ctx context.Context) {
	c.startOnce.Do(func() {
		c.feed = make([]*sinkChannel, 0, len(c.sinks))
		for _, s := range c.sinks {
			// onQuarantine decrements activeSinks so HasSinks reports the
			// post-quarantine state; producer-side fast-path then skips
			// emission to dead sinks. Producers reaching the channel
			// before activeSinks settles are absorbed by the consume()
			// quarantine check inside the run loop.
			sc := newSinkChannel(s, defaultBufferSize, func() {
				c.activeSinks.Add(-1)
			})
			c.feed = append(c.feed, sc)
			c.recordSinkActive()
			sc.startSinkLoop(ctx, c.stop)
			logger.Info(ctx).
				Str("sink", s.Name()).
				Str("fsync", s.FsyncPolicy().String()).
				Int("buffer", sc.bufferSize).
				Msg("persistence: sink started")
		}
		c.started.Store(true)
	})
}

// Stop signals every per-sink flush loop to drain its inflight buffer,
// call Sink.Apply on whatever's left, then call Sink.Close. Returns
// once all flush goroutines have exited. Idempotent — a second Stop
// call returns immediately.
//
// Stop blocks the caller until drain completes. Callers with a deadline
// concern should wrap with their own timeout; the coordinator does not
// impose one because durable shutdown ("flush everything") is the more
// important property than bounded shutdown latency.
func (c *Coordinator) Stop(_ context.Context) {
	c.stopOnce.Do(func() {
		close(c.stop)
		for _, sc := range c.feed {
			sc.wg.Wait()
		}
		c.started.Store(false)
		c.activeSinks.Store(0)
	})
}

// AllocateLSN returns the next LSN. Monotonically increasing across all
// callers (atomic add). The caller must use this for any mutation that's
// about to be persisted. ZeroLSN is never returned.
func (c *Coordinator) AllocateLSN() apipersistence.LSN {
	return apipersistence.LSN(c.lsn.Add(1))
}

// CurrentLSN returns the most recently allocated LSN, or ZeroLSN if no
// LSN has been allocated yet.
func (c *Coordinator) CurrentLSN() apipersistence.LSN {
	return apipersistence.LSN(c.lsn.Load())
}

// SetLSN seeds the LSN cursor — used internally on boot and by tests that
// need to install a known cursor. Production code should prefer Boot to
// derive the cursor from recovery state.
func (c *Coordinator) SetLSN(lsn apipersistence.LSN) {
	c.lsn.Store(uint64(lsn))
}

// LastSaveUnix returns the Unix timestamp (seconds) of the most recent
// successful Snapshot() call, or 0 if no save has completed.
func (c *Coordinator) LastSaveUnix() int64 { return c.lastSaveUnix.Load() }

// DroppedMutations returns the total number of mutations dropped because
// a sink buffer was full.
func (c *Coordinator) DroppedMutations() uint64 { return c.droppedMutations.Load() }

// Source returns the registered source (may be nil).
func (c *Coordinator) Source() apipersistence.Source { return c.source }

// Sinks returns the registered sinks (may be empty).
func (c *Coordinator) Sinks() []apipersistence.Sink { return c.sinks }

// SetStore wires the cache abstraction used by BootInto and Snapshot.
// Must be called before either method. Passing nil is valid only when
// neither boot nor snapshot will be invoked (pure pass-through mode).
func (c *Coordinator) SetStore(s apipersistence.CacheStore) { c.store = s }

// RegisterSnapshotter installs the point-in-time snapshot writer. Safe to
// call before or after Start. A nil argument clears the registration.
// Replacing an existing snapshotter is allowed — useful when config
// reload swaps the on-disk format (e.g. gob → v1 binary).
func (c *Coordinator) RegisterSnapshotter(s apipersistence.Snapshotter) {
	c.snapshotterMu.Lock()
	c.snapshotter = s
	c.snapshotterMu.Unlock()
}

// Snapshotter returns the registered snapshotter (may be nil).
func (c *Coordinator) Snapshotter() apipersistence.Snapshotter {
	c.snapshotterMu.RLock()
	defer c.snapshotterMu.RUnlock()
	return c.snapshotter
}

// Snapshot writes a point-in-time dump to the registered snapshotter.
// Iterates the cache via the CacheStore interface, materialises a
// SnapshotSource backed by the captured slice, then delegates to
// snapshotter.SaveSnapshot.
//
// Returns ErrNoSnapshotter when none is registered — callers can choose
// to treat that as fatal (SAVE command) or as a no-op (scheduled worker
// when persistence is disabled).
func (c *Coordinator) Snapshot(ctx context.Context) error {
	s := c.Snapshotter()
	if s == nil {
		return apipersistence.ErrNoSnapshotter
	}
	if c.store == nil {
		return fmt.Errorf("persistence: coordinator store not set")
	}
	if seeder, ok := s.(apipersistence.LSNSeeder); ok {
		seeder.SetLSN(c.CurrentLSN())
	}
	entries := c.store.CaptureSnapshot()
	src := &sliceSnapshotSource{entries: entries}
	if err := s.SaveSnapshot(ctx, src); err != nil {
		return err
	}
	c.lastSaveUnix.Store(time.Now().Unix())
	return nil
}


// sliceSnapshotSource is a SnapshotSource backed by a pre-captured slice.
// Forward-only single-pass — Next walks the slice and returns io.EOF once
// exhausted. Internal to the coordinator; snapshotters never type-assert
// against it.
type sliceSnapshotSource struct {
	entries []apipersistence.SnapshotEntry
	cursor  int
}

func (s *sliceSnapshotSource) Next(_ context.Context) (apipersistence.SnapshotEntry, error) {
	if s.cursor >= len(s.entries) {
		return apipersistence.SnapshotEntry{}, io.EOF
	}
	e := s.entries[s.cursor]
	s.cursor++
	return e, nil
}

// BootInto drives the recovery path: fetches the BootResult from the
// Source, applies it to the CacheStore, and seeds the LSN cursor. The
// returned LSN is the resume point for runtime mutation allocation.
//
// For BootModeInitial (or nil source), returns (ZeroLSN, nil) without
// touching the cache. For BootModeSnapshot, the snapshot's at-LSN seeds
// the cursor and snapshot entries are loaded. For BootModeReplay, each
// replayed Mutation advances the cursor; the final LSN is the resume
// point.
//
// On error, the cache may be in a partially-loaded state — callers should
// abort startup rather than continue serving traffic against a torn cache.
func (c *Coordinator) BootInto(ctx context.Context) (apipersistence.LSN, error) {
	if c.source == nil {
		logger.Debug(ctx).Msg("persistence coordinator: no source registered, BootMode=initial")
		return apipersistence.ZeroLSN, nil
	}

	boot, err := c.source.Boot(ctx)
	if err != nil {
		return 0, fmt.Errorf("source %s: %w", c.source.Name(), err)
	}
	defer func() {
		if cerr := boot.Close(); cerr != nil {
			logger.Warn(ctx).Err(cerr).Str("source", c.source.Name()).Msg("boot result close error")
		}
	}()

	switch boot.Mode {
	case apipersistence.BootModeInitial:
		logger.Debug(ctx).Str("source", c.source.Name()).Msg("persistence: nothing to recover")
		return apipersistence.ZeroLSN, nil

	case apipersistence.BootModeSnapshot:
		if boot.Snapshot == nil {
			return 0, fmt.Errorf("source %s declared BootModeSnapshot with nil Snapshot", c.source.Name())
		}
		loaded, err := c.loadSnapshotInto(ctx, boot.Snapshot)
		if err != nil {
			return 0, fmt.Errorf("source %s snapshot: %w", c.source.Name(), err)
		}
		c.SetLSN(boot.LSN)
		logger.Info(ctx).
			Str("source", c.source.Name()).
			Int("entries", loaded).
			Int64("lsn", int64(boot.LSN)).
			Msg("persistence: snapshot loaded")
		return boot.LSN, nil

	case apipersistence.BootModeReplay:
		if boot.Replay == nil {
			return 0, fmt.Errorf("source %s declared BootModeReplay with nil Replay", c.source.Name())
		}
		last, applied, err := replayInto(ctx, boot.Replay, c.store)
		if err != nil {
			return 0, fmt.Errorf("source %s replay: %w", c.source.Name(), err)
		}
		c.SetLSN(last)
		logger.Info(ctx).
			Str("source", c.source.Name()).
			Int("mutations", applied).
			Int64("lsn", int64(last)).
			Msg("persistence: replay complete")
		return last, nil

	default:
		return 0, fmt.Errorf("%w: %d (source %s)", apipersistence.ErrInvalidBootMode, boot.Mode, c.source.Name())
	}
}

// loadSnapshotInto drains a SnapshotIterator into the cache. Returns the
// number of entries loaded.
func (c *Coordinator) loadSnapshotInto(ctx context.Context, it apipersistence.SnapshotIterator) (int, error) {
	if c.store == nil {
		return 0, fmt.Errorf("persistence: coordinator store not set")
	}
	c.store.Clear(ctx)
	loaded := 0
	now := time.Now().UnixNano()
	for {
		e, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			return loaded, nil
		}
		if err != nil {
			return loaded, fmt.Errorf("read entry %d: %w", loaded, err)
		}
		if e.Expiration > 0 && e.Expiration < now {
			continue
		}
		if err := c.store.LoadEntry(ctx, e); err != nil {
			logger.Warn(ctx).Err(err).Str("key", e.Key).Msg("persistence: skipping malformed entry")
			continue
		}
		loaded++
	}
}

// replayInto drains a ReplayIterator, re-executing each mutation against
// the cache via store.ApplyMutation (ADR-0017). Returns the highest LSN
// observed and the number of mutations applied.
func replayInto(ctx context.Context, it apipersistence.ReplayIterator, store apipersistence.CacheStore) (apipersistence.LSN, int, error) {
	var last apipersistence.LSN
	count := 0
	for {
		m, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			return last, count, nil
		}
		if err != nil {
			return last, count, fmt.Errorf("read mutation %d: %w", count, err)
		}
		if m.LSN > last {
			last = m.LSN
		}
		if err := store.ApplyMutation(ctx, m); err != nil {
			logger.Warn(ctx).Err(err).
				Str("op", m.Op).Str("key", m.Key).
				Int64("lsn", int64(m.LSN)).
				Msg("persistence: skipping mutation during replay")
		}
		count++
	}
}

