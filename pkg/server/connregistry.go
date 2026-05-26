package server

import (
	"errors"
	"sync"

	"gocache/commons/resp"
)

var ErrConnNotFound = errors.New("connection not found")

type ConnHandle struct {
	mu     sync.Mutex
	writer *resp.Writer
	closed bool
}

func (h *ConnHandle) WriteValue(v resp.Value) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrConnNotFound
	}
	return h.writer.Write(v)
}

func (h *ConnHandle) WriteRaw(data []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrConnNotFound
	}
	return h.writer.WriteRaw(data)
}

func (h *ConnHandle) Flush() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrConnNotFound
	}
	return h.writer.Flush()
}

type ConnectionRegistry struct {
	mu    sync.RWMutex
	conns map[string]*ConnHandle
}

func NewConnectionRegistry() *ConnectionRegistry {
	return &ConnectionRegistry{
		conns: make(map[string]*ConnHandle),
	}
}

func (r *ConnectionRegistry) Register(id string, w *resp.Writer) *ConnHandle {
	h := &ConnHandle{writer: w}
	r.mu.Lock()
	r.conns[id] = h
	r.mu.Unlock()
	return h
}

func (r *ConnectionRegistry) Unregister(id string) {
	r.mu.Lock()
	if h, ok := r.conns[id]; ok {
		h.mu.Lock()
		h.closed = true
		h.mu.Unlock()
		delete(r.conns, id)
	}
	r.mu.Unlock()
}

func (r *ConnectionRegistry) Push(connectionID string, data []byte) error {
	r.mu.RLock()
	h, ok := r.conns[connectionID]
	r.mu.RUnlock()
	if !ok {
		return ErrConnNotFound
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrConnNotFound
	}
	if err := h.writer.WriteRaw(data); err != nil {
		return err
	}
	return h.writer.Flush()
}
