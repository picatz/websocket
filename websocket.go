package websocket

import (
	"bufio"
	"compress/flate"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
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

// opcode is a WebSocket frame opcode.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-5.2
type opcode int

const (
	opContinuation opcode = 0x0
	opText         opcode = 0x1
	opBinary       opcode = 0x2
	opClose        opcode = 0x8
	opPing         opcode = 0x9
	opPong         opcode = 0xA
)

func (o opcode) String() string {
	switch o {
	case opContinuation:
		return "continuation"
	case opText:
		return "text"
	case opBinary:
		return "binary"
	case opClose:
		return "close"
	case opPing:
		return "ping"
	case opPong:
		return "pong"
	default:
		return "opcode(" + strconv.Itoa(int(o)) + ")"
	}
}

// wireFrame is one WebSocket frame as seen by a frameExtension.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-5.2
type wireFrame struct {
	Final   bool    // FIN: this is the last frame of a message
	Opcode  opcode  // frame type; opContinuation for later fragments
	Payload []byte  // application data, unmasked
	Masked  bool    // whether the frame is masked on the wire (client to server)
	MaskKey [4]byte // masking key used when Masked is true
	Rsv1    bool    // RSV1, owned by permessage-deflate
	Rsv2    bool    // RSV2, available to other extensions
	Rsv3    bool    // RSV3, available to other extensions
}

// frameExtension is a WebSocket extension negotiated during the opening
// handshake and applied to each frame. newPerMessageDeflate returns the
// built-in compression extension; the others exist only in tests. Each
// connection needs its own instance.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-9
type frameExtension interface {
	// Name returns the extension name.
	Name() string
	// Offer returns the extension offer string for Dial, or the selected
	// response after successful negotiation in Upgrade. Offers may contain
	// comma-separated alternatives. Both paths validate the returned syntax.
	Offer() string
	// Negotiate receives the peer's complete, comma-joined extension header, or
	// an empty string if it is absent. Dial and Upgrade validate its syntax and
	// reject unoffered selections. The extension must validate its own
	// parameters and repetition semantics, and disable itself if not
	// negotiated.
	Negotiate(response string) error
	// ProcessOutgoingFrame allows the extension to modify outgoing frames.
	ProcessOutgoingFrame(frame *wireFrame) error
	// ProcessIncomingFrame allows the extension to modify incoming frames.
	// It must clear the reserved bits whose semantics it has consumed.
	ProcessIncomingFrame(frame *wireFrame) error
	// IsEnabled indicates whether the extension is enabled.
	IsEnabled() bool
}

// perMessageDeflate implements the permessage-deflate extension (RFC 7692).
// Configuration, the actual client offer, and the agreed response are distinct.
type perMessageDeflate struct {
	enabled bool
	server  bool

	configured pmdParameters
	offered    pmdParameters
	offerSet   bool
	negotiated pmdParameters

	level       int // Compression level, as in compress/flate.
	threshold   int // Messages shorter than this are sent uncompressed.
	memoryLimit int // Bytes of state kept between messages; 0 is unlimited.

	receiveMu     sync.Mutex // Receive serialization; never acquired by Close or send.
	receiveLife   sync.Mutex // Short state lifetime guard; never held during I/O.
	receive       *deflateReceiveState
	receiveClosed bool

	send     deflateSender // Used with the connection's write lock held.
	sendInit bool
}

func (pmd *perMessageDeflate) IsEnabled() bool {
	return pmd.enabled
}

// ProcessOutgoingFrame compresses one complete Text or Binary message. Conn
// does not call it: it streams messages through the same compressor, across
// as many frames as needed.
func (pmd *perMessageDeflate) ProcessOutgoingFrame(frame *wireFrame) error {
	if !pmd.enabled {
		return nil
	}
	if frame.Opcode == opContinuation || ((frame.Opcode == opText || frame.Opcode == opBinary) && !frame.Final) {
		return errFragmentedCompression
	}
	if frame.Opcode != opText && frame.Opcode != opBinary {
		return nil
	}
	s := pmd.sender()
	s.begin()
	frame.Payload = s.end(s.write(nil, frame.Payload))
	frame.Rsv1 = true
	return nil
}

func (pmd *perMessageDeflate) ProcessIncomingFrame(frame *wireFrame) error {
	return pmd.processIncomingFrame(frame, 0)
}

// sender configures the send state from the negotiated parameters on first
// use. Negotiation always completes before I/O begins.
func (pmd *perMessageDeflate) sender() *deflateSender {
	if !pmd.sendInit {
		pmd.sendInit = true
		pmd.send = deflateSender{bits: pmd.localWindowBits(), level: pmd.level, retention: retainCompressor}
		if pmd.memoryLimit > 0 {
			// A sender may always use a smaller window than agreed and may
			// always forgo context takeover (RFC 7692 section 7.2.1).
			if bits, ok := pmd.budgetWindowBits(); ok {
				pmd.send.bits = min(pmd.send.bits, bits)
				pmd.send.retention = retainHistory
			} else {
				pmd.send.retention = retainNothing
			}
		}
		if pmd.localNoContextTakeover() {
			pmd.send.retention = retainNothing
		}
	}
	return &pmd.send
}

// pmdOption configures newPerMessageDeflate. Compression is the public form.
type pmdOption func(*perMessageDeflate)

// withClientNoContextTakeover sets the client_no_context_takeover option.
func withClientNoContextTakeover() pmdOption {
	return func(pmd *perMessageDeflate) {
		pmd.configured.clientNoContextTakeover = true
	}
}

// withServerNoContextTakeover sets the server_no_context_takeover option.
func withServerNoContextTakeover() pmdOption {
	return func(pmd *perMessageDeflate) {
		pmd.configured.serverNoContextTakeover = true
	}
}

// withClientMaxWindowBits sets the client_max_window_bits option: the base-2
// logarithm of the LZ77 window a client compresses with, from 8 to 15.
//
// For Dial it is offered as the largest window the client will use; a server
// may select a smaller one. For Upgrade it limits the client's window when the
// client's offer permits that, and such offers are declined otherwise. Values
// outside 8-15 are ignored.
//
// https://www.rfc-editor.org/rfc/rfc7692#section-7.1.2.2
func withClientMaxWindowBits(bits int) pmdOption {
	return func(pmd *perMessageDeflate) {
		if bits >= 8 && bits <= 15 {
			pmd.configured.clientMaxWindowBits = bits
		}
	}
}

// withServerMaxWindowBits sets the server_max_window_bits option: the base-2
// logarithm of the LZ77 window a server compresses with, from 8 to 15.
//
// For Dial it is a constraint the server must accept. For Upgrade the server
// selects this window, or a smaller one the client requests. Smaller windows
// use less memory per connection and compress less. Values outside 8-15 are
// ignored.
//
// https://www.rfc-editor.org/rfc/rfc7692#section-7.1.2.1
func withServerMaxWindowBits(bits int) pmdOption {
	return func(pmd *perMessageDeflate) {
		if bits >= 8 && bits <= 15 {
			pmd.configured.serverMaxWindowBits = bits
		}
	}
}

// withCompressionLevel sets the DEFLATE level used to compress outgoing
// messages, from compress/flate.HuffmanOnly (-2) to
// compress/flate.BestCompression (9). compress/flate.DefaultCompression (-1) is
// the default. Other values are ignored. The level is independent of the window
// size.
func withCompressionLevel(level int) pmdOption {
	return func(pmd *perMessageDeflate) {
		if level >= flate.HuffmanOnly && level <= flate.BestCompression {
			pmd.level = level
		}
	}
}

// withCompressionThreshold sends messages shorter than n bytes uncompressed.
// Compressing a tiny message costs CPU and can make it larger; RFC 7692
// section 6 lets a sender choose per message. Uncompressed messages do not
// enter the compression context. The default, 0, compresses every message.
// Negative values are ignored.
//
// https://www.rfc-editor.org/rfc/rfc7692#section-6
func withCompressionThreshold(n int) pmdOption {
	return func(pmd *perMessageDeflate) {
		if n >= 0 {
			pmd.threshold = n
		}
	}
}

// withCompressionMemoryLimit bounds the compression state one connection keeps
// between messages to n bytes: half for the history it receives with, half for
// the history it sends with. Buffers used while a message is being encoded or
// decoded come from pools shared by all connections and are not counted.
//
// The limit is enforced through negotiation (RFC 7692 section 7.1). Each half
// selects the largest window of 2^8 to 2^15 bytes that fits: a client offers
// server_max_window_bits and a client_max_window_bits hint, and a server
// responds with server_max_window_bits and, when the client offered it,
// client_max_window_bits. Where a window cannot fit, or the peer's window
// cannot be bounded, the corresponding no_context_takeover parameter is used
// instead. A sender with a limit keeps only its window of history and
// re-indexes it for each message, trading CPU for memory; without a limit it
// keeps a whole compressor, which is faster but uses 0.5 to 1 MiB per
// connection.
//
// The default, [DefaultCompressionMemoryLimit], keeps full 2^15-byte windows,
// so it changes no negotiated parameter, only what the sender retains. Pass 0
// to remove the limit. Negative values are ignored.
func withCompressionMemoryLimit(n int) pmdOption {
	return func(pmd *perMessageDeflate) {
		if n >= 0 {
			pmd.memoryLimit = n
		}
	}
}

// DefaultCompressionMemoryLimit is the default [Compression.MemoryLimit]:
// about 64 KiB of state per idle connection, the two 32 KiB histories that
// context takeover with the largest window needs (RFC 7692 section 7.1.1).
const DefaultCompressionMemoryLimit = 64 << 10

// newPerMessageDeflate creates an opt-in permessage-deflate (RFC 7692)
// compression extension. Conn compresses and decompresses whole messages,
// fragmented or not, with context takeover unless a no_context_takeover
// parameter is negotiated, and with any negotiated window from 8 to 15 bits.
//
// Conn applies other enabled extensions to each frame after compression on
// send and before decompression on receive. They may use RSV2 and RSV3, but
// RSV1 belongs to permessage-deflate (RFC 7692 section 5); a frame whose RSV1
// another extension changed fails with errUnsupportedExtensionComposition,
// as does enabling more than one permessage-deflate instance.
//
// Each connection needs its own instance; Compression creates one per
// connection. Direct callbacks require complete messages; do not mix them
// concurrently with Conn I/O. Configuration and negotiation must finish before
// I/O; concurrent reconfiguration is not supported.
//
// https://www.rfc-editor.org/rfc/rfc7692
func newPerMessageDeflate(options ...pmdOption) frameExtension {
	pmd := &perMessageDeflate{level: flate.DefaultCompression, memoryLimit: DefaultCompressionMemoryLimit}
	for _, option := range options {
		option(pmd)
	}
	return pmd
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

// readFailure attaches status only at library-owned validation sites. Removing
// it at the public boundary preserves existing sentinel equality and
// diagnostics. Arbitrary callback errors must never acquire status through
// errors.Is alone.
type readFailure struct {
	cause error
	code  StatusCode
}

func (e *readFailure) Error() string { return e.cause.Error() }
func (e *readFailure) Unwrap() error { return e.cause }

func failWith(code StatusCode, cause error) error { return &readFailure{cause: cause, code: code} }

func (c *Conn) abort() {
	c.closeCompression()
	c.abortOnce.Do(func() { _ = c.abortConn.Close() })
}

// failRead publishes terminal state before attempting notification. The
// original error always wins over cleanup errors, including timeout and partial
// writes.
func (c *Conn) failRead(err error) error {
	var code StatusCode
	if failure, ok := err.(*readFailure); ok {
		code, err = failure.code, failure.cause
	}
	c.closeMu.Lock()
	c.closeCompression()
	alreadyClosed := c.closed.Swap(true)
	c.markDoneLocked()
	c.failed = true
	c.closeMu.Unlock()
	if alreadyClosed || code == 0 || !c.failureCloseSafe() || !c.writeMu.TryLock() {
		c.abort()
		return err
	}
	defer c.writeMu.Unlock()
	if c.closeSent || c.writeErr != nil {
		c.abort()
		return err
	}
	c.closeSent = true

	// No net.Conn deadline getter exists. A watchdog bounds notification
	// without extending or clearing an earlier deadline set by the caller.
	// The write stays synchronous; the callback only aborts transport I/O.
	// Only this owner stops/joins it, even when concurrent Close aborts early.
	done := make(chan struct{})
	timer := time.AfterFunc(time.Second, func() {
		c.abort()
		close(done)
	})
	var payload [2]byte
	binary.BigEndian.PutUint16(payload[:], uint16(code))
	_ = c.writeFrameRaw(&wireFrame{Final: true, Opcode: opClose, Payload: payload[:], Masked: !c.isServer})
	c.releaseSendCompression()
	c.abort()
	if !timer.Stop() {
		<-done
	}
	return err
}

// The built-in extension leaves control frames unchanged. Custom extensions
// have no interruptible, safe control-encoding contract; do not reenter their
// outgoing callbacks or guess an encoding during automatic failure cleanup.
func (c *Conn) failureCloseSafe() bool {
	for _, ext := range c.extensions {
		if _, builtin := ext.(*perMessageDeflate); !builtin && ext.IsEnabled() {
			return false
		}
	}
	return true
}

// ReadMessage reads the next complete WebSocket message. EOF before a message
// begins is reported as io.EOF; a truncated frame or fragmented message returns
// an error matching io.ErrUnexpectedEOF. A peer close frame returns a
// *CloseError carrying the peer's status code and reason, which matches io.EOF
// only for a normal close. A header that already proves a protocol or size
// violation is rejected without waiting for its payload, even when that payload
// is absent or truncated. Invalid text is rejected as its payload arrives. A
// compressed message is validated as it is decoded, but Go's inflater may
// buffer up to 32 KiB before exposing output, so invalid text or a small
// decoded overflow can remain hidden while waiting for compressed input. An
// incomplete rune may span reads, frames, controls, and DEFLATE streams. Size
// limits do not bound CPU time. Any error makes the connection terminal: later
// reads and writes fail. Known protocol and size violations send an appropriate
// best-effort Close when safe, then abort the transport. Notification uses at
// most one second without changing caller deadlines. A custom transport's Close
// implementation can still block. Once Shutdown has started, ReadMessage
// discards data messages and returns only the peer's *CloseError or an error. A
// read interrupted by Close or CloseWithStatus returns an error matching
// net.ErrClosed, even if the peer's answer to the Close frame arrived first.
func (c *Conn) ReadMessage() (messageType MessageType, data []byte, err error) {
	for {
		op, data, err := c.readMessage()
		if err != nil || !c.shuttingDown.Load() {
			// The peer may have answered our Close frame before the
			// transport closed; readError reports that as net.ErrClosed.
			return MessageType(op), data, c.readError(err)
		}
		// Shutdown has started: drop the message and keep reading until the
		// peer's Close frame or an error.
	}
}

// readMessage reads one complete message for ReadMessage.
func (c *Conn) readMessage() (messageType opcode, data []byte, err error) {
	if c.closed.Load() {
		return 0, nil, errClosed
	}

	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.closed.Load() {
		return 0, nil, errClosed
	}
	if err := c.discardReader(); err != nil {
		return 0, nil, err
	}
	return c.readMessageLocked()
}

// readMessageLocked is ReadMessage after readMu is held and any NextReader
// message has been discarded.
func (c *Conn) readMessageLocked() (messageType opcode, data []byte, err error) {
	pmd, compositionErr := messageCompression(c.extensions)
	if compositionErr != nil {
		return 0, nil, c.failRead(compositionErr)
	}
	peerClosed := false
	defer func() {
		if err != nil && !peerClosed {
			err = c.failRead(err)
		}
	}()

	var message []byte
	var validated int // Complete UTF-8 bytes; at most three bytes remain pending.
	var messageTypeSet bool
	var frame wireFrame
	for {
		payloadLen, err := c.readFrameHeader(&frame)
		if err == nil {
			err = c.validateMessageHeader(&frame, payloadLen, messageTypeSet, len(message))
		}
		if err == nil && pmd != nil && frame.Rsv1 && (frame.Opcode == opText || frame.Opcode == opBinary) {
			data, peerClosed, err = c.readCompressedMessage(pmd, &frame, payloadLen)
			if peerClosed {
				return 0, nil, c.peerClose
			}
			if err != nil {
				return 0, nil, err
			}
			return frame.Opcode, data, nil
		}
		var checkedText, inPlace bool
		var pending int
		if err == nil {
			// Extensions may transform payload, opcode, or FIN. Even registered
			// disabled extensions keep the complete-frame, post-decode path.
			checkedText = len(c.extensions) == 0 && (frame.Opcode == opText ||
				(frame.Opcode == opContinuation && messageType == opText))
			// Without extensions, validateMessageHeader has already checked the
			// message state and size, so a data frame's payload can be read
			// straight onto the end of the message.
			inPlace = len(c.extensions) == 0 && frame.Opcode < opClose
			var dst []byte
			if inPlace {
				dst = message
			}
			var buf []byte
			buf, pending, err = c.readFramePayloadText(&frame, payloadLen, checkedText, message[validated:], dst)
			if err == nil && inPlace {
				message = buf
			}
		}
		if err != nil {
			if messageTypeSet && err == io.EOF {
				return 0, nil, io.ErrUnexpectedEOF
			}
			return 0, nil, err
		}

		switch frame.Opcode {
		case opText, opBinary:
			if messageTypeSet {
				return 0, nil, failWith(StatusProtocolError, errUnexpectedFrame)
			}
			messageType = frame.Opcode
			messageTypeSet = true

			if !inPlace {
				if c.maxBytes > 0 && len(frame.Payload) > c.maxBytes-len(message) {
					return 0, nil, failWith(StatusMessageTooBig, ErrMessageTooLarge)
				}
				message = append(message, frame.Payload...)
			}
			if messageType == opText {
				valid := true
				if checkedText {
					validated = len(message) - pending
				} else {
					var n int
					n, valid = validUTF8Prefix(message[validated:])
					validated += n
				}
				if !valid || (frame.Final && validated != len(message)) {
					return 0, nil, failWith(StatusInvalidFramePayloadData, errInvalidFrame)
				}
			}
			if frame.Final {
				return messageType, message, nil
			}

		case opContinuation:
			if !messageTypeSet {
				return 0, nil, failWith(StatusProtocolError, errUnexpectedContinuation)
			}
			if !inPlace {
				if c.maxBytes > 0 && len(frame.Payload) > c.maxBytes-len(message) {
					return 0, nil, failWith(StatusMessageTooBig, ErrMessageTooLarge)
				}
				message = append(message, frame.Payload...)
			}
			if messageType == opText {
				valid := true
				if checkedText {
					validated = len(message) - pending
				} else {
					var n int
					n, valid = validUTF8Prefix(message[validated:])
					validated += n
				}
				if !valid || (frame.Final && validated != len(message)) {
					return 0, nil, failWith(StatusInvalidFramePayloadData, errInvalidFrame)
				}
			}
			if frame.Final {
				return messageType, message, nil
			}

		case opClose, opPing, opPong:
			peerClosed, err = c.handleControl(&frame)
			if err != nil {
				return 0, nil, err
			}
			if peerClosed {
				return 0, nil, c.peerClose
			}

		default:
			return 0, nil, failWith(StatusProtocolError, invalidOpcode(frame.Opcode))
		}
	}
}

// readCompressedMessage decodes a permessage-deflate message whose first
// frame header has been read. It reports whether a peer Close interrupted it.
// It is separate from ReadMessage so that the state the decoder shares stays
// off ReadMessage's frame for uncompressed messages.
func (c *Conn) readCompressedMessage(pmd *perMessageDeflate, frame *wireFrame, payloadLen uint64) (data []byte, peerClosed bool, err error) {
	source := &deflateFrameReader{c: c, peerClosed: &peerClosed, first: true}
	for _, ext := range c.extensions {
		if _, builtin := ext.(*perMessageDeflate); !builtin && ext.IsEnabled() {
			source.frameExts = append(source.frameExts, ext)
		}
	}
	if err := source.start(frame, payloadLen); err != nil {
		return nil, peerClosed, err
	}
	data, err = pmd.decodeMessage(&deflateMessageInput{source: source, peerClosed: &peerClosed}, c.maxBytes, frame.Opcode == opText)
	return data, peerClosed, err
}

// validUTF8Prefix validates p, allowing only a potentially valid incomplete
// rune at its end. It returns the number of complete bytes. ReadMessage keeps
// those trailing bytes in its existing message buffer. Each complete prefix is
// validated once, with at most three pending bytes reconsidered per fragment
// and no additional copying.
func validUTF8Prefix(p []byte) (int, bool) {
	n := len(p)
	if n > 0 {
		start := n - 1
		for start > 0 && n-start < utf8.UTFMax && !utf8.RuneStart(p[start]) {
			start--
		}
		// FullRune also reports true for invalid encodings, including invalid
		// short prefixes (such as ED A0). Only a prefix that could become a
		// valid rune is left pending for the next fragment.
		if !utf8.FullRune(p[start:]) {
			n = start
		}
	}
	return n, utf8.Valid(p[:n])
}

// WriteMessage writes a WebSocket message. Transport errors wrap the
// underlying error for errors.Is and errors.As. Close the connection after a
// transport error; the transport is not closed automatically, and clearing a
// write deadline cannot make a partially written message safe to retry. Later
// writes fail with the original cause, unless argument validation or a closed
// connection takes precedence. Argument validation before transport I/O does
// not poison writes.
func (c *Conn) WriteMessage(messageType MessageType, data []byte) error {
	if messageType != TextMessage && messageType != BinaryMessage {
		return errInvalidMessageType
	}
	if messageType == TextMessage && !utf8.Valid(data) {
		return errInvalidUTF8
	}
	if c.closed.Load() {
		return errClosed
	}
	if err := c.acquireData(); err != nil {
		return err
	}
	defer c.releaseData()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() || c.closeSent {
		return errClosed
	}

	if c.writeErr != nil {
		return fmt.Errorf("%w: failed to write frame header: %w", errWriteFailed, c.writeErr)
	}
	var buf [4]frameExtension
	enabled, err := enabledExtensions(c.extensions, buf[:0])
	if err != nil {
		return c.failRead(err)
	}
	for _, ext := range enabled {
		if _, ok := ext.(*perMessageDeflate); ok {
			w := c.newMessageWriter(opcode(messageType), enabled)
			if _, err := w.Write(data); err != nil {
				return err
			}
			return w.Close()
		}
	}
	frame := &wireFrame{
		Final:   true,
		Opcode:  opcode(messageType),
		Payload: data,
		Masked:  !c.isServer,
	}
	return c.writeFrameExtensions(frame, enabled)
}

// writeControlFrame writes a control frame to the connection for Ping,
// CloseWrite, Shutdown and the automatic Pong reply. A successful Close frame
// starts the closing handshake: subsequent data, Ping and Close frames return
// an error matching net.ErrClosed, while reads and Pongs remain available until
// the peer closes or Close is called. Call Close to release the transport if
// the peer does not respond; this method does not impose a handshake timeout.
//
// Transport errors wrap the underlying error for errors.Is and errors.As.
// Close after a transport error; the transport is not closed automatically,
// and clearing a write deadline cannot make the frame stream reusable. Later
// writes fail with the original cause, unless argument validation or the
// closing state takes precedence. Argument validation before transport I/O
// does not poison writes.
func (c *Conn) writeControlFrame(opcode opcode, data []byte) error {
	if opcode != opClose && opcode != opPing && opcode != opPong {
		return errInvalidOpcode
	}
	if opcode == opClose {
		if err := validateClosePayload(data); err != nil {
			return err
		}
	}
	if len(data) > 125 {
		return fmt.Errorf("%w: control frame payload too large: %d", errInvalidFrame, len(data))
	}
	if c.closed.Load() {
		return errClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	// After a Close frame only Pongs, which answer the peer's Pings while
	// the connection drains, may follow it.
	if c.closed.Load() || (opcode != opPong && c.closeSent) {
		return errClosed
	}

	frame := &wireFrame{
		Final:   true,
		Opcode:  opcode,
		Payload: data,
		Masked:  !c.isServer,
	}
	if err := c.writeFrame(frame); err != nil {
		return err
	}
	if opcode == opClose {
		c.closeSent = true
	}
	return nil
}

// readFrameHeader reads a frame header into frame, which it resets, and
// validates lengths and frame structure before any body is read. It returns
// the declared payload length. The masking key, if any, is read with the body.
func (c *Conn) readFrameHeader(frame *wireFrame) (uint64, error) {
	// Read the first two bytes of the frame header, which
	// contains the FIN, RSV1, RSV2, RSV3, opcode, and mask bit.
	var header [2]byte
	if err := readSmall(c.br, header[:]); err != nil {
		if err == io.EOF {
			return 0, io.EOF
		}
		return 0, readError("frame header", err)
	}

	b0 := header[0]
	b1 := header[1]

	*frame = wireFrame{
		Final:  (b0 & 0x80) != 0,
		Opcode: opcode(b0 & 0x0F),
		Masked: (b1 & 0x80) != 0,
		Rsv1:   (b0 & 0x40) != 0,
		Rsv2:   (b0 & 0x20) != 0,
		Rsv3:   (b0 & 0x10) != 0,
	}
	payloadLen := uint64(b1 & 0x7F)

	switch frame.Opcode {
	case opContinuation, opText, opBinary, opClose, opPing, opPong:
	default:
		return 0, failWith(StatusProtocolError, errInvalidOpcode)
	}
	if frame.Masked && !c.isServer {
		return 0, failWith(StatusProtocolError, errMaskedFrame)
	}
	if !frame.Masked && c.isServer {
		return 0, failWith(StatusProtocolError, errUnmaskedFrame)
	}

	// Control frames must not be fragmented
	if !frame.Final && frame.Opcode >= 0x8 {
		return 0, failWith(StatusProtocolError, errControlFrameFragment)
	}

	// Control frames must have payload length <= 125
	if frame.Opcode >= 0x8 && payloadLen > 125 {
		return 0, failWith(StatusProtocolError, controlTooLarge(payloadLen))
	}

	// Built-in compression can consume RSV1 only on data message frames.
	// An enabled custom extension owns its reserved-bit semantics, so leave
	// those checks to the post-extension validation below.
	if frame.Rsv1 || frame.Rsv2 || frame.Rsv3 {
		var consumable bool
		for _, ext := range c.extensions {
			if !ext.IsEnabled() {
				continue
			}
			if _, ok := ext.(*perMessageDeflate); !ok ||
				(!frame.Rsv2 && !frame.Rsv3 && (frame.Opcode == opText || frame.Opcode == opBinary)) {
				consumable = true
				break
			}
		}
		if !consumable {
			return 0, failWith(StatusProtocolError, errUnsupportedExtensions)
		}
	}

	// Read extended payload length if necessary
	switch payloadLen {
	case 126:
		var ext [2]byte
		if err := readField(c.br, ext[:], "extended payload length"); err != nil {
			return 0, err
		}
		extLen := binary.BigEndian.Uint16(ext[:])
		if extLen < 126 {
			return 0, failWith(StatusProtocolError, errInvalidFrame)
		}
		payloadLen = uint64(extLen)
	case 127:
		var ext [8]byte
		if err := readField(c.br, ext[:], "extended payload length"); err != nil {
			return 0, err
		}
		extLen := binary.BigEndian.Uint64(ext[:])
		if extLen >= (1<<63) || extLen < 65536 {
			return 0, failWith(StatusProtocolError, errInvalidFrame)
		}
		payloadLen = extLen
	}
	// Validate lengths before converting to int or allocating payload storage.
	if payloadLen > uint64(^uint(0)>>1) ||
		(c.maxBytes > 0 && frame.Opcode < opClose && payloadLen > c.frameLimit()) {
		return 0, failWith(StatusMessageTooBig, ErrMessageTooLarge)
	}

	return payloadLen, nil
}

// frameLimit returns the largest data frame payload readFrameHeader accepts
// when maxBytes is positive. Without compression it is maxBytes itself.
//
// With permessage-deflate any frame may belong to a compressed message, whose
// decoded size is checked against maxBytes as it inflates, so the encoded
// frame may be larger than the message it carries: deflate expands data it
// cannot compress, such as media or encrypted payloads, and browsers compress
// every message. The limit allows for the bound zlib's deflateBound gives for
// any settings, about 13% over maxBytes, plus the flush that ends each
// message, so that no message of at most maxBytes is refused for its encoded
// size. Uncompressed messages are still held to maxBytes exactly by
// validateMessageHeader.
//
// https://github.com/madler/zlib/blob/v1.3.1/deflate.c#L838-L842
func (c *Conn) frameLimit() uint64 {
	n := uint64(c.maxBytes)
	for _, ext := range c.extensions {
		if _, ok := ext.(*perMessageDeflate); ok && ext.IsEnabled() {
			return n + n>>3 + n>>8 + n>>9 + 4 + 6
		}
	}
	return n
}

// validateMessageHeader rejects a frame before its payload is read whenever
// its wire header determines the message state and decoded size. Custom
// extensions may transform both, so their results are still checked by
// ReadMessage after processing.
func (c *Conn) validateMessageHeader(frame *wireFrame, payloadLen uint64, started bool, messageBytes int) error {
	for _, ext := range c.extensions {
		if _, builtin := ext.(*perMessageDeflate); !builtin && ext.IsEnabled() {
			return nil
		}
	}
	switch frame.Opcode {
	case opText, opBinary:
		if started {
			return failWith(StatusProtocolError, errUnexpectedFrame)
		}
	case opContinuation:
		if !started {
			return failWith(StatusProtocolError, errUnexpectedContinuation)
		}
	case opClose:
		if payloadLen == 1 {
			return failWith(StatusProtocolError, errInvalidFrame)
		}
	}
	// Compressed data may shrink after decoding. Continuations are not
	// transformed by the built-in extension. Controls have their own limit
	// and neither consume nor reset a fragmented message's remaining budget.
	if c.maxBytes > 0 && frame.Opcode < opClose && !frame.Rsv1 &&
		payloadLen > uint64(c.maxBytes-messageBytes) {
		return failWith(StatusMessageTooBig, ErrMessageTooLarge)
	}
	return nil
}

// readFrame reads a single frame without reassembled-message state.
func (c *Conn) readFrame() (*wireFrame, error) {
	frame := new(wireFrame)
	payloadLen, err := c.readFrameHeader(frame)
	if err != nil {
		return nil, err
	}
	if err := c.readFramePayload(frame, payloadLen); err != nil {
		return nil, err
	}
	return frame, nil
}

func (c *Conn) readFramePayload(frame *wireFrame, payloadLen uint64) error {
	_, _, err := c.readFramePayloadText(frame, payloadLen, false, nil, nil)
	return err
}

// readFramePayloadText optionally validates an extension-free text payload as
// bytes arrive. tail contains at most three incomplete UTF-8 bytes from the
// previous frame; the returned count carries them across empty continuations.
// The payload is appended to dst, which may be nil; buf is the result and
// frame.Payload its new part.
func (c *Conn) readFramePayloadText(frame *wireFrame, payloadLen uint64, checkText bool, tail, dst []byte) (buf []byte, pending int, err error) {
	if buf, pending, err = c.readFrameBodyText(frame, payloadLen, checkText, tail, dst); err != nil {
		return nil, 0, err
	}
	return buf, pending, c.processIncomingFrame(frame)
}

// readFrameBody reads and unmasks a frame payload without calling extensions.
func (c *Conn) readFrameBody(frame *wireFrame, payloadLen uint64) error {
	_, _, err := c.readFrameBodyText(frame, payloadLen, false, nil, nil)
	return err
}

func (c *Conn) readFrameBodyText(frame *wireFrame, payloadLen uint64, checkText bool, tail, dst []byte) (buf []byte, pending int, err error) {
	if checkText {
		pending = len(tail)
	}
	// Read masking key if necessary
	if frame.Masked {
		if err := readField(c.br, frame.MaskKey[:], "masking key"); err != nil {
			return nil, 0, err
		}
	}

	// Read payload data
	buf = dst
	if payloadLen > 0 {
		r := payloadReader{r: c.br, remaining: int64(payloadLen), key: frame.MaskKey, masked: frame.Masked, text: checkText}
		if checkText {
			r.pending = byte(copy(r.carry[:], tail))
		}
		// A final frame ends its message. Earlier fragments may leave room
		// for later ones, but never past the message size limit.
		limit := 0 // The frame's own end.
		if !frame.Final {
			limit = math.MaxInt
			if c.maxBytes > 0 {
				limit = c.maxBytes
			}
		}
		buf, err = r.readAll(dst, limit)
		pending = int(r.pending)
		if err != nil {
			// Preserve the typed protocol failure for failRead's close mapping.
			if _, invalid := err.(*readFailure); invalid {
				return nil, 0, err
			}
			return nil, 0, readError("payload data", err)
		}
		if uint64(len(buf)-len(dst)) != payloadLen {
			return nil, 0, io.ErrUnexpectedEOF
		}
	}
	frame.Payload = buf[len(dst):]
	return buf, pending, nil
}

// readSmall fills p, which must be no larger than br's buffer, from br. It
// returns io.EOF if no byte was available and io.ErrUnexpectedEOF if only
// some were. Unlike io.ReadFull, it never hands p to an io.Reader, so a
// stack-allocated p stays on the stack.
func readSmall(br *bufio.Reader, p []byte) error {
	b, err := br.Peek(len(p))
	n := copy(p, b)
	_, _ = br.Discard(n)
	if err == io.EOF && n > 0 {
		return io.ErrUnexpectedEOF
	}
	return err
}

// readField reads a fixed-size frame field such as the masking key. A stream
// that ends inside a frame is truncated, so io.EOF becomes
// io.ErrUnexpectedEOF.
func readField(br *bufio.Reader, p []byte, name string) error {
	err := readSmall(br, p)
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return io.ErrUnexpectedEOF
	}
	return readError(name, err)
}

// The read path's error constructors are kept out of line so that their
// formatting state is not part of the read functions' stack frames. A
// connection's goroutine holds those frames for as long as it waits for the
// next message, and they decide whether its stack outgrows the runtime's
// initial size.

//go:noinline
func readError(name string, err error) error {
	return fmt.Errorf("failed to read %s: %w", name, err)
}

//go:noinline
func controlTooLarge(n uint64) error {
	return fmt.Errorf("%w: control frame payload too large: %d", errInvalidFrame, n)
}

//go:noinline
func invalidOpcode(op opcode) error {
	return fmt.Errorf("%w: %d", errInvalidOpcode, op)
}

// processIncomingFrame passes a complete frame through the enabled extensions
// in registration order, then rejects any reserved bit none of them consumed.
func (c *Conn) processIncomingFrame(frame *wireFrame) error {
	for _, ext := range c.extensions {
		if ext.IsEnabled() {
			var err error
			if pmd, ok := ext.(*perMessageDeflate); ok {
				if frame.Rsv1 {
					err = pmd.processIncomingFrame(frame, c.maxBytes)
				}
			} else {
				err = processIncomingFrame(ext, frame)
			}
			if err != nil {
				cause := fmt.Errorf("extension %s failed to process incoming frame: %w", ext.Name(), err)
				if _, builtin := ext.(*perMessageDeflate); builtin && err == ErrMessageTooLarge {
					return failWith(StatusMessageTooBig, cause)
				}
				return cause
			}
		}
	}
	// Extensions consume the reserved bits whose semantics they implement.
	if frame.Rsv1 || frame.Rsv2 || frame.Rsv3 {
		return failWith(StatusProtocolError, errUnsupportedExtensions)
	}
	return nil
}

// processIncomingFrame calls a custom extension with a heap copy of frame,
// so that frames the connection reads can stay on its stack.
func processIncomingFrame(ext frameExtension, frame *wireFrame) error {
	f := *frame
	err := ext.ProcessIncomingFrame(&f)
	*frame = f
	return err
}

// payloadReader reads a single frame's payload without preallocating from its
// wire length. Only newly received bytes are unmasked and, for text, validated.
// The mask phase resets each frame, while carry bridges an incomplete rune
// across frames.
type payloadReader struct {
	r         *bufio.Reader
	remaining int64
	key       [4]byte
	carry     [utf8.UTFMax]byte
	phase     byte
	pending   byte
	masked    bool
	text      bool // Validate UTF-8 as bytes arrive.
}

// minPayloadAlloc is the most payload storage readAll allocates before any of
// a frame's bytes arrive. It matches the connection's read buffer, so a peer
// that declares a large frame and then stalls costs at most twice the memory
// of an idle connection.
const minPayloadAlloc = 4096

// readAll appends the rest of the payload to dst. Storage grows with the
// bytes that have actually arrived, never with the declared length alone:
// each step adds three times what dst already holds, what is already
// buffered, or the rest of the frame up to minPayloadAlloc, whichever is
// most, and never past limit or the payload's end, whichever is later.
// Quadrupling takes half the steps and copies of doubling, while a peer that
// stalls mid-frame still pins at most four times what it sent.
//
// A final frame passes its end as the limit, so a single-frame message has
// no spare capacity and the caller may keep it without trimming. A message's
// earlier frames grow past their own end, up to the message limit, so a long
// run of fragments is not copied once per frame.
func (r *payloadReader) readAll(dst []byte, limit int) ([]byte, error) {
	buf := dst
	end := len(dst) + int(r.remaining)
	limit = max(limit, end)
	for len(buf) < end {
		if len(buf) == cap(buf) {
			size := len(buf) + max(3*len(buf), min(end-len(buf), minPayloadAlloc), r.r.Buffered())
			size = min(size, limit)
			grown := make([]byte, len(buf), size)
			copy(grown, buf)
			buf = grown
		}
		m, err := r.Read(buf[len(buf):min(cap(buf), end)])
		buf = buf[:len(buf)+m]
		if err == io.EOF {
			break
		}
		if err != nil {
			return buf, err
		}
	}
	return buf, nil
}

func (r *payloadReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.r.Read(p)
	r.remaining -= int64(n)
	if n > 0 {
		if r.masked {
			key := [4]byte{r.key[r.phase], r.key[(r.phase+1)&3], r.key[(r.phase+2)&3], r.key[(r.phase+3)&3]}
			xor(key[:], p[:n])
			r.phase = (r.phase + byte(n&3)) & 3
		}
		// Invalid bytes prove a protocol failure even if this read also
		// delivered a transport error. Otherwise preserve that error.
		if r.text && !r.check(p[:n]) {
			return n, failWith(StatusInvalidFramePayloadData, errInvalidFrame)
		}
	}
	return n, err
}

func (r *payloadReader) check(p []byte) bool {
	// Complete the previous chunk's rune with at most three new bytes.
	for r.pending > 0 && len(p) > 0 {
		r.carry[r.pending] = p[0]
		r.pending++
		p = p[1:]
		if utf8.FullRune(r.carry[:r.pending]) {
			if !utf8.Valid(r.carry[:r.pending]) {
				return false
			}
			r.pending = 0
		}
	}
	if r.pending > 0 {
		return true
	}
	n, valid := validUTF8Prefix(p)
	if !valid {
		return false
	}
	r.pending = byte(copy(r.carry[:], p[n:]))
	return true
}

func validateClosePayload(payload []byte) error {
	if err := validateIncomingClosePayload(payload); err != nil {
		return errInvalidFrame
	}
	return nil
}

func validateIncomingClosePayload(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	if len(payload) == 1 {
		return failWith(StatusProtocolError, errInvalidFrame)
	}
	code := binary.BigEndian.Uint16(payload)
	if code < 1000 || code >= 5000 || code == 1004 || code == 1005 || code == 1006 || (code >= 1015 && code < 3000) {
		return failWith(StatusProtocolError, errInvalidFrame)
	}
	if !utf8.Valid(payload[2:]) {
		return failWith(StatusInvalidFramePayloadData, errInvalidFrame)
	}
	return nil
}

// writeFrame writes a WebSocket frame to the connection. writeMu must be held.
func (c *Conn) writeFrame(frame *wireFrame) error {
	if c.writeErr != nil {
		return fmt.Errorf("%w: failed to write frame header: %w", errWriteFailed, c.writeErr)
	}
	// Ask each extension once whether it is enabled. An unsupported composition
	// is terminal and performs no I/O; a write after a transport failure
	// returns above without calling extensions at all.
	var buf [4]frameExtension
	enabled, err := enabledExtensions(c.extensions, buf[:0])
	if err != nil {
		return c.failRead(err)
	}
	return c.writeFrameExtensions(frame, enabled)
}

// writeFrameExtensions passes frame through the already-resolved enabled
// extensions, then writes it. writeMu must be held.
func (c *Conn) writeFrameExtensions(frame *wireFrame, enabled []frameExtension) error {
	if len(enabled) == 0 {
		return c.writeFrameRaw(frame)
	}
	// Extensions get a heap copy, so that callers' frames can stay on their
	// stacks when no extension is enabled.
	f := *frame
	for _, ext := range enabled {
		if err := ext.ProcessOutgoingFrame(&f); err != nil {
			return fmt.Errorf("extension %s failed to process outgoing frame: %w", ext.Name(), err)
		}
	}
	return c.writeFrameRaw(&f)
}

// writeFrameRaw encodes a frame without calling extensions. writeMu must be
// held; any transport failure poisons the stream for later close notification.
func (c *Conn) writeFrameRaw(frame *wireFrame) error {
	return c.writeFrameBuffer(frame, false)
}

// writeFrameBuffer is writeFrameRaw for a payload the connection owns, which
// may then be masked in place instead of copied.
func (c *Conn) writeFrameBuffer(frame *wireFrame, owned bool) error {
	// Every frame is flushed, so no frame leaves bytes behind and the write
	// buffer is borrowed from a pool for one frame instead of being held by
	// every idle connection.
	pool := &writerPool
	if frame.Masked && !owned && len(frame.Payload) > writeBufferSize {
		pool = &maskedWriterPool
	}
	w := pool.Get().(*bufio.Writer)
	w.Reset(c.conn)
	err := c.writeFrameTo(w, frame, owned)
	releaseWriter(pool, w)
	return err
}

// writeFrameTo encodes frame into w, an empty write buffer, and flushes it.
func (c *Conn) writeFrameTo(w *bufio.Writer, frame *wireFrame, owned bool) error {
	b0 := byte(frame.Opcode)
	if frame.Final {
		b0 |= 0x80
	}
	if frame.Rsv1 {
		b0 |= 0x40
	}
	if frame.Rsv2 {
		b0 |= 0x20
	}
	if frame.Rsv3 {
		b0 |= 0x10
	}

	b1 := byte(0)
	if frame.Masked {
		b1 |= 0x80
	}

	// Encode the header straight into the empty write buffer, so a whole
	// small frame reaches the transport in one write.
	header := append(w.AvailableBuffer(), b0, b1)

	// Extended payload length
	payloadLen := len(frame.Payload)
	if payloadLen <= 125 {
		header[1] |= byte(payloadLen)
	} else if payloadLen <= 65535 {
		header[1] |= 126
		header = binary.BigEndian.AppendUint16(header, uint16(payloadLen))
	} else {
		header[1] |= 127
		header = binary.BigEndian.AppendUint64(header, uint64(payloadLen))
	}

	// Masking key
	if frame.Masked {
		header = append(header, 0, 0, 0, 0)
		key := header[len(header)-4:]
		rand.Read(key) // never fails; see crypto/rand.Read
		frame.MaskKey = [4]byte(key)
	}

	// The header fits the empty buffer, so this only commits it.
	_, _ = w.Write(header)

	// Write payload
	var err error
	switch {
	case len(frame.Payload) == 0:
	case !frame.Masked:
		_, err = w.Write(frame.Payload)
	case owned:
		xor(frame.MaskKey[:], frame.Payload)
		_, err = w.Write(frame.Payload)
	default:
		// Never mutate the caller's slice, even temporarily. It may be shared
		// with another connection or being inspected by the application.
		err = writeMasked(w, frame.MaskKey, frame.Payload)
	}
	if err != nil {
		c.writeErr = err
		return fmt.Errorf("%w: failed to write frame payload: %w", errWriteFailed, err)
	}

	// Flush the buffer to ensure the data is sent
	if err := w.Flush(); err != nil {
		c.writeErr = err
		return fmt.Errorf("%w: failed to flush data: %w", errWriteFailed, err)
	}

	return nil
}

// writerPool holds the write buffers connections borrow for one frame at a
// time.
var writerPool = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, writeBufferSize) }}

