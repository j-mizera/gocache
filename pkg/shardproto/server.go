package shardproto

import (
	"context"
	"errors"

	"net"
	"strings"
	"sync"

	"gocache/commons/resp"
)

// Server is a minimal RESP server exposing only the three commands the
// diagnosis benchmarks exercise: GET, SET, HSET. Anything else returns a
// RESP error. It is intentionally narrow — a wider command surface is the
// concern of the production implementation, not the prototype.
type Server struct {
	listener net.Listener
	engine   *Engine
	connsWG  sync.WaitGroup
	stopOnce sync.Once
	stopped  chan struct{}
}

// NewServer wires a server around an already-running engine.
func NewServer(engine *Engine) *Server {
	return &Server{engine: engine, stopped: make(chan struct{})}
}

// Listen binds to addr (use ":0" for an ephemeral port) and stores the
// listener; call Serve to accept connections.
func (s *Server) Listen(addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.listener = l
	return nil
}

// Addr returns the local TCP address — useful when Listen used ":0".
func (s *Server) Addr() string { return s.listener.Addr().String() }

// Serve accepts connections until the listener is closed. Each connection
// is handled in its own goroutine until the client disconnects or Stop is
// called.
func (s *Server) Serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.connsWG.Add(1)
		go func(c net.Conn) {
			defer s.connsWG.Done()
			defer c.Close()
			s.handleConn(c)
		}(conn)
	}
}

// Stop closes the listener and waits for in-flight handlers to drain.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopped)
		if s.listener != nil {
			_ = s.listener.Close()
		}
		s.connsWG.Wait()
	})
}

func (s *Server) handleConn(c net.Conn) {
	rd := resp.NewReader(c)
	w := resp.NewWriter(c)
	ctx := context.Background()
	for {
		v, err := rd.Read()
		if err != nil {
			return
		}
		if v.Type != resp.Array || len(v.Array) == 0 {
			s.writeErr(w, "ERR malformed command")
			continue
		}
		cmd := strings.ToUpper(v.Array[0].Str)
		args := v.Array[1:]
		s.dispatch(ctx, w, cmd, args)
		// Pipelining: drain anything more buffered before flushing once.
		if rd.Buffered() == 0 {
			if err := w.Flush(); err != nil {
				return
			}
		}
	}
}

func (s *Server) dispatch(ctx context.Context, w *resp.Writer, cmd string, args []resp.Value) {
	switch cmd {
	case "GET":
		s.handleGet(ctx, w, args)
	case "SET":
		s.handleSet(ctx, w, args)
	case "HSET":
		s.handleHset(ctx, w, args)
	case "PING":
		_ = w.Write(resp.MarshalSimpleString("PONG"))
	default:
		s.writeErr(w, "ERR unsupported command in shardproto: "+cmd)
	}
}

func (s *Server) writeErr(w *resp.Writer, msg string) {
	_ = w.Write(resp.MarshalError(msg))
}

func (s *Server) handleGet(ctx context.Context, w *resp.Writer, args []resp.Value) {
	if len(args) != 1 {
		s.writeErr(w, "ERR wrong number of arguments for 'get'")
		return
	}
	key := args[0].Str
	res, err := s.engine.Dispatch(ctx, key, func(sh *Shard) any {
		sh.mu.RLock()
		defer sh.mu.RUnlock()
		v, ok := sh.items[key]
		if !ok {
			return nil
		}
		return v
	})
	if err != nil {
		s.writeErr(w, "ERR "+err.Error())
		return
	}
	if res == nil {
		_ = w.Write(resp.MarshalNull())
		return
	}
	switch v := res.(type) {
	case string:
		_ = w.Write(resp.MarshalBulkString(v))
	default:
		s.writeErr(w, "WRONGTYPE Operation against a key holding the wrong kind of value")
	}
}

func (s *Server) handleSet(ctx context.Context, w *resp.Writer, args []resp.Value) {
	if len(args) != 2 {
		s.writeErr(w, "ERR wrong number of arguments for 'set'")
		return
	}
	key := args[0].Str
	val := args[1].Str
	_, err := s.engine.Dispatch(ctx, key, func(sh *Shard) any {
		sh.mu.Lock()
		sh.items[key] = val
		sh.mu.Unlock()
		return nil
	})
	if err != nil {
		s.writeErr(w, "ERR "+err.Error())
		return
	}
	_ = w.Write(resp.MarshalSimpleString("OK"))
}

func (s *Server) handleHset(ctx context.Context, w *resp.Writer, args []resp.Value) {
	if len(args) < 3 || len(args)%2 == 0 {
		s.writeErr(w, "ERR wrong number of arguments for 'hset'")
		return
	}
	key := args[0].Str
	pairs := args[1:]
	res, err := s.engine.Dispatch(ctx, key, func(sh *Shard) any {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		raw, ok := sh.items[key]
		var h map[string]string
		if ok {
			h, ok = raw.(map[string]string)
			if !ok {
				return errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")
			}
		} else {
			h = make(map[string]string, len(pairs)/2)
			sh.items[key] = h
		}
		added := 0
		for i := 0; i < len(pairs); i += 2 {
			f, v := pairs[i].Str, pairs[i+1].Str
			if _, exists := h[f]; !exists {
				added++
			}
			h[f] = v
		}
		return added
	})
	if err != nil {
		s.writeErr(w, "ERR "+err.Error())
		return
	}
	if e, ok := res.(error); ok {
		s.writeErr(w, e.Error())
		return
	}
	if n, ok := res.(int); ok {
		_ = w.Write(resp.Value{Type: resp.Integer, Integer: n})
		return
	}
	s.writeErr(w, "ERR internal: unexpected handler result")
}
