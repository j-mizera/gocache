package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// HTTP response content types.
const (
	contentTypeJSON       = "application/json"
	contentTypePrometheus = "text/plain; version=0.0.4; charset=utf-8"
)

type serverQuerier interface {
	QueryServer(ctx context.Context, topic string, params map[string]string) (map[string]string, error)
}

// metricsHandler returns an HTTP handler that serves Prometheus metrics.
func metricsHandler(p *prometheusPlugin, name, version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypePrometheus)
		if p.collector == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "# prometheus collector unavailable")
			return
		}
		if p.session != nil {
			data, err := p.session.QueryServer(r.Context(), "metrics.commands", nil)
			if err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w, "# metrics.commands query failed: %s\n", err)
				return
			}
			if err := p.collector.ReplaceFromQuery(data); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w, "# metrics.commands snapshot invalid: %s\n", err)
				return
			}
		}
		p.collector.WritePrometheus(w, name, version)
	})
}

// telemetryHandler returns an HTTP handler for diagnostic telemetry snapshots.
func telemetryHandler(p *prometheusPlugin) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)

		if p.session == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "initializing",
				"hint":   "wait for the prometheus plugin to receive a GCPC session before reading metrics.telemetry",
			})
			return
		}

		telemetrySnapshot, err := p.session.QueryServer(r.Context(), "metrics.telemetry", nil)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "unavailable",
				"error":  err.Error(),
				"hint":   "ensure the prometheus plugin has the 'server:query:metrics.telemetry' scope in the server config",
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(telemetrySnapshot)
	})
}

// healthzHandler returns an HTTP handler for liveness checks.
// Queries the server's "health" topic via GCPC.
func healthzHandler(p *prometheusPlugin) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)

		if p.session == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "initializing"})
			return
		}

		data, err := p.session.QueryServer(r.Context(), "health", nil)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "unavailable",
				"error":  err.Error(),
				"hint":   "ensure the prometheus plugin has the 'server:query:health' scope in the server config",
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
func readyzHandler(p *prometheusPlugin) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)

		if p.session == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "initializing"})
			return
		}

		healthData, err := p.session.QueryServer(r.Context(), "health", nil)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "unavailable",
				"error":  err.Error(),
				"hint":   "ensure the prometheus plugin has the 'server:query:health' scope",
			})
			return
		}

		pluginData, err := p.session.QueryServer(r.Context(), "plugins", nil)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "unavailable",
				"error":  err.Error(),
				"hint":   "ensure the prometheus plugin has the 'server:query:plugins' scope",
			})
			return
		}

		// Check if server is healthy.
		ready := healthData["status"] == "ok"

		// Check if any critical plugin is unhealthy.
		if ready {
			ready = checkCriticalPlugins(pluginData)
		}

		result := map[string]any{
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
