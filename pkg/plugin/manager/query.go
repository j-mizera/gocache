package manager

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	apicommand "gocache/api/command"
	apiobs "gocache/api/observability"
	ops "gocache/api/operations"
	commonobs "gocache/commons/observability"
	"gocache/pkg/benchstats"
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
	qr.Register("bench.stats", benchStatsHandler())
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

func benchStatsHandler() QueryHandlerFunc {
	return func(params map[string]string) (map[string]string, error) {
		reset := false
		if params != nil {
			reset = parseQueryBool(params["reset"])
		}
		return benchstats.Snapshot(reset), nil
	}
}

func parseQueryBool(value string) bool {
	switch value {
	case "1", "true", "TRUE", "True", "yes", "YES", "on", "ON":
		return true
	default:
		return false
	}
}

func pluginIPCHandler(ipcStats PluginIPCStatsProvider) QueryHandlerFunc {
	return func(_ map[string]string) (map[string]string, error) {
		stats := ipcStats()
		data := make(map[string]string, len(stats)*25)
		for _, st := range stats {
			prefix := st.PluginName + "."
			data[prefix+"queue_capacity"] = strconv.Itoa(st.QueueCapacity)
			data[prefix+"queue_depth"] = strconv.Itoa(st.QueueDepth)
			data[prefix+"queue_headroom"] = strconv.Itoa(st.QueueHeadroom)
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

// OperationTracker is the subset needed by plugin-initiated operation
// management queries. Production wiring uses TelemetryOperationTracker so runtime
// telemetry is submitted to OperationTracker telemetry records before projection.
type OperationTracker interface {
	Start(opType ops.Type, parentID string) *ops.Operation
	Complete(id string)
	Fail(id, reason string)
}

type telemetryActiveOperation struct {
	op    *ops.Operation
	scope commonobs.OperationScope
}

// TelemetryOperationTracker implements plugin-initiated operation queries on top
// of the compact telemetry tracker while preserving the existing query contract
// that returns an operation context map to plugins.
type TelemetryOperationTracker struct {
	manager      *commonobs.SlotOperationTrackerManager
	identityBase apiobs.InternalOperationIdentity
	sequence     atomic.Uint64
	active       sync.Map // operation id -> telemetryActiveOperation
}

func NewTelemetryOperationTracker(manager *commonobs.SlotOperationTrackerManager, identityBase apiobs.InternalOperationIdentity) *TelemetryOperationTracker {
	return &TelemetryOperationTracker{manager: manager, identityBase: identityBase}
}

func (t *TelemetryOperationTracker) Start(opType ops.Type, parentID string) *ops.Operation {
	op := ops.New(opType, parentID)
	enrichTelemetryOperationStart(op)
	if t == nil || t.manager == nil {
		return op
	}
	sequence := t.sequence.Add(1)
	if sequence == 0 {
		sequence = t.sequence.Add(1)
	}
	operation := t.identityBase + apiobs.InternalOperationIdentity(sequence)
	ref := apiobs.NewOperationRef(op.ID, parentID)
	handle, ok := t.manager.StartOperationWithMetadata(operation, apiobs.NewParentRef(parentID), 0, commonobs.OperationSnapshotMetadata{
		Type:          string(op.Type),
		Ref:           ref,
		StartUnixNano: op.StartTime.UnixNano(),
	})
	if !ok {
		return op
	}
	scope := commonobs.NewOperationScope(t.manager, handle, operation, ref)
	scope.ContextUpdateStrings(
		apicommand.OperationID, op.ID,
		apicommand.StartNs, strconv.FormatInt(op.StartTime.UnixNano(), 10),
		"_operation_type", string(op.Type),
		"_parent_operation_id", parentID,
	)
	scope.OperationStartString(string(op.Type),
		apicommand.OperationID, op.ID,
		"_operation_type", string(op.Type),
		"_parent_operation_id", parentID,
	)
	t.active.Store(op.ID, telemetryActiveOperation{op: op, scope: scope})
	return op
}

func (t *TelemetryOperationTracker) Complete(id string) {
	t.finish(id, commonobs.SlotTerminalFinished, "")
}

func (t *TelemetryOperationTracker) Fail(id, reason string) {
	t.finish(id, commonobs.SlotTerminalFailed, reason)
}

func (t *TelemetryOperationTracker) EventString(id, eventName string, fields ...string) bool {
	if t == nil {
		return false
	}
	value, ok := t.active.Load(id)
	if !ok {
		return false
	}
	active := value.(telemetryActiveOperation)
	fields = append([]string{apicommand.OperationID, id}, fields...)
	return active.scope.EventString(eventName, fields...)
}

func (t *TelemetryOperationTracker) LogString(id string, level apiobs.TelemetryLogLevel, message string, fields ...string) bool {
	if t == nil {
		return false
	}
	value, ok := t.active.Load(id)
	if !ok {
		return false
	}
	active := value.(telemetryActiveOperation)
	record := apiobs.NewLogRecordString(active.scope.Operation(), level, message)
	record.TimestampUnixNano = time.Now().UnixNano()
	for i := 0; i+1 < len(fields); i += 2 {
		record.AddFieldString(fields[i], fields[i+1])
	}
	return active.scope.Record(record)
}

func (t *TelemetryOperationTracker) ContextUpdateStrings(id string, fields ...string) bool {
	if t == nil {
		return false
	}
	value, ok := t.active.Load(id)
	if !ok {
		return false
	}
	active := value.(telemetryActiveOperation)
	return active.scope.ContextUpdateStrings(fields...)
}

func enrichTelemetryOperationStart(op *ops.Operation) {
	if op == nil {
		return
	}
	op.Enrich(apicommand.OperationID, op.ID)
	op.Enrich(apicommand.StartNs, strconv.FormatInt(op.StartTime.UnixNano(), 10))
	op.Enrich("_operation_type", string(op.Type))
	if op.ParentID != "" {
		op.Enrich("_parent_operation_id", op.ParentID)
	}
}

func (t *TelemetryOperationTracker) finish(id string, terminal commonobs.SlotTerminalStatus, reason string) {
	if t == nil {
		return
	}
	value, ok := t.active.LoadAndDelete(id)
	if !ok {
		return
	}
	active := value.(telemetryActiveOperation)
	if reason != "" {
		active.op.Fail(reason)
	} else {
		active.op.Complete()
	}
	elapsedNs := uint64(active.op.Duration().Nanoseconds())
	status := "completed"
	if terminal == commonobs.SlotTerminalFailed {
		status = "failed"
	}
	if reason != "" {
		active.scope.ContextUpdateStrings(apicommand.ElapsedNs, strconv.FormatUint(elapsedNs, 10), apicommand.ErrorKey, reason)
	} else {
		active.scope.ContextUpdateStrings(apicommand.ElapsedNs, strconv.FormatUint(elapsedNs, 10))
	}
	active.scope.OperationFinishString(string(active.op.Type), elapsedNs,
		apicommand.OperationID, active.op.ID,
		"_operation_type", string(active.op.Type),
		"_status", status,
		apicommand.ElapsedNs, strconv.FormatUint(elapsedNs, 10),
		apicommand.ErrorKey, reason,
	)
	active.scope.Finish(terminal)
}

// RegisterOperationHandlers registers query topics for plugin-initiated
// operation lifecycle management.
func RegisterOperationHandlers(qr *QueryRegistry, operationTracker OperationTracker) {
	qr.Register("operation.start", func(params map[string]string) (map[string]string, error) {
		opType := ops.Type(params["type"])
		if opType == "" {
			opType = ops.TypeCommand
		}
		op := operationTracker.Start(opType, params["parent_id"])
		return op.ContextSnapshot(false), nil
	})
	qr.Register("operation.complete", func(params map[string]string) (map[string]string, error) {
		operationTracker.Complete(params["_operation_id"])
		return nil, nil
	})
	qr.Register("operation.fail", func(params map[string]string) (map[string]string, error) {
		operationTracker.Fail(params["_operation_id"], params["_fail_reason"])
		return nil, nil
	})
}
