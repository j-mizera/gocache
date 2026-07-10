// Package benchsuite provides shared benchmark harness utilities for GoCache's
// plugin-path measurement work.
//
// The package is deliberately a leaf utility. It imports only the standard
// library and contains no router, event bus, command-hook, or plugin-manager
// knowledge. Package-level benchmarks stay in their owning packages and import
// benchsuite to enumerate dimensions, record latency samples, and persist
// baseline provenance.
//
// # Percentile backend decision
//
// Benchmarks use the package-local DurationRecorder instead of
// github.com/HdrHistogram/hdrhistogram-go. HdrHistogram is well-maintained and
// battle-tested, but adding it would expand the dependency graph for a harness
// whose thesis role is to make the measurement method easy to inspect. The
// custom recorder stores []time.Duration samples and computes nearest-rank
// percentiles from a sorted copy. That is less memory-efficient for very large
// runs, but it is dependency-free, transparent in the thesis, and sufficient for
// Go package benchmarks where b.N already bounds sample volume. If future
// benchmark volume makes sample retention too costly, the recorder interface is
// the single replacement point.
//
// # Dimension matrix
//
// BenchmarkDimensions contains the shared matrix axes used by the modular
// overhead benchmark plan:
//
//   - Transport: net.Pipe or AF_UNIX command transport.
//   - Hooks: PRE-hook count, normally 0, 1, or N.
//   - Plugins: plugin connection count, normally 1 or N.
//   - PayloadSize: command payload size in bytes.
//   - FanoutDepth: event or telemetry subscriber fan-out.
//   - PipelineDepth: pipelined command depth.
//
// DimensionMatrix can be constructed directly from selected cases or generated
// with EnumerateDimensions from per-axis option lists. This keeps new benchmark
// paths table-driven: extending coverage means adding matrix entries, not
// writing standalone benchmark loops.
//
// # Profiling isolation
//
// Each pprof profile type MUST be captured in a separate go test -bench
// invocation. Combining flags (-cpuprofile + -memprofile + -mutexprofile)
// in a single run produces corrupted evidence because profiling tools
// interfere with each other. This is mandated by FR-008.
//
// Block and mutex profiling are not enabled by default in the Go runtime.
// TestMain reads GOCACHE_BENCH_BLOCK_RATE and GOCACHE_BENCH_MUTEX_FRACTION
// env vars and calls runtime.SetBlockProfileRate / runtime.SetMutexProfileFraction
// before any benchmark runs. Normal benchmark runs have zero profiling overhead.
//
// Profile-type-to-benchmark mapping:
//   - CPU:       all benchmark classes (where CPU time is spent)
//   - Heap:      all classes (alloc sources) — required for FR-009 alloc-site
//     classification
//   - Mutex:     concurrent throughput (FR-004) and hook dispatch — shows
//     lock contention hotspots
//   - Block:     concurrent throughput and hook dispatch — shows goroutine
//     blocking and scheduling latency
//   - Goroutine: all benchmark classes — leak detection after run completes
//
// # Versioning and EventSink
//
// The suite is versioned: v1 = pre-AOF baseline. All fire-and-forget
// numbers are scoped as "send-cost only" and become meaningless post-AOF.
// SuiteVersion and SuiteScope constants provide metadata for benchmark
// output.
//
// EventSink is the interface for telemetry/event delivery backends. The
// benchmark suite targets EventSink so tmpfs (current) and future AOF
// backends can be compared without suite redesign. TmpfsTelemetryWriter
// in commons/observability already satisfies EventSink. When AOF lands,
// a new AOFEventSink implementing EventSink enables before/after comparison
// with the same benchmark code.
//
// Usage example
//
//	matrix := benchsuite.DimensionMatrix{
//		{Transport: benchsuite.TransportNetPipe, Hooks: 0, Plugins: 1, PayloadSize: 64, FanoutDepth: 0, PipelineDepth: 1},
//		{Transport: benchsuite.TransportAFUnix, Hooks: 1, Plugins: 1, PayloadSize: 64, FanoutDepth: 0, PipelineDepth: 1},
//	}
//	benchsuite.RunMatrix(b, matrix, func(b *testing.B, dimensions benchsuite.BenchmarkDimensions, recorder *benchsuite.DurationRecorder) {
//		b.ReportAllocs()
//		b.ResetTimer()
//		for i := 0; i < b.N; i++ {
//			startedAt := time.Now()
//			// benchmark owner performs package-specific work here.
//			recorder.RecordSince(startedAt)
//		}
//	})
package benchsuite
