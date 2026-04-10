// Package rex provides REX (RESP EXtensions) metadata support for GoCache.
//
// REX metadata allows clients to attach per-command or connection-scoped
// key-value metadata that flows through the plugin hook chain. Plugins
// like auth, tenancy, and observability consume this metadata.
package rex

import (
	"fmt"
	"strings"
	"sync"
)

// Store holds connection-scoped REX metadata. It is safe for concurrent use.
// A Store is lazily initialized on the first REX.META SET/MSET command.
type Store struct {
	mu   sync.RWMutex
	data map[string]string
}

// NewStore creates an empty metadata store.
func NewStore() *Store {
	return &Store{data: make(map[string]string)}
}

// ValidateKey checks that a metadata key is valid.
// Keys starting with "_" are reserved for server-internal use.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("empty metadata key")
	}
	if strings.HasPrefix(key, "_") {
		return fmt.Errorf("metadata key %q: reserved prefix '_'", key)
	}
	if strings.HasPrefix(key, "shared.") {
		return fmt.Errorf("metadata key %q: reserved prefix 'shared.'", key)
	}
	return nil
}

// Set stores a metadata key-value pair. Returns an error if the key is invalid.
func (s *Store) Set(key, value string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	s.data[key] = value
	s.mu.Unlock()
	return nil
}

// Get returns the value for a key and whether it was found.
func (s *Store) Get(key string) (string, bool) {
	s.mu.RLock()
	v, ok := s.data[key]
	s.mu.RUnlock()
	return v, ok
}

// Del removes a key. Returns true if the key was present.
func (s *Store) Del(key string) bool {
	s.mu.Lock()
	_, ok := s.data[key]
	if ok {
		delete(s.data, key)
	}
	s.mu.Unlock()
	return ok
}

// All returns a snapshot copy of all metadata.
func (s *Store) All() map[string]string {
	s.mu.RLock()
	cp := make(map[string]string, len(s.data))
	for k, v := range s.data {
		cp[k] = v
	}
	s.mu.RUnlock()
	return cp
}

// Len returns the number of stored metadata entries.
func (s *Store) Len() int {
	s.mu.RLock()
	n := len(s.data)
	s.mu.RUnlock()
	return n
}
