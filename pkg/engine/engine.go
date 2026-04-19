// Package engine serialises all cache mutations through a single goroutine.
//
// Callers submit work via Dispatch or DispatchWithResult. The engine goroutine
// executes the work under the cache lock and returns the result through a
// per-call channel. Callers pass a context.Context so shutdown and timeouts
// propagate correctly; if the context expires before the work starts, the
// engine returns ctx.Err() without executing the function. If the engine is
// stopped before the work starts, ErrEngineStopped is returned.
package engine

import (
	"context"
	"errors"
	"sync"

	"gocache/api/logger"
	"gocache/pkg/cache"
)

// ErrEngineStopped is returned by Dispatch/DispatchWithResult when the engine
// has been stopped before the work could be executed.
var ErrEngineStopped = errors.New("engine stopped")

// cmdChanCapacity sizes the buffered submission channel. Writers block when
// the engine goroutine is behind; a modest buffer smooths microbursts without
// allowing unbounded queueing.
const cmdChanCapacity = 100

type Command struct {
	Execute func() any
	ResChan chan any
}

type Engine struct {
	cache    *cache.Cache
	cmdChan  chan Command
	stopChan chan struct{}
	stopOnce sync.Once
}

func New(c *cache.Cache) *Engine {
	return &Engine{
		cache:    c,
		cmdChan:  make(chan Command, cmdChanCapacity),
		stopChan: make(chan struct{}),
	}
}

func (e *Engine) Run() {
	logger.InfoNoCtx().Msg("engine dispatch loop started")
	for {
		select {
		case cmd := <-e.cmdChan:
			e.cache.Lock()
			res := cmd.Execute()
			e.cache.Unlock()
			cmd.ResChan <- res
		case <-e.stopChan:
			return
		}
	}
}

func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		logger.InfoNoCtx().Msg("engine stop signal received")
		close(e.stopChan)
	})
}

// sendAndWait submits fn via the engine's command channel and blocks for its
// result. Shared implementation behind Dispatch and DispatchWithResult.
func (e *Engine) sendAndWait(ctx context.Context, fn func() any) (any, error) {
	resChan := make(chan any, 1)
	select {
	case e.cmdChan <- Command{Execute: fn, ResChan: resChan}:
	case <-e.stopChan:
		return nil, ErrEngineStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case res := <-resChan:
		return res, nil
	case <-e.stopChan:
		return nil, ErrEngineStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Dispatch submits fn to the engine and blocks until it runs (or the engine
// stops, or ctx is cancelled). Returns nil on success, ErrEngineStopped if
// the engine stopped before execution, or ctx.Err() if ctx was cancelled.
func (e *Engine) Dispatch(ctx context.Context, fn func()) error {
	_, err := e.sendAndWait(ctx, func() any {
		fn()
		return nil
	})
	return err
}

// DispatchWithResult submits fn to the engine and blocks until it runs.
// Returns (result, nil) on success, (nil, ErrEngineStopped) if the engine
// stopped before execution, or (nil, ctx.Err()) if ctx was cancelled.
func (e *Engine) DispatchWithResult(ctx context.Context, fn func() any) (any, error) {
	return e.sendAndWait(ctx, fn)
}
