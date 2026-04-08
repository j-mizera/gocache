package engine

import (
	"gocache/pkg/cache"
	"gocache/pkg/logger"
	"sync"
)

type Command struct {
	Execute func() interface{}
	ResChan chan interface{}
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
		cmdChan:  make(chan Command, 100),
		stopChan: make(chan struct{}),
	}
}

func (e *Engine) Run() {
	logger.Info().Msg("engine dispatch loop started")
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
		logger.Info().Msg("engine stop signal received")
		close(e.stopChan)
	})
}

func (e *Engine) Dispatch(fn func()) {
	resChan := make(chan interface{}, 1)
	select {
	case e.cmdChan <- Command{
		Execute: func() interface{} {
			fn()
			return nil
		},
		ResChan: resChan,
	}:
	case <-e.stopChan:
		return
	}
	select {
	case <-resChan:
	case <-e.stopChan:
	}
}

func (e *Engine) DispatchWithResult(fn func() interface{}) interface{} {
	resChan := make(chan interface{}, 1)
	select {
	case e.cmdChan <- Command{
		Execute: fn,
		ResChan: resChan,
	}:
	case <-e.stopChan:
		return nil
	}
	select {
	case res := <-resChan:
		return res
	case <-e.stopChan:
		return nil
	}
}
