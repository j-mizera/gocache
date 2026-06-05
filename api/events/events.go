// Package events defines event types for the GoCache server event system.
//
// The server emits structured events at key points (like Linux kernel tracepoints).
// Plugins subscribe to event types via GCPC and receive fire-and-forget notifications.
// Events are informational — they cannot deny or modify operations (use hooks for that).
//
// This package lives in api/ and has zero dependencies on server internals.
package events

import (
	"time"

	gcpc "gocache/api/gcpc/v1"
)

// Type identifies an event category.
type Type string

const (
	CommandStarted   Type = "command.started"
	CommandCompleted Type = "command.completed"

	ConnectionOpen  Type = "connection.open"
	ConnectionClose Type = "connection.close"

	ServerStart    Type = "server.start"
	ServerShutdown Type = "server.shutdown"

	PluginRegistered Type = "plugin.registered"
	PluginCrashed    Type = "plugin.crashed"
	PluginRestarted  Type = "plugin.restarted"
	PluginStarted    Type = "plugin.started"
	PluginStopped    Type = "plugin.stopped"

	PluginRegistrationFailed        Type = "plugin.registration_failed"
	PluginCommandRegistered         Type = "plugin.command.registered"
	PluginCommandRegistrationFailed Type = "plugin.command.registration_failed"

	ConfigReloaded Type = "config.reloaded"

	AuthFailed Type = "auth.failed"

	CacheEviction Type = "cache.eviction"

	// RuntimeLogBatch carries diagnostic runtime logs through the normal event
	// subscription stream. Producers batch and flush it periodically; it is not
	// emitted from operation completion paths.
	RuntimeLogBatch Type = "runtime.logs"

	OperationStarted   Type = "operation.started"
	OperationCompleted Type = "operation.completed"

	// ReplayGap marks that the event bus's replay ring dropped events before
	// a subscriber connected. The payload is a dedicated ReplayGapEventV1 so
	// subscribers do not need to interpret diagnostic logs as control signals.
	ReplayGap Type = "replay.gap"
)

// Event is a structured notification emitted by the server.
// It wraps a gcpc.EventV1 with the type and timestamp already set.
type Event struct {
	Proto *gcpc.EventV1
}

// NewCommandStarted creates a command.started event.
func NewCommandStarted(command string, args []string, metadata map[string]string) Event {
	e := newEventProto(CommandStarted)
	e.Data = &gcpc.EventV1_CommandPre{CommandPre: &gcpc.CommandPreEventV1{
		Command: command, Args: args, Metadata: metadata,
	}}
	return Event{Proto: e}
}

// NewCommandCompleted creates a command.completed event.
func NewCommandCompleted(command string, args []string, elapsedNs uint64, result, errStr string, metadata map[string]string) Event {
	e := newEventProto(CommandCompleted)
	e.Data = &gcpc.EventV1_CommandPost{CommandPost: &gcpc.CommandPostEventV1{
		Command: command, Args: args, ElapsedNs: elapsedNs, Result: result, Error: errStr, Metadata: metadata,
	}}
	return Event{Proto: e}
}

// NewConnectionOpen creates a connection.open event.
func NewConnectionOpen(remoteAddr, connectionID string) Event {
	e := newEventProto(ConnectionOpen)
	e.Data = &gcpc.EventV1_ConnectionOpen{ConnectionOpen: &gcpc.ConnectionOpenEventV1{
		RemoteAddr: remoteAddr, ConnectionId: connectionID,
	}}
	return Event{Proto: e}
}

// NewConnectionClose creates a connection.close event.
func NewConnectionClose(remoteAddr string, durationNs uint64, connectionID string) Event {
	e := newEventProto(ConnectionClose)
	e.Data = &gcpc.EventV1_ConnectionClose{ConnectionClose: &gcpc.ConnectionCloseEventV1{
		RemoteAddr: remoteAddr, DurationNs: durationNs, ConnectionId: connectionID,
	}}
	return Event{Proto: e}
}

