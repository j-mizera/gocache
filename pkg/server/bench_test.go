package server

// Profiling harness for the command-flow-optimization diagnosis pass.
//
// Two flavours per workload:
//   - InProcess: drives evaluator.Evaluate directly through the engine queue.
//     Strips the network layer so the profile reflects cache + engine +
//     evaluator cost. 50 parallel goroutines via b.RunParallel.
//   - TCP: drives the same workload over a real TCP loopback connection.
//     Includes server.handleConnection + RESP encode/decode in the profile.
//     The delta between the two attributions localizes cost between the
//     core dispatch path and the connection layer.
//
// Pipelined variants exist only in the TCP flavour — pipelining at the
// in-process API is meaningless because the engine queue serialises every
// call regardless of whether one or ten commands are in flight.

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"gocache/commons/resp"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/engine"
	"gocache/pkg/events"
	"gocache/pkg/pipeline"
	"gocache/pkg/watch"
)

// ----- in-process setup -------------------------------------------------

type inProcRig struct {
	cache *cache.Cache
	eng   *engine.Engine
	eval  *pipeline.Pipeline
}

func newInProcRig(b *testing.B) *inProcRig {
	b.Helper()
	// Match production binary defaults from config.go: MaxMemoryMB=1024, LRU.
	// Without this, c.maxBytes == 0 and the RawSet/RawSetPacked memory-limit
	// branch is skipped entirely — the harness understates per-command cost.
	c := cache.NewWithConfig(1024, cache.EvictionLRU)
	e := engine.New(c)
	b.Cleanup(func() { e.Stop() })

	br := blocking.NewRegistry()
	wm := watch.NewManager()
	c.SetOnMutate(wm.NotifyMutation)

	ev := pipeline.New(c, e, "", br, wm)
	// Match production: cmd/server/main.go always wires the event bus, even
	// when no plugins are loaded. Without this, the evaluator's emitter
	// branch is skipped and the harness understates per-command cost by
	// the bus.Emit lock+ring-push overhead per call.
	ev.SetEmitter(events.NewBus())
	return &inProcRig{cache: c, eng: e, eval: ev}
}

// preloadStringKeys writes n keys "k:0".."k:n-1" with 64-byte values.
func (r *inProcRig) preloadStringKeys(b *testing.B, n int) {
	b.Helper()
	ctx := context.Background()
	cli := clientctx.New()
	val := make([]byte, 64)
	for i := range val {
		val[i] = 'x'
	}
	for i := 0; i < n; i++ {
		res := r.eval.Evaluate(ctx, cli, "SET", []string{"k:" + strconv.Itoa(i), string(val)})
		if res.Err != nil {
			b.Fatalf("preload SET: %v", res.Err)
		}
	}
}

// ----- TCP setup --------------------------------------------------------

type tcpRig struct {
	srv  *Server
	addr string
}

func newTCPRig(b *testing.B) *tcpRig {
	b.Helper()
	// Match production binary defaults from config.go: MaxMemoryMB=1024, LRU.
	// Without this, c.maxBytes == 0 and the RawSet/RawSetPacked memory-limit
	// branch is skipped entirely — the harness understates per-command cost.
	c := cache.NewWithConfig(1024, cache.EvictionLRU)
	e := engine.New(c)
	b.Cleanup(func() { e.Stop() })

	br := blocking.NewRegistry()
	wm := watch.NewManager()
	c.SetOnMutate(wm.NotifyMutation)

	srv := New("127.0.0.1:0", c, e, "", br, wm)
	srv.SetEmitter(events.NewBus())

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	srv.listener = listener
	go srv.acceptConnections(context.Background())
	b.Cleanup(func() { _ = srv.Shutdown(2 * time.Second) })
	return &tcpRig{srv: srv, addr: listener.Addr().String()}
}

