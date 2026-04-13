package main

import (
	"encoding/json"
	"net/http"
)

// metricsHandler returns an HTTP handler that serves Prometheus metrics.
func metricsHandler(collector *Collector, name, version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		collector.WritePrometheus(w, name, version)
	})
}

// healthzHandler returns an HTTP handler for liveness checks.
// Queries the server's "health" topic via GCPC.
func healthzHandler(p *gobservabilityPlugin) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if p.session == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "initializing"})
			return
		}

		data, err := p.session.QueryServer(r.Context(), "health")
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "unavailable",
				"error":  err.Error(),
				"hint":   "ensure the gobservability plugin has the 'server:query:health' scope in the server config",
			})
			return
		}

		if data["status"] != "ok" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(data)
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(data)
	})
}

// readyzHandler returns an HTTP handler for readiness checks.
// Queries both "health" and "plugins" topics to determine overall readiness.
func readyzHandler(p *gobservabilityPlugin) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if p.session == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "initializing"})
			return
		}

		healthData, err := p.session.QueryServer(r.Context(), "health")
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "unavailable",
				"error":  err.Error(),
				"hint":   "ensure the gobservability plugin has the 'server:query:health' scope",
			})
			return
		}

		pluginData, err := p.session.QueryServer(r.Context(), "plugins")
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "unavailable",
				"error":  err.Error(),
				"hint":   "ensure the gobservability plugin has the 'server:query:plugins' scope",
			})
			return
		}

		// Check if server is healthy.
		ready := healthData["status"] == "ok"

		// Check if any critical plugin is unhealthy.
		if ready {
			ready = checkCriticalPlugins(pluginData)
		}

		result := map[string]interface{}{
			"status":  "ready",
			"server":  healthData,
			"plugins": pluginData,
		}

		if !ready {
			result["status"] = "not_ready"
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}

		_ = json.NewEncoder(w).Encode(result)
	})
}

// checkCriticalPlugins returns false if any critical plugin is not running.
func checkCriticalPlugins(pluginData map[string]string) bool {
	// Collect plugin names from the data keys.
	plugins := make(map[string]struct{})
	for k := range pluginData {
		// Keys are "name.state" or "name.critical".
		for i := len(k) - 1; i >= 0; i-- {
			if k[i] == '.' {
				plugins[k[:i]] = struct{}{}
				break
			}
		}
	}

	for name := range plugins {
		if pluginData[name+".critical"] == "true" && pluginData[name+".state"] != "running" {
			return false
		}
	}
	return true
}
