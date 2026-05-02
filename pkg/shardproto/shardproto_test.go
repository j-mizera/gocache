package shardproto

import (
	"context"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"gocache/pkg/resp"
)

// startServer brings up a shardproto server on an ephemeral port for one
// test. It returns the dial-able address; cleanup tears it down.
func startServer(t *testing.T, n int) string {
	t.Helper()
	c := NewCache(n)
	e := NewEngine(c)
	e.Run()

	srv := NewServer(e)
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve()

	t.Cleanup(func() {
		srv.Stop()
		e.Stop()
	})
	return srv.Addr()
}

func sendCmd(t *testing.T, conn net.Conn, parts ...string) resp.Value {
	t.Helper()
	w := resp.NewWriter(conn)
	rd := resp.NewReader(conn)
	args := make([]resp.Value, len(parts))
	for i, p := range parts {
		args[i] = resp.MarshalBulkString(p)
	}
	if err := w.Write(resp.ValueArray(args...)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	v, err := rd.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return v
}

func TestSetGet(t *testing.T) {
	addr := startServer(t, 8)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if v := sendCmd(t, conn, "SET", "k", "v"); v.Str != "OK" {
		t.Fatalf("SET: %q", v.Str)
	}
	if v := sendCmd(t, conn, "GET", "k"); v.Str != "v" {
		t.Fatalf("GET: %q", v.Str)
	}
	if v := sendCmd(t, conn, "GET", "absent"); !v.IsNull {
		t.Fatalf("GET absent: %+v", v)
	}
}

func TestHset(t *testing.T) {
	addr := startServer(t, 8)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	v := sendCmd(t, conn, "HSET", "h", "f1", "v1", "f2", "v2")
	if v.Integer != 2 {
		t.Fatalf("HSET first: %d (want 2)", v.Integer)
	}
	v = sendCmd(t, conn, "HSET", "h", "f1", "v1updated", "f3", "v3")
	if v.Integer != 1 {
		t.Fatalf("HSET second: %d (want 1)", v.Integer)
	}
}

// TestRoutingDeterministic confirms that a key always hashes to the same
// shard, and that the load distribution is reasonable at N=16.
func TestRoutingDeterministic(t *testing.T) {
	c := NewCache(16)
	for i := 0; i < 100; i++ {
		k := "k:" + strconv.Itoa(i)
		first := c.shardIndex(k)
		for j := 0; j < 10; j++ {
			if c.shardIndex(k) != first {
				t.Fatalf("non-deterministic routing for %q", k)
			}
		}
	}

	const total = 100_000
	counts := make([]int, c.ShardCount())
	for i := 0; i < total; i++ {
		counts[c.shardIndex("k:"+strconv.Itoa(i))]++
	}
	expected := total / c.ShardCount()
	for i, n := range counts {
		dev := float64(n-expected) / float64(expected)
		if dev > 0.30 || dev < -0.30 {
			t.Errorf("shard %d load %d, expected ~%d (deviation %.1f%%)", i, n, expected, dev*100)
		}
	}
}

// TestConcurrentSingleKey hammers disjoint keys from many connections to
// confirm the per-shard engine model stays race-clean. Run under -race
// to surface any sharing slip.
func TestConcurrentSingleKey(t *testing.T) {
	addr := startServer(t, 8)

	const clients = 16
	const ops = 200

	var wg sync.WaitGroup
	wg.Add(clients)
	for cid := 0; cid < clients; cid++ {
		go func(id int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			defer conn.Close()
			for i := 0; i < ops; i++ {
				k := "c" + strconv.Itoa(id) + ":" + strconv.Itoa(i)
				if v := sendCmd(t, conn, "SET", k, "v"); v.Str != "OK" {
					t.Errorf("SET %s: %q", k, v.Str)
					return
				}
				if v := sendCmd(t, conn, "GET", k); v.Str != "v" {
					t.Errorf("GET %s: %q", k, v.Str)
					return
				}
			}
		}(cid)
	}
	wg.Wait()
}

// TestEngineStop confirms a Dispatch in flight returns ErrEngineStopped.
func TestEngineStop(t *testing.T) {
	c := NewCache(4)
	e := NewEngine(c)
	e.Run()
	e.Stop()

	_, err := e.Dispatch(context.Background(), "k", func(s *Shard) any { return nil })
	if err == nil {
		t.Fatalf("expected ErrEngineStopped, got nil")
	}
}

// TestPanicsOnNonPow2 documents the constructor's contract.
func TestPanicsOnNonPow2(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("expected panic on n=3")
		}
	}()
	_ = NewCache(3)
}

// Compile-time assurance that shutdown deadline tooling still imports cleanly.
var _ = time.Second