// preloadTCPStringKeys writes n keys via a single TCP connection.
func (r *tcpRig) preloadTCPStringKeys(b *testing.B, n int) {
	b.Helper()
	conn, err := net.Dial("tcp", r.addr)
	if err != nil {
		b.Fatalf("preload dial: %v", err)
	}
	defer conn.Close()
	w := resp.NewWriter(conn)
	rd := resp.NewReader(conn)
	val := make([]byte, 64)
	for i := range val {
		val[i] = 'x'
	}
	for i := 0; i < n; i++ {
		if err := w.Write(respCmd("SET", "k:"+strconv.Itoa(i), string(val))); err != nil {
			b.Fatalf("preload write: %v", err)
		}
		if err := w.Flush(); err != nil {
			b.Fatalf("preload flush: %v", err)
		}
		if _, err := rd.Read(); err != nil {
			b.Fatalf("preload read: %v", err)
		}
	}
}

func respCmd(args ...string) resp.Value {
	vals := make([]resp.Value, len(args))
	for i, a := range args {
		vals[i] = resp.MarshalBulkString(a)
	}
	return resp.ValueArray(vals...)
}

// ----- in-process benchmarks --------------------------------------------

// parallelism is the b.SetParallelism multiplier. With -cpu=4, a value of
// 13 yields ~52 goroutines, matching the 50 concurrent clients used by the
// production redis-benchmark harness.
const parallelism = 13

// BenchmarkInProc_HSET — many goroutines mutating one hash with rotating fields.
// Worst-case collection-write surface: contended on cache.Lock, packed-encoding
// recopies on each mutation, and the full instrumentation block runs per op.
func BenchmarkInProc_HSET(b *testing.B) {
	rig := newInProcRig(b)
	const hashKey = "h:bench"
	var seq atomic.Uint64
	b.SetParallelism(parallelism)
	b.SetParallelism(parallelism)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		cli := clientctx.New()
		for pb.Next() {
			i := seq.Add(1)
			field := "f:" + strconv.FormatUint(i, 10)
			res := rig.eval.Evaluate(ctx, cli, "HSET", []string{hashKey, field, "v"})
			if res.Err != nil {
				b.Fatalf("HSET: %v", res.Err)
			}
		}
	})
}

// TestHSET_PromotedHash_O1 — regression test for #23. Issuing 100 000
// HSETs to a single hash that has been promoted to EncNative must complete
// in well under a second. Before the fix, every HSET walked the entire
// map via estimateSize → chargedSize, making the workload O(N²) and
// taking ~140 s on this hardware.
func TestHSET_PromotedHash_O1(t *testing.T) {
	c := cache.New()
	e := engine.New(c)
	t.Cleanup(func() { e.Stop() })
	br := blocking.NewRegistry()
	wm := watch.NewManager()
	c.SetOnMutate(wm.NotifyMutation)
	ev := pipeline.New(c, e, "", br, wm)
	cli := clientctx.New()

	const N = 100_000
	deadline := time.After(10 * time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < N; i++ {
			res := ev.Evaluate(context.Background(), cli, "HSET",
				[]string{"myhash", "f:" + strconv.Itoa(i), "v"})
			if res.Err != nil {
				t.Errorf("HSET #%d failed: %v", i, res.Err)
				return
			}
		}
	}()
	select {
	case <-done:
		// pass
	case <-deadline:
		t.Fatalf("100k HSETs to promoted hash did not complete in 10s — estimateSize O(N²) regression")
	}
}

