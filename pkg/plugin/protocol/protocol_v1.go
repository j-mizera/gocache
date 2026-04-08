package protocol

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	gcpc "gocache/proto/gcpc/v1"
)

// ProtocolVersion is the current GCPC protocol version.
const ProtocolVersion = 1

var nextID atomic.Uint64

func id() uint64 { return nextID.Add(1) }

func NewRegister(name, version string, critical bool, commands []*gcpc.CommandDeclV1, priority int32) *gcpc.EnvelopeV1 {
	return &gcpc.EnvelopeV1{
		Version: ProtocolVersion,
		Id:      id(),
		Payload: &gcpc.EnvelopeV1_Register{
			Register: &gcpc.RegisterV1{
				Name:     name,
				Version:  version,
				Critical: critical,
				Commands: commands,
				Priority: priority,
			},
		},
	}
}

func NewRegisterAck(accepted bool, reason string, grantedScopes []string) *gcpc.EnvelopeV1 {
	return &gcpc.EnvelopeV1{
		Version: ProtocolVersion,
		Id:      id(),
		Payload: &gcpc.EnvelopeV1_RegisterAck{
			RegisterAck: &gcpc.RegisterAckV1{
				Accepted:      accepted,
				Reason:        reason,
				GrantedScopes: grantedScopes,
			},
		},
	}
}

func NewHealthCheck() *gcpc.EnvelopeV1 {
	return &gcpc.EnvelopeV1{
		Version: ProtocolVersion,
		Id:      id(),
		Payload: &gcpc.EnvelopeV1_HealthCheck{
			HealthCheck: &gcpc.HealthCheckV1{
				Timestamp: uint64(time.Now().UnixNano()),
			},
		},
	}
}

func NewHealthResponse(ok bool, status string) *gcpc.EnvelopeV1 {
	return &gcpc.EnvelopeV1{
		Version: ProtocolVersion,
		Id:      id(),
		Payload: &gcpc.EnvelopeV1_HealthResponse{
			HealthResponse: &gcpc.HealthResponseV1{
				Ok:     ok,
				Status: status,
			},
		},
	}
}

func NewShutdown(deadline time.Time) *gcpc.EnvelopeV1 {
	return &gcpc.EnvelopeV1{
		Version: ProtocolVersion,
		Id:      id(),
		Payload: &gcpc.EnvelopeV1_Shutdown{
			Shutdown: &gcpc.ShutdownV1{
				DeadlineNs: uint64(deadline.UnixNano()),
			},
		},
	}
}

func NewShutdownAck() *gcpc.EnvelopeV1 {
	return &gcpc.EnvelopeV1{
		Version: ProtocolVersion,
		Id:      id(),
		Payload: &gcpc.EnvelopeV1_ShutdownAck{
			ShutdownAck: &gcpc.ShutdownAckV1{},
		},
	}
}

func NewHookRequest(requestID string, phase gcpc.HookPhaseV1, command string, args []string, resultValue, resultError string) *gcpc.EnvelopeV1 {
	return &gcpc.EnvelopeV1{
		Version: ProtocolVersion,
		Id:      id(),
		Payload: &gcpc.EnvelopeV1_HookRequest{
			HookRequest: &gcpc.HookRequestV1{
				RequestId:   requestID,
				Phase:       phase,
				Command:     command,
				Args:        args,
				ResultValue: resultValue,
				ResultError: resultError,
			},
		},
	}
}

func NewHookResponse(requestID string, deny bool, denyReason string) *gcpc.EnvelopeV1 {
	return &gcpc.EnvelopeV1{
		Version: ProtocolVersion,
		Id:      id(),
		Payload: &gcpc.EnvelopeV1_HookResponse{
			HookResponse: &gcpc.HookResponseV1{
				RequestId:  requestID,
				Deny:       deny,
				DenyReason: denyReason,
			},
		},
	}
}

func NewCommandRequest(command string, args []string, requestID string) *gcpc.EnvelopeV1 {
	return &gcpc.EnvelopeV1{
		Version: ProtocolVersion,
		Id:      id(),
		Payload: &gcpc.EnvelopeV1_CommandRequest{
			CommandRequest: &gcpc.CommandRequestV1{
				Command:   command,
				Args:      args,
				RequestId: requestID,
			},
		},
	}
}

