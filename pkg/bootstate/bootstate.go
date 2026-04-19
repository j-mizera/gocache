// Package bootstate writes an atomic boot-stage marker to disk.
//
// The marker is a single tiny file containing the current boot stage name
// and a timestamp. It is overwritten atomically (tmp + rename) at each
// phase transition in main(). On the next boot, reading the previous
// marker reveals where the prior process died — even when no crash dump
// was written (e.g. a SIGKILL or OOM).
//
// Semantics: this is a debuggability aid, not a durable state machine.
// Don't rely on it for correctness.
package bootstate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is the on-disk marker format. Kept intentionally small.
type State struct {
	Stage     string    `json:"stage"`
	Timestamp time.Time `json:"ts"`
	PID       int       `json:"pid"`
}

// StageRunning is the stable stage marker set once boot finishes and the
// listener is accepting connections. A previous-run marker that is NOT
// StageRunning at startup means the prior process crashed mid-boot.
const StageRunning = "running"

// Write atomically replaces the marker at path with a new State for the
// given stage. Creates the parent directory if it does not exist. The
// write is atomic via tmp + rename on the same filesystem.
func Write(path, stage string) error {
	if path == "" {
		return fmt.Errorf("bootstate: empty path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("bootstate: mkdir: %w", err)
	}
	s := State{Stage: stage, Timestamp: time.Now(), PID: os.Getpid()}
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("bootstate: marshal: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".boot.state.tmp.*")
	if err != nil {
		return fmt.Errorf("bootstate: create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("bootstate: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("bootstate: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("bootstate: rename: %w", err)
	}
	return nil
}

// Read returns the State at path. Returns (_, os.ErrNotExist) if the file
// does not exist — a clean signal for "no prior boot recorded."
func Read(path string) (State, error) {
	var s State
	data, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("bootstate: unmarshal: %w", err)
	}
	return s, nil
}

// Clear removes the marker file. Call on clean shutdown if you want the
// next run to see "no prior boot" rather than "prior boot finished clean."
// Most deployments prefer to leave the StageRunning marker in place so a
// later diff shows the transition.
func Clear(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