// runPromotedCollectionO1 is the shared body of the
// TestSADD/LPUSH/RPUSH/ZADD_Promoted*_O1 regression tests for #33. It
// builds a minimal evaluator rig, fires N mutations against one key, and
// asserts completion within deadline. The rig setup mirrors
// TestHSET_PromotedHash_O1 — the existing #23 regression — so any drift in
// the test scaffolding shows up identically across all four tests.
func runPromotedCollectionO1(t *testing.T, deadline time.Duration, n int, makeArgs func(i int) []string, op string) {
	t.Helper()
	c := cache.NewWithConfig(1024, cache.EvictionLRU)
	e := engine.New(c)
	t.Cleanup(func() { e.Stop() })

	br := blocking.NewRegistry()
	wm := watch.NewManager()
	c.SetOnMutate(wm.NotifyMutation)
	ev := pipeline.New(c, e, "", br, wm)
	cli := clientctx.New()

	deadlineCh := time.After(deadline)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			res := ev.Evaluate(context.Background(), cli, op, makeArgs(i))
			if res.Err != nil {
				t.Errorf("%s #%d failed: %v", op, i, res.Err)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-deadlineCh:
		t.Fatalf("100k %s to promoted collection did not complete in %v — estimateSize regression", op, deadline)
	}
}

// TestSADD_PromotedSet_O1 — regression test for #33 (set branch).
func TestSADD_PromotedSet_O1(t *testing.T) {
	runPromotedCollectionO1(t, 10*time.Second, 100_000,
		func(i int) []string { return []string{"myset", "m:" + strconv.Itoa(i)} },
		"SADD",
	)
}

// TestRPUSH_PromotedList_O1 — regression test for #33 (list branch).
// Threshold is 30 s (vs 10 s for hash/set/zset) because the list slice
// itself is still O(N²) per-call due to the slice-copy on append; that's
// a separate concern (would need a different list encoding). Incremental
// tracking only removes the size-computation walk; with the fix the
// 100 000 sequential RPUSHes complete in ~3-5 s on typical hardware.
func TestRPUSH_PromotedList_O1(t *testing.T) {
	runPromotedCollectionO1(t, 30*time.Second, 100_000,
		func(i int) []string { return []string{"mylist", "v:" + strconv.Itoa(i)} },
		"RPUSH",
	)
}

// TestZADD_PromotedZSet_O1 — regression test for #33 (sorted-set branch).
func TestZADD_PromotedZSet_O1(t *testing.T) {
	runPromotedCollectionO1(t, 10*time.Second, 100_000,
		func(i int) []string {
			return []string{"myzset", strconv.Itoa(i), "m:" + strconv.Itoa(i)}
		},
		"ZADD",
	)
}

// BenchmarkInProc_HSET_Spread — 100k unique hashes, one field each. This
// pattern never promotes a hash to EncNative, so it exercises the packed
// encoding hot path and avoids the estimateSize walk that dominates the
// rotating-fields variant. Mirrors what valkey-benchmark -t hset does.
func BenchmarkInProc_HSET_Spread(b *testing.B) {
	rig := newInProcRig(b)
	var seq atomic.Uint64
	b.SetParallelism(parallelism)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		cli := clientctx.New()
		for pb.Next() {
			i := seq.Add(1) % 100_000
			res := rig.eval.Evaluate(ctx, cli, "HSET",
				[]string{"h:" + strconv.FormatUint(i, 10), "f", "v"})
			if res.Err != nil {
				b.Fatalf("HSET: %v", res.Err)
			}
		}
	})
}

// BenchmarkTCP_HSET_Spread_Standard — production-shape HSET over TCP.
func BenchmarkTCP_HSET_Spread_Standard(b *testing.B) {
	rig := newTCPRig(b)
	var seq atomic.Uint64
	b.SetParallelism(parallelism)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		conn, err := net.Dial("tcp", rig.addr)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		w := resp.NewWriter(conn)
		rd := resp.NewReader(conn)
		for pb.Next() {
			i := seq.Add(1) % 100_000
			if err := w.Write(respCmd("HSET", "h:"+strconv.FormatUint(i, 10), "f", "v")); err != nil {
				b.Fatalf("write: %v", err)
			}
			if err := w.Flush(); err != nil {
				b.Fatalf("flush: %v", err)
			}
			if _, err := rd.Read(); err != nil {
				b.Fatalf("read: %v", err)
			}
		}
	})
}

