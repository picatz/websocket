package websocket

import (
	"context"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// errWriterClosed reports use of a NextWriter writer after Close or a failure.
var errWriterClosed = errors.New("websocket: message writer closed")

// NextWriter starts a data message of messageType and returns a writer for its
// payload, so a large message can be sent without holding it in memory. Each
// Write may send complete frames; Close sends the final frame and must be
// called to finish the message.
//
// Only one data message is written at a time: WriteMessage and another
// NextWriter wait until the writer is closed or fails, or the connection
// becomes terminal. Control frames, including Ping and the automatic Pong from
// ReadMessage, may be sent between the message's frames, as RFC 6455 section
// 5.4 allows. A WriteMessage waiting behind an open writer counts as in
// progress: if its ctx ends, the connection is aborted. The returned writer
// must not be used concurrently.
//
// For TextMessage, the payload is checked as UTF-8 across Write calls. An
// invalid sequence, or a rune left incomplete at Close, fails the writer as
// WriteMessage fails invalid text. Errors follow WriteMessage: once part of the
// message has reached the transport, a failure poisons later writes, and the
// connection should be closed. After an error or Close, the writer returns an
// error and no longer blocks other writers.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-5.4
//
// ctx bounds both the wait for a concurrent writer and the writing of the
// message. If ctx is already done, NextWriter returns the context's cause
// without writing. If ctx ends before the writer is closed or fails, the
// connection is aborted as described for ReadMessage, and the call, Write or
// Close returns an error matching both the context's cause and the underlying
// failure. Once the message ends, ctx no longer matters.
func (c *Conn) NextWriter(ctx context.Context, messageType MessageType) (io.WriteCloser, error) {
	stop, err := c.watch(ctx)
	if err != nil {
		return nil, err
	}
	w, err := c.nextWriter(messageType)
	if err != nil {
		return nil, ctxError(ctx, stop, err)
	}
	w.ctx, w.stopCtx = ctx, stop
	return w, nil
}

// nextWriter is NextWriter without a context.
func (c *Conn) nextWriter(messageType MessageType) (*streamWriter, error) {
	if messageType != TextMessage && messageType != BinaryMessage {
		return nil, errInvalidMessageType
	}
	if c.closed.Load() {
		return nil, errClosed
	}
	if err := c.acquireData(); err != nil {
		return nil, err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() || c.closeSent {
		c.releaseData()
		return nil, errClosed
	}
	if c.writeErr != nil {
		c.releaseData()
		return nil, fmt.Errorf("%w: failed to write frame header: %w", errWriteFailed, c.writeErr)
	}
	var buf [4]frameExtension
	enabled, err := enabledExtensions(c.extensions, buf[:0])
	if err != nil {
		c.releaseData()
		return nil, c.failRead(err)
	}
	return &streamWriter{c: c, w: c.newMessageWriter(opcode(messageType), enabled), text: messageType == TextMessage}, nil
}

// streamWriter adapts a messageWriter to io.WriteCloser. It holds the data
// slot for the message's lifetime and takes writeMu only for each call, so
// control frames can interleave between frames.
type streamWriter struct {
	c    *Conn
	w    *messageWriter
	text bool
	utf8 utf8Stream
	done bool

	// ctx is NextWriter's context, which bounds the message. stopCtx
	// unregisters its abort when the message ends; ctxEnded records that
	// ctx had already ended the message by then.
	ctx      context.Context
	stopCtx  func() bool
	ctxEnded bool
}

func (s *streamWriter) Write(p []byte) (int, error) {
	if s.done {
		return 0, errWriterClosed
	}
	s.c.writeMu.Lock()
	defer s.c.writeMu.Unlock()
	if err := s.writable(); err != nil {
		return 0, s.finish(err)
	}
	if s.text && !s.utf8.check(p) {
		return 0, s.finish(s.w.fail(errInvalidUTF8))
	}
	n, err := s.w.Write(p)
	if err != nil {
		return 0, s.finish(err)
	}
	return n, nil
}

// Close sends the final frame. It does not close the connection.
func (s *streamWriter) Close() error {
	if s.done {
		return errWriterClosed
	}
	s.c.writeMu.Lock()
	defer s.c.writeMu.Unlock()
	if err := s.writable(); err != nil {
		return s.finish(err)
	}
	if s.utf8.pending != 0 {
		return s.finish(s.w.fail(errInvalidUTF8))
	}
	return s.finish(s.w.Close())
}

// writable reports why the message cannot continue: the connection closed,
// a Close frame was sent in between, or another write failed. writeMu is held.
func (s *streamWriter) writable() error {
	switch {
	case s.c.closed.Load() || s.c.closeSent:
		return s.w.fail(errClosed)
	case s.c.writeErr != nil && s.w.err == nil:
		return s.w.fail(fmt.Errorf("%w: failed to write frame header: %w", errWriteFailed, s.c.writeErr))
	}
	return nil
}

// finish ends the writer and frees the data slot for the next message.
func (s *streamWriter) finish(err error) error {
	s.done = true
	s.c.releaseData()
	if s.stopCtx != nil && !s.stopCtx() {
		s.ctxEnded = true
	}
	s.stopCtx = nil
	if s.ctxEnded && err != nil {
		return fmt.Errorf("%w: %w", context.Cause(s.ctx), err)
	}
	return err
}

// utf8Stream validates text that arrives in pieces. It keeps at most three
// bytes of an incomplete rune between calls, so a rune may span any split.
type utf8Stream struct {
	carry   [utf8.UTFMax]byte // Trailing bytes of an incomplete rune
	pending int
}

// check validates p as the continuation of the text seen so far. A rune left
// incomplete at the end of the text shows as pending != 0.
func (s *utf8Stream) check(p []byte) bool {
	if s.pending > 0 {
		// Complete the carried rune from the head of p.
		var joined [2 * utf8.UTFMax]byte
		k := copy(joined[copy(joined[:], s.carry[:s.pending]):], p[:min(len(p), utf8.UTFMax-s.pending)])
		n, ok := validUTF8Prefix(joined[:s.pending+k])
		if !ok {
			return false
		}
		if n == 0 { // Still incomplete; all of p was used.
			s.pending += copy(s.carry[s.pending:], p)
			return true
		}
		p = p[n-s.pending:]
		s.pending = 0
	}
	n, ok := validUTF8Prefix(p)
	if !ok {
		return false
	}
	s.pending = copy(s.carry[:], p[n:])
	return true
}

// acquireData takes the connection's single data-message slot, waiting for an
// open NextWriter to finish. It gives up once the connection is terminal, so a
// writer that is never closed cannot block writes on a closed connection.
func (c *Conn) acquireData() error {
	c.dataOnce.Do(func() { c.dataSlot = make(chan struct{}, 1) })
	select {
	case c.dataSlot <- struct{}{}:
		return nil
	default:
	}
	select {
	case c.dataSlot <- struct{}{}:
		return nil
	case <-c.doneChan():
		return errClosed
	}
}

func (c *Conn) releaseData() { <-c.dataSlot }
