package websocket

import (
	"bufio"
	"crypto/tls"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultMaxMessageSize is the incoming payload limit used when a Dialer,
// Upgrader or ConnConfig leaves MaxMessageSize zero. It bounds each data frame
// and the reassembled decoded message, not total memory, connection lifetime,
// or outgoing messages.
const DefaultMaxMessageSize = 1 << 20

// DefaultMaxResponseHeaderBytes is Dial's HTTP response-head byte limit when
// Dialer.MaxResponseHeaderBytes is zero. It includes the status line, all
// header lines and their separators, and the final empty line.
const DefaultMaxResponseHeaderBytes = 1 << 20

// MessageType is the type of a WebSocket data message: TextMessage or
// BinaryMessage. Its values are the frame opcodes that start each kind of
// message.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-5.6
type MessageType int

const (
	TextMessage   MessageType = 1 // UTF-8 text (RFC 6455 Section 5.6)
	BinaryMessage MessageType = 2 // arbitrary bytes (RFC 6455 Section 5.6)
)

// String returns "TextMessage" or "BinaryMessage", or "MessageType(n)" for
// any other value.
func (t MessageType) String() string {
	switch t {
	case TextMessage:
		return "TextMessage"
	case BinaryMessage:
		return "BinaryMessage"
	default:
		return "MessageType(" + strconv.Itoa(int(t)) + ")"
	}
}

// Conn is a WebSocket connection, created by Dial, Upgrade or NewConn.
//
// One goroutine may read at a time; concurrent reads are serialized and
// interleave messages unpredictably, so use a single read loop. Any number of
// goroutines may write: each WriteMessage sends its whole message before the
// next starts. Close, CloseWithStatus, CloseWrite, Shutdown, Ping and the
// deadline setters may be called concurrently with reads and writes. Deadlines
// are shared transport state, so goroutines that set them must coordinate.
type Conn struct {
	conn     net.Conn      // Underlying network connection
	br       *bufio.Reader // Buffered reader; writes borrow a pooled buffer per frame (see writeFrameBuffer)
	isServer bool          // True if this Conn is on the server side
	maxBytes int           // Maximum message size in bytes

	subprotocol string // Negotiated Sec-WebSocket-Protocol; immutable after the handshake

	readMu  sync.Mutex // Protects read operations
	writeMu sync.Mutex // Protects write operations
	closeMu sync.Mutex // Protects shutdown reservation; never held during I/O

	closed    atomic.Bool    // Indicates if the connection is closed
	failed    bool           // Protected by closeMu; failure permits concurrent abort
	closeSent bool           // Protected by writeMu; forbids later data frames
	peerClose *CloseError    // Protected by readMu; the peer's close frame, once received
	reader    *messageReader // Protected by readMu; the unfinished NextReader message
	writeErr  error          // Protected by writeMu; first transport write failure
	abortConn net.Conn       // Raw transport for abnormal shutdown (never protocol I/O)
	abortOnce sync.Once      // Independent of normal, potentially graceful TLS Close

	// peerClosed reports, to goroutines other than the reader, that the
	// peer's Close frame arrived. It is set before the connection is closed.
	peerClosed atomic.Bool
	// shuttingDown makes ReadMessage discard data messages once Shutdown
	// has started.
	shuttingDown atomic.Bool
	// localClosed records that Close or CloseWithStatus was called, so a
	// read it interrupts reports net.ErrClosed even when the peer's reply
	// to the Close frame arrives first.
	localClosed atomic.Bool

	// testHookControl, when set by in-package tests, sees each Ping and Pong
	// before ReadMessage handles it, and skips that handling (the Pong reply
	// or Ping match) by returning true.
	testHookControl func(op opcode, payload []byte) (handled bool)

	// Extensions
	extensions []frameExtension

	// Reusable buffers for compressed messages. Protected by writeMu.
	writeBuf, writeScratch []byte

	// dataSlot admits one data message at a time, from WriteMessage or for a
	// NextWriter's whole lifetime. It is taken before writeMu.
	dataOnce sync.Once
	dataSlot chan struct{}

	// Ping bookkeeping. done is created lazily and closed, under closeMu, once
	// the connection becomes terminal; pings maps outstanding Ping payloads to
	// the channel their matching Pong closes.
	done    chan struct{}
	doneSet bool
	pingSeq atomic.Uint64
	pingMu  sync.Mutex
	pings   map[string]chan struct{}
}

// newConn creates a Conn whose read buffer br may already hold bytes read past
// the handshake. br must read from conn. maxBytes 0 means no limit.
func newConn(conn net.Conn, br *bufio.Reader, isServer bool, extensions []frameExtension, maxBytes int) *Conn {
	wsConn := &Conn{
		conn:       conn,
		br:         br,
		isServer:   isServer,
		maxBytes:   maxBytes,
		extensions: extensions,
	}

	for _, ext := range extensions {
		if pmd, ok := ext.(*perMessageDeflate); ok {
			pmd.server = isServer
		}
	}

	wsConn.abortConn = conn
	if tlsConn, ok := conn.(*tls.Conn); ok {
		wsConn.abortConn = tlsConn.NetConn()
	}

	return wsConn
}

// SetDeadline sets the underlying transport's read and write deadlines,
// including currently blocked I/O. A zero value clears both deadlines. It
// returns the transport's error unchanged and may run concurrently with reads,
// writes and Close. Callers must coordinate updates to the shared deadline
// state.
//
// Deadlines bound transport I/O, not an entire method call: already buffered
// data may still be consumed, and compression work and lock waits are not
// preempted. ReadMessage may write automatic Pong responses, so bounding both
// directions may be necessary. A read timeout is terminal. Close after a write
// timeout; a partially written frame cannot be retried safely. Normal Close may
// replace the write deadline with its own one-second deadline; failure cleanup
// preserves caller deadlines.
func (c *Conn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

// SetReadDeadline sets the underlying transport's deadline for future and
// currently blocked reads. A zero value clears it. Like SetDeadline, it returns
// the transport's error unchanged and may run concurrently with I/O and Close.
// A read timeout makes the connection terminal; clearing the deadline afterward
// does not make it reusable. This does not bound ReadMessage's automatic Pong
// writes; set an appropriate write deadline too, or use SetDeadline.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// SetWriteDeadline sets the underlying transport's deadline for future and
// currently blocked writes, including automatic control replies. A zero value
// clears it. Like SetDeadline, it returns the transport's error unchanged and
// may run concurrently with I/O and Close. Close after a write timeout; a
// partially written frame cannot be retried safely. Normal Close may replace
// this deadline with its own one-second deadline.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

// Close closes the transport, making a best-effort StatusNormalClosure
// notification with a one-second write deadline when no other writer is active,
// replacing any write deadline set by the caller. It does not wait for the
// peer's closing handshake. Reads, writes and Pings after Close, including
// calls it interrupts, return errors matching net.ErrClosed; repeated calls to
// Close return one too. Custom transport Close implementations can still block.
//
// Use CloseWithStatus to send another status code or a reason, and Shutdown
// to wait for the peer's reply before closing the transport.
func (c *Conn) Close() error {
	return c.CloseWithStatus(StatusNormalClosure, "")
}

func (c *Conn) closeWithPayload(payload []byte) error {
	c.closeMu.Lock()
	c.closeCompression()
	alreadyClosed := c.closed.Swap(true)
	c.markDoneLocked()
	if alreadyClosed {
		failed := c.failed
		c.closeMu.Unlock()
		if failed {
			c.abort()
		}
		return errAlreadyClosed
	}
	c.closeMu.Unlock()

	// Do not wait behind a stalled writer. Reserve the only close attempt
	// before writing, and never reuse a stream after a partial write.
	if c.writeMu.TryLock() {
		if !c.closeSent && c.writeErr == nil {
			// Query extensions only when this call will write. A reentrant
			// Close from an extension callback, or one after a write failure,
			// does not.
			var buf [4]frameExtension
			enabled, err := enabledExtensions(c.extensions, buf[:0])
			if err != nil {
				// An unsupported composition must not emit a Close frame that
				// bypasses the extension chain. Abort the transport instead.
				c.writeMu.Unlock()
				c.closeMu.Lock()
				c.failed = true
				c.closeMu.Unlock()
				c.abort()
				return err
			}
			c.closeSent = true
			if err := c.conn.SetWriteDeadline(time.Now().Add(time.Second)); err == nil {
				_ = c.writeFrameExtensions(&wireFrame{Final: true, Opcode: opClose, Payload: payload, Masked: !c.isServer}, enabled)
			}
		}
		c.releaseSendCompression()
		c.writeMu.Unlock()
	}

	// Ordinary and valid-peer closure retain TLS close_notify behavior.
	// This must not share abortOnce: concurrent failure must interrupt it.
	return c.conn.Close()
}