// BenchmarkInProc_GET — random GETs over a 100k preloaded keyspace.
func BenchmarkInProc_GET(b *testing.B) {
	rig := newInProcRig(b)
	rig.preloadStringKeys(b, 100_000)
	var seq atomic.Uint64
	b.SetParallelism(parallelism)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		cli := clientctx.New()
		for pb.Next() {
			i := seq.Add(1) % 100_000
			res := rig.eval.Evaluate(ctx, cli, "GET", []string{"k:" + strconv.FormatUint(i, 10)})
			if res.Err != nil {
				b.Fatalf("GET: %v", res.Err)
			}
		}
	})
}

// BenchmarkInProc_SET — control workload: simple writes, no collection.
// If HSET shares dispatch cost with SET, the rps gap should be small.
func BenchmarkInProc_SET(b *testing.B) {
	rig := newInProcRig(b)
	var seq atomic.Uint64
	b.SetParallelism(parallelism)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		cli := clientctx.New()
		for pb.Next() {
			i := seq.Add(1)
			res := rig.eval.Evaluate(ctx, cli, "SET", []string{"k:" + strconv.FormatUint(i, 10), "v"})
			if res.Err != nil {
				b.Fatalf("SET: %v", res.Err)
			}
		}
	})
}

// ----- TCP benchmarks ---------------------------------------------------

// BenchmarkTCP_GET_Pipelined — TCP loopback, P=10, many parallel connections.
// The read-lock-bypass workload: each command pays the engine channel hop.
func BenchmarkTCP_GET_Pipelined(b *testing.B) {
	const pipeline = 10
	rig := newTCPRig(b)
	rig.preloadTCPStringKeys(b, 100_000)
	var seq atomic.Uint64
	b.SetParallelism(parallelism)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		conn, err := net.Dial("tcp", rig.addr)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		w := resp.NewWriter(conn)
		rd := resp.NewReader(conn)
		batch := make([]resp.Value, pipeline)
		for pb.Next() {
			for j := 0; j < pipeline; j++ {
				i := seq.Add(1) % 100_000
				batch[j] = respCmd("GET", "k:"+strconv.FormatUint(i, 10))
			}
			for j := 0; j < pipeline; j++ {
				if err := w.Write(batch[j]); err != nil {
					b.Fatalf("write: %v", err)
				}
			}
			if err := w.Flush(); err != nil {
				b.Fatalf("flush: %v", err)
			}
			for j := 0; j < pipeline; j++ {
				if _, err := rd.Read(); err != nil {
					b.Fatalf("read: %v", err)
				}
			}
		}
	})
}

// BenchmarkTCP_HSET_Pipelined — TCP loopback, P=10, many parallel connections.
// The pipelined-write inversion workload (baseline: 469 rps).
func BenchmarkTCP_HSET_Pipelined(b *testing.B) {
	const pipeline = 10
	rig := newTCPRig(b)
	const hashKey = "h:bench"
	var seq atomic.Uint64
	b.SetParallelism(parallelism)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		conn, err := net.Dial("tcp", rig.addr)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		w := resp.NewWriter(conn)
		rd := resp.NewReader(conn)
		batch := make([]resp.Value, pipeline)
		for pb.Next() {
			for j := 0; j < pipeline; j++ {
				i := seq.Add(1)
				batch[j] = respCmd("HSET", hashKey, "f:"+strconv.FormatUint(i, 10), "v")
			}
			for j := 0; j < pipeline; j++ {
				if err := w.Write(batch[j]); err != nil {
					b.Fatalf("write: %v", err)
				}
			}
			if err := w.Flush(); err != nil {
				b.Fatalf("flush: %v", err)
			}
			for j := 0; j < pipeline; j++ {
				if _, err := rd.Read(); err != nil {
					b.Fatalf("read: %v", err)
				}
			}
		}
	})
}

