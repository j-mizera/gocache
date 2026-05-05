package persistence

import (
	"encoding/gob"

	"gocache/pkg/cache"
)

func init() {
	gob.Register(map[string]string{})
	gob.Register(map[string]struct{}{})
	gob.Register([]string{})
}

type SnapshotEntry struct {
	Key        string
	ValueType  cache.ValueType
	Encoding   cache.Encoding
	Value      any
	Expiration int64
}
