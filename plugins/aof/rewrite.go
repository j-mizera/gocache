//go:build aof

package aof

import (
	"bufio"
	"context"
	"fmt"
	"os"

	"gocache/commons/logger"
	apipersistence "gocache/api/persistence"
)

// Rewrite compacts the AOF by capturing a snapshot and writing the
// minimal set of mutations needed to reconstruct the current state.
// The rewritten file replaces the current AOF via atomic rename.
func Rewrite(ctx context.Context, store apipersistence.CacheStore, sink *AOFSink, aofPath string) error {
	logger.Info(ctx).Msg("aof: rewrite started")

	entries := store.CaptureSnapshot()

	tmpPath := aofPath + ".rewrite.tmp"
	tmp, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("aof: rewrite create temp: %w", err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmpPath)
	}()

	bw := bufio.NewWriterSize(tmp, 64*1024)
	if err := writeHeader(bw); err != nil {
		return fmt.Errorf("aof: rewrite header: %w", err)
	}

	var lsn apipersistence.LSN
	scratch := make([]byte, 0, 4096)
	for _, e := range entries {
		lsn++
		m := synthesizeMutation(lsn, e)
		scratch, err = encodeRecord(bw, m, scratch)
		if err != nil {
			return fmt.Errorf("aof: rewrite encode: %w", err)
		}
	}

	if err := bw.Flush(); err != nil {
		return fmt.Errorf("aof: rewrite flush: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("aof: rewrite fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("aof: rewrite close: %w", err)
	}

	if err := sink.ReplaceFile(tmpPath); err != nil {
		return err
	}

	logger.Info(ctx).Int("entries", len(entries)).Msg("aof: rewrite complete")
	return nil
}

// synthesizeMutation converts a snapshot entry into the equivalent SET
// mutation that reproduces it on replay.
func synthesizeMutation(lsn apipersistence.LSN, e apipersistence.SnapshotEntry) apipersistence.Mutation {
	key := []byte(e.Key)

	switch e.ValueType {
	case apipersistence.ValueTypeHash:
		h, ok := e.Value.(map[string]string)
		if !ok {
			return delMutation(lsn, key)
		}
		args := make([][]byte, 0, 1+len(h)*2)
		args = append(args, key)
		for f, v := range h {
			args = append(args, []byte(f), []byte(v))
		}
		return apipersistence.Mutation{LSN: lsn, Op: "HSET", Key: e.Key, Args: args}

	case apipersistence.ValueTypeSet:
		s, ok := e.Value.(map[string]struct{})
		if !ok {
			return delMutation(lsn, key)
		}
		args := make([][]byte, 0, 1+len(s))
		args = append(args, key)
		for m := range s {
			args = append(args, []byte(m))
		}
		return apipersistence.Mutation{LSN: lsn, Op: "SADD", Key: e.Key, Args: args}

	case apipersistence.ValueTypeSortedSet:
		members, ok := e.Value.(map[string]float64)
		if !ok {
			return delMutation(lsn, key)
		}
		args := make([][]byte, 0, 1+len(members)*2)
		args = append(args, key)
		for m, score := range members {
			args = append(args, []byte(fmt.Sprintf("%g", score)), []byte(m))
		}
		return apipersistence.Mutation{LSN: lsn, Op: "ZADD", Key: e.Key, Args: args}

	case apipersistence.ValueTypeList:
		items, ok := e.Value.([]string)
		if !ok {
			return delMutation(lsn, key)
		}
		args := make([][]byte, 0, 1+len(items))
		args = append(args, key)
		for _, item := range items {
			args = append(args, []byte(item))
		}
		return apipersistence.Mutation{LSN: lsn, Op: "RPUSH", Key: e.Key, Args: args}

	default:
		var val []byte
		switch v := e.Value.(type) {
		case []byte:
			val = v
		case string:
			val = []byte(v)
		default:
			return delMutation(lsn, key)
		}
		return apipersistence.Mutation{LSN: lsn, Op: "SET", Key: e.Key, Args: [][]byte{key, val}}
	}
}

func delMutation(lsn apipersistence.LSN, key []byte) apipersistence.Mutation {
	return apipersistence.Mutation{LSN: lsn, Op: "DEL", Key: string(key), Args: [][]byte{key}}
}
