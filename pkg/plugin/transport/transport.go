package transport

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	gcpc "gocache/proto/gcpc/v1"

	"google.golang.org/protobuf/proto"
)

// MaxFrameSize is the maximum allowed size for a single protobuf frame (1 MB).
const MaxFrameSize = 1 << 20

var (
	ErrFrameTooLarge = errors.New("frame exceeds maximum size")
	ErrConnClosed    = errors.New("connection closed")
)

// Conn wraps a net.Conn with length-prefixed protobuf framing.
type Conn struct {
	conn net.Conn
	mu   sync.Mutex // protects writes
}

// NewConn wraps an existing connection with framed protobuf I/O.
func NewConn(c net.Conn) *Conn {
	return &Conn{conn: c}
}

// Send marshals the envelope and writes it as a length-prefixed frame.
func (c *Conn) Send(env *gcpc.EnvelopeV1) error {
	data, err := proto.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if len(data) > MaxFrameSize {
		return ErrFrameTooLarge
	}

	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(data)))

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := c.conn.Write(header); err != nil {
		return fmt.Errorf("write frame header: %w", err)
	}
	if _, err := c.conn.Write(data); err != nil {
		return fmt.Errorf("write frame payload: %w", err)
	}
	return nil
}

// Recv reads a length-prefixed frame and unmarshals it into an Envelope.
func (c *Conn) Recv() (*gcpc.EnvelopeV1, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(c.conn, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrConnClosed
		}
		return nil, fmt.Errorf("read frame header: %w", err)
	}

	size := binary.BigEndian.Uint32(header)
	if size > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}

	data := make([]byte, size)
	if _, err := io.ReadFull(c.conn, data); err != nil {
		return nil, fmt.Errorf("read frame payload: %w", err)
	}

	env := &gcpc.EnvelopeV1{}
	if err := proto.Unmarshal(data, env); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	return env, nil
}

// Close closes the underlying connection.
func (c *Conn) Close() error {
	return c.conn.Close()
}

// Listener manages a server-side Unix domain socket.
type Listener struct {
	ln       net.Listener
	sockPath string
}

// NewListener creates a Unix domain socket listener at the given path.
// Any stale socket file is removed before binding.
func NewListener(sockPath string) (*Listener, error) {
	// Remove stale socket file if it exists.
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", sockPath, err)
	}

	return &Listener{ln: ln, sockPath: sockPath}, nil
}

// Accept waits for a plugin to connect and returns a framed Conn.
func (l *Listener) Accept() (*Conn, error) {
	conn, err := l.ln.Accept()
	if err != nil {
		return nil, err
	}
	return NewConn(conn), nil
}

// Close closes the listener and removes the socket file.
func (l *Listener) Close() error {
	err := l.ln.Close()
	_ = os.Remove(l.sockPath)
	return err
}

// Addr returns the socket path.
func (l *Listener) Addr() string {
	return l.sockPath
}
