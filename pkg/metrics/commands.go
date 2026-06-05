package metrics

import (
	"sort"
	"sync"
	"sync/atomic"
)

const (
	// CommandsTopic is the server-query topic for command metrics snapshots.
	CommandsTopic = "metrics.commands"

	nsPerSec = 1e9

	defaultCommandMetricsRingCapacity = 8192
)

// DefaultCommandDurationBuckets are the command-latency histogram bucket
// boundaries in seconds. They intentionally match the Prometheus plugin's
// current exposition buckets so the server can aggregate before IPC without a
// new GCPC schema.
var DefaultCommandDurationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0}

type commandStats struct {
	total  uint64
	errors uint64
	sumNs  uint64
	counts []uint64
}

type commandMetricRecord struct {
	command   string
	elapsedNs uint64
	isError   bool
}

// CommandSnapshot is a copy of one command's accumulated metrics.
type CommandSnapshot struct {
	Command string
	Total   uint64
	Errors  uint64
	SumNs   uint64
	Counts  []uint64
}

// CommandCollector aggregates low-cardinality command metrics in-process.
// Recording is gated by an active-consumer reference count so deployments that
// do not grant a metrics query scope pay only a cheap atomic check. Active
// producers enqueue compact records into a bounded sidecar ring; aggregation and
// map/histogram mutation happen when snapshots are polled, not on the command
// goroutine.
type CommandCollector struct {
	active atomic.Int32
	drops  atomic.Uint64

	pending chan commandMetricRecord

	mu      sync.Mutex
	stats   map[string]*commandStats
	buckets []float64
}

// NewCommandCollector creates a command metrics collector.
func NewCommandCollector() *CommandCollector {
	return newCommandCollector(defaultCommandMetricsRingCapacity)
}

func newCommandCollector(ringCapacity int) *CommandCollector {
	if ringCapacity < 1 {
		ringCapacity = 1
	}
	buckets := append([]float64(nil), DefaultCommandDurationBuckets...)
	return &CommandCollector{
		pending: make(chan commandMetricRecord, ringCapacity),
		stats:   make(map[string]*commandStats),
		buckets: buckets,
	}
}

// AddConsumer enables recording for one active metrics consumer.
func (c *CommandCollector) AddConsumer() {
	if c == nil {
		return
	}
	c.active.Add(1)
}

// RemoveConsumer disables recording for one active metrics consumer.
func (c *CommandCollector) RemoveConsumer() {
	if c == nil {
		return
	}
	for {
		current := c.active.Load()
		if current <= 0 {
			return
		}
		if c.active.CompareAndSwap(current, current-1) {
			return
		}
	}
}

// HasCommandMetricsSink reports whether command metrics have an active
// consumer. The pipeline calls this on the command path, so it must remain a
// lock-free check.
func (c *CommandCollector) HasCommandMetricsSink() bool {
	return c != nil && c.active.Load() > 0
}

// RecordCommand enqueues one command completion for sidecar aggregation. It
// intentionally accepts the compact metrics tuple Prometheus needs instead of a
// full event payload. The enqueue is non-blocking; if the bounded ring is full,
// the occurrence is dropped rather than stalling command execution.
func (c *CommandCollector) RecordCommand(command string, elapsedNs uint64, isError bool) {
	if !c.HasCommandMetricsSink() {
		return
	}

	record := commandMetricRecord{command: command, elapsedNs: elapsedNs, isError: isError}
	select {
	case c.pending <- record:
	default:
		c.drops.Add(1)
	}
}

// DroppedRecords reports command metric occurrences dropped because the sidecar
// ring was full.
func (c *CommandCollector) DroppedRecords() uint64 {
	if c == nil {
		return 0
	}
	return c.drops.Load()
}

// Buckets returns a copy of the histogram bucket boundaries.
func (c *CommandCollector) Buckets() []float64 {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]float64(nil), c.buckets...)
}

// Snapshot returns a deterministic copy of the accumulated command metrics.
func (c *CommandCollector) Snapshot() []CommandSnapshot {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.drainPendingLocked()

	commands := make([]string, 0, len(c.stats))
	for command := range c.stats {
		commands = append(commands, command)
	}
	sort.Strings(commands)

	snapshots := make([]CommandSnapshot, len(commands))
	for i, command := range commands {
		s := c.stats[command]
		counts := make([]uint64, len(s.counts))
		copy(counts, s.counts)
		snapshots[i] = CommandSnapshot{
			Command: command,
			Total:   s.total,
			Errors:  s.errors,
			SumNs:   s.sumNs,
			Counts:  counts,
		}
	}
	return snapshots
}

func (c *CommandCollector) drainPendingLocked() {
	for {
		select {
		case record := <-c.pending:
			c.aggregateLocked(record)
		default:
			return
		}
	}
}

func (c *CommandCollector) aggregateLocked(record commandMetricRecord) {
	s := c.stats[record.command]
	if s == nil {
		s = &commandStats{counts: make([]uint64, len(c.buckets)+1)}
		c.stats[record.command] = s
	}

	s.total++
	s.sumNs += record.elapsedNs
	if record.isError {
		s.errors++
	}

	durationSec := float64(record.elapsedNs) / nsPerSec
	for i, bound := range c.buckets {
		if durationSec <= bound {
			s.counts[i]++
			return
		}
	}
	s.counts[len(c.buckets)]++
}
