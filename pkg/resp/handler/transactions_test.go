package handler_test

import (
	"strings"
	"testing"

	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/resp/handler"
	"gocache/pkg/transaction"
)

func TestHandler_Transactions(t *testing.T) {
	c, e, ctx := setup(t)
	tm := transaction.NewManager()

	// evalFn re-enters through the handler map, like the evaluator pipeline.
	var evalFn func(*clientctx.ClientContext, string, []string, bool) command.Result
	evalFn = func(client *clientctx.ClientContext, op string, args []string, inBatch bool) command.Result {
		op = strings.ToUpper(op)
		h, ok := handlers[op]
		if !ok {
			t.Fatalf("unknown command in evalFn: %s", op)
		}
		return h(&command.Context{
			Client:      client,
			Op:          op,
			Args:        args,
			InBatch:     inBatch,
			Engine:      e,
			Cache:       c,
			Transaction: tm,
			EvalFn:      evalFn,
		})
	}

	makeCtx := func(op string, args []string) *command.Context {
		return &command.Context{
			Client:      ctx,
			Op:          op,
			Args:        args,
			Engine:      e,
			Cache:       c,
			Transaction: tm,
			EvalFn:      evalFn,
		}
	}

	// MULTI
	res := handler.HandleMulti(makeCtx("MULTI", nil))
	if res.Value != "OK" {
		t.Fatalf("MULTI: expected OK, got %v", res.Value)
	}

	// Queue SET a 1 -- should return QUEUED via transaction logic
	// Since we bypass the evaluator pipeline, we simulate queueing manually.
	ctx.EnqueueCommand([]string{"SET", "a", "1"})
	ctx.EnqueueCommand([]string{"SET", "b", "2"})

	// EXEC
	res = handler.HandleExec(makeCtx("EXEC", nil))
	results, ok := res.Value.([]interface{})
	if !ok {
		t.Fatalf("EXEC: expected []interface{}, got %T", res.Value)
	}
	if len(results) != 2 || results[0] != "OK" || results[1] != "OK" {
		t.Errorf("EXEC: expected [OK OK], got %v", results)
	}

	// Verify values were set
	res = eval(t, c, e, ctx, "GET", []string{"a"})
	if res.Value != "1" {
		t.Errorf("GET a: expected 1, got %v", res.Value)
	}
}
