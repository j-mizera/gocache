// Package crashdump is an embedded plugin that surfaces crash dumps from
// prior process runs so they show up in logs/observability pipelines
// without requiring a human to notice files on disk.
//
// The actual dump-writing happens in cmd/server/main.go's top-level
// deferred recover, which calls pkg/crashdump.WriteFromPanic directly.
// This plugin handles only the scan-and-report half of the lifecycle:
// on ConfigLoaded it lists dumps in the configured directory, logs each
// at WARN (so log collector + gobservability pick them up), and deletes
// successfully reported entries.
package crashdump

import (
	"context"
	"os"
	"strconv"

	"gocache/api/logger"
	"gocache/pkg/config"
	"gocache/pkg/crashdump"
	"gocache/pkg/embedded"
)

// Env var overrides. Evaluated in BootInit so they take effect even when
// config.Load hasn't run yet.
const (
	envDir      = "GOCACHE_CRASHDUMP_DIR"
	envDisabled = "GOCACHE_CRASHDUMP_DISABLED"
	defaultDir  = "crashes"
)

type plugin struct {
	dir      string
	disabled bool
}

func init() {
	embedded.Register(&plugin{dir: defaultDir})
}

func (p *plugin) Name() string { return "crashdump" }

func (p *plugin) BootInit(_ context.Context) error {
	if v := os.Getenv(envDir); v != "" {
		p.dir = v
	}
	if v := os.Getenv(envDisabled); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			p.disabled = parsed
		}
	}
	return nil
}

// ConfigLoaded is where scan-and-report runs. We wait for the proper
// logger to be initialized (with the configured level + writer) so the
// WARN lines flow through the log collector into the event bus and then
// out to observability plugins. Scanning in BootInit would predate that
// wiring and the entries would only hit stderr.
func (p *plugin) ConfigLoaded(ctx context.Context, _ *config.Config) error {
	if p.disabled {
		return nil
	}
	results, err := crashdump.Scan(p.dir)
	if err != nil {
		// Not fatal — a missing or unreadable dir just means no prior crashes
		// to report. Log at debug level, don't spam.
		logger.Debug(ctx).Err(err).Str("dir", p.dir).Msg("crashdump scan failed")
		return nil
	}
	for _, r := range results {
		p.report(ctx, r)
		if err := crashdump.Delete(r.Path); err != nil {
			logger.Warn(ctx).Str("path", r.Path).Err(err).Msg("failed to delete processed crash dump")
		}
	}
	if len(results) > 0 {
		logger.Info(ctx).Int("count", len(results)).Str("dir", p.dir).Msg("processed prior crash dumps")
	}
	return nil
}

func (p *plugin) ProcessShutdown(_ context.Context) error { return nil }

// report emits a single WARN log line per dump with structured fields so
// observability pipelines can key on it. The stack trace can be large —
// truncate to keep the JSON reasonable, full content remains on disk
// until the Delete call above.
func (p *plugin) report(ctx context.Context, r crashdump.ScanResult) {
	d := r.Dump
	ev := logger.Warn(ctx).
		Str("event", "crash.recovered").
		Str("path", r.Path).
		Str("panic_value", d.PanicValue).
		Str("boot_stage", d.BootStage).
		Int("prior_pid", d.PID).
		Int("active_ops", len(d.ActiveOps)).
		Str("stack", truncateStack(d.Stack, 4096))
	if d.Version != "" {
		ev = ev.Str("prior_version", d.Version)
	}
	ev.Msg("prior process crash recovered")
}

// truncateStack caps long stack traces so a single log line does not blow
// past the log collector's line buffer. Full content stays on disk via
// crashdump.Scan's Path field until we Delete it.
func truncateStack(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}
