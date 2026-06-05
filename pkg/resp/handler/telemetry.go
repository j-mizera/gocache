package handler

import (
	"time"

	apiobs "gocache/api/observability"
	commonobs "gocache/commons/observability"
)

func submitHandlerErrorLog(scope commonobs.OperationScope, message, key string, err error) bool {
	if scope.IsZero() {
		return false
	}
	record := apiobs.NewLogRecordString(scope.Operation(), apiobs.TelemetryLogLevelError, message)
	record.TimestampUnixNano = time.Now().UnixNano()
	if key != "" {
		record.AddFieldString("key", key)
	}
	if err != nil {
		record.AddFieldString("error", err.Error())
	}
	return scope.Record(record)
}
