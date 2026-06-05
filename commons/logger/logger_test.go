package logger

import (
	"bytes"
	"context"
	"strings"
	"testing"

	ops "gocache/api/operations"
)

func TestLoggerDoesNotCorrelateFromContextOperation(t *testing.T) {
	var output bytes.Buffer
	log := New(&output, "test", "debug")

	op := ops.New(ops.TypeCommand, "")
	op.Enrich("db", "0")
	ctx := ops.WithContext(context.Background(), op)

	log.Info(ctx).Str("key", "value").Msg("cache event")

	line := output.String()
	for _, want := range []string{`"level":"info"`, `"key":"value"`, `"message":"cache event"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q missing %s", line, want)
		}
	}
	for _, forbidden := range []string{`"_operation_id"`, `"_ctx"`} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("log line %q unexpectedly contains implicit operation field %s", line, forbidden)
		}
	}
}
