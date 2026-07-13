package transport

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	gcpc "gocache/api/gcpc/v1"
)

// MaxFrameSize is the maximum allowed size for a single protobuf frame (1 MB).
const MaxFrameSize = 1 << 20

// frameHeaderSize is the length in bytes of the big-endian uint32 length
// prefix that precedes every framed protobuf payload.
const frameHeaderSize = 4

var (
	ErrFrameTooLarge = errors.New("frame exceeds maximum size")
	ErrConnClosed    = errors.New("connection closed")
)

// Conn wraps a net.Conn with length-prefixed protobuf framing.
type Conn struct {
	conn     net.Conn
	mu       sync.Mutex // protects writes
	writeBuf []byte     // reusable frame buffer for SendBatch, accessed under mu
	readBuf  []byte     // reusable payload buffer for Recv, single-reader
}

// NewConn wraps an existing connection with framed protobuf I/O.
func NewConn(c net.Conn) *Conn {
	return &Conn{conn: c}
}

// Send marshals the envelope and writes it as a length-prefixed frame.
func (c *Conn) Send(env *gcpc.EnvelopeV1) error {
	return c.SendBatch([]*gcpc.EnvelopeV1{env})
}

// SendBatch marshals envelopes and writes their length-prefixed frames with one
// locked write. The wire format remains a stream of normal GCPC frames, so
// receivers continue to call Recv once per envelope while bursty producers avoid
// one syscall per frame.
func (c *Conn) SendBatch(envs []*gcpc.EnvelopeV1) error {
	if len(envs) == 0 {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.writeBuf = c.writeBuf[:0]
	for _, env := range envs {
		payload, err := env.MarshalVT()
		if err != nil {
			return fmt.Errorf("marshal envelope: %w", err)
		}
		if len(payload) > MaxFrameSize {
			return ErrFrameTooLarge
		}
		var header [frameHeaderSize]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
		c.writeBuf = append(c.writeBuf, header[:]...)
		c.writeBuf = append(c.writeBuf, payload...)
	}

	n, err := c.conn.Write(c.writeBuf)
	if err != nil {
		return fmt.Errorf("write frame batch: %w", err)
	}
	if n != len(c.writeBuf) {
		return io.ErrShortWrite
	}
	clear(c.writeBuf[:cap(c.writeBuf)])
	c.writeBuf = c.writeBuf[:0]
	return nil
}

// Recv reads a length-prefixed frame and unmarshals it into an Envelope.
func (c *Conn) Recv() (*gcpc.EnvelopeV1, error) {
	header := make([]byte, frameHeaderSize)
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

	if int(size) > cap(c.readBuf) {
		c.readBuf = make([]byte, size)
	} else {
		c.readBuf = c.readBuf[:size]
	}

	if _, err := io.ReadFull(c.conn, c.readBuf); err != nil {
		return nil, fmt.Errorf("read frame payload: %w", err)
	}

	env := &gcpc.EnvelopeV1{}
	if err := env.UnmarshalVT(c.readBuf); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	clear(c.readBuf[:cap(c.readBuf)])
	c.readBuf = c.readBuf[:0]
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

// Close closes the listener and removes the socket file. Errors from both
// operations are joined via errors.Join so neither is silently lost; an
// os.ErrNotExist on Remove is expected (double-close or listener never
// bound) and is filtered out.
func (l *Listener) Close() error {
	closeErr := l.ln.Close()
	removeErr := os.Remove(l.sockPath)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}

// Addr returns the socket path.
func (l *Listener) Addr() string {
	return l.sockPath
}
