package manager

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	ops "gocache/api/operations"
	commandmetrics "gocache/pkg/metrics"
	"gocache/pkg/plugin/router"
)

// ServerStateProvider is the interface the plugin manager uses to query
// core server state. Defined here to avoid import cycles — the server
// package implements this interface, and wiring happens in cmd/server/main.go.
type ServerStateProvider interface {
	IsShuttingDown() bool
	StartTime() time.Time
	ActiveConnections() int
	CacheKeys() int
	CacheUsedBytes() int64
	CacheMaxBytes() int64
}

// QueryHandlerFunc handles a server query topic and returns key-value data.
type QueryHandlerFunc func(params map[string]string) (map[string]string, error)

// QueryRegistry maps query topics to handler functions.
type QueryRegistry struct {
	mu       sync.RWMutex
	handlers map[string]QueryHandlerFunc
}

func NewQueryRegistry() *QueryRegistry {
	return &QueryRegistry{
		handlers: make(map[string]QueryHandlerFunc),
	}
}

// Register adds a handler for a query topic.
func (r *QueryRegistry) Register(topic string, handler QueryHandlerFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[topic] = handler
}

// Handle executes the handler for a topic and returns the result.
func (r *QueryRegistry) Handle(topic string, params map[string]string) (map[string]string, error) {
	r.mu.RLock()
	h, ok := r.handlers[topic]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown query topic %q", topic)
	}
	return h(params)
}

// Topics returns the list of registered topic names.
func (r *QueryRegistry) Topics() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	topics := make([]string, 0, len(r.handlers))
	for t := range r.handlers {
		topics = append(topics, t)
	}
	return topics
}

// PluginIPCStatsProvider returns point-in-time IPC measurements for plugin
// connections. The manager supplies this from its connection map so event-only
// plugins are visible even when they do not register commands with the router.
type PluginIPCStatsProvider func() []router.PluginConnStats

// CommandMetricsProvider returns point-in-time command metrics snapshots for
// metrics exporters that pull aggregates through server-query instead of
// receiving one event per command.
type CommandMetricsProvider interface {
	Buckets() []float64
	Snapshot() []commandmetrics.CommandSnapshot
}

// RegisterBuiltinHandlers registers the built-in query topics on the registry.
func RegisterBuiltinHandlers(qr *QueryRegistry, registry *Registry, ipcStats PluginIPCStatsProvider, sp ServerStateProvider) {
	if sp != nil {
		qr.Register("health", healthHandler(sp))
		qr.Register("stats", statsHandler(sp))
	}
	qr.Register("plugins", pluginsHandler(registry))
	if ipcStats != nil {
		qr.Register("plugin.ipc", pluginIPCHandler(ipcStats))
	}
}

// RegisterCommandMetricsHandlers registers command metrics query topics.
func RegisterCommandMetricsHandlers(qr *QueryRegistry, provider CommandMetricsProvider) {
	if provider != nil {
		qr.Register(commandmetrics.CommandsTopic, commandMetricsHandler(provider))
	}
}

func healthHandler(sp ServerStateProvider) QueryHandlerFunc {
	return func(_ map[string]string) (map[string]string, error) {
		status := "ok"
		if sp.IsShuttingDown() {
			status = "shutting_down"
		}
		uptime := time.Since(sp.StartTime())
		return map[string]string{
			"status":      status,
			"uptime_ns":   strconv.FormatInt(uptime.Nanoseconds(), 10),
			"connections": strconv.Itoa(sp.ActiveConnections()),
		}, nil
	}
}

func statsHandler(sp ServerStateProvider) QueryHandlerFunc {
	return func(_ map[string]string) (map[string]string, error) {
		return map[string]string{
			"keys":             strconv.Itoa(sp.CacheKeys()),
			"memory_bytes":     strconv.FormatInt(sp.CacheUsedBytes(), 10),
			"max_memory_bytes": strconv.FormatInt(sp.CacheMaxBytes(), 10),
		}, nil
	}
}

func pluginsHandler(registry *Registry) QueryHandlerFunc {
	return func(_ map[string]string) (map[string]string, error) {
		plugins := registry.All()
		data := make(map[string]string, len(plugins)*2)
		for _, p := range plugins {
			data[p.Name+".state"] = p.State().String()
			data[p.Name+".critical"] = strconv.FormatBool(p.Critical())
		}
		return data, nil
	}
}

func commandMetricsHandler(provider CommandMetricsProvider) QueryHandlerFunc {
	return func(_ map[string]string) (map[string]string, error) {
		buckets := provider.Buckets()
		snapshots := provider.Snapshot()
		data := make(map[string]string, 2+len(buckets)+len(snapshots)*(4+len(buckets)+1))
		data["buckets.count"] = strconv.Itoa(len(buckets))
		for i, bucket := range buckets {
			data[fmt.Sprintf("bucket.%d.le", i)] = strconv.FormatFloat(bucket, 'g', -1, 64)
		}
		data["commands.count"] = strconv.Itoa(len(snapshots))
		for i, snapshot := range snapshots {
			prefix := fmt.Sprintf("command.%d.", i)
			data[prefix+"name"] = snapshot.Command
			data[prefix+"total"] = strconv.FormatUint(snapshot.Total, 10)
			data[prefix+"errors"] = strconv.FormatUint(snapshot.Errors, 10)
			data[prefix+"sum_ns"] = strconv.FormatUint(snapshot.SumNs, 10)
			for j, count := range snapshot.Counts {
				data[prefix+"bucket."+strconv.Itoa(j)] = strconv.FormatUint(count, 10)
			}
		}
		return data, nil
	}
}

