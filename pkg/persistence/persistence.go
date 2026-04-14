package persistence

import (
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gocache/api/logger"
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
	Value      any
	Expiration int64
}

// SaveSnapshot writes a snapshot atomically: it encodes into a sibling temp
// file first, fsyncs, then renames over the destination. A crash mid-write
// leaves the previous snapshot intact instead of corrupting it.
func SaveSnapshot(filename string, cacheInstance *cache.Cache) error {
	dir := filepath.Dir(filename)
	tmp, err := os.CreateTemp(dir, ".snapshot-*.tmp")
	if err != nil {
		logger.Error().Err(err).Str("file", filename).Msg("failed to create snapshot temp file")
		return fmt.Errorf("create snapshot temp %s: %w", filename, err)
	}
	tmpName := tmp.Name()
	// Clean up the temp file on any failure path; a successful rename makes
	// the cleanup a no-op because the name no longer exists.
	defer func() {
		_ = os.Remove(tmpName)
	}()

	// Collect all entries first so we can write the count header.
	var entries []SnapshotEntry
	cacheInstance.Range(func(key string, entry *cache.Entry, expiration int64) bool {
		entries = append(entries, SnapshotEntry{
			Key:        key,
			ValueType:  entry.ValueType,
			Value:      entry.Value,
			Expiration: expiration,
		})
		return true
	})

	encoder := gob.NewEncoder(tmp)
	// Write count header first so the decoder knows exactly how many entries follow.
	if err := encoder.Encode(len(entries)); err != nil {
		_ = tmp.Close()
		logger.Error().Err(err).Str("file", filename).Msg("snapshot encode error")
		return fmt.Errorf("encode snapshot %s: %w", filename, err)
	}
	for _, e := range entries {
		if err := encoder.Encode(e); err != nil {
			_ = tmp.Close()
			logger.Error().Err(err).Str("file", filename).Msg("snapshot encode error")
			return fmt.Errorf("encode snapshot %s: %w", filename, err)
		}
	}

	// Flush to stable storage before rename. Without Sync, a crash between
	// rename and kernel writeback could still leave a zero-length file at the
	// destination path.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync snapshot %s: %w", filename, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close snapshot %s: %w", filename, err)
	}
	if err := os.Rename(tmpName, filename); err != nil {
		return fmt.Errorf("rename snapshot %s: %w", filename, err)
	}

	logger.Info().Str("file", filename).Int("entries", len(entries)).Msg("snapshot saved")
	return nil
}

func LoadSnapshot(filename string, cacheInstance *cache.Cache) error {
	file, err := os.Open(filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logger.Debug().Str("file", filename).Msg("snapshot file not found, skipping")
			return nil
		}
		logger.Error().Err(err).Str("file", filename).Msg("failed to open snapshot file")
		return fmt.Errorf("open snapshot %s: %w", filename, err)
	}
	defer file.Close()

	decoder := gob.NewDecoder(file)
	cacheInstance.Clear()

	var count int
	if err := decoder.Decode(&count); err != nil {
		logger.Error().Err(err).Str("file", filename).Msg("snapshot decode error")
		return fmt.Errorf("decode snapshot %s: %w", filename, err)
	}

	loaded := 0
	for i := 0; i < count; i++ {
		var e SnapshotEntry
		if err := decoder.Decode(&e); err != nil {
			logger.Error().Err(err).Str("file", filename).Msg("snapshot decode error")
			return fmt.Errorf("decode snapshot %s: %w", filename, err)
		}

		if e.Expiration > 0 && e.Expiration < time.Now().UnixNano() {
			logger.Trace().Str("key", e.Key).Msg("skipped expired entry during load")
			continue
		}
		cacheInstance.RawLoad(e.Key, e.Value, e.Expiration)
		loaded++
	}

	logger.Info().Str("file", filename).Int("entries", loaded).Msg("snapshot loaded")
	return nil
}
