package cache

import (
	"context"
	"fmt"

	apipersistence "gocache/api/persistence"
)

var _ apipersistence.CacheStore = (*Cache)(nil)

// CaptureSnapshot walks all entries and returns a serializable snapshot.
// Caller must hold c.Lock() or ensure exclusive access (e.g. engine.Dispatch).
func (c *Cache) CaptureSnapshot() []apipersistence.SnapshotEntry {
	var entries []apipersistence.SnapshotEntry
	c.Range(func(key string, entry Entry, expiration int64) bool {
		var v any
		switch {
		case entry.Encoding == EncPacked:
			src := c.ResolvePacked(entry)
			buf := make([]byte, len(src))
			copy(buf, src)
			v = buf
		case entry.ValueType == ObjTypeSortedSet:
			if z, ok := entry.Value.(*SortedSet); ok {
				v = z.Members()
			} else {
				v = entry.Value
			}
		default:
			v = entry.Value
		}
		entries = append(entries, apipersistence.SnapshotEntry{
			Key:        key,
			ValueType:  apipersistence.ValueType(entry.ValueType),
			Encoding:   apipersistence.Encoding(entry.Encoding),
			Value:      v,
			Expiration: expiration,
		})
		return true
	})
	return entries
}

func (c *Cache) LoadEntry(_ context.Context, e apipersistence.SnapshotEntry) error {
	if e.Encoding == apipersistence.EncodingPacked {
		var buf []byte
		switch v := e.Value.(type) {
		case []byte:
			buf = v
		case string:
			buf = []byte(v)
		default:
			return fmt.Errorf("packed entry has non-byte payload: %T", e.Value)
		}
		c.RawLoadPacked(e.Key, ValueType(e.ValueType), buf, e.Expiration)
		return nil
	}
	value := e.Value
	if e.ValueType == apipersistence.ValueTypeSortedSet {
		members, ok := value.(map[string]float64)
		if !ok {
			return fmt.Errorf("sorted-set entry has non-map payload: %T", e.Value)
		}
		z := NewSortedSet()
		for member, score := range members {
			z.Add(member, score)
		}
		value = z
	}
	c.RawLoad(e.Key, value, e.Expiration)
	return nil
}
