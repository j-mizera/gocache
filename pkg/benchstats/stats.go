package benchstats

import (
	"os"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// EnvVar enables benchmark attribution counters and runtime snapshots.
	// It is intentionally benchmark-scoped so normal deployments avoid counter
	// mutations and runtime snapshots; disabled probes still pay only a cheap
	// enabled-check branch on command/event hot paths.
	EnvVar = "GOCACHE_BENCH_STATS"
)

var enabled atomic.Bool

var global counters

type operationTrackerStats interface {
	SkippedOperations() uint64
	DroppedRecords() uint64
	DroppedCompletedOperations() uint64
}

var operationTracker struct {
	mu      sync.RWMutex
	manager operationTrackerStats
}

func init() {
	enabled.Store(parseBool(os.Getenv(EnvVar)))
}

func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	default:
		return false
	}
}

// Enabled reports whether benchmark attribution counters are active.
func Enabled() bool {
	return enabled.Load()
}

// SetEnabled changes benchmark attribution at runtime. It exists for tests and
// benchmark-only harnesses; production code should prefer EnvVar at startup.
func SetEnabled(value bool) {
	enabled.Store(value)
}

// StartTimer returns a start timestamp only when benchmark attribution is
// enabled. Callers can pass the zero value to Record*Duration functions; they
// will ignore it. This keeps disabled probes from paying time.Now costs.
func StartTimer() time.Time {
	if !Enabled() {
		return time.Time{}
	}
	return time.Now()
}

// Reset clears all benchmark attribution counters.
func Reset() {
	global.reset()
}

// SetOperationTrackerManager wires live operation-tracker loss counters into
// benchmark snapshots. Passing nil clears the tracker.
func SetOperationTrackerManager(manager operationTrackerStats) {
	operationTracker.mu.Lock()
	operationTracker.manager = manager
	operationTracker.mu.Unlock()
}

// Snapshot returns benchmark attribution counters plus a small runtime/metrics
// sample. If reset is true, counters are cleared after the snapshot is built.
func Snapshot(reset bool) map[string]string {
	data := global.snapshot()
	data["enabled"] = strconv.FormatBool(Enabled())
	addOperationTrackerStats(data)
	addRuntimeMetrics(data)
	if reset {
		Reset()
	}
	return data
}

func addOperationTrackerStats(data map[string]string) {
	operationTracker.mu.RLock()
	manager := operationTracker.manager
	operationTracker.mu.RUnlock()
	if manager == nil {
		data["operation_tracker.skipped_operations"] = "0"
		data["operation_tracker.dropped_records"] = "0"
		data["operation_tracker.dropped_completed"] = "0"
		return
	}
	data["operation_tracker.skipped_operations"] = formatUint(manager.SkippedOperations())
	data["operation_tracker.dropped_records"] = formatUint(manager.DroppedRecords())
	data["operation_tracker.dropped_completed"] = formatUint(manager.DroppedCompletedOperations())
}

// RecordPipelineEvaluation records one core command evaluation. Plugin-routed
// commands are tracked by plugin IPC stats instead.
func RecordPipelineEvaluation() {
	if !Enabled() {
		return
	}
	global.pipelineEvaluations.Add(1)
}

// RecordPipelineFastPath is retained for older benchmark snapshots. The command
// pipeline no longer uses a no-sink telemetry bypass.
func RecordPipelineFastPath() {
	if !Enabled() {
		return
	}
	global.pipelineFastPath.Add(1)
}

// RecordPipelineMetricsOnlyPath is retained for older benchmark snapshots. A
// command metrics sink no longer suppresses command telemetry capture.
func RecordPipelineMetricsOnlyPath() {
	if !Enabled() {
		return
	}
	global.pipelineMetricsOnlyPath.Add(1)
}

func RecordPipelineFullPath() {
	if !Enabled() {
		return
	}
	global.pipelineFullPath.Add(1)
}

