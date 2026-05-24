//go:build aof

package aof

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"gocache/api/logger"
	apipersistence "gocache/api/persistence"
)

// AOFSink appends mutation records to the AOF file.
type AOFSink struct {
	mu      sync.Mutex
	file    *os.File
	bw      *bufio.Writer
	scratch []byte

	fsyncMu   sync.Mutex
	policy    apipersistence.FsyncPolicy
	fsyncStop chan struct{}
	fsyncWg   sync.WaitGroup
}

// NewSink opens (or creates) the AOF file and returns a Sink. A new
// file gets the GOCAOF header; an existing file is validated and
// appended to.
func NewSink(path string, policy apipersistence.FsyncPolicy) (*AOFSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("aof: open %s: %w", path, err)
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("aof: stat %s: %w", path, err)
	}

	if info.Size() == 0 {
		if err := writeHeader(f); err != nil {
			f.Close()
			return nil, fmt.Errorf("aof: write header: %w", err)
		}
	} else {
		if _, err := f.Seek(0, 0); err != nil {
			f.Close()
			return nil, err
		}
		if err := readHeader(f); err != nil {
			f.Close()
			return nil, err
		}
		if _, err := f.Seek(0, 2); err != nil {
			f.Close()
			return nil, err
		}
	}

	s := &AOFSink{
		file:    f,
		bw:      bufio.NewWriterSize(f, 64*1024),
		scratch: make([]byte, 0, 4096),
		policy:  policy,
	}

	if policy == apipersistence.FsyncEverySec {
		s.fsyncStop = make(chan struct{})
		s.fsyncWg.Add(1)
		go s.fsyncLoop()
	}
	return s, nil
}

func (*AOFSink) Name() string                            { return "aof" }
func (s *AOFSink) FsyncPolicy() apipersistence.FsyncPolicy { return s.policy }

// Apply encodes and appends a batch of mutations.
func (s *AOFSink) Apply(_ context.Context, batch []apipersistence.Mutation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var err error
	for i := range batch {
		s.scratch, err = encodeRecord(s.bw, batch[i], s.scratch)
		if err != nil {
			return fmt.Errorf("aof: encode: %w", err)
		}
	}
	if err := s.bw.Flush(); err != nil {
		return fmt.Errorf("aof: flush: %w", err)
	}
	if s.policy == apipersistence.FsyncAlways {
		if err := s.file.Sync(); err != nil {
			return fmt.Errorf("aof: fsync: %w", err)
		}
	}
	return nil
}

// Close flushes, syncs, and closes the file.
func (s *AOFSink) Close(_ context.Context) error {
	s.fsyncMu.Lock()
	if s.fsyncStop != nil {
		close(s.fsyncStop)
		s.fsyncWg.Wait()
		s.fsyncStop = nil
	}
	s.fsyncMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.bw.Flush(); err != nil {
		s.file.Close()
		return fmt.Errorf("aof: final flush: %w", err)
	}
	if err := s.file.Sync(); err != nil {
		s.file.Close()
		return fmt.Errorf("aof: final sync: %w", err)
	}
	return s.file.Close()
}

// SetFsyncPolicy swaps the fsync policy at runtime (hot reload).
func (s *AOFSink) SetFsyncPolicy(p apipersistence.FsyncPolicy) {
	s.fsyncMu.Lock()
	defer s.fsyncMu.Unlock()

	s.mu.Lock()
	old := s.policy
	s.policy = p
	s.mu.Unlock()

	if old == apipersistence.FsyncEverySec && p != apipersistence.FsyncEverySec {
		if s.fsyncStop != nil {
			close(s.fsyncStop)
			s.fsyncWg.Wait()
			s.fsyncStop = nil
		}
	}
	if old != apipersistence.FsyncEverySec && p == apipersistence.FsyncEverySec {
		s.fsyncStop = make(chan struct{})
		s.fsyncWg.Add(1)
		go s.fsyncLoop()
	}
}

// FilePath returns the current AOF file path under the mutex.
func (s *AOFSink) FilePath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Name()
}

// ReplaceFile atomically swaps the underlying file (used by rewrite).
// Caller must hold no other lock on s.
func (s *AOFSink) ReplaceFile(newPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.bw.Flush(); err != nil {
		return fmt.Errorf("aof: flush before replace: %w", err)
	}
	oldPath := s.file.Name()
	s.file.Close()

	if err := os.Rename(newPath, oldPath); err != nil {
		return fmt.Errorf("aof: rename %s -> %s: %w", newPath, oldPath, err)
	}

	f, err := os.OpenFile(oldPath, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("aof: reopen %s: %w", oldPath, err)
	}
	s.file = f
	s.bw.Reset(f)
	return nil
}

func (s *AOFSink) fsyncLoop() {
	defer s.fsyncWg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.fsyncStop:
			return
		case <-ticker.C:
			s.mu.Lock()
			if err := s.file.Sync(); err != nil {
				logger.WarnNoCtx().Err(err).Msg("aof: everysec fsync failed")
			}
			s.mu.Unlock()
		}
	}
}
