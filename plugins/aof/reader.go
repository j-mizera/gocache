//go:build aof

package aof

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"

	"gocache/api/logger"
	apipersistence "gocache/api/persistence"
)

// AOFSource reads an AOF file at boot to produce a replay iterator.
type AOFSource struct {
	path string
}

// NewSource returns a Source backed by the given file path.
func NewSource(path string) *AOFSource {
	return &AOFSource{path: path}
}

func (*AOFSource) Name() string { return "aof" }

// SetPath updates the file path (hot reload).
func (s *AOFSource) SetPath(p string) { s.path = p }

// Boot opens the AOF file and returns a replay iterator. Missing or
// empty file → BootModeInitial (fresh start).
func (s *AOFSource) Boot(_ context.Context) (apipersistence.BootResult, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return apipersistence.BootResult{Mode: apipersistence.BootModeInitial}, nil
		}
		return apipersistence.BootResult{}, fmt.Errorf("aof: open %s: %w", s.path, err)
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return apipersistence.BootResult{}, fmt.Errorf("aof: stat %s: %w", s.path, err)
	}
	if info.Size() == 0 {
		f.Close()
		return apipersistence.BootResult{Mode: apipersistence.BootModeInitial}, nil
	}
	if info.Size() < headerSize {
		f.Close()
		return apipersistence.BootResult{}, fmt.Errorf("aof: file too small (%d bytes)", info.Size())
	}

	if err := readHeader(f); err != nil {
		f.Close()
		return apipersistence.BootResult{}, err
	}

	if info.Size() == headerSize {
		f.Close()
		return apipersistence.BootResult{Mode: apipersistence.BootModeInitial}, nil
	}

	return apipersistence.BootResult{
		Mode:   apipersistence.BootModeReplay,
		Replay: &aofIterator{file: f, br: bufio.NewReader(f), path: s.path, goodOffset: headerSize},
	}, nil
}

// aofIterator walks forward through AOF records. On a torn/malformed
// record it truncates the file at the last-good offset and returns
// io.EOF — recovering all intact mutations (ADR-0016).
type aofIterator struct {
	file       *os.File
	br         *bufio.Reader
	path       string
	goodOffset int64
}

func (it *aofIterator) Next(_ context.Context) (apipersistence.Mutation, error) {
	m, err := decodeRecord(it.br, it.br)
	if err == io.EOF {
		return apipersistence.Mutation{}, io.EOF
	}
	if err != nil {
		logger.WarnNoCtx().Str("file", it.path).Int64("offset", it.goodOffset).
			Err(err).Msg("aof: torn record detected, truncating")
		it.truncate()
		return apipersistence.Mutation{}, io.EOF
	}

	pos, err := it.file.Seek(0, 1)
	if err != nil {
		return apipersistence.Mutation{}, fmt.Errorf("aof: seek: %w", err)
	}
	it.goodOffset = pos - int64(it.br.Buffered())

	return m, nil
}

func (it *aofIterator) Close() error {
	return it.file.Close()
}

func (it *aofIterator) truncate() {
	if err := it.file.Truncate(it.goodOffset); err != nil {
		logger.WarnNoCtx().Err(err).Str("file", it.path).
			Msg("aof: truncation failed")
	}
}