// writeBufferSize is the size of the buffers in writerPool.
const writeBufferSize = 4 << 10

// maskedWriteBufferSize is the size of the buffers in maskedWriterPool.
const maskedWriteBufferSize = 32 << 10

// maskedWriterPool holds larger write buffers for masking a payload the
// connection does not own and that does not fit writerPool's buffers.
// writeMasked copies such a payload through the buffer, so a 1 MiB client
// message takes 32 writes to the transport instead of 256.
var maskedWriterPool = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, maskedWriteBufferSize) }}

// releaseWriter returns w to pool, discarding anything a failed write left
// buffered and dropping its reference to the connection.
func releaseWriter(pool *sync.Pool, w *bufio.Writer) {
	w.Reset(nil)
	pool.Put(w)
}

// writeMasked writes p masked with key, copying it through w's buffer one
// buffer at a time, so that masking needs no storage of its own.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-5.3
func writeMasked(w *bufio.Writer, key [4]byte, p []byte) error {
	for len(p) > 0 {
		if w.Available() == 0 {
			if err := w.Flush(); err != nil {
				return err
			}
		}
		buf := w.AvailableBuffer()
		n := min(len(p), cap(buf))
		buf = append(buf, p[:n]...)
		xor(key[:], buf)
		if _, err := w.Write(buf); err != nil {
			return err
		}
		p = p[n:]
		// Continue the four-byte mask cycle where this chunk left it.
		key = [4]byte{key[n&3], key[(n+1)&3], key[(n+2)&3], key[(n+3)&3]}
	}
	return nil
}

