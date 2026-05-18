package persistence

import (
	"encoding/gob"
)

func init() {
	gob.Register(map[string]string{})
	gob.Register(map[string]struct{}{})
	gob.Register([]string{})
}

// SnapshotEntry is the gob-internal on-disk representation. ValueType and
// Encoding are int (not uint8) to preserve gob wire compatibility with
// snapshots written before the api/persistence types existed.
type SnapshotEntry struct {
	Key        string
	ValueType  int
	Encoding   int
	Value      any
	Expiration int64
}
