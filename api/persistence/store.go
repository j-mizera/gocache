package persistence

import "context"

// CacheStore is the persistence coordinator's view of the cache.
// The server's *cache.Cache satisfies this interface; the coordinator
// never imports pkg/cache.
type CacheStore interface {
	// CaptureSnapshot returns every live entry as a SnapshotEntry slice.
	// Packed values are copied out of slab storage; sorted sets are
	// flattened to map[string]float64. The slice is safe to use after
	// the call returns — no aliases into cache-internal memory.
	CaptureSnapshot() []SnapshotEntry

	// LoadEntry applies one snapshot entry to the cache. Packed entries
	// go through the slab path; native sorted-set entries are
	// reconstructed from map[string]float64.
	LoadEntry(ctx context.Context, e SnapshotEntry) error

	// Clear drops every entry in the cache.
	Clear(ctx context.Context)
}