// dialOptions is a Dialer resolved for one Dial: defaults applied, the
// subprotocol list detached and extensions created for this connection.
type dialOptions struct {
	extensions             []frameExtension
	header                 http.Header
	tlsConfig              *tls.Config
	maxBytes               int
	maxResponseHeaderBytes int64
	netDialContext         func(ctx context.Context, network, addr string) (net.Conn, error)
	proxy                  func(*http.Request) (*url.URL, error)
	subprotocols           []string
}

// dial performs the client handshake for a resolved Dialer.
func dial(ctx context.Context, urlStr string, opts *dialOptions) (ws *Conn, response *http.Response, err error) {
	if err := validateHandshakeHeaders(opts.header, false); err != nil {
		return nil, nil, err
	}
	if opts.subprotocols != nil {
		if err := validateSubprotocolOption(opts.subprotocols, opts.header); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", errInvalidHandshakeHeader, err)
		}
		// Clone so the caller's header map is never modified.
		opts.header = opts.header.Clone()
		if opts.header == nil {
			opts.header = make(http.Header, 1)
		}
		opts.header.Set("Sec-WebSocket-Protocol", strings.Join(opts.subprotocols, ", "))
	}

	offered, err := parseSubprotocolOffers(opts.header)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errInvalidHandshakeHeader, err)
	}

	extOffer, offeredExtensions, err := extensionOffers(opts.extensions)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errInvalidHandshakeHeader, err)
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid URL: %w", ErrInvalidConfig, err)
	}
	if (u.Scheme != "ws" && u.Scheme != "wss") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, nil, fmt.Errorf("%w: URL must use ws or wss, with a host and no user info or fragment", ErrInvalidConfig)
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	host := net.JoinHostPort(u.Hostname(), port)

	// Establish the network connection, directly or to the selected proxy.
	conn, proxyURL, err := dialTransport(ctx, opts, u, host)
	if err != nil {
		return nil, nil, err
	}
	// Closing the raw transport interrupts blocked HTTP reads/writes as well
	// as TLS. Join the callback before returning so it cannot close a live
	// connection after a successful handshake has been handed to the caller.
	rawConn := conn
	// Deferred first so that it runs last, after the callback is joined.
	defer func() {
		if ctx.Err() != nil {
			rawConn.Close()
			ws = nil
			err = fmt.Errorf("%w: %w", ErrBadHandshake, context.Cause(ctx))
		}
	}()
	if ctx.Done() != nil {
		// A context that can never be canceled needs no callback.
		canceled := make(chan struct{})
		stopCancel := context.AfterFunc(ctx, func() {
			rawConn.Close()
			close(canceled)
		})
		defer func() {
			if !stopCancel() {
				<-canceled
			}
		}()
	}

	// Tunnel through the proxy before any end-to-end TLS. The cancellation
	// callback above closes the raw transport, interrupting this exchange too.
	if proxyURL != nil {
		if conn, err = connectProxy(ctx, conn, proxyURL, host, opts); err != nil {
			rawConn.Close()
			return nil, nil, err
		}
	}

	// If using TLS, perform the handshake
	if u.Scheme == "wss" {
		config := &tls.Config{}
		if opts.tlsConfig != nil {
			config = opts.tlsConfig.Clone()
		}
		if config.ServerName == "" {
			config.ServerName = u.Hostname()
		}
		config.NextProtos = []string{"http/1.1"}
		tlsConn := tls.Client(conn, config)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("%w: failed to perform TLS handshake: %w", ErrBadHandshake, err)
		}
		conn = tlsConn
	}

	key := generateKey()

	// Build the request. Offers were validated before connecting, including
	// extension-produced values.
	req := make([]byte, 0, 512)
	req = append(req, "GET "...)
	req = append(req, u.RequestURI()...)
	req = append(req, " HTTP/1.1\r\nHost: "...)
	req = append(req, u.Host...)
	req = append(req, "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: "...)
	req = append(req, key[:]...)
	req = append(req, "\r\nSec-WebSocket-Version: 13\r\n"...)
	if extOffer != "" {
		req = append(req, "Sec-WebSocket-Extensions: "...)
		req = append(req, extOffer...)
		req = append(req, "\r\n"...)
	}
	for k, vs := range opts.header {
		for _, v := range vs {
			req = append(req, k...)
			req = append(req, ": "...)
			req = append(req, v...)
			req = append(req, "\r\n"...)
		}
	}
	req = append(req, "\r\n"...)

	// Send the request
	if _, err = conn.Write(req); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("%w: failed to send handshake request: %w", ErrBadHandshake, err)
	}

	// Read the response
	resp, br, err := readHandshakeResponse(conn, opts.maxResponseHeaderBytes)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("%w: failed to read handshake response: %w", ErrBadHandshake, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		keepRejectedBody(conn, resp)
		conn.Close()
		return nil, resp, fmt.Errorf("%w: unexpected status code %d", ErrBadHandshake, resp.StatusCode)
	}
	if !headerContains(resp.Header, "Connection", "Upgrade") {
		conn.Close()
		return nil, resp, errInvalidConnectionHeader
	}
	if !headerContains(resp.Header, "Upgrade", "websocket") {
		conn.Close()
		return nil, resp, errInvalidUpgradeHeader
	}
	accept := resp.Header["Sec-Websocket-Accept"]
	if len(accept) != 1 {
		conn.Close()
		return nil, resp, errInvalidSecAccept
	}
	if expected := acceptKey(key[:]); accept[0] != string(expected[:]) {
		conn.Close()
		return nil, resp, errInvalidSecAccept
	}

	subprotocol, err := validateSubprotocolSelection(resp.Header, offered)
	if err != nil {
		conn.Close()
		return nil, resp, err
	}

	// Validate the complete selection before calling any extension. In
	// particular, no configured extensions means no selections are allowed.
	extValues := extensionValues(resp.Header)
	selectedExtensions, err := parseExtensions(extValues)
	if err == nil {
		err = validateExtensionSelection(selectedExtensions, offeredExtensions)
	}
	if err != nil {
		conn.Close()
		return nil, resp, err
	}
	extHeader := strings.Join(extValues, ", ")
	for _, ext := range opts.extensions {
		if err := ext.Negotiate(extHeader); err != nil {
			conn.Close()
			return nil, resp, fmt.Errorf("%w: extension %s: %w", errInvalidExtension, ext.Name(), err)
		}
	}

	if _, err := messageCompression(opts.extensions); err != nil {
		conn.Close()
		return nil, resp, fmt.Errorf("%w: %w", errInvalidExtension, err)
	}

	ws = newConn(conn, br, false, opts.extensions, opts.maxBytes)
	ws.subprotocol = subprotocol
	ws.abortConn = rawConn
	return ws, resp, nil
}

