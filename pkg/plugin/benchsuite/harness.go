package benchsuite

import "testing"

// BenchmarkFunc runs one benchmark case and records optional latency samples.
type BenchmarkFunc func(b *testing.B, dimensions BenchmarkDimensions, recorder *DurationRecorder)

// RunMatrix executes benchmarkFunc once per dimension case as sub-benchmarks.
func RunMatrix(b *testing.B, matrix DimensionMatrix, benchmarkFunc BenchmarkFunc) {
	b.Helper()
	for _, benchmarkCase := range matrix {
		benchmarkCase := benchmarkCase
		b.Run(benchmarkCase.Name(), func(b *testing.B) {
			b.Helper()
			recorder := NewDurationRecorder(b.N)
			benchmarkFunc(b, benchmarkCase, recorder)
			snapshot := recorder.Snapshot()
			if snapshot.Count == 0 {
				return
			}
			if warning := snapshot.SampleSizeWarning(); warning != "" {
				b.Logf("WARNING: %s (count=%d)", warning, snapshot.Count)
			}
			b.ReportMetric(float64(snapshot.P50.Nanoseconds()), "p50-ns")
			b.ReportMetric(float64(snapshot.P95.Nanoseconds()), "p95-ns")
			b.ReportMetric(float64(snapshot.P99.Nanoseconds()), "p99-ns")
			b.ReportMetric(float64(snapshot.P999.Nanoseconds()), "p999-ns")
		})
	}
}
