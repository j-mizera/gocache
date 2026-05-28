// Package lifecycleotlp is an embedded plugin that exports lifecycle OTLP spans
// directly from the server binary, so process start, config load, fatal boot
// failures, and shutdown events can land in Grafana/Tempo/Jaeger before any IPC
// instrumentation plugin is reachable.
//
// Gated by the `lifecycleotlp` build tag. Without the tag this file is the only
// one that compiles — the package exists but registers nothing — so default
// `go build` produces a binary with no OTLP dependency baked in. Build with
// `-tags=lifecycleotlp` to include it.
package lifecycleotlp