// upgradeOptions is an Upgrader resolved for one Upgrade.
type upgradeOptions struct {
	extensions     []frameExtension
	responseHeader http.Header
	maxBytes       int
	originCheck    func(*http.Request, string) bool
	subprotocols   []string
}

// upgrade performs the server handshake for a resolved Upgrader.
func upgrade(w http.ResponseWriter, r *http.Request, opts *upgradeOptions) (*Conn, error) {
	// Each step is its own function so that only one step's locals are on
	// the stack at a time. The handler goroutine keeps whatever stack the
	// handshake grew to for the connection's lifetime.
	acceptKey, subprotocol, err := checkUpgradeRequest(r, opts)
	if err != nil {
		return nil, err
	}
	extensions, err := negotiateUpgradeExtensions(r, opts)
	if err != nil {
		return nil, err
	}

	// Preserve direct Hijacker errors and use the standard controller to reach
	// hijacking support through middleware that exposes Unwrap.
	hj, ok := w.(http.Hijacker)
	if !ok {
		hj = http.NewResponseController(w)
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		if !ok && errors.Is(err, http.ErrNotSupported) {
			return nil, ErrNotHijacker
		}
		return nil, fmt.Errorf("%w: %w", errHandshakeFailed, err)
	}
	if err := writeUpgradeResponse(bufrw.Writer, acceptKey, opts, extensions, subprotocol); err != nil {
		conn.Close()
		return nil, err
	}

	// Keep the hijacked reader, which may already hold the client's first
	// frames. The hijacked writer is released: frames are written through a
	// shared, pooled buffer only while one is being written.
	ws := newConn(conn, bufrw.Reader, true, opts.extensions, opts.maxBytes)
	ws.subprotocol = subprotocol
	return ws, nil
}

