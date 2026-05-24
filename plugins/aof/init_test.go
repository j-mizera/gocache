//go:build aof

package aof

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiconfig "gocache/api/config"
	apipersistence "gocache/api/persistence"
)

var _ apiconfig.PluginConfig = (*mapConfig)(nil)

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
	apipersistence.ResetProvidersForTest()
	t.Cleanup(apipersistence.ResetProvidersForTest)

	p := &provider{}
	cfg := newMapConfig()
	cfg.values[keyFile] = filepath.Join(t.TempDir(), "test.aof")

	backend, err := p.Build(cfg, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if backend.Source == nil {
		t.Error("Build returned nil Source")
	}
	if backend.Sink == nil {
		t.Error("Build returned nil Sink")
	}
	if backend.Commands == nil {
		t.Error("Build returned nil Commands func")
	}
	if backend.OnReload == nil {
		t.Error("Build returned nil OnReload")
	}

	if p.Name() != "aof" {
		t.Errorf("Name() = %q, want aof", p.Name())
	}

	backend.Sink.Close(context.Background())
}

func TestProvider_Build_AppliesDefaults(t *testing.T) {
	apipersistence.ResetProvidersForTest()
	t.Cleanup(apipersistence.ResetProvidersForTest)

	cfg := newMapConfig()
	p := &provider{}
	backend, err := p.Build(cfg, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { backend.Sink.Close(context.Background()) })

	if got := cfg.GetString(keyFile); got != defaultFile {
		t.Errorf("default file not applied: got %q, want %q", got, defaultFile)
	}
	if got := cfg.GetString(keyFsync); got != defaultFsync {
		t.Errorf("default fsync not applied: got %q, want %q", got, defaultFsync)
	}
	os.Remove(defaultFile)
}

func TestProvider_Build_EmptyFile_Errors(t *testing.T) {
	cfg := newMapConfig()
	cfg.values[keyFile] = ""

	p := &provider{}
	_, err := p.Build(cfg, nil)
	if err == nil {
		t.Error("expected error when file is empty, got none")
	}
}

func TestProvider_OnConfigReload(t *testing.T) {
	dir := t.TempDir()
	cfg := newMapConfig()
	cfg.values[keyFile] = filepath.Join(dir, "test.aof")
	cfg.values[keyFsync] = "no"

	p := &provider{}
	backend, err := p.Build(cfg, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { backend.Sink.Close(context.Background()) })

	sink := p.sink
	if sink.FsyncPolicy() != apipersistence.FsyncNo {
		t.Fatalf("initial policy = %v, want FsyncNo", sink.FsyncPolicy())
	}

	reloadCfg := newMapConfig()
	reloadCfg.values[keyFsync] = "always"
	p.OnConfigReload(reloadCfg)

	if sink.FsyncPolicy() != apipersistence.FsyncAlways {
		t.Errorf("after reload policy = %v, want FsyncAlways", sink.FsyncPolicy())
	}
}

func TestReplaceFile(t *testing.T) {
	dir := t.TempDir()
	aofPath := filepath.Join(dir, "live.aof")

	sink, err := NewSink(aofPath, apipersistence.FsyncAlways)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}

	err = sink.Apply(context.Background(), []apipersistence.Mutation{
		{LSN: 1, Op: "SET", Key: "old", Args: [][]byte{[]byte("old"), []byte("val")}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	tmpPath := filepath.Join(dir, "replacement.aof")
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		t.Fatalf("create tmp: %v", err)
	}
	tmpBw := bufio.NewWriterSize(tmpFile, 4096)
	if err := writeHeader(tmpBw); err != nil {
		t.Fatalf("writeHeader: %v", err)
	}
	m := apipersistence.Mutation{LSN: 10, Op: "SET", Key: "replaced", Args: [][]byte{[]byte("replaced"), []byte("v")}}
	if _, err := encodeRecord(tmpBw, m, nil); err != nil {
		t.Fatalf("encodeRecord: %v", err)
	}
	tmpBw.Flush()
	tmpFile.Sync()
	tmpFile.Close()

	if err := sink.ReplaceFile(tmpPath); err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}

	err = sink.Apply(context.Background(), []apipersistence.Mutation{
		{LSN: 11, Op: "SET", Key: "after", Args: [][]byte{[]byte("after"), []byte("w")}},
	})
	if err != nil {
		t.Fatalf("Apply after replace: %v", err)
	}
	sink.Close(context.Background())

	src := NewSource(aofPath)
	boot, err := src.Boot(context.Background())
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if boot.Mode != apipersistence.BootModeReplay {
		t.Fatalf("mode = %v, want Replay", boot.Mode)
	}

	ctx := context.Background()
	got1, err := boot.Replay.Next(ctx)
	if err != nil {
		t.Fatalf("Next(0): %v", err)
	}
	if got1.LSN != 10 || got1.Key != "replaced" {
		t.Errorf("record 0: LSN=%d Key=%q, want LSN=10 Key=replaced", got1.LSN, got1.Key)
	}

	got2, err := boot.Replay.Next(ctx)
	if err != nil {
		t.Fatalf("Next(1): %v", err)
	}
	if got2.LSN != 11 || got2.Key != "after" {
		t.Errorf("record 1: LSN=%d Key=%q, want LSN=11 Key=after", got2.LSN, got2.Key)
	}

	_, err = boot.Replay.Next(ctx)
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
	boot.Replay.Close()
}

