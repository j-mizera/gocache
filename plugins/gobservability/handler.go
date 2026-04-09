package main

import "net/http"

// metricsHandler returns an HTTP handler that serves Prometheus metrics.
func metricsHandler(collector *Collector, name, version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		collector.WritePrometheus(w, name, version)
	})
}
