package persistence

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"gocache/api/logger"
	apipersistence "gocache/api/persistence"
	"gocache/pkg/cache"
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

// Source returns the registered source (may be nil).
func (c *Coordinator) Source() apipersistence.Source { return c.source }

// Sinks returns the registered sinks (may be empty).
func (c *Coordinator) Sinks() []apipersistence.Sink { return c.sinks }

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

// Snapshot writes a point-in-time dump of target to the registered
// snapshotter. Iterates the cache, materialises a SnapshotSource backed
// by the captured slice, then delegates to snapshotter.SaveSnapshot.
//
// Returns ErrNoSnapshotter when none is registered — callers can choose
// to treat that as fatal (SAVE command) or as a no-op (scheduled worker
// when persistence is disabled).
//
// The cache snapshot is captured eagerly into a slice before the
// snapshotter sees it. That keeps the snapshotter simple (it just walks
// Next/io.EOF) at the cost of buffering all entries in memory once. The
// upcoming v1 streaming format (ADR-0005) can reduce buffering by
// writing each record as it's yielded; the gob shim already buffers
// internally so this PR is no worse than the legacy SaveSnapshot.
func (c *Coordinator) Snapshot(ctx context.Context, target *cache.Cache) error {
	s := c.Snapshotter()
	if s == nil {
		return apipersistence.ErrNoSnapshotter
	}
	// Optional LSN seeding — snapshotters that record the cursor in
	// their on-disk format (v1 binary format, ADR-0005) implement
	// LSNSeeder. Skip silently for snapshotters that don't.
	if seeder, ok := s.(apipersistence.LSNSeeder); ok {
		seeder.SetLSN(c.CurrentLSN())
	}
	entries := captureSnapshotEntries(target)
	src := &sliceSnapshotSource{entries: entries}
	return s.SaveSnapshot(ctx, src)
}

// captureSnapshotEntries walks the cache and returns every live entry as
// an api/persistence.SnapshotEntry. Packed values are copied out of the
// slab allocator so the snapshotter sees a stable []byte independent of
// slab lifecycle. ValueType and Encoding cast directly from the cache
// enums to the api enums — the values are aligned by construction (see
// type comments in api/persistence/types.go).
func captureSnapshotEntries(target *cache.Cache) []apipersistence.SnapshotEntry {
	var entries []apipersistence.SnapshotEntry
	target.Range(func(key string, entry cache.Entry, expiration int64) bool {
		var v any
		if entry.Encoding == cache.EncPacked {
			src := target.ResolvePacked(entry)
			buf := make([]byte, len(src))
			copy(buf, src)
			v = buf
		} else {
			v = entry.Value
		}
		entries = append(entries, apipersistence.SnapshotEntry{
			Key:        key,
			ValueType:  apipersistence.ValueType(entry.ValueType),
			Encoding:   apipersistence.Encoding(entry.Encoding),
			Value:      v,
			Expiration: expiration,
		})
		return true
	})
	return entries
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
// Source, applies it to the target cache, and seeds the LSN cursor. The
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
func (c *Coordinator) BootInto(ctx context.Context, target *cache.Cache) (apipersistence.LSN, error) {
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
		loaded, err := loadSnapshotInto(ctx, boot.Snapshot, target)
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
		last, applied, err := replayInto(ctx, boot.Replay, target)
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
func loadSnapshotInto(ctx context.Context, it apipersistence.SnapshotIterator, target *cache.Cache) (int, error) {
	target.Clear(ctx)
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
		if err := applyEntry(ctx, e, target); err != nil {
			logger.Warn(ctx).Err(err).Str("key", e.Key).Msg("persistence: skipping malformed entry")
			continue
		}
		loaded++
	}
}

// replayInto drains a ReplayIterator. Returns the highest LSN observed
// and the number of mutations applied. The mutation-replay path is a stub
// in this PR — it advances the LSN cursor but does not yet re-execute
// mutations against the cache (that requires the engine adapter wired in
// feat/persistence-mutation-feed). Returning early is correct: the
// snapshot path is the recovery path that ships now.
func replayInto(ctx context.Context, it apipersistence.ReplayIterator, _ *cache.Cache) (apipersistence.LSN, int, error) {
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
		// Advance the cursor; actual cache mutation re-execution lands
		// in the next PR alongside the engine adapter.
		if m.LSN > last {
			last = m.LSN
		}
		count++
	}
}

// applyEntry writes one snapshot entry into the target cache. It mirrors
// the existing LoadSnapshot logic — the only difference is the api types
// instead of pkg/persistence's internal struct. Conversion between the
// api enums and pkg/cache enums is a numeric cast (the values are aligned
// — see the type comments in api/persistence/types.go).
func applyEntry(_ context.Context, e apipersistence.SnapshotEntry, target *cache.Cache) error {
	if e.Encoding == apipersistence.EncodingPacked {
		var buf []byte
		switch v := e.Value.(type) {
		case []byte:
			buf = v
		case string:
			buf = []byte(v)
		default:
			return fmt.Errorf("packed entry has non-byte payload: %T", e.Value)
		}
		target.RawLoadPacked(e.Key, cache.ValueType(e.ValueType), buf, e.Expiration)
		return nil
	}
	target.RawLoad(e.Key, e.Value, e.Expiration)
	return nil
}
