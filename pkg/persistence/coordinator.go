package persistence

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"gocache/api/logger"
	apipersistence "gocache/api/persistence"
	"gocache/pkg/cache"
)

// Coordinator orchestrates the persistence Source and Sinks. On boot, it
// asks the registered Source for recovery state and applies it to the
// cache; during runtime, it will (in feat/persistence-mutation-feed)
// allocate LSNs and group-commit mutations to Sinks.
//
// This PR (feat/persistence-contract) wires the boot path only — Sink
// dispatch is intentionally stubbed so the type-shape change ships
// independently of the engine hot-path change. See the persistence-as-
// plugin plan for the staging.
type Coordinator struct {
	source apipersistence.Source
	sinks  []apipersistence.Sink

	// lsn is the most recently allocated LSN. AllocateLSN increments it;
	// boot reads it from the snapshot and seeds the runtime allocator.
	lsn atomic.Uint64
}

// New returns a coordinator. source may be nil — in that case Boot returns
// BootModeInitial without error and the coordinator runs in pass-through
// mode (no recovery, LSN starts at zero). Sinks may be empty; they will
// be honoured once the mutation feed is wired in the follow-up PR.
func New(source apipersistence.Source, sinks ...apipersistence.Sink) *Coordinator {
	return &Coordinator{
		source: source,
		sinks:  sinks,
	}
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
