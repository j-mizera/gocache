package cache

import (
	"time"

	apiobs "gocache/api/observability"
	commonobs "gocache/commons/observability"
)

const logMessageOutOfMemory = "write rejected, out of memory"

func logOutOfMemory(scope commonobs.OperationScope, key string, usedBytes int64) bool {
	return cacheLog(scope, apiobs.TelemetryLogLevelWarn, logMessageOutOfMemory, "key", key, usedBytes)
}

func cacheLog(scope commonobs.OperationScope, level apiobs.TelemetryLogLevel, message, fieldKey, fieldValue string, number int64) bool {
	if scope.IsZero() {
		return false
	}
	record := apiobs.NewLogRecordString(scope.Operation(), level, message)
	record.TimestampUnixNano = time.Now().UnixNano()
	record.Number = number
	if fieldKey != "" {
		record.AddFieldString(fieldKey, fieldValue)
	}
	return scope.Record(record)
}
