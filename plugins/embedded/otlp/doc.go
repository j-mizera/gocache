// Package otlp is an embedded plugin that exports boot-time OTLP spans
// directly from the server binary, so process_start + config_load +
// shutdown events land in Grafana/Tempo/Jaeger without requiring the
// IPC-plugin gobservability to be running.
//
// Gated by the `otlp_embedded` build tag. Without the tag this file
// is the only one that compiles — the package exists but registers
// nothing — so default `go build` produces a binary with no OTLP
// dependency baked in. Build with `-tags=otlp_embedded` to include it.
package otlp
