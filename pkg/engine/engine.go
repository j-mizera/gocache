// Package engine serialises all cache mutations through per-shard
// mutexes. Callers submit work via the Dispatch* methods; the engine
// acquires the target shard lock(s), runs the work, and releases.
// No goroutines, channels, or result queues are involved — the calling
// goroutine holds the lock directly.
package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"gocache/api/logger"
	"gocache/pkg/cache"
)

// ErrEngineStopped is returned when the engine has been stopped before
// the work could be executed.
var ErrEngineStopped = errors.New("engine stopped")

// Engine is a stateless dispatch layer over per-shard mutexes.
type Engine struct {
	cache    *cache.Cache
	stopped  atomic.Bool
	stopOnce sync.Once
}

// New constructs an Engine backed by the given cache.
func New(c *cache.Cache) *Engine {
	return &Engine{cache: c}
}

// Stop marks the engine as stopped. Safe to call multiple times.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		logger.InfoNoCtx().Msg("engine stop signal received")
		e.stopped.Store(true)
	})
}

// Dispatch acquires every shard's write lock in shard-id order and runs
// fn under all of them, returning when fn completes. Used by callers
// that need a globally-consistent view of the cache: snapshot worker,
// cleanup worker, FLUSHDB, and other full-keyspace paths.
func (e *Engine) Dispatch(ctx context.Context, fn func()) error {
	if e.stopped.Load() {
		return ErrEngineStopped
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.cache.Lock()
	defer e.cache.Unlock()
	if e.stopped.Load() {
		return ErrEngineStopped
	}
	fn()
	return nil
}

// DispatchWithResult is Dispatch with a return value. Used by MULTI/EXEC
// (bulk lock for atomic batch) and full-keyspace read commands (KEYS, SCAN).
func (e *Engine) DispatchWithResult(ctx context.Context, fn func() any) (any, error) {
	if e.stopped.Load() {
		return nil, ErrEngineStopped
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.cache.Lock()
	defer e.cache.Unlock()
	if e.stopped.Load() {
		return nil, ErrEngineStopped
	}
	return fn(), nil
}

// DispatchToShard acquires the named shard's write lock, runs fn, and
// releases. Used by per-key handlers (SET, GET, INCR, LPUSH, HSET, SADD,
// and every other single-key command).
func (e *Engine) DispatchToShard(ctx context.Context, shard int, fn func() any) (any, error) {
	if e.stopped.Load() {
		return nil, ErrEngineStopped
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := e.cache.ShardByIndex(shard)
	s.Lock()
	defer s.Unlock()
	if e.stopped.Load() {
		return nil, ErrEngineStopped
	}
	return fn(), nil
}

// DispatchToShards acquires the listed shards' write locks in
// ascending order, runs fn under the umbrella, and releases them.
// Used by multi-key handlers that touch a known subset of shards
// (MGET, MSET, RENAME, etc.).
func (e *Engine) DispatchToShards(ctx context.Context, shardIDs []int, fn func() any) (any, error) {
	if e.stopped.Load() {
		return nil, ErrEngineStopped
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	release := e.cache.LockShards(shardIDs, true)
	defer release()
	if e.stopped.Load() {
		return nil, ErrEngineStopped
	}
	return fn(), nil
}

// ShardCount exposes the underlying cache's shard count for callers that
// need to mirror the routing (the pipeline does this when computing
// the destination shard from a command's key).
func (e *Engine) ShardCount() int { return e.cache.ShardCount() }