// NewServerStart creates a server.start event.
func NewServerStart(addr, version string) Event {
	e := newEventProto(ServerStart)
	e.Data = &gcpc.EventV1_ServerStart{ServerStart: &gcpc.ServerStartEventV1{
		Addr: addr, Version: version,
	}}
	return Event{Proto: e}
}

// NewServerShutdown creates a server.shutdown event.
func NewServerShutdown(reason string) Event {
	e := newEventProto(ServerShutdown)
	e.Data = &gcpc.EventV1_ServerShutdown{ServerShutdown: &gcpc.ServerShutdownEventV1{
		Reason: reason,
	}}
	return Event{Proto: e}
}

// NewPluginRegistered creates a plugin.registered event.
func NewPluginRegistered(name, version string, critical bool) Event {
	e := newEventProto(PluginRegistered)
	e.Data = &gcpc.EventV1_PluginRegistered{PluginRegistered: &gcpc.PluginRegisteredEventV1{
		Name: name, Version: version, Critical: critical,
	}}
	return Event{Proto: e}
}

// NewPluginCrashed creates a plugin.crashed event.
func NewPluginCrashed(name string, critical bool, errStr string) Event {
	e := newEventProto(PluginCrashed)
	e.Data = &gcpc.EventV1_PluginCrashed{PluginCrashed: &gcpc.PluginCrashedEventV1{
		Name: name, Critical: critical, Error: errStr,
	}}
	return Event{Proto: e}
}

// NewPluginRestarted creates a plugin.restarted event.
func NewPluginRestarted(name string, critical bool, restartCount int) Event {
	e := newEventProto(PluginRestarted)
	e.Data = &gcpc.EventV1_PluginRestarted{PluginRestarted: &gcpc.PluginRestartedEventV1{
		Name: name, Critical: critical, RestartCount: int32(restartCount),
	}}
	return Event{Proto: e}
}

// NewPluginStarted creates a plugin.started event.
func NewPluginStarted(name string, critical bool, pid int) Event {
	e := newEventProto(PluginStarted)
	e.Data = &gcpc.EventV1_PluginStarted{PluginStarted: &gcpc.PluginStartedEventV1{
		Name: name, Critical: critical, Pid: int32(pid),
	}}
	return Event{Proto: e}
}

// NewPluginStopped creates a plugin.stopped event.
func NewPluginStopped(name string, critical bool, reason string) Event {
	e := newEventProto(PluginStopped)
	e.Data = &gcpc.EventV1_PluginStopped{PluginStopped: &gcpc.PluginStoppedEventV1{
		Name: name, Critical: critical, Reason: reason,
	}}
	return Event{Proto: e}
}

// NewPluginRegistrationFailed creates a plugin.registration_failed event.
func NewPluginRegistrationFailed(name, version string, critical bool, errStr string) Event {
	e := newEventProto(PluginRegistrationFailed)
	e.Data = &gcpc.EventV1_PluginRegistrationFailed{PluginRegistrationFailed: &gcpc.PluginRegistrationFailedEventV1{
		Name: name, Version: version, Critical: critical, Error: errStr,
	}}
	return Event{Proto: e}
}

// NewPluginCommandRegistered creates a plugin.command.registered event.
func NewPluginCommandRegistered(name, command string, namespaced, readonly bool) Event {
	e := newEventProto(PluginCommandRegistered)
	e.Data = &gcpc.EventV1_PluginCommandRegistered{PluginCommandRegistered: &gcpc.PluginCommandRegisteredEventV1{
		Name: name, Command: command, Namespaced: namespaced, Readonly: readonly,
	}}
	return Event{Proto: e}
}

// NewPluginCommandRegistrationFailed creates a plugin.command.registration_failed event.
func NewPluginCommandRegistrationFailed(name, command, errStr string) Event {
	e := newEventProto(PluginCommandRegistrationFailed)
	e.Data = &gcpc.EventV1_PluginCommandRegistrationFailed{PluginCommandRegistrationFailed: &gcpc.PluginCommandRegistrationFailedEventV1{
		Name: name, Command: command, Error: errStr,
	}}
	return Event{Proto: e}
}

