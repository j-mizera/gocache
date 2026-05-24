//go:build aof

package aof

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	apipersistence "gocache/api/persistence"
)

func TestSinkAndSource_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.aof")

	sink, err := NewSink(path, apipersistence.FsyncAlways)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}

	mutations := []apipersistence.Mutation{
		{LSN: 1, Op: "SET", Key: "a", Args: [][]byte{[]byte("a"), []byte("1")}},
		{LSN: 2, Op: "HSET", Key: "h", Args: [][]byte{[]byte("h"), []byte("f"), []byte("v")}},
		{LSN: 3, Op: "DEL", Key: "a", Args: [][]byte{[]byte("a")}},
	}
	if err := sink.Apply(context.Background(), mutations); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	src := NewSource(path)
	boot, err := src.Boot(context.Background())
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if boot.Mode != apipersistence.BootModeReplay {
		t.Fatalf("mode = %v, want Replay", boot.Mode)
	}

	ctx := context.Background()
	for i, want := range mutations {
		got, err := boot.Replay.Next(ctx)
		if err != nil {
			t.Fatalf("Next(%d): %v", i, err)
		}
		if got.LSN != want.LSN || got.Op != want.Op {
			t.Errorf("[%d] got LSN=%d Op=%q, want LSN=%d Op=%q",
				i, got.LSN, got.Op, want.LSN, want.Op)
		}
	}
	_, err = boot.Replay.Next(ctx)
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
	boot.Replay.Close()
}

func TestSource_MissingFile(t *testing.T) {
	src := NewSource(filepath.Join(t.TempDir(), "nonexistent.aof"))
	boot, err := src.Boot(context.Background())
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if boot.Mode != apipersistence.BootModeInitial {
		t.Errorf("mode = %v, want Initial", boot.Mode)
	}
}

func TestSource_EmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.aof")
	os.WriteFile(path, nil, 0644)

	src := NewSource(path)
	boot, err := src.Boot(context.Background())
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if boot.Mode != apipersistence.BootModeInitial {
		t.Errorf("mode = %v, want Initial", boot.Mode)
	}
}

func TestSource_TornWrite_Truncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torn.aof")

	sink, _ := NewSink(path, apipersistence.FsyncAlways)
	mutations := []apipersistence.Mutation{
		{LSN: 1, Op: "SET", Key: "a", Args: [][]byte{[]byte("a"), []byte("1")}},
		{LSN: 2, Op: "SET", Key: "b", Args: [][]byte{[]byte("b"), []byte("2")}},
	}
	sink.Apply(context.Background(), mutations)
	sink.Close(context.Background())

	// Corrupt: truncate the last few bytes to simulate torn write
	data, _ := os.ReadFile(path)
	os.WriteFile(path, data[:len(data)-3], 0644)

	src := NewSource(path)
	boot, err := src.Boot(context.Background())
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if boot.Mode != apipersistence.BootModeReplay {
		t.Fatalf("mode = %v, want Replay", boot.Mode)
	}

	ctx := context.Background()
	got, err := boot.Replay.Next(ctx)
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if got.LSN != 1 {
		t.Errorf("first LSN = %d, want 1", got.LSN)
	}

	// Second record should be detected as torn and trigger EOF
	_, err = boot.Replay.Next(ctx)
	if err != io.EOF {
		t.Errorf("expected EOF on torn record, got %v", err)
	}
	boot.Replay.Close()

	// File should be truncated to only include the first good record
	info, _ := os.Stat(path)
	if info.Size() >= int64(len(data)) {
		t.Errorf("file not truncated: size=%d, original=%d", info.Size(), len(data))
	}
}

func TestProvider_Build(t *testing.T) {
	apipersistence.ResetAOFProviderForTest()
	t.Cleanup(apipersistence.ResetAOFProviderForTest)
	apipersistence.RegisterAOFProvider(&provider{})

	p := apipersistence.AOFProviderRegistered()
	if p == nil {
		t.Fatal("provider not registered")
	}
	if p.Name() != "aof" {
		t.Errorf("name = %q, want aof", p.Name())
	}
}