// BenchmarkTCP_HSET_Standard — TCP loopback, no pipelining, many parallel
// connections. Pairs with BenchmarkInProc_HSET to localize cost between
// the dispatch path and the connection layer.
func BenchmarkTCP_HSET_Standard(b *testing.B) {
	rig := newTCPRig(b)
	const hashKey = "h:bench"
	var seq atomic.Uint64
	b.SetParallelism(parallelism)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		conn, err := net.Dial("tcp", rig.addr)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		w := resp.NewWriter(conn)
		rd := resp.NewReader(conn)
		for pb.Next() {
			i := seq.Add(1)
			if err := w.Write(respCmd("HSET", hashKey, "f:"+strconv.FormatUint(i, 10), "v")); err != nil {
				b.Fatalf("write: %v", err)
			}
			if err := w.Flush(); err != nil {
				b.Fatalf("flush: %v", err)
			}
			if _, err := rd.Read(); err != nil {
				b.Fatalf("read: %v", err)
			}
		}
	})
}

// BenchmarkTCP_GET_Standard — pairs with BenchmarkInProc_GET.
func BenchmarkTCP_GET_Standard(b *testing.B) {
	rig := newTCPRig(b)
	rig.preloadTCPStringKeys(b, 100_000)
	var seq atomic.Uint64
	b.SetParallelism(parallelism)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		conn, err := net.Dial("tcp", rig.addr)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		w := resp.NewWriter(conn)
		rd := resp.NewReader(conn)
		for pb.Next() {
			i := seq.Add(1) % 100_000
			if err := w.Write(respCmd("GET", "k:"+strconv.FormatUint(i, 10))); err != nil {
				b.Fatalf("write: %v", err)
			}
			if err := w.Flush(); err != nil {
				b.Fatalf("flush: %v", err)
			}
			if _, err := rd.Read(); err != nil {
				b.Fatalf("read: %v", err)
			}
		}
	})
}

// ----- mixed-workload benchmarks ----------------------------------------
//
// These drive concurrent readers and writers against one server to expose
// the cross-path interaction cost that single-workload benchmarks miss.
// Each goroutine takes a deterministic role at startup (reader or writer)
// based on a global atomic counter; readers and writers run in parallel
// for the duration of the benchmark.
//
// Why these exist: the #28 read-lock-bypass arc proved that single-workload
// block profiles can attribute cost correctly while still missing the
// interaction effect when one path's lock primitive changes affect the
// other path. These mixed benchmarks make that interaction directly
// measurable for the per-shard locking diagnosis (issue #34).

// BenchmarkTCP_Mixed_GetSet_Pipelined — half goroutines pipeline GET,
// half pipeline SET. The cross-path workload that matters most for
// per-shard locking: writers contend on cache.Lock, readers wait for
// the engine queue, both share the channel-hop scheduler cost.
func BenchmarkTCP_Mixed_GetSet_Pipelined(b *testing.B) {
	const pipeline = 10
	rig := newTCPRig(b)
	rig.preloadTCPStringKeys(b, 100_000)
	var seq atomic.Uint64
	var roleCounter atomic.Int64
	b.SetParallelism(parallelism)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		isReader := roleCounter.Add(1)%2 == 0
		conn, err := net.Dial("tcp", rig.addr)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		w := resp.NewWriter(conn)
		rd := resp.NewReader(conn)
		batch := make([]resp.Value, pipeline)
		for pb.Next() {
			for j := 0; j < pipeline; j++ {
				i := seq.Add(1) % 100_000
				if isReader {
					batch[j] = respCmd("GET", "k:"+strconv.FormatUint(i, 10))
				} else {
					batch[j] = respCmd("SET", "k:"+strconv.FormatUint(i, 10), "v")
				}
			}
			for j := 0; j < pipeline; j++ {
				if err := w.Write(batch[j]); err != nil {
					b.Fatalf("write: %v", err)
				}
			}
			if err := w.Flush(); err != nil {
				b.Fatalf("flush: %v", err)
			}
			for j := 0; j < pipeline; j++ {
				if _, err := rd.Read(); err != nil {
					b.Fatalf("read: %v", err)
				}
			}
		}
	})
}

