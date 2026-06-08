package manager

import (
	"reflect"
	"testing"

	apiEvents "gocache/api/events"
	gcpcv1 "gocache/api/gcpc/v1"
)

func TestProjectEventForPluginFiltersContextPayloads(t *testing.T) {
	ctx := map[string]string{
		"_command":               "SET",
		"shared.traceparent":     "00-abc-def-01",
		"test-plugin.visible":    "yes",
		"other-plugin.private":   "hidden",
		"other-plugin.secret.id": "hidden-secret",
	}
	want := map[string]string{
		"_command":            "SET",
		"shared.traceparent":  "00-abc-def-01",
		"test-plugin.visible": "yes",
	}

	tests := []struct {
		name      string
		event     *gcpcv1.EventV1
		projected func(*gcpcv1.EventV1) map[string]string
		original  func(*gcpcv1.EventV1) map[string]string
	}{
		{
			name:      "operation start",
			event:     apiEvents.NewOperationStarted("cmd_1", "command", "conn_1", ctx).Proto,
			projected: func(evt *gcpcv1.EventV1) map[string]string { return evt.GetOperationStart().Context },
			original:  func(evt *gcpcv1.EventV1) map[string]string { return evt.GetOperationStart().Context },
		},
		{
			name:      "operation complete",
			event:     apiEvents.NewOperationCompleted("cmd_1", "command", 42_000, "completed", "", ctx).Proto,
			projected: func(evt *gcpcv1.EventV1) map[string]string { return evt.GetOperationComplete().Context },
			original:  func(evt *gcpcv1.EventV1) map[string]string { return evt.GetOperationComplete().Context },
		},
		{
			name:      "command pre",
			event:     apiEvents.NewCommandStarted("SET", []string{"key", "value"}, ctx).Proto,
			projected: func(evt *gcpcv1.EventV1) map[string]string { return evt.GetCommandPre().Metadata },
			original:  func(evt *gcpcv1.EventV1) map[string]string { return evt.GetCommandPre().Metadata },
		},
		{
			name:      "command post",
			event:     apiEvents.NewCommandCompleted("SET", []string{"key", "value"}, 42_000, "OK", "", ctx).Proto,
			projected: func(evt *gcpcv1.EventV1) map[string]string { return evt.GetCommandPost().Metadata },
			original:  func(evt *gcpcv1.EventV1) map[string]string { return evt.GetCommandPost().Metadata },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projected := projectEventForPlugin(tt.event, "test-plugin")
			if projected == tt.event {
				t.Fatal("projectEventForPlugin returned the original event for context-bearing payload")
			}
			got := tt.projected(projected)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("projected context = %#v, want %#v", got, want)
			}
			got["_command"] = "mutated"
			if tt.original(tt.event)["_command"] != "SET" {
				t.Fatalf("projected context shared original map: %#v", tt.original(tt.event))
			}
			if _, ok := got["other-plugin.private"]; ok {
				t.Fatal("projected context leaked another plugin private key")
			}
			if _, ok := got["other-plugin.secret.id"]; ok {
				t.Fatal("projected context leaked another plugin secret key")
			}
		})
	}
}

func TestProjectEventForPluginFiltersRuntimeLogBatch(t *testing.T) {
	fields := map[string]string{
		"_command":               "SET",
		"shared.traceparent":     "00-abc-def-01",
		"test-plugin.visible":    "yes",
		"other-plugin.private":   "hidden",
		"other-plugin.secret.id": "hidden-secret",
	}
	evt := apiEvents.NewRuntimeLogBatch([]*gcpcv1.RuntimeLogRecordV1{
		nil,
		{
			Timestamp:   123,
			OperationId: "cmd_1",
			Level:       "info",
			Source:      "server",
			Message:     "runtime log",
			Caller:      "manager_projection_test.go",
			Fields:      fields,
		},
	}).Proto

	projected := projectEventForPlugin(evt, "test-plugin")
	if projected == evt {
		t.Fatal("projectEventForPlugin returned the original runtime log batch")
	}
	batch := projected.GetRuntimeLogBatch()
	if batch == nil || len(batch.Records) != 1 {
		t.Fatalf("projected runtime log batch = %#v, want one non-nil record", batch)
	}
	record := batch.Records[0]
	wantFields := map[string]string{
		"_command":            "SET",
		"shared.traceparent":  "00-abc-def-01",
		"test-plugin.visible": "yes",
	}
	if !reflect.DeepEqual(record.Fields, wantFields) {
		t.Fatalf("projected fields = %#v, want %#v", record.Fields, wantFields)
	}
	record.Fields["_command"] = "mutated"
	if fields["_command"] != "SET" {
		t.Fatalf("projected fields shared original map: %#v", fields)
	}
	if _, ok := record.Fields["other-plugin.private"]; ok {
		t.Fatal("projected fields leaked another plugin private key")
	}
	if _, ok := record.Fields["other-plugin.secret.id"]; ok {
		t.Fatal("projected fields leaked another plugin secret key")
	}
}
