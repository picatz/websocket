package websocket

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// NetConn wraps c as a net.Conn that carries a byte stream in messages of one
// type, for tunnelling stream protocols such as gRPC, SSH or HTTP/2 over a
// WebSocket. Each Write sends one message of messageType; Read returns message
// payloads back to back, without message boundaries. The wrapper takes over c:
// do not call c's read or write methods while it is in use.
//
// messageType must be BinaryMessage or TextMessage; NetConn panics otherwise.
// Prefer BinaryMessage. With TextMessage every Write must be valid UTF-8 on its
// own, so a Write of arbitrary bytes fails.
//
// When ctx ends, the connection is aborted as described for ReadMessageContext
// and blocked calls return. Close stops watching ctx and closes c.
//
// Read returns io.EOF once the peer closes the WebSocket with
// StatusNormalClosure, StatusGoingAway or no status. Any other close status is
// returned as the peer's *CloseError, so io.Copy reports a stream the peer
// ended abnormally instead of accepting it as complete. A message of another
// type fails the connection with StatusUnsupportedData. Read and Write may be
// called concurrently with each other and with Close, as net.Conn requires;
// concurrent Reads are serialized, as are concurrent Writes. After Close, or
// once the connection is aborted, errors match net.ErrClosed.
//
// The deadline methods delegate to c, so a read timeout is terminal, unlike a
// net.TCPConn: clearing the deadline does not make the connection reusable. The
// returned net.Error reports Timeout as the transport did. Each message is
// subject to c's incoming size limit, and is buffered whole before Read
// returns its first byte.
func NetConn(ctx context.Context, c *Conn, messageType MessageType) net.Conn {
	if messageType != BinaryMessage && messageType != TextMessage {
		panic(fmt.Sprintf("websocket: NetConn requires BinaryMessage or TextMessage, got %v", messageType))
	}
	nc := &netConn{c: c, messageType: messageType}
	nc.stop = context.AfterFunc(ctx, c.cancelIO)
	return nc
}

type netConn struct {
	c           *Conn
	messageType MessageType
	stop        func() bool

	readMu  sync.Mutex
	pending []byte // Unread bytes of the current message
	readErr error  // Sticky; Read never resumes after an error
}

func (nc *netConn) Read(p []byte) (int, error) {
	nc.readMu.Lock()
	defer nc.readMu.Unlock()
	for len(nc.pending) == 0 {
		if nc.readErr != nil {
			return 0, nc.readErr
		}
		if len(p) == 0 {
			return 0, nil
		}
		typ, data, err := nc.c.ReadMessage()
		switch {
		case err != nil:
			nc.readErr = netConnError(err)
			return 0, nc.readErr
		case typ != nc.messageType:
			// https://datatracker.ietf.org/doc/html/rfc6455#section-7.4.1
			nc.readErr = fmt.Errorf("%w: NetConn got %v, want %v", errUnexpectedFrame, typ, nc.messageType)
			_ = nc.c.CloseWithStatus(StatusUnsupportedData, "")
			return 0, nc.readErr
		}
		nc.pending = data
	}
	n := copy(p, nc.pending)
	nc.pending = nc.pending[n:]
	return n, nil
}

// Write sends p as one message. An empty p sends nothing.
func (nc *netConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := nc.c.WriteMessage(nc.messageType, p); err != nil {
		return 0, netConnError(err)
	}
	return len(p), nil
}

// Close stops watching the context and closes the WebSocket with
// StatusNormalClosure.
func (nc *netConn) Close() error {
	nc.stop()
	return nc.c.Close()
}

func (nc *netConn) LocalAddr() net.Addr                { return nc.c.LocalAddr() }
func (nc *netConn) RemoteAddr() net.Addr               { return nc.c.RemoteAddr() }
func (nc *netConn) SetDeadline(t time.Time) error      { return nc.c.SetDeadline(t) }
func (nc *netConn) SetReadDeadline(t time.Time) error  { return nc.c.SetReadDeadline(t) }
func (nc *netConn) SetWriteDeadline(t time.Time) error { return nc.c.SetWriteDeadline(t) }

// netConnError maps Conn errors onto net.Conn conventions: a peer's clean
// close becomes the bare io.EOF that stream readers such as io.Copy compare
// against with ==. Other errors, including abnormal closes and timeouts, are
// unchanged; a closed connection's errors already match net.ErrClosed.
func netConnError(err error) error {
	if ce, ok := errors.AsType[*CloseError](err); ok {
		switch ce.Code {
		case StatusNormalClosure, StatusGoingAway, StatusNoStatusReceived:
			return io.EOF
		}
		return err
	}
	return err
}
