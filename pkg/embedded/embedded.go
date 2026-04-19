// Package embedded provides the registry for compile-time-linked plugins.
//
// An embedded plugin is a piece of functionality that is statically linked
// into the server binary via init()-side-effect imports in cmd/server/main.go.
// Unlike IPC plugins in pkg/plugin, embedded plugins:
//
//   - Run inside the server process (no Protobuf wire, no Unix socket)
//   - Are configured purely by environment variables (config.Load has not yet
//     run at BootInit time)
//   - Are compiled in at build time — pick the set with build tags
//
// The tradeoff vs IPC plugins: no process isolation (a panic inside an
// embedded plugin can kill the server if not recovered), no hot-swap, no
// scope/permission sandbox. In exchange, they run from instruction #1 of
// main() — the GraalVM/Java-agent equivalent for Go.
//
// Canonical use cases: crash dumps, OTLP exporter, early metrics —
// anything the server must be observable for even while its own config is
// still loading.
package embedded

import (
	"context"
	"fmt"

	"gocache/api/logger"
	"gocache/pkg/config"
)

// Plugin is the interface an embedded plugin implements.
//
// Lifecycle:
//
//  1. BootInit fires at the top of main(), before config.Load. Only env vars
//     are available. Return an error to signal a plugin-level failure —
//     the server keeps running unless the plugin was RegisterStrict'd.
//  2. ConfigLoaded fires after successful config parse, before goroutines
//     spawn. Plugins can upgrade from env-var-only config to YAML-backed
//     config here (e.g. swap OTLP endpoint).
//  3. ProcessShutdown fires from a top-level deferred call in main() —
//     runs on normal exit AND after a top-level panic. Last chance to
//     flush exporters.
//
// All three methods receive the process-wide context; cancellation
// semantics are the same as main()'s ctx.
type Plugin interface {
	Name() string
	BootInit(ctx context.Context) error
	ConfigLoaded(ctx context.Context, cfg *config.Config) error
	ProcessShutdown(ctx context.Context) error
}

// registration pairs a Plugin with its criticality.
type registration struct {
	plugin      Plugin
	mustSucceed bool
}

// Package-level registry, populated from init()-side-effect imports.
// Not thread-safe for writes after main() starts — all registrations
// happen during Go's init phase which is serial.
var registry []registration

// Register adds p to the embedded plugin registry with default
// (non-critical) semantics: BootInit/ConfigLoaded errors are logged and
// the server continues. Call from init() in the plugin's package.
func Register(p Plugin) {
	registry = append(registry, registration{plugin: p, mustSucceed: false})
}

// RegisterStrict adds p with halt-on-failure semantics: if BootInit or
// ConfigLoaded returns an error or panics, the process exits fatally.
// Use only when the plugin provides a capability the server cannot run
// without (rare).
func RegisterStrict(p Plugin) {
	registry = append(registry, registration{plugin: p, mustSucceed: true})
}

// Count returns the number of registered plugins. Useful for startup
// logging ("3 embedded plugins loaded").
func Count() int { return len(registry) }

// Names returns the names of all registered plugins in registration order.
func Names() []string {
	names := make([]string, len(registry))
	for i, r := range registry {
		names[i] = r.plugin.Name()
	}
	return names
}

// BootAll invokes BootInit on every registered plugin, in registration
// order. Errors and panics from non-strict plugins are logged and the
// iteration continues; strict plugins halt the process via Fatal.
func BootAll(ctx context.Context) {
	for _, r := range registry {
		invoke(ctx, r, "boot_init", func() error { return r.plugin.BootInit(ctx) })
	}
}

// ConfigLoadedAll invokes ConfigLoaded on every registered plugin after
// the initial config parse, in registration order. Same failure semantics
// as BootAll.
func ConfigLoadedAll(ctx context.Context, cfg *config.Config) {
	for _, r := range registry {
		invoke(ctx, r, "config_loaded", func() error { return r.plugin.ConfigLoaded(ctx, cfg) })
	}
}

// ShutdownAll invokes ProcessShutdown on every registered plugin in reverse
// registration order (LIFO — matches defer semantics, so a plugin relying
// on another's still-active state gets its chance to flush first).
// All errors are logged; the strict flag is ignored here (there is no
// meaningful way to "halt" during shutdown).
func ShutdownAll(ctx context.Context) {
	for i := len(registry) - 1; i >= 0; i-- {
		r := registry[i]
		invoke(ctx, r, "process_shutdown", func() error { return r.plugin.ProcessShutdown(ctx) })
	}
}

// invoke runs fn wrapped in a panic recover. On error from a strict plugin,
// it logs Fatal (which calls os.Exit). Non-strict errors/panics are logged
// at Error level and iteration continues.
func invoke(ctx context.Context, r registration, phase string, fn func() error) {
	defer func() {
		if rec := recover(); rec != nil {
			if r.mustSucceed {
				logger.Fatal(ctx).Str("embedded_plugin", r.plugin.Name()).Str("phase", phase).
					Interface("panic", rec).Msg("strict embedded plugin panicked")
			}
			logger.Error(ctx).Str("embedded_plugin", r.plugin.Name()).Str("phase", phase).
				Interface("panic", rec).Msg("embedded plugin panicked — continuing")
		}
	}()
	if err := fn(); err != nil {
		wrapped := fmt.Errorf("embedded plugin %q %s: %w", r.plugin.Name(), phase, err)
		if r.mustSucceed {
			logger.Fatal(ctx).Err(wrapped).Msg("strict embedded plugin failed")
		}
		logger.Error(ctx).Err(wrapped).Msg("embedded plugin failed — continuing")
	}
}

// ResetForTesting clears the registry. Test-only.
func ResetForTesting() { registry = nil }