// checkUpgradeRequest validates the client's opening handshake and the
// caller's response fields. It returns the Sec-WebSocket-Accept value and the
// selected subprotocol, if any.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-4.2.1
func checkUpgradeRequest(r *http.Request, opts *upgradeOptions) (string, string, error) {
	if err := validateHandshakeHeaders(opts.responseHeader, true); err != nil {
		return "", "", err
	}

	if !headerContains(r.Header, "Connection", "Upgrade") {
		return "", "", errInvalidConnectionHeader
	}
	if !headerContains(r.Header, "Upgrade", "websocket") {
		return "", "", errInvalidUpgradeHeader
	}
	if r.Method != http.MethodGet {
		return "", "", errInvalidMethod
	}
	if !r.ProtoAtLeast(1, 1) {
		return "", "", ErrBadHandshake
	}
	// Header keys are indexed in canonical form directly: Get and Values
	// canonicalize their argument, which allocates for "WebSocket".
	if version := r.Header["Sec-Websocket-Version"]; len(version) != 1 || version[0] != "13" {
		return "", "", ErrUnsupportedVersion
	}
	keys := r.Header["Sec-Websocket-Key"]
	if len(keys) == 0 || keys[0] == "" {
		return "", "", errMissingSecKey
	}
	key := keys[0]
	if len(keys) != 1 || !validChallengeKey(key) {
		return "", "", errInvalidSecKey
	}
	offered, err := parseSubprotocolOffers(r.Header)
	if err != nil {
		return "", "", err
	}
	if opts.subprotocols != nil {
		if err := validateSubprotocolOption(opts.subprotocols, opts.responseHeader); err != nil {
			return "", "", fmt.Errorf("%w: %w", errInvalidHandshakeHeader, err)
		}
	}
	subprotocol, err := validateSubprotocolSelection(opts.responseHeader, offered)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", errInvalidHandshakeHeader, err)
	}
	if subprotocol == "" {
		subprotocol = selectSubprotocol(opts.subprotocols, offered)
	}
	if !upgradeOriginAllowed(r, opts.originCheck) {
		return "", "", ErrOriginNotAllowed
	}
	return computeAcceptKey(key), subprotocol, nil

}

