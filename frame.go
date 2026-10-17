package websocket

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"sync"
	"unicode/utf8"
)

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