func RecordPipelineContextSnapshot(start time.Time) {
	if !Enabled() || start.IsZero() {
		return
	}
	global.pipelineContextSnapshots.Add(1)
	observeDuration(&global.pipelineContextSnapshotTotalNs, &global.pipelineContextSnapshotMaxNs, time.Since(start))
}

func RecordPipelineOperationStartedBuilt(start time.Time) {
	if !Enabled() {
		return
	}
	global.pipelineOperationStartedBuilt.Add(1)
	observeSince(&global.pipelineOperationStartedBuildTotalNs, &global.pipelineOperationStartedBuildMaxNs, start)
}

func RecordPipelineOperationCompletedBuilt(start time.Time) {
	if !Enabled() {
		return
	}
	global.pipelineOperationCompletedBuilt.Add(1)
	observeSince(&global.pipelineOperationCompletedBuildTotalNs, &global.pipelineOperationCompletedBuildMaxNs, start)
}

func RecordPipelineCommandStartedBuilt(start time.Time) {
	if !Enabled() {
		return
	}
	global.pipelineCommandStartedBuilt.Add(1)
	observeSince(&global.pipelineCommandStartedBuildTotalNs, &global.pipelineCommandStartedBuildMaxNs, start)
}

func RecordPipelineCommandCompletedBuilt(start time.Time) {
	if !Enabled() {
		return
	}
	global.pipelineCommandCompletedBuilt.Add(1)
	observeSince(&global.pipelineCommandCompletedBuildTotalNs, &global.pipelineCommandCompletedBuildMaxNs, start)
}

func RecordEventBusInterestCheck(hit bool) {
	if !Enabled() {
		return
	}
	global.eventBusInterestChecks.Add(1)
	if hit {
		global.eventBusInterestHits.Add(1)
	}
}

func RecordEventBusEmit(targets int, start time.Time) {
	if !Enabled() {
		return
	}
	global.eventBusEmits.Add(1)
	global.eventBusFanoutTargets.Add(uint64(targets))
	observeMax(&global.eventBusMaxFanout, uint64(targets))
	observeSince(&global.eventBusEmitTotalNs, &global.eventBusEmitMaxNs, start)
}

func RecordEventBusNoSubscriberEmit(start time.Time) {
	if !Enabled() {
		return
	}
	global.eventBusNoSubscriberEmits.Add(1)
	observeSince(&global.eventBusNoSubscriberEmitTotalNs, &global.eventBusNoSubscriberEmitMaxNs, start)
}

func RecordEventBusDelivery(start time.Time) {
	if !Enabled() || start.IsZero() {
		return
	}
	global.eventBusDeliveries.Add(1)
	observeDuration(&global.eventBusDeliveryTotalNs, &global.eventBusDeliveryMaxNs, time.Since(start))
}

func RecordManagerEventReceived() {
	if !Enabled() {
		return
	}
	global.managerEventReceived.Add(1)
}

func RecordManagerBridgeOffDrop() {
	if !Enabled() {
		return
	}
	global.managerBridgeOffDrops.Add(1)
}

func RecordManagerBridgeHandler(start time.Time) {
	if !Enabled() {
		return
	}
	global.managerBridgeHandlerRuns.Add(1)
	observeSince(&global.managerBridgeHandlerTotalNs, &global.managerBridgeHandlerMaxNs, start)
}

func RecordManagerEventEnqueue(start time.Time) {
	if !Enabled() {
		return
	}
	global.managerEventEnqueueAttempts.Add(1)
	observeSince(&global.managerEventEnqueueTotalNs, &global.managerEventEnqueueMaxNs, start)
}

func RecordManagerProjection(start time.Time) {
	if !Enabled() {
		return
	}
	global.managerProjectionBuilds.Add(1)
	observeSince(&global.managerProjectionTotalNs, &global.managerProjectionMaxNs, start)
}