// BenchmarkTCP_Mixed_GetHset_Pipelined — readers vs collection writers.
// HSET on a shared hash key is the worst-case collection-write surface
// (#28's writer side). Pairing with GET captures the precise interaction
// per-shard locking aims to reduce: HSET writers no longer block GETs on
// disjoint keys.
func BenchmarkTCP_Mixed_GetHset_Pipelined(b *testing.B) {
	const pipeline = 10
	const hashKey = "h:bench"
	rig := newTCPRig(b)
	rig.preloadTCPStringKeys(b, 100_000)
	var seq atomic.Uint64
	var roleCounter atomic.Int64
	b.SetParallelism(parallelism)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		isReader := roleCounter.Add(1)%2 == 0
		conn, err := net.Dial("tcp", rig.addr)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		w := resp.NewWriter(conn)
		rd := resp.NewReader(conn)
		batch := make([]resp.Value, pipeline)
		for pb.Next() {
			for j := 0; j < pipeline; j++ {
				i := seq.Add(1)
				if isReader {
					batch[j] = respCmd("GET", "k:"+strconv.FormatUint(i%100_000, 10))
				} else {
					batch[j] = respCmd("HSET", hashKey, "f:"+strconv.FormatUint(i, 10), "v")
				}
			}
			for j := 0; j < pipeline; j++ {
				if err := w.Write(batch[j]); err != nil {
					b.Fatalf("write: %v", err)
				}
			}
			if err := w.Flush(); err != nil {
				b.Fatalf("flush: %v", err)
			}
			for j := 0; j < pipeline; j++ {
				if _, err := rd.Read(); err != nil {
					b.Fatalf("read: %v", err)
				}
			}
		}
	})
}

// BenchmarkTCP_Mixed_GetSet_Standard — non-pipelined cross-path baseline.
// Pairs with the pipelined variant to localize whether the interaction
// cost is amortized by pipelining or paid per round-trip.
func BenchmarkTCP_Mixed_GetSet_Standard(b *testing.B) {
	rig := newTCPRig(b)
	rig.preloadTCPStringKeys(b, 100_000)
	var seq atomic.Uint64
	var roleCounter atomic.Int64
	b.SetParallelism(parallelism)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		isReader := roleCounter.Add(1)%2 == 0
		conn, err := net.Dial("tcp", rig.addr)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		w := resp.NewWriter(conn)
		rd := resp.NewReader(conn)
		for pb.Next() {
			i := seq.Add(1) % 100_000
			var cmd resp.Value
			if isReader {
				cmd = respCmd("GET", "k:"+strconv.FormatUint(i, 10))
			} else {
				cmd = respCmd("SET", "k:"+strconv.FormatUint(i, 10), "v")
			}
			if err := w.Write(cmd); err != nil {
				b.Fatalf("write: %v", err)
			}
			if err := w.Flush(); err != nil {
				b.Fatalf("flush: %v", err)
			}
			if _, err := rd.Read(); err != nil {
				b.Fatalf("read: %v", err)
			}
		}
	})
}

// BenchmarkTCP_SET_Standard — pairs with BenchmarkInProc_SET.
func BenchmarkTCP_SET_Standard(b *testing.B) {
	rig := newTCPRig(b)
	var seq atomic.Uint64
	b.SetParallelism(parallelism)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		conn, err := net.Dial("tcp", rig.addr)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		w := resp.NewWriter(conn)
		rd := resp.NewReader(conn)
		for pb.Next() {
			i := seq.Add(1)
			if err := w.Write(respCmd("SET", "k:"+strconv.FormatUint(i, 10), "v")); err != nil {
				b.Fatalf("write: %v", err)
			}
			if err := w.Flush(); err != nil {
				b.Fatalf("flush: %v", err)
			}
			if _, err := rd.Read(); err != nil {
				b.Fatalf("read: %v", err)
			}
		}
	})
}
