//go:build pprof

package main

import (
	"encoding/json"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* handlers
	"os"
	"runtime"

	"gocache/commons/logger"
	"gocache/pkg/benchstats"
)

// Build-tag-gated pprof endpoint. Compiled in only when the `pprof` build
// tag is passed (e.g. via `go build -tags=pprof` or the bench Dockerfile
// when `--build-arg PPROF=1` is set).
//
// The endpoint binds to GOCACHE_PPROF_ADDR (default "0.0.0.0:6060") on a
// goroutine that runs alongside the main server. Used to attribute the
// docker-bench-vs-Go-bench gap from issue #45 — capture profiles during a
// running valkey-benchmark suite without changing the workload shape.
//
// Block + mutex profiling are also enabled here. Without these runtime
// calls the /debug/pprof/block and /debug/pprof/mutex endpoints return
// empty samples. The rates match the values the pkg/server bench profile
// runs (#38) used so docker captures are directly comparable.
func init() {
	addr := os.Getenv("GOCACHE_PPROF_ADDR")
	if addr == "" {
		addr = "0.0.0.0:6060"
	}
	// Match Go-bench profile rates (see #38's bench/profiles/run-profiles.sh).
	runtime.SetBlockProfileRate(10000)  // sample one in every 10000 ns of blocking
	runtime.SetMutexProfileFraction(10) // 1-in-10 mutex contention events

	// Register benchstats snapshot endpoint alongside pprof.
	http.HandleFunc("/debug/benchstats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		reset := r.URL.Query().Get("reset") == "true"
		data := benchstats.Snapshot(reset)
		_ = json.NewEncoder(w).Encode(data)
	})

	go func() {
		logger.InfoNoCtx().Str("addr", addr).Msg("pprof endpoint starting")
		if err := http.ListenAndServe(addr, nil); err != nil {
			logger.WarnNoCtx().Err(err).Msg("pprof endpoint exited")
		}
	}()
}
