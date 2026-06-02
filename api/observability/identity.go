// Package observability defines plugin-safe operation telemetry contracts.
//
// The package is intentionally limited to stable value types and small
// interfaces. Concrete sidecar queues, workers, context-version stores, GCPC
// projection, and server lookup state stay outside api/.
package observability

// OperationID is the public operation correlation identifier visible at API,
// SDK, GCPC, log, event, and plugin boundaries.
type OperationID string

// String returns the public operation id as a plain string.
func (id OperationID) String() string { return string(id) }

// IsZero reports whether id is empty.
func (id OperationID) IsZero() bool { return id == "" }

// OperationHandle is the plugin-safe handle shape exposed to API consumers.
// It deliberately carries only the exported operation id; server-internal
// identities used for sharding and worker lookup are not exposed here.
type OperationHandle struct {
	id OperationID
}

// NewOperationHandle returns a handle for an exported operation id.
func NewOperationHandle(id string) OperationHandle {
	return OperationHandle{id: OperationID(id)}
}

// ID returns the exported operation id.
func (h OperationHandle) ID() OperationID { return h.id }

// String returns the exported operation id as a plain string.
func (h OperationHandle) String() string { return h.id.String() }

// IsZero reports whether the handle has no exported operation id.
func (h OperationHandle) IsZero() bool { return h.id.IsZero() }

// OperationRef is the boundary-safe reference shape for an operation and its
// public parent. ParentID is empty for root operations.
type OperationRef struct {
	ID       OperationID
	ParentID OperationID
}

// NewOperationRef returns a public operation reference.
func NewOperationRef(id, parentID string) OperationRef {
	return OperationRef{ID: OperationID(id), ParentID: OperationID(parentID)}
}

// IsZero reports whether the reference has no operation id.
func (r OperationRef) IsZero() bool { return r.ID.IsZero() }

// InternalOperationIdentity is an opaque in-process sidecar identity. It is
// suitable for sharding, ordering, worker lookup, and map keys inside a tracker
// engine. It is not a public operation id and must not cross GCPC/plugin
// boundaries as identity.
type InternalOperationIdentity int64

// IsZero reports whether id is the empty internal operation identity.
func (id InternalOperationIdentity) IsZero() bool { return id == 0 }

// ConnectionIdentity identifies a connection for internal sidecar routing and
// context-version ownership. It is not a public tracing id.
type ConnectionIdentity uint64

// ConnectionContextVersion identifies an immutable connection-context snapshot
// stored centrally by OperationTrackerManager implementations.
type ConnectionContextVersion uint64

// IsZero reports whether version is the empty connection-context version.
func (version ConnectionContextVersion) IsZero() bool { return version == 0 }

// TraceFlags carries the W3C trace-flags byte used by trace-context rendering.
type TraceFlags byte

// OperationIdentityInput is the data a configured strategy may need to render a
// public operation identity. It intentionally omits node/fleet identifiers in
// the first implementation; replica-aware identity is a future concern.
type OperationIdentityInput struct {
	Internal      InternalOperationIdentity
	Parent        OperationRef
	StartUnixNano int64
	Sequence      uint64
	TraceID       [16]byte
	SpanID        [8]byte
	ParentSpanID  [8]byte
	TraceFlags    TraceFlags
}

// OperationIdentity is the strategy-rendered public identity plus trace fields
// needed by tracing-compatible renderers.
type OperationIdentity struct {
	ID           OperationID
	ParentID     OperationID
	TraceID      [16]byte
	SpanID       [8]byte
	ParentSpanID [8]byte
	TraceFlags   TraceFlags
}

// Ref returns the public operation reference carried by identity.
func (id OperationIdentity) Ref() OperationRef {
	return OperationRef{ID: id.ID, ParentID: id.ParentID}
}

// OperationIdentityStrategy renders the public operation identity for API,
// SDK, GCPC, log, event, and plugin boundaries. Implementations may use the
// internal identity as input, but must return only exported identity values.
type OperationIdentityStrategy interface {
	Render(OperationIdentityInput) (OperationIdentity, error)
}
