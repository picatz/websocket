package websocket

import (
	"fmt"
)

// writeFragmentSize is the payload size at which a compressed message is cut
// into another frame. It bounds the encoded bytes buffered per message
// without adding meaningful framing overhead.
const writeFragmentSize = 64 << 10

// messageWriter frames one outgoing data message as one or more frames
// (RFC 6455 section 5.4). The caller holds writeMu for the writer's whole
// lifetime. Write may emit complete frames; Close emits the final frame.
//
// With built-in compression, the first frame carries RSV1 and every frame
// carries part of a single DEFLATE stream (RFC 7692 section 6). A message
// shorter than the configured threshold is sent uncompressed instead, which
// section 6 also permits; the decision is made once that much input is
// buffered, or at Close.
//
// WriteMessage drives a messageWriter with a single Write. It is also the seam
// for a streaming writer that returns it to applications as an io.WriteCloser.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-5.4
// https://www.rfc-editor.org/rfc/rfc7692#section-6
type messageWriter struct {
	c         *Conn
	opcode    Opcode             // The next frame's opcode: the message type, then ContinuationFrame.
	pmd       *perMessageDeflate // Compression extension, or nil.
	compress  bool               // Compression decided and started for this message.
	frameExts []frameExtension   // Enabled custom extensions, applied to each frame in order.
	buf       []byte             // Encoded (or raw, before a decision) bytes not yet framed.
	sent      bool               // At least one frame was handed to the transport.
	err       error
}

// newMessageWriter starts a message. enabled holds the connection's enabled
// extensions, as resolved once for this message.
func (c *Conn) newMessageWriter(opcode Opcode, enabled []frameExtension) *messageWriter {
	w := &messageWriter{c: c, opcode: opcode, buf: c.writeBuf[:0]}
	for _, ext := range enabled {
		if pmd, ok := ext.(*perMessageDeflate); ok {
			w.pmd = pmd
		} else {
			w.frameExts = append(w.frameExts, ext)
		}
	}
	if w.pmd != nil && w.pmd.threshold == 0 {
		w.startCompression()
	}
	return w
}

func (w *messageWriter) startCompression() {
	w.compress = true
	s := w.pmd.sender()
	s.begin()
	if len(w.buf) > 0 {
		// Compress the raw prefix buffered while below the threshold.
		raw := w.buf
		w.buf = s.write(w.c.writeScratch[:0], raw)
		w.c.writeScratch = raw[:0]
	}
}

// Write buffers or compresses p and sends every complete fragment.
func (w *messageWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	switch {
	case w.compress:
		w.buf = w.pmd.send.write(w.buf, p)
	case w.pmd != nil && len(w.buf)+len(p) >= w.pmd.threshold:
		w.buf = append(w.buf, p...)
		w.startCompression()
	default:
		w.buf = append(w.buf, p...)
	}
	for len(w.buf) >= writeFragmentSize+1 {
		// Keep at least one byte so the final frame is never empty. This also
		// keeps the frame boundary away from Close's flushed tail.
		if err := w.frame(false, w.buf[:writeFragmentSize]); err != nil {
			return 0, err
		}
		w.buf = w.buf[:copy(w.buf, w.buf[writeFragmentSize:])]
	}
	return len(p), nil
}

// Close finishes the message with a final frame.
func (w *messageWriter) Close() error {
	if w.err != nil {
		return w.err
	}
	if w.compress {
		w.buf = w.pmd.send.end(w.buf)
	}
	for len(w.buf) > writeFragmentSize {
		if err := w.frame(false, w.buf[:writeFragmentSize]); err != nil {
			return err
		}
		w.buf = w.buf[:copy(w.buf, w.buf[writeFragmentSize:])]
	}
	err := w.frame(true, w.buf)
	w.release()
	return err
}

// frame passes one frame through the custom extensions and writes it.
func (w *messageWriter) frame(final bool, payload []byte) error {
	frame := &wireFrame{
		Final:   final,
		Opcode:  w.opcode,
		Payload: payload,
		Masked:  !w.c.isServer,
		Rsv1:    w.compress && w.opcode != ContinuationFrame,
	}
	rsv1 := frame.Rsv1
	for _, ext := range w.frameExts {
		if err := ext.ProcessOutgoingFrame(frame); err != nil {
			return w.fail(fmt.Errorf("extension %s failed to process outgoing frame: %w", ext.Name(), err))
		}
	}
	// RFC 7692 section 5: permessage-deflate owns RSV1 for data frames.
	if w.pmd != nil && frame.Rsv1 != rsv1 {
		return w.fail(fmt.Errorf("%w: an extension changed RSV1 owned by permessage-deflate", errUnsupportedExtensionComposition))
	}
	w.opcode = ContinuationFrame
	w.sent = true
	// Without frame extensions the payload is our own buffer; mask in place.
	if err := w.c.writeFrameBuffer(frame, len(w.frameExts) == 0); err != nil {
		return w.fail(err)
	}
	return nil
}

// fail records a terminal error. Once a frame was sent, or compression has
// consumed input into a context that outlives this message, the frame stream
// can no longer continue, so later writes are poisoned like a transport error.
func (w *messageWriter) fail(err error) error {
	if w.err == nil {
		w.err = err
		if w.c.writeErr == nil && (w.sent || (w.compress && w.pmd.send.retention != retainNothing)) {
			w.c.writeErr = err
		}
		if w.compress {
			w.pmd.send.release()
		}
		w.release()
	}
	return w.err
}

// release keeps a modest buffer for the connection's next message.
func (w *messageWriter) release() {
	if cap(w.buf) <= 2*writeFragmentSize {
		w.c.writeBuf = w.buf[:0]
	}
	w.buf = nil
}
