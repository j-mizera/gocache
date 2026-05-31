package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeSession struct {
	queries []string
	data    map[string]map[string]string
}

func (f *fakeSession) QueryServer(_ context.Context, topic string, params map[string]string) (map[string]string, error) {
	f.queries = append(f.queries, topic)
	if topic == topicBenchStats && params[queryParamReset] != "true" {
		return map[string]string{"error": "reset missing"}, nil
	}
	return f.data[topic], nil
}

func TestSnapshotIncludesBenchStatsAndPluginIPC(t *testing.T) {
	fs := &fakeSession{data: map[string]map[string]string{
		topicBenchStats: {"pipeline.evaluations": "42"},
		topicPluginIPC:  {"instrumentation.send_attempts": "7"},
	}}
	plugin := &benchprobePlugin{session: fs}

	resp, err := plugin.snapshot(context.Background(), true, true)
	if err != nil {
		t.Fatalf("snapshot error: %v", err)
	}
	if resp.BenchStats["pipeline.evaluations"] != "42" {
		t.Fatalf("pipeline.evaluations=%q, want 42", resp.BenchStats["pipeline.evaluations"])
	}
	if resp.PluginIPC["instrumentation.send_attempts"] != "7" {
		t.Fatalf("instrumentation.send_attempts=%q, want 7", resp.PluginIPC["instrumentation.send_attempts"])
	}
	if len(fs.queries) != 2 || fs.queries[0] != topicBenchStats || fs.queries[1] != topicPluginIPC {
		t.Fatalf("queries=%v, want bench.stats then plugin.ipc", fs.queries)
	}
}

func TestSnapshotHandlerCanSkipIPC(t *testing.T) {
	fs := &fakeSession{data: map[string]map[string]string{
		topicBenchStats: {"pipeline.evaluations": "1"},
		topicPluginIPC:  {"instrumentation.send_attempts": "7"},
	}}
	plugin := &benchprobePlugin{session: fs}
	req := httptest.NewRequest(http.MethodGet, "/snapshot?reset=true&include=bench", nil)
	w := httptest.NewRecorder()

	snapshotHandler(plugin).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}
	var body snapshotResponse
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.BenchStats["pipeline.evaluations"] != "1" {
		t.Fatalf("pipeline.evaluations=%q, want 1", body.BenchStats["pipeline.evaluations"])
	}
	if body.PluginIPC != nil {
		t.Fatalf("plugin_ipc=%v, want omitted", body.PluginIPC)
	}
	if len(fs.queries) != 1 || fs.queries[0] != topicBenchStats {
		t.Fatalf("queries=%v, want bench.stats only", fs.queries)
	}
}

func TestScopesIncludeBenchStatsAndPluginIPCQueries(t *testing.T) {
	plugin := &benchprobePlugin{}
	scopes := plugin.Scopes()
	want := map[string]bool{
		"server:query:health":      false,
		"server:query:bench.stats": false,
		"server:query:plugin.ipc":  false,
	}
	for _, scope := range scopes {
		if _, ok := want[scope]; ok {
			want[scope] = true
		}
	}
	for scope, found := range want {
		if !found {
			t.Fatalf("missing scope %q in %v", scope, scopes)
		}
	}
}
