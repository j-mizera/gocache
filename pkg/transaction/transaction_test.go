package transaction

import (
	"errors"
	"gocache/pkg/clientctx"
	"testing"
)

func TestMulti(t *testing.T) {
	m := NewManager()
	ctx := clientctx.New()

	res, err := m.Multi(ctx)
	if err != nil {
		t.Fatalf("Multi failed: %v", err)
	}
	if res != "OK" {
		t.Errorf("expected OK, got %s", res)
	}
	if !ctx.InTransaction {
		t.Error("expected InTransaction true")
	}
}

func TestMulti_Nested(t *testing.T) {
	m := NewManager()
	ctx := clientctx.New()

	_, _ = m.Multi(ctx)
	_, err := m.Multi(ctx)
	if !errors.Is(err, clientctx.ErrNestedMulti) {
		t.Errorf("expected ErrNestedMulti, got %v", err)
	}
}

func TestDiscard(t *testing.T) {
	m := NewManager()
	ctx := clientctx.New()

	_, _ = m.Multi(ctx)
	ctx.EnqueueCommand([]string{"SET", "a", "1"})

	res, err := m.Discard(ctx)
	if err != nil {
		t.Fatalf("Discard failed: %v", err)
	}
	if res != "OK" {
		t.Errorf("expected OK, got %s", res)
	}
	if ctx.InTransaction {
		t.Error("expected InTransaction false after Discard")
	}
}

func TestDiscard_WithoutMulti(t *testing.T) {
	m := NewManager()
	ctx := clientctx.New()

	_, err := m.Discard(ctx)
	if !errors.Is(err, clientctx.ErrDiscardWithoutMulti) {
		t.Errorf("expected ErrDiscardWithoutMulti, got %v", err)
	}
}
