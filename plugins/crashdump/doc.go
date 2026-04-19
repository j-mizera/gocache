// Package crashdump is an embedded plugin that surfaces crash dumps from
// prior process runs into the running log/event pipeline.
//
// Gated by the `crashdump` build tag. Without the tag this file is the
// only one that compiles — the package exists but registers nothing —
// so default `go build` produces a binary with no automatic crash dump
// reporting. Build with `-tags=crashdump` to include it.
//
// See the main package file for lifecycle and env var details.
package crashdump