func TestRewrite_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	aofPath := filepath.Join(dir, "test.aof")

	sink, err := NewSink(aofPath, apipersistence.FsyncAlways)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}

	err = sink.Apply(context.Background(), []apipersistence.Mutation{
		{LSN: 1, Op: "SET", Key: "k1", Args: [][]byte{[]byte("k1"), []byte("old")}},
		{LSN: 2, Op: "SET", Key: "k1", Args: [][]byte{[]byte("k1"), []byte("new")}},
		{LSN: 3, Op: "HSET", Key: "h1", Args: [][]byte{[]byte("h1"), []byte("f1"), []byte("v1")}},
		{LSN: 4, Op: "SADD", Key: "s1", Args: [][]byte{[]byte("s1"), []byte("a"), []byte("b")}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	origInfo, _ := os.Stat(aofPath)
	origSize := origInfo.Size()

	store := &fakeStore{
		entries: []apipersistence.SnapshotEntry{
			{Key: "k1", Value: []byte("new"), ValueType: apipersistence.ValueTypeBytes},
			{Key: "h1", Value: map[string]string{"f1": "v1"}, ValueType: apipersistence.ValueTypeHash},
			{Key: "s1", Value: map[string]struct{}{"a": {}, "b": {}}, ValueType: apipersistence.ValueTypeSet},
		},
	}

	if err := Rewrite(context.Background(), store, sink, aofPath); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	sink.Close(context.Background())

	rewrittenInfo, _ := os.Stat(aofPath)
	if rewrittenInfo.Size() >= origSize {
		t.Errorf("rewritten file (%d) not smaller than original (%d)", rewrittenInfo.Size(), origSize)
	}

	src := NewSource(aofPath)
	boot, err := src.Boot(context.Background())
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if boot.Mode != apipersistence.BootModeReplay {
		t.Fatalf("mode = %v, want Replay", boot.Mode)
	}

	ctx := context.Background()
	ops := map[string]string{}
	for {
		m, err := boot.Replay.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		ops[m.Key] = m.Op
	}
	boot.Replay.Close()

	if len(ops) != 3 {
		t.Fatalf("expected 3 mutations, got %d: %v", len(ops), ops)
	}
	for key, wantOp := range map[string]string{"k1": "SET", "h1": "HSET", "s1": "SADD"} {
		if ops[key] != wantOp {
			t.Errorf("key %q: op = %q, want %q", key, ops[key], wantOp)
		}
	}
}

func TestIntegration_WriteBootReplayVerify(t *testing.T) {
	dir := t.TempDir()
	aofPath := filepath.Join(dir, "integration.aof")
	ctx := context.Background()

	mutations := []apipersistence.Mutation{
		{LSN: 1, Op: "SET", Key: "k1", Args: [][]byte{[]byte("k1"), []byte("hello")}},
		{LSN: 2, Op: "HSET", Key: "h1", Args: [][]byte{[]byte("h1"), []byte("f1"), []byte("v1"), []byte("f2"), []byte("v2")}},
		{LSN: 3, Op: "SADD", Key: "s1", Args: [][]byte{[]byte("s1"), []byte("a"), []byte("b")}},
		{LSN: 4, Op: "SET", Key: "k1", Args: [][]byte{[]byte("k1"), []byte("updated")}},
		{LSN: 5, Op: "DEL", Key: "k2", Args: [][]byte{[]byte("k2")}},
	}

	sink, err := NewSink(aofPath, apipersistence.FsyncAlways)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	if err := sink.Apply(ctx, mutations); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	sink.Close(ctx)

	src := NewSource(aofPath)
	boot, err := src.Boot(ctx)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if boot.Mode != apipersistence.BootModeReplay {
		t.Fatalf("mode = %v, want Replay", boot.Mode)
	}

	store := apipersistence.NewMemoryStore()
	count := 0
	for {
		m, err := boot.Replay.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if err := store.ApplyMutation(ctx, m); err != nil {
			t.Fatalf("ApplyMutation(%s): %v", m.Op, err)
		}
		count++
	}
	boot.Replay.Close()

	if count != len(mutations) {
		t.Errorf("replayed %d mutations, want %d", count, len(mutations))
	}

	got, ok := store.GetString("k1")
	if !ok {
		t.Fatal("k1 not found after replay")
	}
	if got != "updated" {
		t.Errorf("k1 = %q, want %q", got, "updated")
	}

	hv, ok := store.Get("h1")
	if !ok {
		t.Fatal("h1 not found after replay")
	}
	h := hv.(map[string]string)
	if h["f1"] != "v1" || h["f2"] != "v2" {
		t.Errorf("h1 = %v, want {f1:v1, f2:v2}", h)
	}

	sv, ok := store.Get("s1")
	if !ok {
		t.Fatal("s1 not found after replay")
	}
	s := sv.(map[string]struct{})
	if len(s) != 2 {
		t.Errorf("s1 len = %d, want 2", len(s))
	}

	if _, ok := store.Get("k2"); ok {
		t.Error("k2 should not exist (DEL'd a non-existent key)")
	}
}

func TestBGREWRITEAOF_ConcurrentRejection(t *testing.T) {
	apipersistence.ResetProvidersForTest()
	t.Cleanup(apipersistence.ResetProvidersForTest)

	dir := t.TempDir()
	cfg := newMapConfig()
	cfg.values[keyFile] = filepath.Join(dir, "test.aof")
	cfg.values[keyFsync] = "no"

	store := &fakeStore{
		entries: []apipersistence.SnapshotEntry{
			{Key: "k1", Value: []byte("v1"), ValueType: apipersistence.ValueTypeBytes},
		},
	}

	p := &provider{}
	backend, err := p.Build(cfg, store)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { backend.Sink.Close(context.Background()) })

	cmds := backend.Commands(nil)
	if len(cmds) == 0 {
		t.Fatal("no commands returned")
	}

	rewriteCmd := cmds[0]

	p.rewriting.Lock()

	result, err := rewriteCmd.Fn(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msg, ok := result.(string)
	if !ok {
		t.Fatalf("result type = %T, want string", result)
	}
	if msg != "Background append only file rewriting already in progress" {
		t.Errorf("got %q, want rejection message", msg)
	}

	p.rewriting.Unlock()
}

// --- test helpers ---

type mapConfig struct {
	values   map[string]any
	defaults map[string]any
}

func newMapConfig() *mapConfig {
	return &mapConfig{values: map[string]any{}, defaults: map[string]any{}}
}

func (m *mapConfig) lookup(key string) any {
	if v, ok := m.values[key]; ok {
		return v
	}
	if v, ok := m.defaults[key]; ok {
		return v
	}
	return nil
}

func (m *mapConfig) GetString(key string) string {
	if v, ok := m.lookup(key).(string); ok {
		return v
	}
	return ""
}
func (m *mapConfig) GetInt(string) int                     { return 0 }
func (m *mapConfig) GetInt64(string) int64                 { return 0 }
func (m *mapConfig) GetBool(string) bool                   { return false }
func (m *mapConfig) GetDuration(string) time.Duration      { return 0 }
func (m *mapConfig) GetStringSlice(string) []string        { return nil }
func (m *mapConfig) IsSet(key string) bool                 { _, ok := m.values[key]; return ok }
func (m *mapConfig) SetDefault(key string, value any)      { m.defaults[key] = value }

type fakeStore struct {
	entries []apipersistence.SnapshotEntry
}

func (f *fakeStore) CaptureSnapshot() []apipersistence.SnapshotEntry { return f.entries }
func (f *fakeStore) LoadEntry(context.Context, apipersistence.SnapshotEntry) error { return nil }
func (f *fakeStore) Clear(context.Context)                                         {}
func (f *fakeStore) ApplyMutation(context.Context, apipersistence.Mutation) error  { return nil }
