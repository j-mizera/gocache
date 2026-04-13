package v1

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// ProtocolVersion is the current GCPC protocol version.
const ProtocolVersion = 1

var nextID atomic.Uint64

func envelopeID() uint64 { return nextID.Add(1) }

func NewRegister(name, version string, critical bool, commands []*CommandDeclV1, priority int32) *EnvelopeV1 {
	return &EnvelopeV1{
		Version: ProtocolVersion,
		Id:      envelopeID(),
		Payload: &EnvelopeV1_Register{
			Register: &RegisterV1{
				Name:     name,
				Version:  version,
				Critical: critical,
				Commands: commands,
				Priority: priority,
			},
		},
	}
}

func NewRegisterAck(accepted bool, reason string, grantedScopes []string) *EnvelopeV1 {
	return &EnvelopeV1{
		Version: ProtocolVersion,
		Id:      envelopeID(),
		Payload: &EnvelopeV1_RegisterAck{
			RegisterAck: &RegisterAckV1{
				Accepted:      accepted,
				Reason:        reason,
				GrantedScopes: grantedScopes,
			},
		},
	}
}

func NewHealthCheck() *EnvelopeV1 {
	return &EnvelopeV1{
		Version: ProtocolVersion,
		Id:      envelopeID(),
		Payload: &EnvelopeV1_HealthCheck{
			HealthCheck: &HealthCheckV1{
				Timestamp: uint64(time.Now().UnixNano()),
			},
		},
	}
}

func NewHealthResponse(ok bool, status string) *EnvelopeV1 {
	return &EnvelopeV1{
		Version: ProtocolVersion,
		Id:      envelopeID(),
		Payload: &EnvelopeV1_HealthResponse{
			HealthResponse: &HealthResponseV1{
				Ok:     ok,
				Status: status,
			},
		},
	}
}

func NewShutdown(deadline time.Time) *EnvelopeV1 {
	return &EnvelopeV1{
		Version: ProtocolVersion,
		Id:      envelopeID(),
		Payload: &EnvelopeV1_Shutdown{
			Shutdown: &ShutdownV1{
				DeadlineNs: uint64(deadline.UnixNano()),
			},
		},
	}
}

func NewShutdownAck() *EnvelopeV1 {
	return &EnvelopeV1{
		Version: ProtocolVersion,
		Id:      envelopeID(),
		Payload: &EnvelopeV1_ShutdownAck{
			ShutdownAck: &ShutdownAckV1{},
		},
	}
}

func NewHookRequest(requestID string, phase HookPhaseV1, command string, args []string, resultValue, resultError string, ctx map[string]string, metadata map[string]string) *EnvelopeV1 {
	return &EnvelopeV1{
		Version: ProtocolVersion,
		Id:      envelopeID(),
		Payload: &EnvelopeV1_HookRequest{
			HookRequest: &HookRequestV1{
				RequestId:   requestID,
				Phase:       phase,
				Command:     command,
				Args:        args,
				ResultValue: resultValue,
				ResultError: resultError,
				Context:     ctx,
				Metadata:    metadata,
			},
		},
	}
}

func NewHookResponse(requestID string, deny bool, denyReason string, contextValues map[string]string) *EnvelopeV1 {
	return &EnvelopeV1{
		Version: ProtocolVersion,
		Id:      envelopeID(),
		Payload: &EnvelopeV1_HookResponse{
			HookResponse: &HookResponseV1{
				RequestId:     requestID,
				Deny:          deny,
				DenyReason:    denyReason,
				ContextValues: contextValues,
			},
		},
	}
}

func NewCommandRequest(command string, args []string, requestID string, metadata map[string]string) *EnvelopeV1 {
	return &EnvelopeV1{
		Version: ProtocolVersion,
		Id:      envelopeID(),
		Payload: &EnvelopeV1_CommandRequest{
			CommandRequest: &CommandRequestV1{
				Command:   command,
				Args:      args,
				RequestId: requestID,
				Metadata:  metadata,
			},
		},
	}
}

func NewCommandResponse(requestID string, result *ResultV1) *EnvelopeV1 {
	return &EnvelopeV1{
		Version: ProtocolVersion,
		Id:      envelopeID(),
		Payload: &EnvelopeV1_CommandResponse{
			CommandResponse: &CommandResponseV1{
				RequestId: requestID,
				Result:    result,
			},
		},
	}
}

