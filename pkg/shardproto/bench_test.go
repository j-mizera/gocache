package shardproto

import (
	"net"
	"strconv"
	"sync/atomic"
	"testing"

	"gocache/commons/resp"
)

// Benchmarks mirror pkg/server/bench_test.go's BenchmarkTCP_Mixed_* and
// BenchmarkTCP_GET_Pipelined / SET_Pipelined / GET_Standard / SET_Standard
// so the prototype runs head-to-head against the diagnosis baseline. The
// only structural difference: the rig points at the shardproto server,
// and the constructor takes N (shard count).
//
// Use BENCH_SHARDS=8 (or 4/16/32) to run a single N; without the env var,
// the helper benchmarks below sweep all four counts as sub-benchmarks.

const benchParallelism = 13 // matches pkg/server/bench_test.go's parallelism

type benchRig struct {
	addr string
}

func newBenchRig(b *testing.B, n int) *benchRig {
	b.Helper()
	c := NewCache(n)
	e := NewEngine(c)
	e.Run()

	srv := NewServer(e)
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		b.Fatalf("listen: %v", err)
	}
	go srv.Serve()

	b.Cleanup(func() {
		srv.Stop()
		e.Stop()
	})
	return &benchRig{addr: srv.Addr()}
}

func (r *benchRig) preloadKeys(b *testing.B, n int) {
	b.Helper()
	conn, err := net.Dial("tcp", r.addr)
	if err != nil {
		b.Fatalf("preload dial: %v", err)
	}
	defer conn.Close()
	w := resp.NewWriter(conn)
	rd := resp.NewReader(conn)
	for i := 0; i < n; i++ {
		if err := w.Write(respCmd("SET", "k:"+strconv.Itoa(i), "v")); err != nil {
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
	vs := make([]resp.Value, len(args))
	for i, a := range args {
		vs[i] = resp.MarshalBulkString(a)
	}
	return resp.ValueArray(vs...)
}

// nForSweep is the canonical sweep — tested counts from the diagnosis
// plan. 1 is included as a sanity baseline (engine-per-cache with a
// single shard ≈ production today, minus instrumentation).
var nForSweep = []int{1, 4, 8, 16, 32}

// runMixedPipelined drives 50/50 reader/writer mixed load, P=10, on a
// shardproto server with shardCount shards.
func runMixedPipelined(b *testing.B, shardCount int, writeCmd func(uint64) resp.Value, readCmd func(uint64) resp.Value) {
	const pipeline = 10
	rig := newBenchRig(b, shardCount)
	rig.preloadKeys(b, 100_000)
	var seq atomic.Uint64
	var role atomic.Int64
	b.SetParallelism(benchParallelism)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		isReader := role.Add(1)%2 == 0
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
					batch[j] = readCmd(i)
				} else {
					batch[j] = writeCmd(i)
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

func BenchmarkTCPShard_Mixed_GetSet_Pipelined(b *testing.B) {
	for _, n := range nForSweep {
		b.Run("N="+strconv.Itoa(n), func(b *testing.B) {
			runMixedPipelined(b, n,
				func(i uint64) resp.Value { return respCmd("SET", "k:"+strconv.FormatUint(i%100_000, 10), "v") },
				func(i uint64) resp.Value { return respCmd("GET", "k:"+strconv.FormatUint(i%100_000, 10)) },
			)
		})
	}
}

func BenchmarkTCPShard_Mixed_GetHset_Pipelined(b *testing.B) {
	const hashKey = "h:bench"
	for _, n := range nForSweep {
		b.Run("N="+strconv.Itoa(n), func(b *testing.B) {
			runMixedPipelined(b, n,
				func(i uint64) resp.Value {
					return respCmd("HSET", hashKey, "f:"+strconv.FormatUint(i, 10), "v")
				},
				func(i uint64) resp.Value {
					return respCmd("GET", "k:"+strconv.FormatUint(i%100_000, 10))
				},
			)
		})
	}
}

// runSinglePipelined drives a single-mode pipelined workload (all readers
// or all writers). Used to validate Phase-1 gate criterion (≥900k GET,
// ≥775k SET).
func runSinglePipelined(b *testing.B, shardCount int, mkCmd func(uint64) resp.Value) {
	const pipeline = 10
	rig := newBenchRig(b, shardCount)
	rig.preloadKeys(b, 100_000)
	var seq atomic.Uint64
	b.SetParallelism(benchParallelism)
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
				batch[j] = mkCmd(i)
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

func BenchmarkTCPShard_GET_Pipelined(b *testing.B) {
	for _, n := range nForSweep {
		b.Run("N="+strconv.Itoa(n), func(b *testing.B) {
			runSinglePipelined(b, n, func(i uint64) resp.Value {
				return respCmd("GET", "k:"+strconv.FormatUint(i, 10))
			})
		})
	}
}

func BenchmarkTCPShard_SET_Pipelined(b *testing.B) {
	for _, n := range nForSweep {
		b.Run("N="+strconv.Itoa(n), func(b *testing.B) {
			runSinglePipelined(b, n, func(i uint64) resp.Value {
				return respCmd("SET", "k:"+strconv.FormatUint(i, 10), "v")
			})
		})
	}
}

// runSingleStandard is the non-pipelined variant; pairs with the
// pipelined runs to check the per-shard overhead at low concurrency.
func runSingleStandard(b *testing.B, shardCount int, mkCmd func(uint64) resp.Value) {
	rig := newBenchRig(b, shardCount)
	rig.preloadKeys(b, 100_000)
	var seq atomic.Uint64
	b.SetParallelism(benchParallelism)
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
			if err := w.Write(mkCmd(i)); err != nil {
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

func BenchmarkTCPShard_GET_Standard(b *testing.B) {
	for _, n := range nForSweep {
		b.Run("N="+strconv.Itoa(n), func(b *testing.B) {
			runSingleStandard(b, n, func(i uint64) resp.Value {
				return respCmd("GET", "k:"+strconv.FormatUint(i, 10))
			})
		})
	}
}

func BenchmarkTCPShard_SET_Standard(b *testing.B) {
	for _, n := range nForSweep {
		b.Run("N="+strconv.Itoa(n), func(b *testing.B) {
			runSingleStandard(b, n, func(i uint64) resp.Value {
				return respCmd("SET", "k:"+strconv.FormatUint(i, 10), "v")
			})
		})
	}
}
