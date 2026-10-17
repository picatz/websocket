package websocket

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"sync"
)

var (
	// ErrUnsupportedExtensionComposition identifies more than one enabled
	// built-in PMD instance, or a custom extension that changes RSV1 while
	// built-in PMD is enabled. RFC 7692 section 5 gives RSV1 to one PMCE.
	ErrUnsupportedExtensionComposition = errors.New("websocket: unsupported enabled extension composition")
	// ErrFragmentedCompression identifies direct PMD frame callbacks that do
	// not contain one complete message. Conn handles fragmentation internally.
	ErrFragmentedCompression = errors.New("websocket: direct compression callbacks require a complete message")
	// ErrInvalidCompressedPayload identifies malformed DEFLATE message data.
	ErrInvalidCompressedPayload = errors.New("websocket: invalid compressed payload")
)

// compressedPayloadError preserves both the classification and inflater cause.
type compressedPayloadError struct{ cause error }

func (e *compressedPayloadError) Error() string {
	return fmt.Sprintf("%v: %v", ErrInvalidCompressedPayload, e.cause)
}
func (e *compressedPayloadError) Unwrap() error        { return e.cause }
func (e *compressedPayloadError) Is(target error) bool { return target == ErrInvalidCompressedPayload }

// messageCompression returns the enabled built-in PMD extension, if any. It
// calls IsEnabled once per extension and rejects multiple built-in instances.
func messageCompression(extensions []Extension) (*perMessageDeflate, error) {
	var buf [4]Extension
	enabled, err := enabledExtensions(extensions, buf[:0])
	if err != nil || len(enabled) == 0 {
		return nil, err
	}
	pmd, _ := enabled[0].(*perMessageDeflate)
	return pmd, nil
}

// enabledExtensions appends the enabled extensions to dst, in order, calling
// IsEnabled exactly once on each. At most one built-in PMD may be enabled: RFC
// 7692 section 5 gives RSV1 to a single PMCE. Custom frame extensions may be
// combined with it (see Conn's composition order).
func enabledExtensions(extensions, dst []Extension) ([]Extension, error) {
	var pmd bool
	for _, ext := range extensions {
		if ext == nil {
			return nil, fmt.Errorf("%w: nil extension", ErrInvalidExtension)
		}
		if !ext.IsEnabled() {
			continue
		}
		if _, ok := ext.(*perMessageDeflate); ok {
			if pmd {
				return nil, fmt.Errorf("%w: multiple permessage-deflate owners", ErrUnsupportedExtensionComposition)
			}
			pmd = true
		}
		dst = append(dst, ext)
	}
	return dst, nil
}

// closeCompression does not acquire receive serialization or touch active
// inflater buffers. It only detaches idle state and prohibits future commits.
func (c *Conn) closeCompression() {
	for _, ext := range c.extensions {
		if p, ok := ext.(*perMessageDeflate); ok {
			p.closeReceive()
		}
	}
}

// releaseSendCompression returns idle send compressors to their pools once no
// further data frames can be written. writeMu must be held.
func (c *Conn) releaseSendCompression() {
	for _, ext := range c.extensions {
		if p, ok := ext.(*perMessageDeflate); ok && !p.send.active {
			p.send.release()
		}
	}
}

func (p *perMessageDeflate) closeReceive() {
	p.receiveLife.Lock()
	p.receiveClosed = true
	p.receive = nil
	p.receiveLife.Unlock()
}

const deflateWindow = 32768

var deflateTail = [...]byte{0, 0, 255, 255, 1, 0, 0, 255, 255}

// deflateReceiveState is what a connection keeps between compressed messages:
// with the peer's context takeover, the last window bytes of decoded output.
type deflateReceiveState struct {
	history []byte
	window  int // Bytes of history the peer may reference (2^peer window bits).
}

// inflaterState is the decoder working state. It is pooled across all
// connections and held only while a message is being decoded.
type inflaterState struct {
	inflater io.ReadCloser
	seed     []byte
	buffer   [4096]byte
}

var inflaterPool sync.Pool

type emptyDeflateInput struct{}

func (emptyDeflateInput) Read([]byte) (int, error) { return 0, io.EOF }
func (emptyDeflateInput) ReadByte() (byte, error)  { return 0, io.EOF }

type deflateInput interface {
	io.Reader
	io.ByteReader
}

// deflateMessageInput owns both read methods and a single lookahead byte.
// No per-stream buffered reader may hide bytes beyond a BFINAL boundary.
type deflateMessageInput struct {
	source     deflateInput
	peerClosed *bool
	sourceErr  error
	look       byte
	hasLook    bool
	suffix     int
}

