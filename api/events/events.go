// Package events defines event types for the GoCache server event system.
//
// The server emits structured events at key points (like Linux kernel tracepoints).
// Plugins subscribe to event types via GCPC and receive fire-and-forget notifications.
// Events are informational — they cannot deny or modify operations (use hooks for that).
//
// This package lives in api/ and has zero dependencies on server internals.
package events

import (
	"strconv"
	"time"

	gcpc "gocache/api/gcpc/v1"
)

// Type identifies an event category.
type Type string

const (
	CommandPre  Type = "command.pre"
	CommandPost Type = "command.post"

	ConnectionOpen  Type = "connection.open"
	ConnectionClose Type = "connection.close"

	ServerStart    Type = "server.start"
	ServerShutdown Type = "server.shutdown"

	PluginRegistered Type = "plugin.registered"
	PluginCrashed    Type = "plugin.crashed"
	PluginRestarted  Type = "plugin.restarted"

	ConfigReloaded Type = "config.reloaded"

	AuthFailed Type = "auth.failed"

	CacheEviction Type = "cache.eviction"

	LogEntry Type = "log.entry"

	OperationStart    Type = "operation.start"
	OperationComplete Type = "operation.complete"

	// ReplayGap marks that the event bus's replay ring dropped events before
	// a subscriber connected. The payload is a LogEntryEventV1 whose fields
	// carry the dropped count so Phase B subscribers route by Type without
	// requiring protobuf regen; Phase C will introduce a dedicated oneof.
	ReplayGap Type = "replay.gap"
)

// Event is a structured notification emitted by the server.
// It wraps a gcpc.EventV1 with the type and timestamp already set.
type Event struct {
	Proto *gcpc.EventV1
}

// NewCommandPre creates a command.pre event.
func NewCommandPre(command string, args []string, metadata map[string]string) Event {
	e := newEventProto(CommandPre)
	e.Data = &gcpc.EventV1_CommandPre{CommandPre: &gcpc.CommandPreEventV1{
		Command: command, Args: args, Metadata: metadata,
	}}
	return Event{Proto: e}
}

// NewCommandPost creates a command.post event.
func NewCommandPost(command string, args []string, elapsedNs uint64, result, errStr string, metadata map[string]string) Event {
	e := newEventProto(CommandPost)
	e.Data = &gcpc.EventV1_CommandPost{CommandPost: &gcpc.CommandPostEventV1{
		Command: command, Args: args, ElapsedNs: elapsedNs, Result: result, Error: errStr, Metadata: metadata,
	}}
	return Event{Proto: e}
}

// NewConnectionOpen creates a connection.open event.
func NewConnectionOpen(remoteAddr string) Event {
	e := newEventProto(ConnectionOpen)
	e.Data = &gcpc.EventV1_ConnectionOpen{ConnectionOpen: &gcpc.ConnectionOpenEventV1{
		RemoteAddr: remoteAddr,
	}}
	return Event{Proto: e}
}

// NewConnectionClose creates a connection.close event.
func NewConnectionClose(remoteAddr string, durationNs uint64) Event {
	e := newEventProto(ConnectionClose)
	e.Data = &gcpc.EventV1_ConnectionClose{ConnectionClose: &gcpc.ConnectionCloseEventV1{
		RemoteAddr: remoteAddr, DurationNs: durationNs,
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

// NewLogEntry creates a log.entry event.
func NewLogEntry(level, message, caller string, fields map[string]string) Event {
	e := newEventProto(LogEntry)
	e.Data = &gcpc.EventV1_LogEntry{LogEntry: &gcpc.LogEntryEventV1{
		Level: level, Message: message, Caller: caller, Fields: fields,
	}}
	return Event{Proto: e}
}

// NewReplayGap creates a replay.gap event. Carries the dropped count and
// subscriber name as LogEntryEventV1 fields — see the ReplayGap type comment.
func NewReplayGap(subscriber string, dropped uint64) Event {
	e := newEventProto(ReplayGap)
	fields := map[string]string{
		"_kind":          "replay_gap",
		"_dropped_count": strconv.FormatUint(dropped, 10),
		"_subscriber":    subscriber,
	}
	e.Data = &gcpc.EventV1_LogEntry{LogEntry: &gcpc.LogEntryEventV1{
		Level:   "warn",
		Message: "event replay gap",
		Fields:  fields,
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

// NewOperationStart creates an operation.start event.
func NewOperationStart(id, opType, parentID string, ctx map[string]string) Event {
	e := newEventProto(OperationStart)
	e.Data = &gcpc.EventV1_OperationStart{OperationStart: &gcpc.OperationStartEventV1{
		Id: id, Type: opType, ParentId: parentID, Context: ctx,
	}}
	e.OperationId = id
	return Event{Proto: e}
}

// NewOperationComplete creates an operation.complete event.
func NewOperationComplete(id, opType string, elapsedNs uint64, status, failReason string, ctx map[string]string) Event {
	e := newEventProto(OperationComplete)
	e.Data = &gcpc.EventV1_OperationComplete{OperationComplete: &gcpc.OperationCompleteEventV1{
		Id: id, Type: opType, ElapsedNs: elapsedNs, Status: status, FailReason: failReason, Context: ctx,
	}}
	e.OperationId = id
	return Event{Proto: e}
}

// Emitter is the interface server components use to emit events.
type Emitter interface {
	Emit(Event)
}

// NoopEmitter discards all events. Used when plugins are disabled.
type NoopEmitter struct{}

func (NoopEmitter) Emit(Event) {}
