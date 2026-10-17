package websocket

import (
	"context"
	"fmt"
	"unicode/utf8"
)

// WriteMessage writes a WebSocket message. Transport errors wrap the
// underlying error for errors.Is and errors.As. Close the connection after a
// transport error; the transport is not closed automatically, and clearing a
// write deadline cannot make a partially written message safe to retry. Later
// writes fail with the original cause, unless argument validation or a closed
// connection takes precedence. Argument validation before transport I/O does
// not poison writes.
//
// ctx bounds the call, including any wait for a concurrent writer. If ctx is
// already done, WriteMessage returns the context's cause without writing, and
// the connection is unaffected. If ctx ends while the write is in progress,
// the connection is aborted as described for ReadMessage, because a partially
// written frame cannot be retried; the returned error matches both the
// context's cause and the underlying failure.
func (c *Conn) WriteMessage(ctx context.Context, messageType MessageType, data []byte) error {
	stop, err := c.watch(ctx)
	if err != nil {
		return err
	}
	return ctxError(ctx, stop, c.writeMessage(messageType, data))
}

// writeMessage is WriteMessage without a context.
func (c *Conn) writeMessage(messageType MessageType, data []byte) error {
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