type counters struct {
	pipelineEvaluations                    atomic.Uint64
	pipelineFastPath                       atomic.Uint64
	pipelineMetricsOnlyPath                atomic.Uint64
	pipelineFullPath                       atomic.Uint64
	pipelineContextSnapshots               atomic.Uint64
	pipelineContextSnapshotTotalNs         atomic.Uint64
	pipelineContextSnapshotMaxNs           atomic.Uint64
	pipelineOperationStartedBuilt          atomic.Uint64
	pipelineOperationStartedBuildTotalNs   atomic.Uint64
	pipelineOperationStartedBuildMaxNs     atomic.Uint64
	pipelineOperationCompletedBuilt        atomic.Uint64
	pipelineOperationCompletedBuildTotalNs atomic.Uint64
	pipelineOperationCompletedBuildMaxNs   atomic.Uint64
	pipelineCommandStartedBuilt            atomic.Uint64
	pipelineCommandStartedBuildTotalNs     atomic.Uint64
	pipelineCommandStartedBuildMaxNs       atomic.Uint64
	pipelineCommandCompletedBuilt          atomic.Uint64
	pipelineCommandCompletedBuildTotalNs   atomic.Uint64
	pipelineCommandCompletedBuildMaxNs     atomic.Uint64
	eventBusInterestChecks                 atomic.Uint64
	eventBusInterestHits                   atomic.Uint64
	eventBusEmits                          atomic.Uint64
	eventBusEmitTotalNs                    atomic.Uint64
	eventBusEmitMaxNs                      atomic.Uint64
	eventBusNoSubscriberEmits              atomic.Uint64
	eventBusNoSubscriberEmitTotalNs        atomic.Uint64
	eventBusNoSubscriberEmitMaxNs          atomic.Uint64
	eventBusFanoutTargets                  atomic.Uint64
	eventBusMaxFanout                      atomic.Uint64
	eventBusDeliveries                     atomic.Uint64
	eventBusDeliveryTotalNs                atomic.Uint64
	eventBusDeliveryMaxNs                  atomic.Uint64
	managerEventReceived                   atomic.Uint64
	managerBridgeOffDrops                  atomic.Uint64
	managerBridgeHandlerRuns               atomic.Uint64
	managerBridgeHandlerTotalNs            atomic.Uint64
	managerBridgeHandlerMaxNs              atomic.Uint64
	managerEventEnqueueAttempts            atomic.Uint64
	managerEventEnqueueTotalNs             atomic.Uint64
	managerEventEnqueueMaxNs               atomic.Uint64
	managerProjectionBuilds                atomic.Uint64
	managerProjectionTotalNs               atomic.Uint64
	managerProjectionMaxNs                 atomic.Uint64
}

func (c *counters) reset() {
	c.pipelineEvaluations.Store(0)
	c.pipelineFastPath.Store(0)
	c.pipelineMetricsOnlyPath.Store(0)
	c.pipelineFullPath.Store(0)
	c.pipelineContextSnapshots.Store(0)
	c.pipelineContextSnapshotTotalNs.Store(0)
	c.pipelineContextSnapshotMaxNs.Store(0)
	c.pipelineOperationStartedBuilt.Store(0)
	c.pipelineOperationStartedBuildTotalNs.Store(0)
	c.pipelineOperationStartedBuildMaxNs.Store(0)
	c.pipelineOperationCompletedBuilt.Store(0)
	c.pipelineOperationCompletedBuildTotalNs.Store(0)
	c.pipelineOperationCompletedBuildMaxNs.Store(0)
	c.pipelineCommandStartedBuilt.Store(0)
	c.pipelineCommandStartedBuildTotalNs.Store(0)
	c.pipelineCommandStartedBuildMaxNs.Store(0)
	c.pipelineCommandCompletedBuilt.Store(0)
	c.pipelineCommandCompletedBuildTotalNs.Store(0)
	c.pipelineCommandCompletedBuildMaxNs.Store(0)
	c.eventBusInterestChecks.Store(0)
	c.eventBusInterestHits.Store(0)
	c.eventBusEmits.Store(0)
	c.eventBusEmitTotalNs.Store(0)
	c.eventBusEmitMaxNs.Store(0)
	c.eventBusNoSubscriberEmits.Store(0)
	c.eventBusNoSubscriberEmitTotalNs.Store(0)
	c.eventBusNoSubscriberEmitMaxNs.Store(0)
	c.eventBusFanoutTargets.Store(0)
	c.eventBusMaxFanout.Store(0)
	c.eventBusDeliveries.Store(0)
	c.eventBusDeliveryTotalNs.Store(0)
	c.eventBusDeliveryMaxNs.Store(0)
	c.managerEventReceived.Store(0)
	c.managerBridgeOffDrops.Store(0)
	c.managerBridgeHandlerRuns.Store(0)
	c.managerBridgeHandlerTotalNs.Store(0)
	c.managerBridgeHandlerMaxNs.Store(0)
	c.managerEventEnqueueAttempts.Store(0)
	c.managerEventEnqueueTotalNs.Store(0)
	c.managerEventEnqueueMaxNs.Store(0)
	c.managerProjectionBuilds.Store(0)
	c.managerProjectionTotalNs.Store(0)
	c.managerProjectionMaxNs.Store(0)
}

