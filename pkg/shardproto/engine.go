package shardproto

import (
	"context"
	"errors"
	"sync"
)

// ErrEngineStopped is returned by Dispatch when the target engine has
// been stopped before its goroutine could pick up the work. Mirrors
// pkg/engine.ErrEngineStopped.
var ErrEngineStopped = errors.New("shardproto: engine stopped")

// cmdChanCapacity is the per-shard buffered submission depth. The same
// value as pkg/engine; small buffer smooths microbursts without allowing
// unbounded queueing.
const cmdChanCapacity = 100

type command struct {
	execute func() any
	result  chan any
}

// Engine runs one goroutine per shard. Each goroutine reads from its own
// cmdChan and executes work under the shard's mutex (acquired by the
// handler closure passed to Dispatch).
//
// The connection goroutine submits commands by routing on key hash to
// the owning shard's cmdChan and waiting on a per-call result channel.
// The same channel-hop pattern as the production engine, but with N
// independent queues instead of one.
type Engine struct {
	cache    *Cache
	shards   []*shardEngine
	stopOnce sync.Once
}

type shardEngine struct {
	cmdChan  chan command
	stopChan chan struct{}
	pool     sync.Pool
}

// NewEngine builds N shard engines, one per shard in c. Call Run to
// start the goroutines and Stop to shut them down.
func NewEngine(c *Cache) *Engine {
	e := &Engine{cache: c, shards: make([]*shardEngine, c.ShardCount())}
	for i := range e.shards {
		e.shards[i] = &shardEngine{
			cmdChan:  make(chan command, cmdChanCapacity),
			stopChan: make(chan struct{}),
			pool:     sync.Pool{New: func() any { return make(chan any, 1) }},
		}
	}
	return e
}

// Run launches one goroutine per shard. Returns immediately.
func (e *Engine) Run() {
	for _, se := range e.shards {
		go se.run()
	}
}

// Stop signals every shard's goroutine to exit. Safe to call multiple
// times. Pending submissions return ErrEngineStopped.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		for _, se := range e.shards {
			close(se.stopChan)
		}
	})
}

// Cache returns the underlying cache for direct shard access (used by
// the inline handlers in server.go).
func (e *Engine) Cache() *Cache { return e.cache }

// Dispatch routes fn to the shard owning key and blocks until it runs
// or the engine stops or ctx is cancelled. Returns the handler's result
// or an error.
func (e *Engine) Dispatch(ctx context.Context, key string, fn func(*Shard) any) (any, error) {
	idx := e.cache.shardIndex(key)
	se := e.shards[idx]
	shard := e.cache.shards[idx]

	resChan := se.pool.Get().(chan any)
	cmd := command{
		execute: func() any { return fn(shard) },
		result:  resChan,
	}
	select {
	case se.cmdChan <- cmd:
	case <-se.stopChan:
		se.pool.Put(resChan)
		return nil, ErrEngineStopped
	case <-ctx.Done():
		se.pool.Put(resChan)
		return nil, ctx.Err()
	}
	select {
	case res := <-resChan:
		se.pool.Put(resChan)
		return res, nil
	case <-se.stopChan:
		// Engine owns the channel — orphan it. Same Put-on-success-only rule
		// as pkg/engine.sendAndWait.
		return nil, ErrEngineStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (se *shardEngine) run() {
	for {
		select {
		case cmd := <-se.cmdChan:
			cmd.result <- cmd.execute()
		case <-se.stopChan:
			return
		}
	}
}