func NewServerQuery(requestID, topic string) *EnvelopeV1 {
	return &EnvelopeV1{
		Version: ProtocolVersion,
		Id:      envelopeID(),
		Payload: &EnvelopeV1_ServerQuery{
			ServerQuery: &ServerQueryV1{
				RequestId: requestID,
				Topic:     topic,
			},
		},
	}
}

func NewServerQueryResponse(requestID string, data map[string]string, errMsg string) *EnvelopeV1 {
	return &EnvelopeV1{
		Version: ProtocolVersion,
		Id:      envelopeID(),
		Payload: &EnvelopeV1_ServerQueryResponse{
			ServerQueryResponse: &ServerQueryResponseV1{
				RequestId: requestID,
				Data:      data,
				Error:     errMsg,
			},
		},
	}
}

// ResultFromInterface converts a Go value to a proto ResultV1.
// Supported types: string, int, int64, float64, nil, error,
// []interface{}, []string, map[string]string, map[string]interface{}.
func ResultFromInterface(val interface{}) *ResultV1 {
	if val == nil {
		return &ResultV1{Value: &ResultV1_IsNull{IsNull: true}}
	}
	switch v := val.(type) {
	case error:
		return &ResultV1{Value: &ResultV1_Error{Error: v.Error()}}
	case string:
		return &ResultV1{Value: &ResultV1_BulkString{BulkString: v}}
	case int:
		return &ResultV1{Value: &ResultV1_Integer{Integer: int64(v)}}
	case int64:
		return &ResultV1{Value: &ResultV1_Integer{Integer: v}}
	case float64:
		return &ResultV1{Value: &ResultV1_DoubleVal{DoubleVal: v}}
	case []string:
		elems := make([]*ResultV1, len(v))
		for i, s := range v {
			elems[i] = &ResultV1{Value: &ResultV1_BulkString{BulkString: s}}
		}
		return &ResultV1{Value: &ResultV1_Array{Array: &ResultArrayV1{Elements: elems}}}
	case []interface{}:
		elems := make([]*ResultV1, len(v))
		for i, item := range v {
			elems[i] = ResultFromInterface(item)
		}
		return &ResultV1{Value: &ResultV1_Array{Array: &ResultArrayV1{Elements: elems}}}
	case map[string]string:
		entries := make([]*ResultEntryV1, 0, len(v))
		for k, val := range v {
			entries = append(entries, &ResultEntryV1{
				Key:   k,
				Value: &ResultV1{Value: &ResultV1_BulkString{BulkString: val}},
			})
		}
		return &ResultV1{Value: &ResultV1_MapVal{MapVal: &ResultMapV1{Entries: entries}}}
	case map[string]interface{}:
		entries := make([]*ResultEntryV1, 0, len(v))
		for k, val := range v {
			entries = append(entries, &ResultEntryV1{
				Key:   k,
				Value: ResultFromInterface(val),
			})
		}
		return &ResultV1{Value: &ResultV1_MapVal{MapVal: &ResultMapV1{Entries: entries}}}
	default:
		return &ResultV1{Value: &ResultV1_BulkString{BulkString: fmt.Sprintf("%v", v)}}
	}
}

func NewEventSubscribe(types []string) *EnvelopeV1 {
	return &EnvelopeV1{
		Version: ProtocolVersion,
		Id:      envelopeID(),
		Payload: &EnvelopeV1_EventSubscribe{
			EventSubscribe: &EventSubscribeV1{
				Types: types,
			},
		},
	}
}

// InterfaceFromResult converts a proto ResultV1 back to a Go interface{} value
// compatible with evaluator.Result.Value types.
func InterfaceFromResult(r *ResultV1) interface{} {
	if r == nil {
		return nil
	}
	switch v := r.Value.(type) {
	case *ResultV1_SimpleString:
		return v.SimpleString
	case *ResultV1_Error:
		return errors.New(v.Error)
	case *ResultV1_Integer:
		return int(v.Integer)
	case *ResultV1_BulkString:
		return v.BulkString
	case *ResultV1_IsNull:
		return nil
	case *ResultV1_DoubleVal:
		return v.DoubleVal
	case *ResultV1_Array:
		if v.Array == nil {
			return nil
		}
		elems := make([]interface{}, len(v.Array.Elements))
		for i, e := range v.Array.Elements {
			elems[i] = InterfaceFromResult(e)
		}
		return elems
	case *ResultV1_MapVal:
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
