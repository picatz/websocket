package websocket

import (
	"compress/flate"
	"sync"

	"github.com/picatz/websocket/internal/deflate"
)

// Compressor pools are shared by every connection. A connection holds a
// compressor only while it encodes a message, unless context takeover in the
// fast retention mode keeps one between messages (see deflateSender).
var (
	// flateWriterPools holds compress/flate writers by level+2 (-2 to 9).
	flateWriterPools [12]sync.Pool
	// encoderPools holds bounded-window encoders by window bits and level+2.
	encoderPools [deflate.MaxWindowBits - deflate.MinWindowBits + 1][12]sync.Pool
)

// appendSink adapts an append-style byte slice to io.Writer for flate.Writer.
type appendSink struct{ buf []byte }

func (s *appendSink) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	return len(p), nil
}

// flateCompressor wraps a compress/flate writer. compress/flate always uses a
// 32 KiB window, so it is used only when the agreed window is 15 bits.
type flateCompressor struct {
	w     *flate.Writer
	sink  appendSink
	level int
}

func getFlateCompressor(level int) *flateCompressor {
	if v := flateWriterPools[level+2].Get(); v != nil {
		c := v.(*flateCompressor)
		c.w.Reset(&c.sink)
		return c
	}
	c := &flateCompressor{level: level}
	c.w, _ = flate.NewWriter(&c.sink, level) // Level was validated by the option.
	return c
}

func (c *flateCompressor) put() {
	c.sink.buf = nil
	flateWriterPools[c.level+2].Put(c)
}

func (c *flateCompressor) write(dst, p []byte) []byte {
	c.sink.buf = dst
	_, _ = c.w.Write(p) // appendSink never fails.
	dst, c.sink.buf = c.sink.buf, nil
	return dst
}

func (c *flateCompressor) flush(dst []byte) []byte {
	c.sink.buf = dst
	_ = c.w.Flush()
	dst, c.sink.buf = c.sink.buf, nil
	return dst
}

func getEncoder(bits, level int, dict []byte) *deflate.Encoder {
	pool := &encoderPools[bits-deflate.MinWindowBits][level+2]
	if v := pool.Get(); v != nil {
		e := v.(*deflate.Encoder)
		e.Reset(dict)
		return e
	}
	e, _ := deflate.NewEncoder(bits, level) // Bits and level were validated.
	e.Reset(dict)
	return e
}

func putEncoder(e *deflate.Encoder) {
	encoderPools[e.WindowBits()-deflate.MinWindowBits][e.Level()+2].Put(e)
}

// sendRetention selects what a connection keeps between compressed messages.
type sendRetention uint8

const (
	// retainNothing returns the compressor to its pool after every message.
	// Used without local context takeover (RFC 7692 section 7.1.1).
	retainNothing sendRetention = iota
	// retainHistory keeps only the last 2^bits bytes of input and borrows a
	// pooled encoder per message, re-indexing that history before encoding.
	retainHistory
	// retainCompressor keeps a compressor between messages: the fastest
	// takeover mode, and the most memory per idle connection.
	retainCompressor
)

// deflateSender produces the compressed payload of outgoing messages for one
// connection. It is used only with the connection's write lock held.
//
// https://www.rfc-editor.org/rfc/rfc7692#section-7.2.1
type deflateSender struct {
	bits      int // Local LZ77 window, at most the negotiated limit.
	level     int
	retention sendRetention

	flate   *flateCompressor
	enc     *deflate.Encoder
	history []byte
	active  bool
}

// begin acquires a compressor for one message.
func (s *deflateSender) begin() {
	s.active = true
	if s.flate != nil || s.enc != nil {
		return
	}
	switch {
	case s.bits == deflate.MaxWindowBits && s.retention != retainHistory:
		// compress/flate is faster than the bounded encoder, and its window
		// is exactly 2^15. It cannot be primed with retained history cheaply.
		s.flate = getFlateCompressor(s.level)
	default:
		s.enc = getEncoder(s.bits, s.level, s.history)
	}
}

// write compresses p, appending available output to dst.
func (s *deflateSender) write(dst, p []byte) []byte {
	if s.flate != nil {
		return s.flate.write(dst, p)
	}
	return s.enc.Write(dst, p)
}

// end sync-flushes the message, removes the 0x00 0x00 0xff 0xff tail that
// RFC 7692 section 7.2.1 strips, and releases or retains the compressor.
// The flush always emits the whole tail, so it never spans earlier output.
func (s *deflateSender) end(dst []byte) []byte {
	if s.flate != nil {
		dst = s.flate.flush(dst)
	} else {
		dst = s.enc.Flush(dst)
	}
	dst = dst[:len(dst)-4]
	switch s.retention {
	case retainNothing:
		s.release()
	case retainHistory:
		s.history = s.enc.History(s.history[:0])
		putEncoder(s.enc)
		s.enc = nil
	}
	s.active = false
	return dst
}

// release returns any compressor to its pool and forgets all history, as after
// a message that could not be sent completely.
func (s *deflateSender) release() {
	if s.flate != nil {
		s.flate.put()
		s.flate = nil
	}
	if s.enc != nil {
		putEncoder(s.enc)
		s.enc = nil
	}
	s.history = nil
	s.active = false
}