func NewCommandResponse(requestID string, result *gcpc.ResultV1) *gcpc.EnvelopeV1 {
	return &gcpc.EnvelopeV1{
		Version: ProtocolVersion,
		Id:      id(),
		Payload: &gcpc.EnvelopeV1_CommandResponse{
			CommandResponse: &gcpc.CommandResponseV1{
				RequestId: requestID,
				Result:    result,
			},
		},
	}
}

// ResultFromInterface converts a Go value to a proto ResultV1.
// Supported types: string, int, int64, float64, nil, error,
// []interface{}, []string, map[string]string, map[string]interface{}.
func ResultFromInterface(val interface{}) *gcpc.ResultV1 {
	if val == nil {
		return &gcpc.ResultV1{Value: &gcpc.ResultV1_IsNull{IsNull: true}}
	}
	switch v := val.(type) {
	case error:
		return &gcpc.ResultV1{Value: &gcpc.ResultV1_Error{Error: v.Error()}}
	case string:
		return &gcpc.ResultV1{Value: &gcpc.ResultV1_BulkString{BulkString: v}}
	case int:
		return &gcpc.ResultV1{Value: &gcpc.ResultV1_Integer{Integer: int64(v)}}
	case int64:
		return &gcpc.ResultV1{Value: &gcpc.ResultV1_Integer{Integer: v}}
	case float64:
		return &gcpc.ResultV1{Value: &gcpc.ResultV1_DoubleVal{DoubleVal: v}}
	case []string:
		elems := make([]*gcpc.ResultV1, len(v))
		for i, s := range v {
			elems[i] = &gcpc.ResultV1{Value: &gcpc.ResultV1_BulkString{BulkString: s}}
		}
		return &gcpc.ResultV1{Value: &gcpc.ResultV1_Array{Array: &gcpc.ResultArrayV1{Elements: elems}}}
	case []interface{}:
		elems := make([]*gcpc.ResultV1, len(v))
		for i, item := range v {
			elems[i] = ResultFromInterface(item)
		}
		return &gcpc.ResultV1{Value: &gcpc.ResultV1_Array{Array: &gcpc.ResultArrayV1{Elements: elems}}}
	case map[string]string:
		entries := make([]*gcpc.ResultEntryV1, 0, len(v))
		for k, val := range v {
			entries = append(entries, &gcpc.ResultEntryV1{
				Key:   k,
				Value: &gcpc.ResultV1{Value: &gcpc.ResultV1_BulkString{BulkString: val}},
			})
		}
		return &gcpc.ResultV1{Value: &gcpc.ResultV1_MapVal{MapVal: &gcpc.ResultMapV1{Entries: entries}}}
	case map[string]interface{}:
		entries := make([]*gcpc.ResultEntryV1, 0, len(v))
		for k, val := range v {
			entries = append(entries, &gcpc.ResultEntryV1{
				Key:   k,
				Value: ResultFromInterface(val),
			})
		}
		return &gcpc.ResultV1{Value: &gcpc.ResultV1_MapVal{MapVal: &gcpc.ResultMapV1{Entries: entries}}}
	default:
		return &gcpc.ResultV1{Value: &gcpc.ResultV1_BulkString{BulkString: fmt.Sprintf("%v", v)}}
	}
}

// InterfaceFromResult converts a proto ResultV1 back to a Go interface{} value
// compatible with evaluator.Result.Value types.
func InterfaceFromResult(r *gcpc.ResultV1) interface{} {
	if r == nil {
		return nil
	}
	switch v := r.Value.(type) {
	case *gcpc.ResultV1_SimpleString:
		return v.SimpleString
	case *gcpc.ResultV1_Error:
		return errors.New(v.Error)
	case *gcpc.ResultV1_Integer:
		return int(v.Integer)
	case *gcpc.ResultV1_BulkString:
		return v.BulkString
	case *gcpc.ResultV1_IsNull:
		return nil
	case *gcpc.ResultV1_DoubleVal:
		return v.DoubleVal
	case *gcpc.ResultV1_Array:
		if v.Array == nil {
			return nil
		}
		elems := make([]interface{}, len(v.Array.Elements))
		for i, e := range v.Array.Elements {
			elems[i] = InterfaceFromResult(e)
		}
		return elems
	case *gcpc.ResultV1_MapVal:
		if v.MapVal == nil {
			return nil
		}
		m := make(map[string]interface{}, len(v.MapVal.Entries))
		for _, entry := range v.MapVal.Entries {
			m[entry.Key] = InterfaceFromResult(entry.Value)
		}
		return m
	default:
		return nil
	}
}