func (c *counters) snapshot() map[string]string {
	return map[string]string{
		"pipeline.evaluations":                            formatUint(c.pipelineEvaluations.Load()),
		"pipeline.path.fast":                              formatUint(c.pipelineFastPath.Load()),
		"pipeline.path.metrics_only":                      formatUint(c.pipelineMetricsOnlyPath.Load()),
		"pipeline.path.full":                              formatUint(c.pipelineFullPath.Load()),
		"pipeline.context_snapshots":                      formatUint(c.pipelineContextSnapshots.Load()),
		"pipeline.context_snapshot_total_ns":              formatUint(c.pipelineContextSnapshotTotalNs.Load()),
		"pipeline.context_snapshot_max_ns":                formatUint(c.pipelineContextSnapshotMaxNs.Load()),
		"pipeline.event.operation_started":                formatUint(c.pipelineOperationStartedBuilt.Load()),
		"pipeline.event.operation_started_build_ns":       formatUint(c.pipelineOperationStartedBuildTotalNs.Load()),
		"pipeline.event.operation_started_build_max_ns":   formatUint(c.pipelineOperationStartedBuildMaxNs.Load()),
		"pipeline.event.operation_completed":              formatUint(c.pipelineOperationCompletedBuilt.Load()),
		"pipeline.event.operation_completed_build_ns":     formatUint(c.pipelineOperationCompletedBuildTotalNs.Load()),
		"pipeline.event.operation_completed_build_max_ns": formatUint(c.pipelineOperationCompletedBuildMaxNs.Load()),
		"pipeline.event.command_started":                  formatUint(c.pipelineCommandStartedBuilt.Load()),
		"pipeline.event.command_started_build_ns":         formatUint(c.pipelineCommandStartedBuildTotalNs.Load()),
		"pipeline.event.command_started_build_max_ns":     formatUint(c.pipelineCommandStartedBuildMaxNs.Load()),
		"pipeline.event.command_completed":                formatUint(c.pipelineCommandCompletedBuilt.Load()),
		"pipeline.event.command_completed_build_ns":       formatUint(c.pipelineCommandCompletedBuildTotalNs.Load()),
		"pipeline.event.command_completed_build_max_ns":   formatUint(c.pipelineCommandCompletedBuildMaxNs.Load()),
		"event_bus.interest_checks":                       formatUint(c.eventBusInterestChecks.Load()),
		"event_bus.interest_hits":                         formatUint(c.eventBusInterestHits.Load()),
		"event_bus.emits":                                 formatUint(c.eventBusEmits.Load()),
		"event_bus.emit_latency_total_ns":                 formatUint(c.eventBusEmitTotalNs.Load()),
		"event_bus.emit_latency_max_ns":                   formatUint(c.eventBusEmitMaxNs.Load()),
		"event_bus.emits_no_subscribers":                  formatUint(c.eventBusNoSubscriberEmits.Load()),
		"event_bus.emit_no_subscriber_latency_total_ns":   formatUint(c.eventBusNoSubscriberEmitTotalNs.Load()),
		"event_bus.emit_no_subscriber_latency_max_ns":     formatUint(c.eventBusNoSubscriberEmitMaxNs.Load()),
		"event_bus.fanout_targets_total":                  formatUint(c.eventBusFanoutTargets.Load()),
		"event_bus.fanout_targets_max":                    formatUint(c.eventBusMaxFanout.Load()),
		"event_bus.deliveries":                            formatUint(c.eventBusDeliveries.Load()),
		"event_bus.delivery_latency_total_ns":             formatUint(c.eventBusDeliveryTotalNs.Load()),
		"event_bus.delivery_latency_max_ns":               formatUint(c.eventBusDeliveryMaxNs.Load()),
		"manager.event_received":                          formatUint(c.managerEventReceived.Load()),
		"manager.bridge_off_drops":                        formatUint(c.managerBridgeOffDrops.Load()),
		"manager.bridge_handler_runs":                     formatUint(c.managerBridgeHandlerRuns.Load()),
		"manager.bridge_handler_latency_total_ns":         formatUint(c.managerBridgeHandlerTotalNs.Load()),
		"manager.bridge_handler_latency_max_ns":           formatUint(c.managerBridgeHandlerMaxNs.Load()),
		"manager.event_enqueue_attempts":                  formatUint(c.managerEventEnqueueAttempts.Load()),
		"manager.event_enqueue_latency_total_ns":          formatUint(c.managerEventEnqueueTotalNs.Load()),
		"manager.event_enqueue_latency_max_ns":            formatUint(c.managerEventEnqueueMaxNs.Load()),
		"manager.projection_builds":                       formatUint(c.managerProjectionBuilds.Load()),
		"manager.projection_latency_total_ns":             formatUint(c.managerProjectionTotalNs.Load()),
		"manager.projection_latency_max_ns":               formatUint(c.managerProjectionMaxNs.Load()),
	}
}