func (r *deflateMessageInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.hasLook {
		p[0] = r.look
		r.hasLook = false
		return 1, nil
	}
	if r.sourceErr == nil {
		n, err := r.source.Read(p)
		if err != nil {
			r.sourceErr = err
		}
		if n != 0 {
			return n, nil
		}
	}
	if r.sourceErr != io.EOF {
		return 0, r.sourceErr
	}
	if r.suffix == len(deflateTail) {
		return 0, io.EOF
	}
	n := copy(p, deflateTail[r.suffix:])
	r.suffix += n
	return n, nil
}
func (r *deflateMessageInput) ReadByte() (byte, error) {
	if r.hasLook {
		r.hasLook = false
		return r.look, nil
	}
	if r.sourceErr == nil {
		b, err := r.source.ReadByte()
		if err == nil {
			return b, nil
		}
		r.sourceErr = err
	}
	if r.sourceErr != io.EOF {
		return 0, r.sourceErr
	}
	if r.suffix == len(deflateTail) {
		return 0, io.EOF
	}
	b := deflateTail[r.suffix]
	r.suffix++
	return b, nil
}
func (r *deflateMessageInput) more() (bool, error) {
	b, err := r.ReadByte()
	if err == io.EOF {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	r.look, r.hasLook = b, true
	return true, nil
}
func (r *deflateMessageInput) closedByPeer() bool { return r.peerClosed != nil && *r.peerClosed }

// deflateFrameReader exposes only the current logical message's encoded bytes.
// Headers/keys are validated/read before each body; controls run synchronously
// with no PMD lifetime lock held. Only physical FIN produces a clean EOF.
//
// With enabled custom extensions, each frame is read whole and passed through
// them in registration order before its bytes reach the inflater, so frame
// extensions apply outside compression in both directions.
type deflateFrameReader struct {
	c          *Conn
	frame      Frame // The current frame; a copy, so callers' frames stay on their stacks.
	remaining  uint64
	phase      uint64
	peerClosed *bool
	frameExts  []Extension // Enabled custom extensions; frames are buffered.
	buf        []byte      // Remaining extension-processed payload.
	first      bool        // The current frame starts the message.
}

func (r *deflateFrameReader) start(frame *Frame, n uint64) error {
	r.frame, r.remaining, r.phase = *frame, n, 0
	if len(r.frameExts) > 0 {
		return r.startBuffered(&r.frame, n)
	}
	if r.frame.Masked {
		if err := readField(r.c.br, r.frame.MaskKey[:], "masking key"); err != nil {
			return err
		}
	}
	return nil
}

// startBuffered reads a whole frame and applies the custom extensions. They
// must consume their own reserved bits and leave RSV1, the opcode and the
// message state to permessage-deflate.
func (r *deflateFrameReader) startBuffered(frame *Frame, n uint64) error {
	opcode, rsv1 := frame.Opcode, frame.Rsv1
	if err := r.c.readFrameBody(frame, n); err != nil {
		return err
	}
	for _, ext := range r.frameExts {
		if err := processIncomingFrame(ext, frame); err != nil {
			return fmt.Errorf("extension %s failed to process incoming frame: %w", ext.Name(), err)
		}
	}
	switch {
	case frame.Opcode != opcode:
		return failWith(StatusProtocolError, ErrUnexpectedFrame)
	case frame.Rsv1 != rsv1:
		return fmt.Errorf("%w: an extension changed RSV1 owned by permessage-deflate", ErrUnsupportedExtensionComposition)
	case frame.Rsv1 != r.first || frame.Rsv2 || frame.Rsv3:
		// RFC 7692 section 6.1: RSV1 only on a message's first frame.
		return failWith(StatusProtocolError, ErrUnsupportedExtensions)
	}
	r.first = false
	r.buf, r.remaining = frame.Payload, uint64(len(frame.Payload))
	return nil
}

func (r *deflateFrameReader) advance() error {
	for r.remaining == 0 {
		if r.c.closed.Load() {
			return io.ErrClosedPipe
		}
		if r.frame.Final {
			return io.EOF
		}
		var f Frame
		n, err := r.c.readFrameHeader(&f)
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		if err != nil {
			return err
		}
		if f.Opcode >= CloseMessage {
			if err := r.c.validateMessageHeader(&f, n, true, 0); err != nil {
				return err
			}
			if err := r.c.readFramePayload(&f, n); err != nil {
				return err
			}
			closed, err := r.c.handleControl(&f)
			if closed {
				*r.peerClosed = true
				return errDeflatePeerClose
			}
			if err != nil {
				return err
			}
			continue
		}
		if f.Opcode != ContinuationFrame {
			return failWith(StatusProtocolError, ErrUnexpectedFrame)
		}
		// readFrameHeader already rejects RSV1 on continuations and bounds each
		// encoded frame. Its length must not spend the remaining decoded budget.
		if err := r.start(&f, n); err != nil {
			return err
		}
	}
	return nil
}
func (r *deflateFrameReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.advance(); err != nil {
		return 0, err
	}
	if r.frameExts != nil {
		n := copy(p, r.buf)
		r.buf, r.remaining = r.buf[n:], r.remaining-uint64(n)
		return n, nil
	}
	if uint64(len(p)) > r.remaining {
		p = p[:int(r.remaining)]
	}
	n, err := r.c.br.Read(p)
	if n > 0 {
		if r.frame.Masked {
			for i := range p[:n] {
				p[i] ^= r.frame.MaskKey[(r.phase+uint64(i))&3]
			}
		}
		r.phase += uint64(n)
		r.remaining -= uint64(n)
	}
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
func (r *deflateFrameReader) ReadByte() (byte, error) {
	if err := r.advance(); err != nil {
		return 0, err
	}
	if r.frameExts != nil {
		b := r.buf[0]
		r.buf, r.remaining = r.buf[1:], r.remaining-1
		return b, nil
	}
	b, err := r.c.br.ReadByte()
	if err == io.EOF {
		return 0, io.ErrUnexpectedEOF
	}
	if err != nil {
		return 0, err
	}
	if r.frame.Masked {
		b ^= r.frame.MaskKey[r.phase&3]
	}
	r.phase++
	r.remaining--
	return b, nil
}

var errDeflatePeerClose = errors.New("websocket: peer closed during compressed message")

func deflateSeed(history, output []byte, scratch *[]byte) []byte {
	if len(output) >= deflateWindow {
		return output[len(output)-deflateWindow:]
	}
	if len(output) == 0 {
		return history
	}
	if len(history) == 0 {
		return output
	}
	n := min(len(history), deflateWindow-len(output))
	need := n + len(output)
	if cap(*scratch) < need {
		*scratch = make([]byte, need)
	} else {
		*scratch = (*scratch)[:need]
	}
	copy(*scratch, history[len(history)-n:])
	copy((*scratch)[n:], output)
	return *scratch
}

// save keeps the final window bytes of history followed by output. The peer
// cannot reference anything older (RFC 7692 section 7.2.2), so a smaller
// negotiated peer window keeps proportionally less memory per connection.
func (d *deflateReceiveState) save(output []byte, noContext bool) {
	if noContext {
		d.history = nil
		return
	}
	window := d.window
	if len(output) >= window {
		if cap(d.history) < window {
			d.history = make([]byte, window)
		} else {
			d.history = d.history[:window]
		}
		copy(d.history, output[len(output)-window:])
		return
	}
	keep := min(len(d.history), window-len(output))
	need := keep + len(output)
	if cap(d.history) < need {
		next := make([]byte, need, min(window, max(need, max(32, cap(d.history)*2))))
		copy(next, d.history[len(d.history)-keep:])
		d.history = next
	} else {
		copy(d.history[:keep], d.history[len(d.history)-keep:])
		d.history = d.history[:need]
	}
	copy(d.history[keep:], output)
}
func appendDecoded(out, p []byte, limit int) []byte {
	need := len(out) + len(p)
	if need > cap(out) {
		n := max(8, cap(out))
		if n <= int(^uint(0)>>1)/2 {
			n *= 2
		}
		n = max(n, need)
		if limit > 0 {
			n = min(n, limit)
		}
		q := make([]byte, len(out), n)
		copy(q, out)
		out = q
	}
	return append(out, p...)
}

// decodeMessage serializes receive only. The lifetime lock is held solely to
// detach/acquire/commit state, never across input, controls, or callbacks.
func (p *perMessageDeflate) decodeMessage(input *deflateMessageInput, limit int, text bool) (out []byte, err error) {
	p.receiveMu.Lock()
	defer p.receiveMu.Unlock()
	p.receiveLife.Lock()
	if p.receiveClosed {
		p.receiveLife.Unlock()
		return nil, io.ErrClosedPipe
	}
	d := p.receive
	p.receive = nil
	p.receiveLife.Unlock()
	if d == nil {
		d = &deflateReceiveState{window: 1 << p.peerWindowBits()}
	}
	noContext := p.peerNoContextTakeover()
	if noContext {
		d.history = nil
	}
	f, _ := inflaterPool.Get().(*inflaterState)
	if f == nil {
		f = new(inflaterState)
	}
	defer func() {
		// Never return an inflater that still references this input.
		if f.inflater != nil {
			if resetErr := f.inflater.(flate.Resetter).Reset(emptyDeflateInput{}, nil); resetErr == nil {
				inflaterPool.Put(f)
			} else if err == nil {
				err = resetErr
			}
		}
		if err == nil {
			d.save(out, noContext)
		}
		p.receiveLife.Lock()
		defer p.receiveLife.Unlock()
		if err != nil {
			p.receiveClosed = true
			out = nil
			return
		}
		if p.receiveClosed {
			out, err = nil, io.ErrClosedPipe
			return
		}
		p.receive = d
	}()
	if f.inflater == nil {
		f.inflater = flate.NewReaderDict(input, d.history)
	} else if err = f.inflater.(flate.Resetter).Reset(input, d.history); err != nil {
		return nil, err
	}
	validated := 0
	for {
		nmax := len(f.buffer)
		remaining := int(^uint(0)>>1) - len(out)
		if limit > 0 {
			remaining = min(remaining, limit-len(out))
		}
		if remaining < nmax {
			nmax = remaining + 1
		}
		n, readErr := f.inflater.Read(f.buffer[:nmax])
		// A source Close can flush invalid/oversized buffered output. Closure wins
		// before even validating or appending that stale output.
		if input.closedByPeer() {
			return nil, errDeflatePeerClose
		}
		if n > remaining {
			return nil, failWith(StatusMessageTooBig, fmt.Errorf("extension permessage-deflate failed to process incoming frame: %w", ErrPayloadTooLarge))
		}
		if n > 0 {
			// Check new bytes plus at most three pending bytes before growth.
			if text {
				offset := 0
				pending := len(out) - validated
				if pending != 0 {
					var carry [4]byte
					used := min(4-pending, n)
					copy(carry[:], out[validated:])
					copy(carry[pending:], f.buffer[:used])
					complete, valid := validUTF8Prefix(carry[:pending+used])
					if !valid {
						return nil, failWith(StatusInvalidFramePayloadData, ErrInvalidFrame)
					}
					validated += complete
					if complete == 0 {
						offset = n
					} else {
						offset = complete - pending
					}
				}
				complete, valid := validUTF8Prefix(f.buffer[offset:n])
				if !valid {
					return nil, failWith(StatusInvalidFramePayloadData, ErrInvalidFrame)
				}
				validated += complete
			}
			out = appendDecoded(out, f.buffer[:n], limit)
		}

		if readErr == nil {
			continue
		}
		if readErr != io.EOF {
			// Preserve source framing, control, and transport causes exactly; only
			// errors originating in the inflater acquire the payload classification.
			if input.sourceErr != nil && input.sourceErr != io.EOF {
				return nil, input.sourceErr
			}
			// Malformed DEFLATE aborts without a Close code, matching the
			// established extension-failure contract for invalid streams.
			return nil, &compressedPayloadError{readErr}
		}
		more, e := input.more()
		if input.closedByPeer() {
			return nil, errDeflatePeerClose
		}
		if e != nil {
			return nil, e
		}
		if !more {
			break
		}
		if err = f.inflater.(flate.Resetter).Reset(input, deflateSeed(d.history, out, &f.seed)); err != nil {
			return nil, err
		}
	}
	if text && validated != len(out) {
		return nil, failWith(StatusInvalidFramePayloadData, ErrInvalidFrame)
	}
	return out, nil
}

func (p *perMessageDeflate) processIncomingFrame(frame *Frame, limit int) error {
	if !p.enabled {
		return nil
	}
	if frame.Opcode == ContinuationFrame || ((frame.Opcode == TextMessage || frame.Opcode == BinaryMessage) && !frame.Final) {
		return ErrFragmentedCompression
	}
	if frame.Opcode != TextMessage && frame.Opcode != BinaryMessage || !frame.Rsv1 {
		return nil
	}
	out, err := p.decodeMessage(&deflateMessageInput{source: bytes.NewReader(frame.Payload)}, limit, false)
	if err != nil {
		return err
	}
	frame.Payload, frame.Rsv1 = out, false
	return nil
}

// handleControl is shared by the legacy frame path and PMD's logical reader.
// Handlers run synchronously with neither the handler nor PMD lifetime lock held.
func (c *Conn) handleControl(frame *Frame) (peerClosed bool, err error) {
	switch frame.Opcode {
	case CloseMessage:
		if err := validateIncomingClosePayload(frame.Payload); err != nil {
			return false, err
		}
		c.peerClose = newCloseError(frame.Payload)
		_ = c.closeWithPayload(frame.Payload)
		return true, nil
	case PingMessage:
		c.handlerMu.RLock()
		handler := c.pingHandler
		c.handlerMu.RUnlock()
		if handler != nil {
			return false, handler(string(frame.Payload))
		}
		return false, c.WriteControlFrame(PongMessage, frame.Payload)
	case PongMessage:
		c.completePing(frame.Payload)
		c.handlerMu.RLock()
		handler := c.pongHandler
		c.handlerMu.RUnlock()
		if handler != nil {
			return false, handler(string(frame.Payload))
		}
		return false, nil
	}
	return false, failWith(StatusProtocolError, ErrInvalidOpcode)
}
