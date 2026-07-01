package benchstats

import (
	"fmt"
	"os"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

type operationTrackerShardStats interface {
	ShardCount() int
	ShardSkipped(index int) uint64
	ShardActiveSlots(index int) int
	ShardFreeSlots(index int) int
	ShardCompletedSlots(index int) int
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
	if shardStats, ok := manager.(operationTrackerShardStats); ok {
		for i := 0; i < shardStats.ShardCount(); i++ {
			prefix := fmt.Sprintf("operation_tracker.shard_%d.", i)
			data[prefix+"skipped"] = formatUint(shardStats.ShardSkipped(i))
			data[prefix+"active"] = formatInt(shardStats.ShardActiveSlots(i))
			data[prefix+"free"] = formatInt(shardStats.ShardFreeSlots(i))
			data[prefix+"completed"] = formatInt(shardStats.ShardCompletedSlots(i))
		}
	}
}

// RecordPipelineEvaluation records one core command evaluation.
func RecordPipelineEvaluation() {
	if !Enabled() {
		return
	}
	global.pipelineEvaluations.Add(1)
}

// RecordPipelineCommandUnknown records one unknown-command pipeline decision.
func RecordPipelineCommandUnknown() {
	if !Enabled() {
		return
	}
	global.pipelineCommandUnknown.Add(1)
}

// RecordPipelineCommandArgError records one argument-error pipeline decision.
func RecordPipelineCommandArgError() {
	if !Enabled() {
		return
	}
	global.pipelineCommandArgError.Add(1)
}

// RecordPipelineCommandQueued records one transaction-queued pipeline decision.
func RecordPipelineCommandQueued() {
	if !Enabled() {
		return
	}
	global.pipelineCommandQueued.Add(1)
}

// RecordPipelinePluginRouted records one plugin-routed pipeline decision.
func RecordPipelinePluginRouted() {
	if !Enabled() {
		return
	}
	global.pipelinePluginRouted.Add(1)
}

// RecordManagerEventReceived records one event received by the plugin manager
// bridge handler.
func RecordManagerEventReceived() {
	if !Enabled() {
		return
	}
	global.managerEventReceived.Add(1)
}

// RecordManagerEventDropped records one event dropped by benchmark event bridge
// mode before IPC enqueue.
func RecordManagerEventDropped() {
	if !Enabled() {
		return
	}
	global.managerEventDropped.Add(1)
}

// RecordManagerProjectionBuild records one event projection built for IPC
// delivery.
func RecordManagerProjectionBuild() {
	if !Enabled() {
		return
	}
	global.managerProjectionBuilds.Add(1)
}

// RecordManagerEventEnqueue records one event enqueue attempt to a plugin
// connection.
func RecordManagerEventEnqueue() {
	if !Enabled() {
		return
	}
	global.managerEventEnqueueAttempts.Add(1)
}

type counters struct {
	pipelineEvaluations         atomic.Uint64
	pipelineCommandUnknown      atomic.Uint64
	pipelineCommandArgError     atomic.Uint64
	pipelineCommandQueued       atomic.Uint64
	pipelinePluginRouted        atomic.Uint64
	managerEventReceived        atomic.Uint64
	managerEventDropped         atomic.Uint64
	managerProjectionBuilds     atomic.Uint64
	managerEventEnqueueAttempts atomic.Uint64
}

func (c *counters) reset() {
	c.pipelineEvaluations.Store(0)
	c.pipelineCommandUnknown.Store(0)
	c.pipelineCommandArgError.Store(0)
	c.pipelineCommandQueued.Store(0)
	c.pipelinePluginRouted.Store(0)
	c.managerEventReceived.Store(0)
	c.managerEventDropped.Store(0)
	c.managerProjectionBuilds.Store(0)
	c.managerEventEnqueueAttempts.Store(0)
}

func (c *counters) snapshot() map[string]string {
	return map[string]string{
		"pipeline.evaluations":           formatUint(c.pipelineEvaluations.Load()),
		"pipeline.command_unknown":       formatUint(c.pipelineCommandUnknown.Load()),
		"pipeline.command_arg_error":     formatUint(c.pipelineCommandArgError.Load()),
		"pipeline.command_queued":        formatUint(c.pipelineCommandQueued.Load()),
		"pipeline.plugin_routed":         formatUint(c.pipelinePluginRouted.Load()),
		"manager.event_received":         formatUint(c.managerEventReceived.Load()),
		"manager.event_dropped":          formatUint(c.managerEventDropped.Load()),
		"manager.projection_builds":      formatUint(c.managerProjectionBuilds.Load()),
		"manager.event_enqueue_attempts": formatUint(c.managerEventEnqueueAttempts.Load()),
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

func formatUint(value uint64) string {
	return strconv.FormatUint(value, 10)
}

func formatInt(v int) string {
	return strconv.Itoa(v)
}
