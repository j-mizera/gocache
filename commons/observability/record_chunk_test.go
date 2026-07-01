package observability

import (
	"fmt"
	"sync"
	"testing"
)

func TestChunkPool_GetPut_Recycle(t *testing.T) {
	pool := NewChunkPool(4)

	chunk := pool.Get(1)
	if chunk == nil {
		t.Fatal("Get(1) returned nil, want a chunk")
	}
	if got, want := len(chunk.records), 64; got != want {
		t.Fatalf("len(chunk.records) = %d, want %d", got, want)
	}
	if got, want := chunk.classIdx, uint8(1); got != want {
		t.Fatalf("chunk.classIdx = %d, want %d", got, want)
	}

	pool.Put(chunk)
	if got, want := pool.Stats().ClassCounts[1], int64(1); got != want {
		t.Fatalf("ClassCounts[1] after Put = %d, want %d", got, want)
	}

	recycled := pool.Get(1)
	if recycled != chunk {
		t.Fatalf("recycled chunk pointer = %p, want original %p", recycled, chunk)
	}
	if got, want := len(recycled.records), 64; got != want {
		t.Fatalf("len(recycled.records) = %d, want %d", got, want)
	}
	if nextChunk := recycled.next.Load(); nextChunk != nil {
		t.Fatalf("recycled.next = %p, want nil", nextChunk)
	}
	if recycled.len != 0 {
		t.Fatalf("recycled.len = %d, want 0", recycled.len)
	}
}

func TestChunkPool_Exhaustion(t *testing.T) {
	pool := NewChunkPool(4)
	chunks := make([]*RecordChunk, 0, 4)
	for i := 0; i < 4; i++ {
		chunk := pool.Get(0)
		if chunk == nil {
			t.Fatalf("Get(0) at allocation %d returned nil, want a chunk", i)
		}
		chunks = append(chunks, chunk)
	}

	if extra := pool.Get(0); extra != nil {
		t.Fatalf("fifth Get(0) = %p, want nil", extra)
	}

	for _, chunk := range chunks {
		pool.Put(chunk)
	}
	if got, want := pool.Stats().ClassCounts[0], int64(4); got != want {
		t.Fatalf("ClassCounts[0] after returning chunks = %d, want %d", got, want)
	}
}

func TestChunkPool_PutFull(t *testing.T) {
	pool := NewChunkPool(1)
	retained := pool.Get(0)
	if retained == nil {
		t.Fatal("Get(0) returned nil, want retained chunk")
	}
	pool.Put(retained)
	if got, want := pool.classes[0].allocated.Load(), int64(1); got != want {
		t.Fatalf("allocated after retaining first chunk = %d, want %d", got, want)
	}

	discarded := newRecordChunk(0)
	pool.classes[0].allocated.Add(1)
	pool.Put(discarded)

	if got, want := pool.Stats().ClassCounts[0], int64(1); got != want {
		t.Fatalf("ClassCounts[0] after Put into full pool = %d, want %d", got, want)
	}
	if got, want := pool.classes[0].allocated.Load(), int64(1); got != want {
		t.Fatalf("allocated after discarded Put = %d, want %d", got, want)
	}
	if recycled := pool.Get(0); recycled != retained {
		t.Fatalf("Get(0) after full Put = %p, want retained %p", recycled, retained)
	}
}

func TestChunkPool_ClassSize(t *testing.T) {
	pool := NewChunkPool(4)
	cases := []struct {
		classIndex int
		want       int
	}{
		{classIndex: 0, want: 32},
		{classIndex: 1, want: 64},
		{classIndex: 2, want: 128},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("class_%d", tc.classIndex), func(t *testing.T) {
			if got := pool.ClassSize(tc.classIndex); got != tc.want {
				t.Fatalf("ClassSize(%d) = %d, want %d", tc.classIndex, got, tc.want)
			}
			chunk := pool.Get(tc.classIndex)
			if chunk == nil {
				t.Fatalf("Get(%d) returned nil, want chunk", tc.classIndex)
			}
			if got := len(chunk.records); got != tc.want {
				t.Fatalf("len(Get(%d).records) = %d, want %d", tc.classIndex, got, tc.want)
			}
		})
	}
}

func TestChunkPool_InvalidClass(t *testing.T) {
	pool := NewChunkPool(4)
	for _, classIndex := range []int{-1, 99} {
		if got := pool.Get(classIndex); got != nil {
			t.Fatalf("Get(%d) = %p, want nil", classIndex, got)
		}
	}
}

func TestChunkPool_Concurrent(t *testing.T) {
	pool := NewChunkPool(64)
	const goroutines = 16
	const iterations = 1_000

	active := make(map[*RecordChunk]int, goroutines)
	var activeMu sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)

	for worker := 0; worker < goroutines; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				classIndex := (worker + i) % numChunkClasses
				chunk := pool.Get(classIndex)
				if chunk == nil {
					errCh <- fmt.Errorf("worker %d iteration %d: Get(%d) returned nil", worker, i, classIndex)
					return
				}
				if got, want := len(chunk.records), chunkClassSizes[classIndex]; got != want {
					errCh <- fmt.Errorf("worker %d iteration %d: len(records) = %d, want %d", worker, i, got, want)
					return
				}

				activeMu.Lock()
				if owner, exists := active[chunk]; exists {
					activeMu.Unlock()
					errCh <- fmt.Errorf("worker %d iteration %d: duplicate active chunk %p already owned by worker %d", worker, i, chunk, owner)
					return
				}
				active[chunk] = worker
				activeMu.Unlock()

				chunk.len = uint64(i + 1)

				activeMu.Lock()
				delete(active, chunk)
				activeMu.Unlock()
				pool.Put(chunk)
			}
		}(worker)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(active) != 0 {
		t.Fatalf("active chunk count after goroutines = %d, want 0", len(active))
	}
}

func TestChunkPool_Stats(t *testing.T) {
	pool := NewChunkPool(4)
	class0A := pool.Get(0)
	class0B := pool.Get(0)
	class1 := pool.Get(1)
	class2 := pool.Get(2)
	if class0A == nil || class0B == nil || class1 == nil || class2 == nil {
		t.Fatalf("Get returned nil chunks: class0A=%p class0B=%p class1=%p class2=%p", class0A, class0B, class1, class2)
	}

	pool.Put(class0A)
	pool.Put(class0B)
	pool.Put(class2)
	if got, want := pool.Stats().ClassCounts, ([numChunkClasses]int64{2, 0, 1}); got != want {
		t.Fatalf("ClassCounts after three Puts = %v, want %v", got, want)
	}

	recycled := pool.Get(0)
	if recycled != class0B {
		t.Fatalf("Get(0) = %p, want LIFO chunk class0B %p", recycled, class0B)
	}
	pool.Put(class1)
	if got, want := pool.Stats().ClassCounts, ([numChunkClasses]int64{1, 1, 1}); got != want {
		t.Fatalf("ClassCounts after Get and Put = %v, want %v", got, want)
	}
}
