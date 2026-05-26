package pluginsdk

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"gocache/api/command"
	gcpc "gocache/api/gcpc/v1"
	ops "gocache/api/operations"
	"gocache/api/transport"
)

// Session provides query capabilities over the plugin's GCPC connection.
// It is safe for concurrent use from multiple goroutines (e.g., HTTP handlers).
type Session struct {
	conn    *transport.Conn
	pending sync.Map // request_id -> chan *gcpc.ServerQueryResponseV1
	idSeq   atomic.Uint64
}

func newSession(conn *transport.Conn) *Session {
	return &Session{conn: conn}
}

// QueryServer sends a query to the server and waits for the response.
// The topic maps to a registered server-side handler (e.g. "health", "plugins", "stats").
func (s *Session) QueryServer(ctx context.Context, topic string, params map[string]string) (map[string]string, error) {
	id := fmt.Sprintf("q-%d", s.idSeq.Add(1))
	ch := make(chan *gcpc.ServerQueryResponseV1, 1)
	s.pending.Store(id, ch)
	defer s.pending.Delete(id)

	if err := s.conn.Send(gcpc.NewServerQuery(id, topic, params)); err != nil {
		return nil, fmt.Errorf("send server query: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != "" {
			return nil, errors.New(resp.Error)
		}
		return resp.Data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// StartOperation creates a server-tracked operation for plugin-initiated
// async work. Returns an enriched context and a PluginOperation whose
// Complete/Fail methods notify the server.
func (s *Session) StartOperation(ctx context.Context, opType string) (context.Context, *PluginOperation, error) {
	data, err := s.QueryServer(ctx, "operation.start", map[string]string{"type": opType})
	if err != nil {
		return ctx, nil, fmt.Errorf("start operation: %w", err)
	}
	op := ops.New(ops.Type(opType), "")
	if id := data[command.OperationID]; id != "" {
		op.ID = id
	}
	op.EnrichMany(data)
	return ops.WithContext(ctx, op), &PluginOperation{session: s, op: op}, nil
}

func (s *Session) completeOperation(operationID string) error {
	_, err := s.QueryServer(context.Background(), "operation.complete", map[string]string{
		"_operation_id": operationID,
	})
	return err
}

func (s *Session) failOperation(operationID, reason string) error {
	_, err := s.QueryServer(context.Background(), "operation.fail", map[string]string{
		"_operation_id":  operationID,
		"_fail_reason":   reason,
	})
	return err
}

// PluginOperation wraps a server-tracked operation started by the plugin.
// Complete and Fail update both the local operation and notify the server.
type PluginOperation struct {
	session *Session
	op      *ops.Operation
}

func (po *PluginOperation) Complete() {
	po.op.Complete()
	_ = po.session.completeOperation(po.op.ID)
}

func (po *PluginOperation) Fail(reason string) {
	po.op.Fail(reason)
	_ = po.session.failOperation(po.op.ID, reason)
}

func (po *PluginOperation) Enrich(key, value string) {
	po.op.Enrich(key, value)
}

// PushToClient sends raw RESP data to a specific client connection.
// The data must be pre-formatted RESP bytes. The server writes them directly
// to the client's TCP connection, bypassing normal command-response flow.
func (s *Session) PushToClient(connID string, data []byte) error {
	return s.conn.Send(gcpc.NewClientPush(connID, data))
}

// dispatch routes a server query response to the waiting caller.
// Called from Run()'s recv loop.
func (s *Session) dispatch(resp *gcpc.ServerQueryResponseV1) {
	if v, ok := s.pending.LoadAndDelete(resp.RequestId); ok {
		v.(chan *gcpc.ServerQueryResponseV1) <- resp
	}
}
