package websocket

import (
	"context"
	"fmt"
)

// ReadMessageContext is like ReadMessage, but ctx bounds the call.
//
// If ctx is already done, it returns ctx's error without reading and the
// connection is unaffected. If ctx ends while the read is in progress, the
// connection is aborted: the transport closes without a closing handshake,
// the blocked read returns an error matching both the context's cause and
// the underlying failure, and later operations fail as after Close. A message
// that completed as ctx ended is still returned, but the connection may
// already be aborted.
//
// This is the same terminal contract as a read timeout. It never changes the
// deadlines set with SetDeadline or SetReadDeadline. To bound a single read
// without risking the connection, use SetReadDeadline knowing that read
// timeouts are terminal too, or leave the read running and select on its result.
func (c *Conn) ReadMessageContext(ctx context.Context) (messageType MessageType, data []byte, err error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, context.Cause(ctx)
	}
	stop := context.AfterFunc(ctx, c.cancelIO)
	messageType, data, err = c.ReadMessage()
	if !stop() && err != nil {
		return 0, nil, fmt.Errorf("%w: %w", context.Cause(ctx), err)
	}
	return messageType, data, err
}

// WriteMessageContext is like WriteMessage, but ctx bounds the call,
// including any wait for a concurrent writer.
//
// If ctx is already done, it returns ctx's error without writing and the
// connection is unaffected. If ctx ends while the write is in progress, the
// connection is aborted as described for ReadMessageContext, because a
// partially written frame cannot be retried. The returned error matches both
// the context's cause and the underlying failure.
func (c *Conn) WriteMessageContext(ctx context.Context, messageType MessageType, data []byte) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	stop := context.AfterFunc(ctx, c.cancelIO)
	err := c.WriteMessage(messageType, data)
	if !stop() && err != nil {
		return fmt.Errorf("%w: %w", context.Cause(ctx), err)
	}
	return err
}

// cancelIO aborts the connection on behalf of an ended context. Like a read
// failure, it publishes terminal state first, so a later Close reports
// ErrAlreadyClosed and concurrent operations observe a closed connection.
func (c *Conn) cancelIO() {
	c.closeMu.Lock()
	c.closed.Store(true)
	c.markDoneLocked()
	c.failed = true
	c.closeMu.Unlock()
	c.abort()
}