// negotiateUpgradeExtensions negotiates the configured extensions with the
// client's offer and returns the response's Sec-WebSocket-Extensions value,
// or "" when none was accepted.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-9.1
func negotiateUpgradeExtensions(r *http.Request, opts *upgradeOptions) (string, error) {
	extValues := extensionValues(r.Header)
	offeredExtensions, err := parseExtensions(extValues)
	if err != nil {
		return "", err
	}
	extHeader := strings.Join(extValues, ", ")
	var acceptedExtensions []frameExtension
	for _, ext := range opts.extensions {
		if ext == nil {
			return "", fmt.Errorf("%w: nil extension", errInvalidExtension)
		}
		var err error
		if pmd, ok := ext.(*perMessageDeflate); ok {
			err = pmd.negotiate(offeredExtensions, true)
		} else {
			err = ext.Negotiate(extHeader)
		}
		if err != nil {
			return "", fmt.Errorf("%w: extension %s: %w", errInvalidExtension, ext.Name(), err)
		}
		if ext.IsEnabled() {
			acceptedExtensions = append(acceptedExtensions, ext)
		}
	}
	if _, err := messageCompression(opts.extensions); err != nil {
		return "", fmt.Errorf("%w: %w", errInvalidExtension, err)
	}
	selection, selectedExtensions, err := extensionOffers(acceptedExtensions)
	if err == nil {
		err = validateExtensionSelection(selectedExtensions, offeredExtensions)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", errInvalidHandshakeHeader, err)
	}
	return selection, nil
}