func pluginIPCHandler(ipcStats PluginIPCStatsProvider) QueryHandlerFunc {
	return func(_ map[string]string) (map[string]string, error) {
		stats := ipcStats()
		data := make(map[string]string, len(stats)*24)
		for _, st := range stats {
			prefix := st.PluginName + "."
			data[prefix+"queue_capacity"] = strconv.Itoa(st.QueueCapacity)
			data[prefix+"queue_depth"] = strconv.Itoa(st.QueueDepth)
			data[prefix+"send_attempts"] = strconv.FormatUint(st.SendAttempts, 10)
			data[prefix+"send_accepted"] = strconv.FormatUint(st.SendAccepted, 10)
			data[prefix+"send_queue_full"] = strconv.FormatUint(st.SendQueueFull, 10)
			data[prefix+"send_plugin_down"] = strconv.FormatUint(st.SendPluginDown, 10)
			data[prefix+"send_context_cancelled"] = strconv.FormatUint(st.SendContextCancelled, 10)
			data[prefix+"blocking_send_attempts"] = strconv.FormatUint(st.BlockingSendAttempts, 10)
			data[prefix+"blocking_send_latency_total_ns"] = strconv.FormatUint(st.BlockingSendLatencyTotalNs, 10)
			data[prefix+"blocking_send_latency_max_ns"] = strconv.FormatUint(st.BlockingSendLatencyMaxNs, 10)
			data[prefix+"fire_and_forget_attempts"] = strconv.FormatUint(st.FireAndForgetAttempts, 10)
			data[prefix+"fire_and_forget_accepted"] = strconv.FormatUint(st.FireAndForgetAccepted, 10)
			data[prefix+"fire_and_forget_drops"] = strconv.FormatUint(st.FireAndForgetDrops, 10)
			data[prefix+"enqueue_latency_total_ns"] = strconv.FormatUint(st.EnqueueLatencyTotalNs, 10)
			data[prefix+"enqueue_latency_max_ns"] = strconv.FormatUint(st.EnqueueLatencyMaxNs, 10)
			data[prefix+"write_attempts"] = strconv.FormatUint(st.WriteAttempts, 10)
			data[prefix+"write_errors"] = strconv.FormatUint(st.WriteErrors, 10)
			data[prefix+"write_batches"] = strconv.FormatUint(st.WriteBatches, 10)
			data[prefix+"write_batch_envelopes"] = strconv.FormatUint(st.WriteBatchEnvelopes, 10)
			data[prefix+"write_batch_max_size"] = strconv.FormatUint(st.WriteBatchMaxSize, 10)
			data[prefix+"write_latency_total_ns"] = strconv.FormatUint(st.WriteLatencyTotalNs, 10)
			data[prefix+"write_latency_max_ns"] = strconv.FormatUint(st.WriteLatencyMaxNs, 10)
			data[prefix+"queue_lag_total_ns"] = strconv.FormatUint(st.QueueLagTotalNs, 10)
			data[prefix+"queue_lag_max_ns"] = strconv.FormatUint(st.QueueLagMaxNs, 10)
		}
		return data, nil
	}
}

func (m *Manager) pluginIPCStats() []router.PluginConnStats {
	var stats []router.PluginConnStats
	m.pluginConns.Range(func(_, value any) bool {
		pc, ok := value.(*router.PluginConn)
		if !ok {
			return true
		}
		stats = append(stats, pc.Stats())
		return true
	})
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].PluginName < stats[j].PluginName
	})
	return stats
}

// OperationTracker is the subset of pkg/operations.Tracker needed by
// plugin-initiated operation management queries.
type OperationTracker interface {
	Start(opType ops.Type, parentID string) *ops.Operation
	Complete(id string)
	Fail(id, reason string)
}

// RegisterOperationHandlers registers query topics for plugin-initiated
// operation lifecycle management.
func RegisterOperationHandlers(qr *QueryRegistry, tracker OperationTracker) {
	qr.Register("operation.start", func(params map[string]string) (map[string]string, error) {
		opType := ops.Type(params["type"])
		if opType == "" {
			opType = ops.TypeCommand
		}
		op := tracker.Start(opType, params["parent_id"])
		return op.ContextSnapshot(false), nil
	})
	qr.Register("operation.complete", func(params map[string]string) (map[string]string, error) {
		tracker.Complete(params["_operation_id"])
		return nil, nil
	})
	qr.Register("operation.fail", func(params map[string]string) (map[string]string, error) {
		tracker.Fail(params["_operation_id"], params["_fail_reason"])
		return nil, nil
	})
}
