// Package crashdump writes a structured snapshot to disk when the server
// panics, and scans accumulated dumps on subsequent boots so an observability
// plugin can export them as historical events.
//
// Why it exists: in-memory replay buffers (see pkg/events ring design) are
// lost with the process. A pre-config panic never reaches the observability
// plugin because the plugin was never there. A filesystem dump is the only
// process-independent artifact that survives — supervisors, containers, and
// Kubernetes' `emptyDir` volumes all preserve it long enough for the next
// boot to pick it up.
//
// Format: one JSON object per file, filename `crash-<pid>-<unix-ns>.json`
// under the configured directory. Single os.WriteFile call per dump — no
// buffered writer, no mkdir after the directory is established, so the
// write path is as short as possible for crash conditions.
package crashdump

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	ops "gocache/api/operations"
)

// Default directory name appended to the configured base dir.
const defaultSubdir = "crashes"

// Dump is the on-disk snapshot. Fields are intentionally flat + primitive
// so the JSON survives a partial write and is still half-readable.
type Dump struct {
	Timestamp  time.Time         `json:"timestamp"`
	PID        int               `json:"pid"`
	Version    string            `json:"version,omitempty"`
	BootStage  string            `json:"boot_stage,omitempty"`
	PanicValue string            `json:"panic_value"`
	Stack      string            `json:"stack"`
	ActiveOps  []OpSnapshot      `json:"active_ops,omitempty"`
	Meta       map[string]string `json:"meta,omitempty"`
}

// OpSnapshot is a minimal projection of an Operation captured at crash time.
// Only fields that survive JSON round-trip and are useful for correlation.
type OpSnapshot struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	ParentID string            `json:"parent_id,omitempty"`
	Started  time.Time         `json:"started"`
	Context  map[string]string `json:"context,omitempty"`
}

// Options tune a crash dump write.
type Options struct {
	// Dir is the output directory. If empty, "crashes" is used relative to
	// the current working directory. Created on demand (mkdir -p).
	Dir string
	// Version is the server version string included in the dump (optional).
	Version string
	// BootStage is the current bootstate marker (optional but strongly
	// recommended — makes triage 10× faster).
	BootStage string
	// ActiveOps is a snapshot of in-flight operations. Pass tracker.Active()
	// at call time.
	ActiveOps []*ops.Operation
	// Meta carries arbitrary key-value pairs (config path, hostname, etc.).
	Meta map[string]string
}

// Write captures a crash dump. panicVal is whatever recover() returned;
// stack is typically debug.Stack(). Returns the path to the written file
// so callers can log it.
//
// Fatal-path rules: no locking, no allocations inside loops when avoidable,
// a single os.WriteFile. If the write fails, the error is returned so the
// caller can decide what to do (usually: log to stderr and re-panic anyway).
func Write(panicVal any, stack []byte, o Options) (string, error) {
	dir := o.Dir
	if dir == "" {
		dir = defaultSubdir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("crashdump: mkdir: %w", err)
	}

	now := time.Now()
	pid := os.Getpid()
	d := Dump{
		Timestamp:  now,
		PID:        pid,
		Version:    o.Version,
		BootStage:  o.BootStage,
		PanicValue: fmt.Sprintf("%v", panicVal),
		Stack:      string(stack),
		Meta:       o.Meta,
	}
	if len(o.ActiveOps) > 0 {
		d.ActiveOps = make([]OpSnapshot, 0, len(o.ActiveOps))
		for _, op := range o.ActiveOps {
			if op == nil {
				continue
			}
			d.ActiveOps = append(d.ActiveOps, OpSnapshot{
				ID:       op.ID,
				Type:     string(op.Type),
				ParentID: op.ParentID,
				Started:  op.StartTime,
				Context:  op.ContextSnapshot(true),
			})
		}
	}

	data, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("crashdump: marshal: %w", err)
	}

	name := fmt.Sprintf("crash-%d-%d.json", pid, now.UnixNano())
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("crashdump: write: %w", err)
	}
	return path, nil
}

// WriteFromPanic is a convenience wrapper suitable for a top-level
// defer / recover chain in main(). Captures debug.Stack() for you.
// Call recovered := recover(); if recovered != nil { crashdump.WriteFromPanic(recovered, o) }.
func WriteFromPanic(panicVal any, o Options) (string, error) {
	return Write(panicVal, debug.Stack(), o)
}

// Scan returns every crash dump in dir, oldest first. Callers typically
// iterate, export to their sink, then call Delete. On empty or missing
// directory, returns an empty slice with no error.
func Scan(dir string) ([]ScanResult, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("crashdump: readdir: %w", err)
	}
	results := make([]ScanResult, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "crash-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue // skip unreadable
		}
		var d Dump
		if err := json.Unmarshal(data, &d); err != nil {
			continue // skip malformed
		}
		results = append(results, ScanResult{Path: path, Dump: d})
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Dump.Timestamp.Before(results[j].Dump.Timestamp)
	})
	return results, nil
}

// ScanResult pairs a dump with its source path so callers can Delete after
// processing.
type ScanResult struct {
	Path string
	Dump Dump
}

// Delete removes a scanned dump by path. Idempotent on missing files.
func Delete(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("crashdump: remove: %w", err)
	}
	return nil
}