// writeUpgradeResponse writes and flushes the 101 Switching Protocols
// response: the caller's header fields, the negotiated extensions and, when
// the Upgrader chose it from its Subprotocols, the subprotocol. Neither of
// those two can also be in the caller's fields, as checkUpgradeRequest
// rejects both before the handshake.
func writeUpgradeResponse(w *bufio.Writer, acceptKey string, opts *upgradeOptions, extensions, subprotocol string) error {
	w.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	w.WriteString("Upgrade: websocket\r\n")
	w.WriteString("Connection: Upgrade\r\n")
	w.WriteString("Sec-WebSocket-Accept: ")
	w.WriteString(acceptKey)
	w.WriteString("\r\n")
	if extensions != "" {
		w.WriteString("Sec-WebSocket-Extensions: ")
		w.WriteString(extensions)
		w.WriteString("\r\n")
	}
	if opts.subprotocols != nil && subprotocol != "" {
		w.WriteString("Sec-WebSocket-Protocol: ")
		w.WriteString(subprotocol)
		w.WriteString("\r\n")
	}
	for k, vs := range opts.responseHeader {
		for _, v := range vs {
			w.WriteString(k)
			w.WriteString(": ")
			w.WriteString(v)
			w.WriteString("\r\n")
		}
	}
	// A bufio.Writer keeps its first error, so checking the last write
	// covers them all.
	if _, err := w.WriteString("\r\n"); err != nil {
		return fmt.Errorf("%w: failed to write handshake response: %w", errHandshakeFailed, err)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("%w: failed to flush handshake response: %w", errHandshakeFailed, err)
	}
	return nil
}

