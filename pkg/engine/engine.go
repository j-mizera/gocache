// Package engine serialises all cache mutations through one goroutine
// per shard. Callers submit work via Dispatch / DispatchWithResult; the
// engine routes the work to the goroutine owning the shard the command's
// key hashes to, where it runs under that shard's write lock.
//
// Today's production configuration is N=1 (single shard, single engine
// goroutine, identical behaviour to the pre-shard cache); the routing
// machinery is in place so a follow-up can bump N without touching call
// sites.
package engine

import (
	"context"
	"errors"
	"sync"

	"gocache/api/logger"
	"gocache/pkg/cache"
)

// ErrEngineStopped is returned when the engine has been stopped before
// the work could be executed.
var ErrEngineStopped = errors.New("engine stopped")

// cmdChanCapacity sizes each shard's buffered submission channel.
const cmdChanCapacity = 100

// Command is the unit of work the engine executes under a shard's write lock.
type Command struct {
	Execute func() any
	ResChan chan any
}

// resChanPool recycles the per-call result channel sendAndWait creates
// for every dispatch. Same Put-on-success-only safety rule as before:
// once the engine goroutine takes ownership of the channel via the
// receive on cmdChan, sendAndWait must NOT return the channel to the
// pool from a stop / cancel branch — the engine may still write to it.
var resChanPool = sync.Pool{
	New: func() any { return make(chan any, 1) },
}

// shardEngine is one goroutine + cmdChan pair, owning a single Shard.
// Each shard's goroutine runs handlers under that shard's write lock.
type shardEngine struct {
	shard    *cache.Shard
	cmdChan  chan Command
	stopChan chan struct{}
}

// Engine fans out commands to N shard engines. The Dispatch* methods
// route by key hash so the right shard's goroutine processes the work.
type Engine struct {
	cache    *cache.Cache
	shards   []*shardEngine
	stopOnce sync.Once
}

// New constructs an Engine with one goroutine per cache shard.
func New(c *cache.Cache) *Engine {
	e := &Engine{cache: c, shards: make([]*shardEngine, c.ShardCount())}
	for i := range e.shards {
		e.shards[i] = &shardEngine{
			shard:    c.ShardByIndex(i),
			cmdChan:  make(chan Command, cmdChanCapacity),
			stopChan: make(chan struct{}),
		}
	}
	return e
}

// Run launches one goroutine per shard. Returns immediately.
func (e *Engine) Run() {
	logger.InfoNoCtx().Int("shards", len(e.shards)).Msg("engine dispatch loop started")
	for _, se := range e.shards {
		go se.run()
	}
}

// Stop signals every shard goroutine to exit. Safe to call multiple times.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		logger.InfoNoCtx().Msg("engine stop signal received")
		for _, se := range e.shards {
			close(se.stopChan)
		}
	})
}

func (se *shardEngine) run() {
	for {
		select {
		case cmd := <-se.cmdChan:
			se.shard.Lock()
			res := cmd.Execute()
			se.shard.Unlock()
			cmd.ResChan <- res
		case <-se.stopChan:
			return
		}
	}
}

// sendAndWait submits fn to the named shard's goroutine and blocks for its
// result. The Put rules:
//   - submit-stage stop/cancel: engine never received the Command, so the
//     channel is still privately owned by sendAndWait → safe to Put back.
//   - successful receive: the buffer slot has just been drained → safe to
//     Put back.
//   - wait-stage stop/cancel: engine already owns the write end and may
//     write to it before observing stop → DO NOT Put back. The channel
//     becomes garbage; sync.Pool just won't see it.
func (e *Engine) sendAndWait(ctx context.Context, shard int, fn func() any) (any, error) {
	se := e.shards[shard]
	resChan := resChanPool.Get().(chan any)
	select {
	case se.cmdChan <- Command{Execute: fn, ResChan: resChan}:
	case <-se.stopChan:
		resChanPool.Put(resChan)
		return nil, ErrEngineStopped
	case <-ctx.Done():
		resChanPool.Put(resChan)
		return nil, ctx.Err()
	}
	select {
	case res := <-resChan:
		resChanPool.Put(resChan)
		return res, nil
	case <-se.stopChan:
		// Engine owns the channel — orphan it.
		return nil, ErrEngineStopped
	case <-ctx.Done():
		// Engine owns the channel — orphan it.
		return nil, ctx.Err()
	}
}

// Dispatch submits fn to the engine goroutine owning shard 0 and blocks
// until it runs. Use DispatchToShard when the caller knows the routing.
// Today (N=1) the two are equivalent; at N>1 this is the legacy multi-
// shard path used by snapshot save / cleanup workers and is the
// transition point where cross-shard coordination needs to be added.
func (e *Engine) Dispatch(ctx context.Context, fn func()) error {
	_, err := e.sendAndWait(ctx, 0, func() any {
		fn()
		return nil
	})
	return err
}

// DispatchWithResult is Dispatch with a return value. Same shard-0
// routing as Dispatch.
func (e *Engine) DispatchWithResult(ctx context.Context, fn func() any) (any, error) {
	return e.sendAndWait(ctx, 0, fn)
}

// DispatchToShard routes fn to the goroutine owning the named shard.
// Used by per-key handlers once the caller has computed the shard from
// the command's key.
func (e *Engine) DispatchToShard(ctx context.Context, shard int, fn func() any) (any, error) {
	if shard < 0 || shard >= len(e.shards) {
		return nil, errors.New("engine: shard index out of range")
	}
	return e.sendAndWait(ctx, shard, fn)
}

// ShardCount exposes the underlying cache's shard count for callers that
// need to mirror the routing (the evaluator does this when computing
// the destination shard from a command's key).
func (e *Engine) ShardCount() int { return len(e.shards) }
