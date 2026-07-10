package benchsuite

import (
	"context"
	"regexp"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestEnumerateDimensions(t *testing.T) {
	matrix := EnumerateDimensions(DimensionOptions{
		Transports:     []TransportKind{TransportNetPipe, TransportAFUnix},
		Hooks:          []int{0, 1},
		Plugins:        []int{1},
		PayloadSizes:   []int{64},
		FanoutDepths:   []int{0},
		PipelineDepths: []int{1, 8},
	})

	if len(matrix) != 8 {
		t.Fatalf("expected 8 generated cases, got %d", len(matrix))
	}

	copiedCases := matrix.Cases()
	copiedCases[0].Hooks = 99
	if matrix[0].Hooks == 99 {
		t.Fatal("Cases must return a copy")
	}
}

func TestEnumerateDimensionsSmallMatrixCartesianProduct(t *testing.T) {
	matrix := EnumerateDimensions(DimensionOptions{
		Transports:     []TransportKind{TransportNetPipe, TransportAFUnix},
		Hooks:          []int{0, 1, 16},
		Plugins:        []int{1},
		PayloadSizes:   []int{64},
		FanoutDepths:   []int{0},
		PipelineDepths: []int{1},
	})

	expectedCases := []BenchmarkDimensions{
		{Transport: TransportNetPipe, Hooks: 0, Plugins: 1, PayloadSize: 64, FanoutDepth: 0, PipelineDepth: 1},
		{Transport: TransportNetPipe, Hooks: 1, Plugins: 1, PayloadSize: 64, FanoutDepth: 0, PipelineDepth: 1},
		{Transport: TransportNetPipe, Hooks: 16, Plugins: 1, PayloadSize: 64, FanoutDepth: 0, PipelineDepth: 1},
		{Transport: TransportAFUnix, Hooks: 0, Plugins: 1, PayloadSize: 64, FanoutDepth: 0, PipelineDepth: 1},
		{Transport: TransportAFUnix, Hooks: 1, Plugins: 1, PayloadSize: 64, FanoutDepth: 0, PipelineDepth: 1},
		{Transport: TransportAFUnix, Hooks: 16, Plugins: 1, PayloadSize: 64, FanoutDepth: 0, PipelineDepth: 1},
	}

	if len(matrix) != len(expectedCases) {
		t.Fatalf("expected %d generated cases, got %d", len(expectedCases), len(matrix))
	}
	for caseIndex, expectedCase := range expectedCases {
		if matrix[caseIndex] != expectedCase {
			t.Fatalf("case %d mismatch: expected %+v, got %+v", caseIndex, expectedCase, matrix[caseIndex])
		}
	}
}

func TestDurationRecorderSnapshot(t *testing.T) {
	recorder := NewDurationRecorder(100)
	for sampleNumber := 1; sampleNumber <= 100; sampleNumber++ {
		recorder.Record(time.Duration(sampleNumber) * time.Millisecond)
	}

	snapshot := recorder.Snapshot()
	if snapshot.Count != 100 {
		t.Fatalf("expected 100 samples, got %d", snapshot.Count)
	}
	if snapshot.Min != time.Millisecond {
		t.Fatalf("expected min 1ms, got %s", snapshot.Min)
	}
	if snapshot.P50 != 50*time.Millisecond {
		t.Fatalf("expected p50 50ms, got %s", snapshot.P50)
	}
	if snapshot.P95 != 95*time.Millisecond {
		t.Fatalf("expected p95 95ms, got %s", snapshot.P95)
	}
	if snapshot.P99 != 99*time.Millisecond {
		t.Fatalf("expected p99 99ms, got %s", snapshot.P99)
	}
	if snapshot.P999 != 100*time.Millisecond {
		t.Fatalf("expected p999 100ms, got %s", snapshot.P999)
	}
	if snapshot.Max != 100*time.Millisecond {
		t.Fatalf("expected max 100ms, got %s", snapshot.Max)
	}
}

func TestDurationRecorderSnapshotP999CapturesTail(t *testing.T) {
	recorder := NewDurationRecorder(1000)
	for sampleNumber := 0; sampleNumber < 998; sampleNumber++ {
		recorder.Record(time.Millisecond)
	}
	recorder.Record(100 * time.Millisecond)
	recorder.Record(time.Second)

	snapshot := recorder.Snapshot()
	if snapshot.Count != 1000 {
		t.Fatalf("expected 1000 samples, got %d", snapshot.Count)
	}
	if snapshot.P99 != time.Millisecond {
		t.Fatalf("expected p99 1ms, got %s", snapshot.P99)
	}
	if snapshot.P999 != 100*time.Millisecond {
		t.Fatalf("expected p999 100ms, got %s", snapshot.P999)
	}
	if snapshot.P999 <= snapshot.P99 {
		t.Fatalf("expected p999 to capture tail beyond p99, got p99=%s p999=%s", snapshot.P99, snapshot.P999)
	}
}

func TestDurationRecorderSnapshotSortsSamplesAndResetKeepsRecorderUsable(t *testing.T) {
	recorder := NewDurationRecorder(-10)
	for _, sample := range []time.Duration{5 * time.Millisecond, time.Millisecond, 3 * time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond} {
		recorder.Record(sample)
	}

	snapshot := recorder.Snapshot()
	if snapshot.Count != 5 {
		t.Fatalf("expected 5 samples, got %d", snapshot.Count)
	}
	if snapshot.Min != time.Millisecond {
		t.Fatalf("expected min 1ms, got %s", snapshot.Min)
	}
	if snapshot.P50 != 3*time.Millisecond {
		t.Fatalf("expected p50 3ms, got %s", snapshot.P50)
	}
	if snapshot.P95 != 5*time.Millisecond {
		t.Fatalf("expected p95 5ms, got %s", snapshot.P95)
	}
	if snapshot.P99 != 5*time.Millisecond {
		t.Fatalf("expected p99 5ms, got %s", snapshot.P99)
	}
	if snapshot.P999 != 5*time.Millisecond {
		t.Fatalf("expected p999 5ms, got %s", snapshot.P999)
	}
	if snapshot.Max != 5*time.Millisecond {
		t.Fatalf("expected max 5ms, got %s", snapshot.Max)
	}

	recorder.Reset()
	resetSnapshot := recorder.Snapshot()
	if resetSnapshot != (PercentileSnapshot{}) {
		t.Fatalf("expected empty snapshot after reset, got %+v", resetSnapshot)
	}
	recorder.Record(7 * time.Millisecond)
	postResetSnapshot := recorder.Snapshot()
	if postResetSnapshot.Count != 1 || postResetSnapshot.P50 != 7*time.Millisecond || postResetSnapshot.Max != 7*time.Millisecond {
		t.Fatalf("expected one 7ms sample after reset, got %+v", postResetSnapshot)
	}
}

func TestRunMatrixInvokesEachCaseAndRecordsPercentileMetrics(t *testing.T) {
	matrix := DimensionMatrix{
		{Transport: TransportNetPipe, Hooks: 0, Plugins: 1, PayloadSize: 64, FanoutDepth: 0, PipelineDepth: 1},
		{Transport: TransportNetPipe, Hooks: 1, Plugins: 1, PayloadSize: 256, FanoutDepth: 1, PipelineDepth: 4},
		{Transport: TransportAFUnix, Hooks: 16, Plugins: 4, PayloadSize: 1024, FanoutDepth: 8, PipelineDepth: 16},
	}

	var mu sync.Mutex
	seenCases := make(map[string]BenchmarkDimensions)
	result := testing.Benchmark(func(b *testing.B) {
		RunMatrix(b, matrix, func(b *testing.B, dimensions BenchmarkDimensions, recorder *DurationRecorder) {
			mu.Lock()
			seenCases[dimensions.Name()] = dimensions
			mu.Unlock()

			for iteration := 0; iteration < b.N; iteration++ {
				recorder.Record(time.Duration(dimensions.PayloadSize+dimensions.Hooks+dimensions.Plugins+dimensions.FanoutDepth+dimensions.PipelineDepth) * time.Nanosecond)
			}
		})
	})

	if result.N <= 0 {
		t.Fatalf("expected benchmark to execute at least one iteration, got %d", result.N)
	}
	if len(seenCases) != len(matrix) {
		t.Fatalf("expected %d invoked benchmark cases, got %d: %+v", len(matrix), len(seenCases), seenCases)
	}
	for _, expectedCase := range matrix {
		if seenCases[expectedCase.Name()] != expectedCase {
			t.Fatalf("expected RunMatrix to invoke case %+v, got %+v", expectedCase, seenCases[expectedCase.Name()])
		}
	}
}

func TestCaptureBaselineProvenanceIncludesCommitGoVersionAndDate(t *testing.T) {
	startedAt := time.Now().UTC()
	provenance, err := CaptureBaselineProvenance(context.Background(), "../../..")
	if err != nil {
		t.Fatalf("capture baseline provenance: %v", err)
	}
	finishedAt := time.Now().UTC()

	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(provenance.CommitSHA) {
		t.Fatalf("expected 40-character git commit SHA, got %q", provenance.CommitSHA)
	}
	if provenance.GoVersion != runtime.Version() {
		t.Fatalf("expected Go version %q, got %q", runtime.Version(), provenance.GoVersion)
	}
	capturedAt, parseErr := time.Parse(time.RFC3339, provenance.Date)
	if parseErr != nil {
		t.Fatalf("expected RFC3339 provenance date, got %q: %v", provenance.Date, parseErr)
	}
	if capturedAt.Before(startedAt.Add(-time.Second)) || capturedAt.After(finishedAt.Add(time.Second)) {
		t.Fatalf("expected provenance date between %s and %s, got %s", startedAt.Format(time.RFC3339), finishedAt.Format(time.RFC3339), capturedAt.Format(time.RFC3339))
	}
}

func BenchmarkRunMatrixSample(b *testing.B) {
	matrix := DimensionMatrix{
		{Transport: TransportNetPipe, Hooks: 0, Plugins: 1, PayloadSize: 64, FanoutDepth: 0, PipelineDepth: 1},
		{Transport: TransportNetPipe, Hooks: 1, Plugins: 1, PayloadSize: 256, FanoutDepth: 1, PipelineDepth: 4},
		{Transport: TransportAFUnix, Hooks: 16, Plugins: 4, PayloadSize: 1024, FanoutDepth: 8, PipelineDepth: 16},
	}

	RunMatrix(b, matrix, func(b *testing.B, dimensions BenchmarkDimensions, recorder *DurationRecorder) {
		b.ReportAllocs()
		b.ResetTimer()
		for iteration := 0; iteration < b.N; iteration++ {
			recorder.Record(time.Duration(dimensions.PayloadSize+dimensions.Hooks+dimensions.Plugins+dimensions.FanoutDepth+dimensions.PipelineDepth) * time.Nanosecond)
		}
	})
}