// validateHandshakeHeaders checks caller fields before acquiring a transport.
// Field names use the HTTP token alphabet; values follow net/http's allowance
// for HTAB, visible ASCII, and obs-text, excluding every other control byte. Do
// not include field values in errors: they may contain credentials or cookies.
func validateHandshakeHeaders(header http.Header, response bool) error {
	for name, values := range header {
		if !isHTTPToken(name) {
			return fmt.Errorf("%w: invalid field name %q", errInvalidHandshakeHeader, name)
		}
		reserved := false
		switch strings.ToLower(name) {
		case "upgrade", "connection", "sec-websocket-extensions", "content-length", "transfer-encoding":
			reserved = true
		case "host", "sec-websocket-key", "sec-websocket-version":
			reserved = !response
		case "sec-websocket-accept":
			reserved = response
		}
		if reserved {
			return fmt.Errorf("%w: reserved field %q", errInvalidHandshakeHeader, name)
		}
		for _, value := range values {
			for i := 0; i < len(value); i++ {
				c := value[i]
				if (c < ' ' && c != '\t') || c == 0x7f {
					return fmt.Errorf("%w: invalid value for field %q", errInvalidHandshakeHeader, name)
				}
			}
		}
	}
	return nil
}

// subprotocolValues includes noncanonical map keys in caller-provided headers.
// A nil/empty slice emits no field and is equivalent to an absent header.
func subprotocolValues(header http.Header) []string {
	var values []string
	for name, fields := range header {
		if strings.EqualFold(name, "Sec-WebSocket-Protocol") {
			values = append(values, fields...)
		}
	}
	return values
}

func isHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))) {
			return false
		}
	}
	return true
}

// RFC 6455 sections 4.1 and 4.3 require unique, case-sensitive token offers.
// The HTTP 1#token list rule permits empty list elements, but at least one
// nonempty token is required when the header is present.
func parseSubprotocolOffers(header http.Header) (map[string]struct{}, error) {
	values := subprotocolValues(header)
	var offered map[string]struct{}
	for _, value := range values {
		for item := range strings.SplitSeq(value, ",") {
			token := strings.Trim(item, " \t")
			if token == "" {
				continue
			}
			if !isHTTPToken(token) {
				return nil, errInvalidSubprotocol
			}
			if _, duplicate := offered[token]; duplicate {
				return nil, errInvalidSubprotocol
			}
			if offered == nil {
				offered = make(map[string]struct{}, 1)
			}
			offered[token] = struct{}{}
		}
	}
	if len(values) != 0 && len(offered) == 0 {
		return nil, errInvalidSubprotocol
	}
	return offered, nil
}

// A response may omit selection, or contain exactly one field with one offered
// token. In particular, repeated response fields and comma lists are forbidden.
// It returns the selected token, or "" when the header is absent.
func validateSubprotocolSelection(header http.Header, offered map[string]struct{}) (string, error) {
	values := subprotocolValues(header)
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 {
		return "", errInvalidSubprotocol
	}
	selected := strings.Trim(values[0], " \t")
	if !isHTTPToken(selected) {
		return "", errInvalidSubprotocol
	}
	if _, ok := offered[selected]; !ok {
		return "", errInvalidSubprotocol
	}
	return selected, nil
}

// computeAcceptKey computes the Sec-WebSocket-Accept value.
func computeAcceptKey(challengeKey string) string {
	accept := acceptKey([]byte(challengeKey))
	return string(accept[:])
}

// acceptGUID is the GUID RFC 6455 section 1.3 appends to the client's key.
const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// acceptKey returns the Sec-WebSocket-Accept value for a 24-byte challenge
// key without allocating; longer keys, which a server rejects first, cost
// one allocation.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-1.3
func acceptKey(challengeKey []byte) [28]byte {
	var buf [24 + len(acceptGUID)]byte
	sum := sha1.Sum(append(append(buf[:0], challengeKey...), acceptGUID...))
	var accept [28]byte
	base64.StdEncoding.Encode(accept[:], sum[:])
	return accept
}

// validChallengeKey reports whether key is a Sec-WebSocket-Key value: the
// base64 encoding of 16 bytes, which is always 24 characters long.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-4.2.1
func validChallengeKey(key string) bool {
	if len(key) != 24 {
		return false
	}
	var nonce [18]byte
	n, err := base64.StdEncoding.Decode(nonce[:], []byte(key))
	return err == nil && n == 16
}

// generateKey generates a Sec-WebSocket-Key value: a random 16-byte nonce,
// base64-encoded.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-4.1
func generateKey() [24]byte {
	var nonce [16]byte
	rand.Read(nonce[:]) // never fails; see crypto/rand.Read
	var key [24]byte
	base64.StdEncoding.Encode(key[:], nonce[:])
	return key
}

// headerContains checks if a header contains a specific value,
// based on case-insensitive comparison to allow for case variations,
// and handling of multiple values separated by commas.
func headerContains(h http.Header, name string, value string) bool {
	for _, s := range h[name] {
		for v := range strings.SplitSeq(s, ",") {
			if strings.EqualFold(strings.TrimSpace(v), value) {
				return true
			}
		}
	}
	return false
}

// xor XORs the payload with the masking key,
// updating the payload in place to avoid additional
// allocations.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-5.3
func xor(key []byte, data []byte) {
	// A whole number of four-byte key cycles preserves the mask phase, so the
	// key repeats across a word. Using encoding/binary keeps the word
	// operations safe for unaligned slices and independent of byte order.
	k := uint64(binary.LittleEndian.Uint32(key))
	k |= k << 32
	// Four independent words per iteration let the CPU overlap the loads and
	// stores, more than doubling throughput from 1 KiB up.
	for len(data) >= 32 {
		d := (*[32]byte)(data)
		binary.LittleEndian.PutUint64(d[0:], binary.LittleEndian.Uint64(d[0:])^k)
		binary.LittleEndian.PutUint64(d[8:], binary.LittleEndian.Uint64(d[8:])^k)
		binary.LittleEndian.PutUint64(d[16:], binary.LittleEndian.Uint64(d[16:])^k)
		binary.LittleEndian.PutUint64(d[24:], binary.LittleEndian.Uint64(d[24:])^k)
		data = data[32:]
	}
	for len(data) >= 8 {
		binary.LittleEndian.PutUint64(data, binary.LittleEndian.Uint64(data)^k)
		data = data[8:]
	}
	for i := range data {
		data[i] ^= byte(k >> (8 * (i & 3)))
	}
}
