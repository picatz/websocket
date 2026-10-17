package websocket

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
)

// Ping sends a Ping frame and waits for the peer's matching Pong
// ([RFC 6455, Section 5.5.2]). It returns nil once the Pong arrives.
//
// Pongs are processed by ReadMessage, so a concurrent read loop must be
// running; otherwise Ping waits until ctx ends. The matching happens before
// any handler set with SetPongHandler runs, and that handler still sees every
// Pong. Each call uses a distinct payload, so concurrent Pings are matched
// independently; an unsolicited Pong carrying the same payload also counts.
//
// If ctx is already done, Ping returns its cause without writing. If ctx ends
// while the Ping frame is being written, the connection is aborted as for
// WriteMessageContext, because a partial frame cannot be retried. If ctx ends
// while waiting for the Pong, Ping returns ctx's cause and the connection is
// left open: deciding that a missing Pong means the peer is gone, and closing,
// is up to the caller. If the connection closes first, Ping returns an error
// matching io.ErrClosedPipe.
//
// [RFC 6455, Section 5.5.2]: https://www.rfc-editor.org/rfc/rfc6455.html#section-5.5.2
func (c *Conn) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	var payload [8]byte
	binary.BigEndian.PutUint64(payload[:], c.pingSeq.Add(1))
	key := string(payload[:])

	pong := make(chan struct{})
	c.pingMu.Lock()
	if c.pings == nil {
		c.pings = make(map[string]chan struct{})
	}
	c.pings[key] = pong
	c.pingMu.Unlock()
	defer func() {
		c.pingMu.Lock()
		delete(c.pings, key)
		c.pingMu.Unlock()
	}()

	stop := context.AfterFunc(ctx, c.cancelIO)
	err := c.WriteControlFrame(PingMessage, payload[:])
	if !stop() && err != nil {
		return fmt.Errorf("%w: %w", context.Cause(ctx), err)
	}
	if err != nil {
		return err
	}

	select {
	case <-pong:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-c.doneChan():
		// A Pong processed just before the connection closed still counts.
		select {
		case <-pong:
			return nil
		default:
		}
		return fmt.Errorf("websocket: connection closed before pong: %w", io.ErrClosedPipe)
	}
}

// completePing releases the Ping waiting for payload, if any. It is called
// from the read path for every incoming Pong.
func (c *Conn) completePing(payload []byte) {
	c.pingMu.Lock()
	defer c.pingMu.Unlock()
	if pong, ok := c.pings[string(payload)]; ok {
		close(pong)
		delete(c.pings, string(payload))
	}
}

// doneChan returns a channel that is closed once the connection is terminal.
func (c *Conn) doneChan() <-chan struct{} {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.done == nil {
		c.done = make(chan struct{})
		if c.closed.Load() {
			close(c.done)
			c.doneSet = true
		}
	}
	return c.done
}

// markDoneLocked publishes terminal state to doneChan waiters. The caller
// holds closeMu and has already set closed; a channel created later observes
// closed and starts out closed.
func (c *Conn) markDoneLocked() {
	if c.done != nil && !c.doneSet {
		close(c.done)
		c.doneSet = true
	}
}
