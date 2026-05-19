package server

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIT_ConcurrentClients(t *testing.T) {
	_, addr := startTestServer(t, "")

	const clients = 10
	const opsPerClient = 50

	var wg sync.WaitGroup
	wg.Add(clients)

	for i := range clients {
		go func(id int) {
			defer wg.Done()
			conn := dial(t, addr)
			defer conn.Close()

			prefix := "c" + strconv.Itoa(id) + ":"
			for j := range opsPerClient {
				key := prefix + strconv.Itoa(j)
				val := "v" + strconv.Itoa(j)

				v := sendCommand(t, conn, "SET", key, val)
				if v.Str != "OK" {
					t.Errorf("client %d SET %s: got %q", id, key, v.Str)
					return
				}
				v = sendCommand(t, conn, "GET", key)
				if v.Str != val {
					t.Errorf("client %d GET %s: expected %q, got %q", id, key, val, v.Str)
					return
				}
			}
		}(i)
	}

	wg.Wait()
}

func TestIT_SharedKeyContention(t *testing.T) {
	_, addr := startTestServer(t, "")

	assertOK(t, sendCommand(t, dial(t, addr), "SET", "counter", "0"))

	const goroutines = 5
	const incrs = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()
			conn := dial(t, addr)
			defer conn.Close()

			for range incrs {
				sendCommand(t, conn, "INCR", "counter")
			}
		}()
	}

	wg.Wait()

	conn := dial(t, addr)
	defer conn.Close()
	v := sendCommand(t, conn, "GET", "counter")
	expected := strconv.Itoa(goroutines * incrs)
	if v.Str != expected {
		t.Errorf("counter: expected %s, got %s", expected, v.Str)
	}
}

// TestIT_WatchPropagation_ReadLockBypass verifies that WATCH dirty bits
// propagate correctly when reads bypass the engine. One client SET-loops
// on a watched key; another client WATCHes + GETs the same key
// repeatedly; a third client tries EXEC on a MULTI that included the
// watched key. Any successful EXEC must imply no concurrent SET happened
// between WATCH and EXEC. Regression coverage for the read-lock-bypass
// invariant: writes still serialise through the engine + write lock; the
// bypass only avoids the hop for pure reads.
func TestIT_WatchPropagation_ReadLockBypass(t *testing.T) {
	_, addr := startTestServer(t, "")

	// Seed.
	{
		c := dial(t, addr)
		assertOK(t, sendCommand(t, c, "SET", "wkey", "0"))
		c.Close()
	}

	const dur = 500 * time.Millisecond
	// close-as-broadcast so both goroutines see the stop signal.
	// time.After is fire-once; only one receiver would observe it.
	stop := make(chan struct{})
	go func() {
		time.Sleep(dur)
		close(stop)
	}()

	var setCount atomic.Int64
	var watchAttempts atomic.Int64
	var watchAborted atomic.Int64

	var wg sync.WaitGroup

	// Writer: SET in tight loop on its own connection.
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn := dial(t, addr)
		defer conn.Close()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			i++
			sendCommand(t, conn, "SET", "wkey", strconv.Itoa(i))
			setCount.Add(1)
		}
	}()

	// Reader-watcher: pipelined GET burst + WATCH/MULTI/GET/EXEC. Any
	// successful EXEC must mean no concurrent SET landed between the
	// WATCH and EXEC. This is what the bypass MUST preserve.
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn := dial(t, addr)
		defer conn.Close()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// A few read-only ops to exercise the bypass path under load.
			sendCommand(t, conn, "GET", "wkey")
			sendCommand(t, conn, "GET", "wkey")
			sendCommand(t, conn, "GET", "wkey")

			// WATCH + transactional GET. EXEC returns nil (RESP-level
			// nil array) if the watched key changed; otherwise the
			// queued GET's result.
			watchAttempts.Add(1)
			sendCommand(t, conn, "WATCH", "wkey")
			sendCommand(t, conn, "MULTI")
			sendCommand(t, conn, "GET", "wkey")
			v := sendCommand(t, conn, "EXEC")
			if v.IsNull {
				watchAborted.Add(1)
			}
		}
	}()

	wg.Wait()

	// Sanity: the workload actually ran.
	if setCount.Load() == 0 {
		t.Fatal("writer never SET")
	}
	if watchAttempts.Load() == 0 {
		t.Fatal("watcher never attempted")
	}
	// Some EXECs must have aborted given the writer's continuous SETs;
	// if zero aborts surface, WATCH propagation is broken.
	if watchAborted.Load() == 0 {
		t.Fatalf("expected at least one WATCH-aborted EXEC; got writes=%d watches=%d aborted=0",
			setCount.Load(), watchAttempts.Load())
	}
	t.Logf("watch stress: writes=%d watches=%d aborted=%d (%.1f%%)",
		setCount.Load(), watchAttempts.Load(), watchAborted.Load(),
		float64(watchAborted.Load())/float64(watchAttempts.Load())*100)
}

