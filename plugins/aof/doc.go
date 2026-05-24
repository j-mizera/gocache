// Package aof implements an append-only file persistence backend for
// gocache (ADR-0016). It provides a Source for boot-time mutation
// replay and a Sink for runtime mutation logging.
//
// The plugin is gated behind the "aof" build tag. Blank-import this
// package in cmd/server/main.go to wire the AOF backend; the init()
// in init.go only runs when the tag is active.
package aof
