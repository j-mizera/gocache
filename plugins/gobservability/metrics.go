package main

import (
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
)

// nsPerSec converts nanoseconds to seconds for histogram observation.
const nsPerSec = 1e9

// Histogram bucket boundaries in seconds.
var defaultBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0}

type commandStats struct {
	total  uint64
	errors uint64
	sum    float64  // sum of durations in seconds
	counts []uint64 // per-bucket counts (len = len(buckets) + 1 for +Inf)
}

// Collector is a thread-safe metrics aggregator.
type Collector struct {
	mu      sync.Mutex
	stats   map[string]*commandStats
	buckets []float64
}

// NewCollector creates a metrics collector with default histogram buckets.
func NewCollector() *Collector {
	return &Collector{
		stats:   make(map[string]*commandStats),
		buckets: defaultBuckets,
	}
}

// Record registers a command execution with its duration and error status.
func (c *Collector) Record(command string, elapsedNs uint64, isError bool) {
	durationSec := float64(elapsedNs) / nsPerSec

	c.mu.Lock()
	defer c.mu.Unlock()

	s := c.stats[command]
	if s == nil {
		s = &commandStats{
			counts: make([]uint64, len(c.buckets)+1),
		}
		c.stats[command] = s
	}

	s.total++
	s.sum += durationSec
	if isError {
		s.errors++
	}

	// Place into histogram bucket.
	placed := false
	for i, bound := range c.buckets {
		if durationSec <= bound {
			s.counts[i]++
			placed = true
			break
		}
	}
	if !placed {
		s.counts[len(c.buckets)]++ // +Inf bucket
	}
}

// WritePrometheus writes all metrics in Prometheus text exposition format.
func (c *Collector) WritePrometheus(w io.Writer, pluginName, pluginVersion string) {
	c.mu.Lock()
	// Snapshot under lock.
	commands := make([]string, 0, len(c.stats))
	for cmd := range c.stats {
		commands = append(commands, cmd)
	}
	sort.Strings(commands)

	type snapshot struct {
		cmd    string
		total  uint64
		errors uint64
		sum    float64
		counts []uint64
	}
	snaps := make([]snapshot, len(commands))
	for i, cmd := range commands {
		s := c.stats[cmd]
		counts := make([]uint64, len(s.counts))
		copy(counts, s.counts)
		snaps[i] = snapshot{cmd: cmd, total: s.total, errors: s.errors, sum: s.sum, counts: counts}
	}
	c.mu.Unlock()

	// Commands total.
	fmt.Fprintln(w, "# HELP gocache_commands_total Total commands processed")
	fmt.Fprintln(w, "# TYPE gocache_commands_total counter")
	for _, s := range snaps {
		fmt.Fprintf(w, "gocache_commands_total{command=%q} %d\n", s.cmd, s.total)
	}

	// Command errors.
	fmt.Fprintln(w, "# HELP gocache_command_errors_total Total command errors")
	fmt.Fprintln(w, "# TYPE gocache_command_errors_total counter")
	for _, s := range snaps {
		if s.errors > 0 {
			fmt.Fprintf(w, "gocache_command_errors_total{command=%q} %d\n", s.cmd, s.errors)
		}
	}

	// Latency histogram.
	fmt.Fprintln(w, "# HELP gocache_command_duration_seconds Command latency histogram")
	fmt.Fprintln(w, "# TYPE gocache_command_duration_seconds histogram")
	for _, s := range snaps {
		cumulative := uint64(0)
		for i, bound := range c.buckets {
			cumulative += s.counts[i]
			fmt.Fprintf(w, "gocache_command_duration_seconds_bucket{command=%q,le=%q} %d\n", s.cmd, formatFloat(bound), cumulative)
		}
		cumulative += s.counts[len(c.buckets)]
		fmt.Fprintf(w, "gocache_command_duration_seconds_bucket{command=%q,le=\"+Inf\"} %d\n", s.cmd, cumulative)
		fmt.Fprintf(w, "gocache_command_duration_seconds_sum{command=%q} %s\n", s.cmd, formatFloat(s.sum))
		fmt.Fprintf(w, "gocache_command_duration_seconds_count{command=%q} %d\n", s.cmd, s.total)
	}

	// Plugin info.
	fmt.Fprintln(w, "# HELP gocache_plugin_info Plugin metadata")
	fmt.Fprintln(w, "# TYPE gocache_plugin_info gauge")
	fmt.Fprintf(w, "gocache_plugin_info{name=%q,version=%q} 1\n", pluginName, pluginVersion)
}

func formatFloat(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%.1f", f)
	}
	return fmt.Sprintf("%g", f)
}
