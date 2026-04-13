package operations

import (
	"sync"
	"testing"

	ops "gocache/api/operations"
)

func TestTracker_StartAndGet(t *testing.T) {
	tr := NewTracker()
	op := tr.Start(ops.TypeCommand, "parent_1")

	if op == nil {
		t.Fatal("expected non-nil operation")
	}
	if op.Status != ops.StatusRunning {
		t.Errorf("expected Running, got %v", op.Status)
	}

	got := tr.Get(op.ID)
	if got != op {
		t.Error("Get should return the same operation")
	}
}

func TestTracker_Complete(t *testing.T) {
	tr := NewTracker()
	op := tr.Start(ops.TypeCommand, "")

	tr.Complete(op.ID)

	if tr.Get(op.ID) != nil {
		t.Error("completed operation should be removed from active")
	}
	if op.Status != ops.StatusCompleted {
		t.Errorf("expected Completed, got %v", op.Status)
	}
	if tr.ActiveCount() != 0 {
		t.Errorf("expected 0 active, got %d", tr.ActiveCount())
	}
}

func TestTracker_Fail(t *testing.T) {
	tr := NewTracker()
	op := tr.Start(ops.TypeSnapshot, "")

	tr.Fail(op.ID, "disk full")

	if tr.Get(op.ID) != nil {
		t.Error("failed operation should be removed from active")
	}
	if op.Status != ops.StatusFailed {
		t.Errorf("expected Failed, got %v", op.Status)
	}
	if op.FailReason != "disk full" {
		t.Errorf("expected 'disk full', got %q", op.FailReason)
	}
}

func TestTracker_Active(t *testing.T) {
	tr := NewTracker()
	op1 := tr.Start(ops.TypeCommand, "")
	op2 := tr.Start(ops.TypeCleanup, "")
	tr.Start(ops.TypeSnapshot, "")

	if tr.ActiveCount() != 3 {
		t.Fatalf("expected 3 active, got %d", tr.ActiveCount())
	}

	tr.Complete(op1.ID)
	if tr.ActiveCount() != 2 {
		t.Errorf("expected 2 active after complete, got %d", tr.ActiveCount())
	}

	active := tr.Active()
	if len(active) != 2 {
		t.Errorf("expected 2 in Active(), got %d", len(active))
	}

	// op1 should not be in active list
	for _, a := range active {
		if a.ID == op1.ID {
			t.Error("completed op should not be in Active()")
		}
	}
	_ = op2 // used
}

func TestTracker_CompleteUnknown(t *testing.T) {
	tr := NewTracker()
	// Should not panic
	tr.Complete("nonexistent")
	tr.Fail("nonexistent", "reason")
}

func TestTracker_GetUnknown(t *testing.T) {
	tr := NewTracker()
	if tr.Get("nonexistent") != nil {
		t.Error("expected nil for unknown ID")
	}
}

func TestTracker_Concurrent(t *testing.T) {
	tr := NewTracker()
	var wg sync.WaitGroup
	const n = 100

	// Concurrent starts
	operations := make([]*ops.Operation, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			operations[idx] = tr.Start(ops.TypeCommand, "")
		}(i)
	}
	wg.Wait()

	if tr.ActiveCount() != n {
		t.Fatalf("expected %d active, got %d", n, tr.ActiveCount())
	}

	// Concurrent completes
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tr.Complete(operations[idx].ID)
		}(i)
	}
	wg.Wait()

	if tr.ActiveCount() != 0 {
		t.Errorf("expected 0 active after all completed, got %d", tr.ActiveCount())
	}
}
