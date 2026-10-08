package websocket

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
)

var (
	// ErrUnsupportedExtensionComposition identifies an enabled built-in PMD
	// selection combined with another enabled extension. That experimental
	// combination has no supported message/frame transformation contract.
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

func messageCompression(extensions []Extension) (*perMessageDeflate, error) {
	var pmd *perMessageDeflate
	var custom bool
	for _, ext := range extensions {
		if ext == nil {
			return nil, fmt.Errorf("%w: nil extension", ErrInvalidExtension)
		}
		if !ext.IsEnabled() {
			continue
		}
		if p, ok := ext.(*perMessageDeflate); ok {
			if pmd != nil {
				return nil, fmt.Errorf("%w: multiple permessage-deflate owners", ErrUnsupportedExtensionComposition)
			}
			pmd = p
		} else {
			custom = true
		}
	}
	if pmd != nil && custom {
		return nil, fmt.Errorf("%w: permessage-deflate with a legacy frame extension", ErrUnsupportedExtensionComposition)
	}
	return pmd, nil
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
func (p *perMessageDeflate) closeReceive() {
	p.receiveLife.Lock()
	p.receiveClosed = true
	p.receive = nil
	p.receiveLife.Unlock()
}

const deflateWindow = 32768

var deflateTail = [...]byte{0, 0, 255, 255, 1, 0, 0, 255, 255}

type deflateReceiveState struct {
	inflater      io.ReadCloser
	history, seed []byte
	buffer        [4096]byte
}

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
type deflateFrameReader struct {
	c          *Conn
	frame      *Frame
	remaining  uint64
	phase      uint64
	peerClosed *bool
}

func (r *deflateFrameReader) start(frame *Frame, n uint64) error {
	r.frame, r.remaining, r.phase = frame, n, 0
	if frame.Masked {
		if _, err := io.ReadFull(r.c.rw, frame.MaskKey[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return io.ErrUnexpectedEOF
			}
			return fmt.Errorf("failed to read masking key: %w", err)
		}
	}
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
		f, n, err := r.c.readFrameHeader()
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		if err != nil {
			return err
		}
		if f.Opcode >= CloseMessage {
			if err := r.c.validateMessageHeader(f, n, true, 0); err != nil {
				return err
			}
			if err := r.c.readFramePayload(f, n); err != nil {
				return err
			}
			closed, err := r.c.handleControl(f)
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
		if err := r.start(f, n); err != nil {
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
	if uint64(len(p)) > r.remaining {
		p = p[:int(r.remaining)]
	}
	n, err := r.c.rw.Read(p)
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
	b, err := r.c.rw.ReadByte()
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
func (d *deflateReceiveState) save(output []byte, noContext bool) {
	if noContext {
		d.history = nil
		return
	}
	if len(output) >= deflateWindow {
		if cap(d.history) < deflateWindow {
			d.history = make([]byte, deflateWindow)
		} else {
			d.history = d.history[:deflateWindow]
		}
		copy(d.history, output[len(output)-deflateWindow:])
		return
	}
	keep := min(len(d.history), deflateWindow-len(output))
	need := keep + len(output)
	if cap(d.history) < need {
		next := make([]byte, need, min(deflateWindow, max(need, max(32, cap(d.history)*2))))
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
		d = new(deflateReceiveState)
	}
	noContext := p.peerNoContextTakeover()
	if noContext {
		d.history = nil
	}
	defer func() {
		if err == nil {
			err = d.inflater.(flate.Resetter).Reset(emptyDeflateInput{}, nil)
			if err == nil {
				d.save(out, noContext)
			}
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
	if d.inflater == nil {
		d.inflater = flate.NewReaderDict(input, d.history)
	} else if err = d.inflater.(flate.Resetter).Reset(input, d.history); err != nil {
		return nil, err
	}
	validated := 0
	for {
		nmax := len(d.buffer)
		remaining := int(^uint(0)>>1) - len(out)
		if limit > 0 {
			remaining = min(remaining, limit-len(out))
		}
		if remaining < nmax {
			nmax = remaining + 1
		}
		n, readErr := d.inflater.Read(d.buffer[:nmax])
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
					copy(carry[pending:], d.buffer[:used])
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
				complete, valid := validUTF8Prefix(d.buffer[offset:n])
				if !valid {
					return nil, failWith(StatusInvalidFramePayloadData, ErrInvalidFrame)
				}
				validated += complete
			}
			out = appendDecoded(out, d.buffer[:n], limit)
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
			return nil, failWith(StatusInvalidFramePayloadData, &compressedPayloadError{readErr})
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
		if err = d.inflater.(flate.Resetter).Reset(input, deflateSeed(d.history, out, &d.seed)); err != nil {
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