// NewConfigReloaded creates a config.reloaded event.
func NewConfigReloaded(file string) Event {
	e := newEventProto(ConfigReloaded)
	e.Data = &gcpc.EventV1_ConfigReloaded{ConfigReloaded: &gcpc.ConfigReloadedEventV1{
		File: file,
	}}
	return Event{Proto: e}
}

// NewAuthFailed creates an auth.failed event.
func NewAuthFailed(remoteAddr, command string) Event {
	e := newEventProto(AuthFailed)
	e.Data = &gcpc.EventV1_AuthFailed{AuthFailed: &gcpc.AuthFailedEventV1{
		RemoteAddr: remoteAddr, Command: command,
	}}
	return Event{Proto: e}
}

// NewCacheEviction creates a cache.eviction event.
func NewCacheEviction(key, reason string) Event {
	e := newEventProto(CacheEviction)
	e.Data = &gcpc.EventV1_CacheEviction{CacheEviction: &gcpc.CacheEvictionEventV1{
		Key: key, Reason: reason,
	}}
	return Event{Proto: e}
}

// NewRuntimeLogBatch creates a runtime.logs event carrying periodically flushed
// diagnostic log records.
func NewRuntimeLogBatch(records []*gcpc.RuntimeLogRecordV1) Event {
	e := newEventProto(RuntimeLogBatch)
	e.Data = &gcpc.EventV1_RuntimeLogBatch{RuntimeLogBatch: &gcpc.RuntimeLogBatchEventV1{
		Records: records,
	}}
	return Event{Proto: e}
}

// NewReplayGap creates a replay.gap event with a dedicated control payload.
func NewReplayGap(subscriber string, dropped uint64) Event {
	e := newEventProto(ReplayGap)
	e.Data = &gcpc.EventV1_ReplayGap{ReplayGap: &gcpc.ReplayGapEventV1{
		Subscriber: subscriber, DroppedCount: dropped,
	}}
	return Event{Proto: e}
}

func newEventProto(t Type) *gcpc.EventV1 {
	return &gcpc.EventV1{
		Type:      string(t),
		Timestamp: uint64(time.Now().UnixNano()),
	}
}

// WithOperationID returns a copy of the event with the operation_id set.
func (e Event) WithOperationID(id string) Event {
	e.Proto.OperationId = id
	return e
}

// NewOperationStarted creates an operation.started event.
func NewOperationStarted(id, opType, parentID string, ctx map[string]string) Event {
	e := newEventProto(OperationStarted)
	e.Data = &gcpc.EventV1_OperationStart{OperationStart: &gcpc.OperationStartEventV1{
		Id: id, Type: opType, ParentId: parentID, Context: ctx,
	}}
	e.OperationId = id
	return Event{Proto: e}
}

// NewOperationCompleted creates an operation.completed event.
func NewOperationCompleted(id, opType string, elapsedNs uint64, status, failReason string, ctx map[string]string) Event {
	e := newEventProto(OperationCompleted)
	e.Data = &gcpc.EventV1_OperationComplete{OperationComplete: &gcpc.OperationCompleteEventV1{
		Id: id, Type: opType, ElapsedNs: elapsedNs, Status: status, FailReason: failReason, Context: ctx,
	}}
	e.OperationId = id
	return Event{Proto: e}
}

// Emitter is the interface server components use to emit events.
//
// HasSubscribers returns true if at least one subscriber is currently
// attached. Implementations must make this check ~zero-cost — typically
// an atomic load — because the evaluator hot path calls it on every
// command to gate the instrumentation block.
type Emitter interface {
	Emit(Event)
	HasSubscribers() bool
	HasSubscribersFor(types ...Type) bool
}

// NoopEmitter discards all events. Used when plugins are disabled.
type NoopEmitter struct{}

func (NoopEmitter) Emit(Event)                           {}
func (NoopEmitter) HasSubscribers() bool                 { return false }
func (NoopEmitter) HasSubscribersFor(types ...Type) bool { return false }
