package manager

import (
	"testing"

	apiEvents "gocache/api/events"
)

func TestProjectEventForPluginCopiesCommandPayloads(t *testing.T) {
	args := []string{"key", "value"}
	metadata := map[string]string{
		"_server":            "visible-server",
		"shared.traceparent": "00-original",
		"prometheus.local":   "visible-own",
		"other.hidden":       "hidden",
	}
	source := apiEvents.NewCommandCompleted("SET", args, 99, "OK", "", metadata).Proto
	projected := projectEventForPlugin(source, "prometheus")

	args[0] = "mutated-key"
	metadata["_server"] = "mutated-server"
	metadata["shared.traceparent"] = "00-mutated"
	metadata["prometheus.local"] = "mutated-own"
	metadata["other.hidden"] = "mutated-hidden"
	metadata["shared.extra"] = "late"

	payload := projected.GetCommandPost()
	if payload == nil {
		t.Fatal("projected command.completed payload missing")
	}
	if got := payload.Args[0]; got != "key" {
		t.Fatalf("projected args[0] = %q, want immutable original", got)
	}
	if got := payload.Metadata["_server"]; got != "visible-server" {
		t.Fatalf("projected server metadata = %q, want immutable original", got)
	}
	if got := payload.Metadata["shared.traceparent"]; got != "00-original" {
		t.Fatalf("projected shared metadata = %q, want immutable original", got)
	}
	if got := payload.Metadata["prometheus.local"]; got != "visible-own" {
		t.Fatalf("projected own metadata = %q, want immutable original", got)
	}
	if _, ok := payload.Metadata["other.hidden"]; ok {
		t.Fatalf("projected metadata leaked hidden key: %+v", payload.Metadata)
	}
	if _, ok := payload.Metadata["shared.extra"]; ok {
		t.Fatalf("projected metadata includes producer mutation: %+v", payload.Metadata)
	}
}

func TestProjectEventForPluginCopiesCommandArgsWithoutMetadata(t *testing.T) {
	args := []string{"key", "value"}
	source := apiEvents.NewCommandStarted("SET", args, nil).Proto
	projected := projectEventForPlugin(source, "prometheus")

	args[0] = "mutated-key"

	payload := projected.GetCommandPre()
	if payload == nil {
		t.Fatal("projected command.started payload missing")
	}
	if got := payload.Args[0]; got != "key" {
		t.Fatalf("projected args[0] = %q, want immutable original", got)
	}
}
