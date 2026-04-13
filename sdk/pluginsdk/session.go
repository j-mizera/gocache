package pluginsdk

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	gcpc "gocache/api/gcpc/v1"
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
func (s *Session) QueryServer(ctx context.Context, topic string) (map[string]string, error) {
	id := fmt.Sprintf("q-%d", s.idSeq.Add(1))
	ch := make(chan *gcpc.ServerQueryResponseV1, 1)
	s.pending.Store(id, ch)
	defer s.pending.Delete(id)

	if err := s.conn.Send(gcpc.NewServerQuery(id, topic)); err != nil {
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

// dispatch routes a server query response to the waiting caller.
// Called from Run()'s recv loop.
func (s *Session) dispatch(resp *gcpc.ServerQueryResponseV1) {
	if v, ok := s.pending.LoadAndDelete(resp.RequestId); ok {
		v.(chan *gcpc.ServerQueryResponseV1) <- resp
	}
}
