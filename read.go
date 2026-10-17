package websocket

import (
	"context"
	"encoding/binary"
	"io"
	"time"
	"unicode/utf8"
)

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
//
// ctx bounds the call. If ctx is already done, ReadMessage returns the
// context's cause without reading, and the connection is unaffected. If ctx
// ends while the read is in progress, the connection is aborted: the transport
// closes without a closing handshake, the read returns an error matching both
// the context's cause and the underlying failure, and later operations fail as
// after Close. This is the same terminal contract as a read timeout, and ctx
// never changes the deadlines set with SetDeadline or SetReadDeadline. A
// message that completed as ctx ended is still returned. A context that is
// never done, such as context.Background, adds no cost.
func (c *Conn) ReadMessage(ctx context.Context) (messageType MessageType, data []byte, err error) {
	stop, err := c.watch(ctx)
	if err != nil {
		return 0, nil, err
	}
	messageType, data, err = c.readDataMessage()
	if err = ctxError(ctx, stop, err); err != nil {
		return 0, nil, err
	}
	return messageType, data, nil
}

// readDataMessage is ReadMessage without a context.
func (c *Conn) readDataMessage() (messageType MessageType, data []byte, err error) {
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