// TestIT_FlushDBPropagation_AcrossConnections exercises the watch.Manager
// NotifyAll path: one connection runs WATCH/MULTI/EXEC loops on a single
// key while another connection issues FLUSHDB repeatedly. FLUSHDB
// invalidates every watching client by calling Manager.NotifyAll, which
// writes ctx.watchDirty across all clients (cross-connection mutation).
//
// Audit coverage for #35: the watchDirty fix from PR #32 also has to
// hold for the NotifyAll path (not only NotifyMutation). The same
// atomic.Bool carries both; this test gives -race coverage for the
// broader notification surface. Without the fix this would race;
// with it the race detector stays clean.
func TestIT_FlushDBPropagation_AcrossConnections(t *testing.T) {
	_, addr := startTestServer(t, "")

	{
		c := dial(t, addr)
		assertOK(t, sendCommand(t, c, "SET", "fkey", "0"))
		c.Close()
	}

	const dur = 500 * time.Millisecond
	stop := make(chan struct{})
	go func() {
		time.Sleep(dur)
		close(stop)
	}()

	var flushCount atomic.Int64
	var watchAttempts atomic.Int64
	var watchAborted atomic.Int64

	var wg sync.WaitGroup

	// Multiple FLUSHDB goroutines increase the chance of overlapping
	// with the WATCH→EXEC window on the watcher connection.
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := dial(t, addr)
			defer conn.Close()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sendCommand(t, conn, "FLUSHDB")
				sendCommand(t, conn, "SET", "fkey", "0")
				flushCount.Add(1)
			}
		}()
	}

	// Watcher: WATCH/MULTI/GET/EXEC loop. Some EXECs MUST abort because
	// FLUSHDB invalidates the watched key via NotifyAll.
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn := dial(t, addr)
		defer conn.Close()
		for {
			select {
			case <-stop:
				return
			default:
			}
			watchAttempts.Add(1)
			sendCommand(t, conn, "WATCH", "fkey")
			sendCommand(t, conn, "MULTI")
			sendCommand(t, conn, "GET", "fkey")
			v := sendCommand(t, conn, "EXEC")
			if v.IsNull {
				watchAborted.Add(1)
			}
		}
	}()

	wg.Wait()

	if flushCount.Load() == 0 {
		t.Fatal("FLUSHDB loop never ran")
	}
	if watchAttempts.Load() == 0 {
		t.Fatal("watcher never attempted")
	}
	if watchAborted.Load() == 0 {
		t.Fatalf("expected at least one FLUSHDB-aborted EXEC; got flushes=%d watches=%d aborted=0",
			flushCount.Load(), watchAttempts.Load())
	}
	t.Logf("flushdb stress: flushes=%d watches=%d aborted=%d (%.1f%%)",
		flushCount.Load(), watchAttempts.Load(), watchAborted.Load(),
		float64(watchAborted.Load())/float64(watchAttempts.Load())*100)
}
