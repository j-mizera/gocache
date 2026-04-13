package manager

import (
	"fmt"
	"strconv"
	"sync"
	"time"
)

// ServerStateProvider is the interface the plugin manager uses to query
// core server state. Defined here to avoid import cycles — the server
// package implements this interface, and wiring happens in cmd/server/main.go.
type ServerStateProvider interface {
	IsShuttingDown() bool
	StartTime() time.Time
	ActiveConnections() int
	CacheKeys() int
	CacheUsedBytes() int64
	CacheMaxBytes() int64
}

// QueryHandlerFunc handles a server query topic and returns key-value data.
type QueryHandlerFunc func() (map[string]string, error)

// QueryRegistry maps query topics to handler functions.
type QueryRegistry struct {
	mu       sync.RWMutex
	handlers map[string]QueryHandlerFunc
}

func NewQueryRegistry() *QueryRegistry {
	return &QueryRegistry{
		handlers: make(map[string]QueryHandlerFunc),
	}
}

// Register adds a handler for a query topic.
func (r *QueryRegistry) Register(topic string, handler QueryHandlerFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[topic] = handler
}

// Handle executes the handler for a topic and returns the result.
func (r *QueryRegistry) Handle(topic string) (map[string]string, error) {
	r.mu.RLock()
	h, ok := r.handlers[topic]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown query topic %q", topic)
	}
	return h()
}

// Topics returns the list of registered topic names.
func (r *QueryRegistry) Topics() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	topics := make([]string, 0, len(r.handlers))
	for t := range r.handlers {
		topics = append(topics, t)
	}
	return topics
}

// RegisterBuiltinHandlers registers the built-in query topics on the registry.
func RegisterBuiltinHandlers(qr *QueryRegistry, registry *Registry, sp ServerStateProvider) {
	if sp != nil {
		qr.Register("health", healthHandler(sp))
		qr.Register("stats", statsHandler(sp))
	}
	qr.Register("plugins", pluginsHandler(registry))
}

func healthHandler(sp ServerStateProvider) QueryHandlerFunc {
	return func() (map[string]string, error) {
		status := "ok"
		if sp.IsShuttingDown() {
			status = "shutting_down"
		}
		uptime := time.Since(sp.StartTime())
		return map[string]string{
			"status":      status,
			"uptime_ns":   strconv.FormatInt(uptime.Nanoseconds(), 10),
			"connections": strconv.Itoa(sp.ActiveConnections()),
		}, nil
	}
}

func statsHandler(sp ServerStateProvider) QueryHandlerFunc {
	return func() (map[string]string, error) {
		return map[string]string{
			"keys":             strconv.Itoa(sp.CacheKeys()),
			"memory_bytes":     strconv.FormatInt(sp.CacheUsedBytes(), 10),
			"max_memory_bytes": strconv.FormatInt(sp.CacheMaxBytes(), 10),
		}, nil
	}
}

func pluginsHandler(registry *Registry) QueryHandlerFunc {
	return func() (map[string]string, error) {
		plugins := registry.All()
		data := make(map[string]string, len(plugins)*2)
		for _, p := range plugins {
			data[p.Name+".state"] = p.State.String()
			data[p.Name+".critical"] = strconv.FormatBool(p.Critical)
		}
		return data, nil
	}
}
