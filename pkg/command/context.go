// Package command provides shared types for command handling in GoCache.
//
// This package defines the handler function signature, execution context,
// result type, and dispatch helper used by all command handler packages
// (resp/handler, rex).
package command

import (
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/engine"
	"gocache/pkg/transaction"
	"gocache/pkg/watch"
)

// Handler is a function that handles a single cache command.
type Handler func(ctx *Context) Result

// Result holds the return value or error from a command handler.
type Result struct {
	Value interface{}
	Err   error
}

// Context carries all dependencies needed to execute a command.
type Context struct {
	Client           *clientctx.ClientContext
	Op               string
	Args             []string
	InBatch          bool
	Engine           *engine.Engine
	Cache            *cache.Cache
	Transaction      *transaction.Manager
	BlockingRegistry *blocking.Registry
	WatchManager     *watch.Manager

	// Evaluator-level config, set by the pipeline before dispatch.
	SnapshotFile string
	RequirePass  string

	// EvalFn re-enters the evaluator pipeline. Used by EXEC to execute
	// queued commands in a batch. The bool parameter is inBatch.
	EvalFn func(ctx *clientctx.ClientContext, op string, args []string, inBatch bool) Result
}

// Dispatch runs fn either directly (when InBatch is true, meaning the engine
// lock is already held) or through the engine dispatcher. It wraps the result
// into a Result, propagating any error.
func Dispatch(ctx *Context, fn func() interface{}) Result {
	var res interface{}
	if ctx.InBatch {
		res = fn()
	} else {
		res = ctx.Engine.DispatchWithResult(fn)
	}
	if err, ok := res.(error); ok {
		return Result{Err: err}
	}
	return Result{Value: res}
}

// Spec defines the minimum and maximum number of arguments a command
// accepts (not counting the command name itself). Max == -1 means unlimited.
type Spec struct {
	Min int
	Max int
}
