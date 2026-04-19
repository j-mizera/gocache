// Package rex provides REX (RESP EXtensions) metadata support for GoCache.
//
// REX metadata allows clients to attach per-command or connection-scoped
// key-value metadata that flows through the plugin hook chain. Plugins
// like auth, tenancy, and observability consume this metadata.
package rex

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Sentinel validation errors. ErrReservedPrefix wraps via %w so callers can
// still extract the offending key and prefix from the message.
var (
	ErrEmptyKey       = errors.New("metadata key is empty")
	ErrReservedPrefix = errors.New("metadata key uses reserved prefix")
)

// Reserved metadata key prefixes. User-supplied REX keys must not start with
// either of these — the server uses "_" for its own context (see api/command
// hook context keys) and "shared." for cross-plugin values (including the
// REX Prefix, see inject.go).
const (
	reservedInternalPrefix = "_"
	reservedSharedPrefix   = "shared."
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

// ValidateKey checks that a metadata key is valid. Keys starting with the
// reserved internal ("_") or shared ("shared.") prefixes are rejected.
// Returns ErrEmptyKey or a wrapped ErrReservedPrefix; both are errors.Is-matchable.
func ValidateKey(key string) error {
	if key == "" {
		return ErrEmptyKey
	}
	if strings.HasPrefix(key, reservedInternalPrefix) {
		return fmt.Errorf("%w: %q starts with %q", ErrReservedPrefix, key, reservedInternalPrefix)
	}
	if strings.HasPrefix(key, reservedSharedPrefix) {
		return fmt.Errorf("%w: %q starts with %q", ErrReservedPrefix, key, reservedSharedPrefix)
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