func addRuntimeMetrics(data map[string]string) {
	samples := []metrics.Sample{
		{Name: "/gc/heap/allocs:bytes"},
		{Name: "/gc/heap/allocs:objects"},
		{Name: "/gc/heap/objects:objects"},
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/sched/goroutines:goroutines"},
		{Name: "/sched/goroutines/waiting:goroutines"},
		{Name: "/sched/goroutines/runnable:goroutines"},
		{Name: "/sync/mutex/wait/total:seconds"},
	}
	metrics.Read(samples)
	for _, sample := range samples {
		key := "runtime." + sanitizeMetricName(sample.Name)
		switch sample.Value.Kind() {
		case metrics.KindUint64:
			data[key] = formatUint(sample.Value.Uint64())
		case metrics.KindFloat64:
			data[key] = strconv.FormatFloat(sample.Value.Float64(), 'g', -1, 64)
		}
	}
}

func sanitizeMetricName(name string) string {
	name = strings.TrimPrefix(name, "/")
	name = strings.ReplaceAll(name, "/", ".")
	name = strings.ReplaceAll(name, ":", ".")
	name = strings.ReplaceAll(name, "-", "_")
	return name
}

func observeSince(total, max *atomic.Uint64, start time.Time) {
	if start.IsZero() {
		return
	}
	observeDuration(total, max, time.Since(start))
}

func observeDuration(total, max *atomic.Uint64, duration time.Duration) {
	ns := uint64(duration.Nanoseconds())
	total.Add(ns)
	observeMax(max, ns)
}

func observeMax(max *atomic.Uint64, value uint64) {
	for {
		current := max.Load()
		if value <= current || max.CompareAndSwap(current, value) {
			return
		}
	}
}

func formatUint(value uint64) string {
	return strconv.FormatUint(value, 10)
}
