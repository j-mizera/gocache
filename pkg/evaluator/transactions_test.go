package evaluator_test

import (
	"testing"
)

func TestEvaluator_Transactions(t *testing.T) {
	_, _, ev, ctx := setup(t)

	ev.Evaluate(ctx, "MULTI", nil)
	ev.Evaluate(ctx, "SET", []string{"a", "1"})
	ev.Evaluate(ctx, "SET", []string{"b", "2"})
	res := ev.Evaluate(ctx, "EXEC", nil)

	results, ok := res.Value.([]interface{})
	if !ok {
		t.Fatalf("expected []interface{}, got %T", res.Value)
	}
	if len(results) != 2 || results[0] != "OK" || results[1] != "OK" {
		t.Errorf("expected [OK OK], got %v", results)
	}

	res = ev.Evaluate(ctx, "GET", []string{"a"})
	if res.Value != "1" {
		t.Errorf("expected 1, got %v", res.Value)
	}
}
