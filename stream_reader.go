package websocket

import (
	"bytes"
	"errors"
	"io"
)

// errReaderSuperseded reports a NextReader reader used after a later
// NextReader or ReadMessage call discarded the rest of its message.
var errReaderSuperseded = errors.New("websocket: message reader superseded by a later read")

// NextReader waits for the next data message and returns its type and a
// reader for its payload, so a large message can be consumed without holding
// it in memory. The reader returns io.EOF at the end of the message. Control
// frames that arrive before or inside the message are handled as in
// ReadMessage, including the automatic Pong.
//
// Uncompressed messages are streamed frame by frame: text is checked as UTF-8
// as it arrives, and the connection's size limit bounds the whole message, so
// set MaxMessageSize on the Dialer, Upgrader or ConnConfig to suit the
// largest stream. A compressed message is still decoded whole before the
// reader returns its first byte; the same limit applies.
//
// Calling NextReader or ReadMessage again discards whatever is left of the
// previous message; the old reader then returns an error. Reads follow
// ReadMessage's error contract: a peer close returns a *CloseError, a read
// interrupted by Close or CloseWithStatus returns an error matching
// net.ErrClosed, and any other error, including one returned partway through
// a message, makes the connection terminal. Once Shutdown has started,
// NextReader discards data messages like ReadMessage, and a reader whose
// message Shutdown discarded returns Shutdown's outcome: the peer's
// *CloseError, or an error. The reader must not be used concurrently.
func (c *Conn) NextReader() (messageType MessageType, r io.Reader, err error) {
	for {
		op, mr, err := c.nextReader()
		if err != nil {
			return 0, nil, c.readError(err)
		}
		if !c.shuttingDown.Load() {
			return MessageType(op), mr, nil
		}
		// Shutdown has started: drop the message and keep reading until the
		// peer's Close frame or an error, as ReadMessage does.
		if _, err := io.Copy(io.Discard, mr); err != nil {
			return 0, nil, err
		}
	}
}

// nextReader starts reading one data message for NextReader.
func (c *Conn) nextReader() (opcode, *messageReader, error) {
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
	pmd, err := messageCompression(c.extensions)
	if err != nil {
		return 0, nil, c.failRead(err)
	}
	var frame wireFrame
	for {
		n, err := c.readFrameHeader(&frame)
		if err == nil {
			err = c.validateMessageHeader(&frame, n, false, 0)
		}
		if err != nil {
			return 0, nil, c.failRead(err)
		}
		switch frame.Opcode {
		case opClose, opPing, opPong:
			if err := c.readFramePayload(&frame, n); err != nil {
				return 0, nil, c.failRead(err)
			}
			peerClosed, err := c.handleControl(&frame)
			if peerClosed {
				return 0, nil, c.peerClose
			}
			if err != nil {
				return 0, nil, c.failRead(err)
			}
		case opText, opBinary:
			mr := &messageReader{c: c, text: frame.Opcode == opText}
			if pmd != nil && frame.Rsv1 {
				data, peerClosed, err := c.readCompressedMessage(pmd, &frame, n)
				if peerClosed {
					return 0, nil, c.peerClose
				}
				if err != nil {
					return 0, nil, c.failRead(err)
				}
				mr.decoded = bytes.NewReader(data)
			} else {
				mr.src = deflateFrameReader{c: c, peerClosed: &mr.peerClosed}
				if err := mr.src.start(&frame, n); err != nil {
					return 0, nil, c.failRead(err)
				}
			}
			c.reader = mr
			return frame.Opcode, mr, nil
		default:
			return 0, nil, c.failRead(failWith(StatusProtocolError, errInvalidOpcode))
		}
	}
}

// readError maps a read's error to ReadMessage's contract: the peer's answer
// to a local Close or CloseWithStatus is reported as net.ErrClosed.
func (c *Conn) readError(err error) error {
	if _, ok := err.(*CloseError); ok && c.localClosed.Load() {
		return errClosed
	}
	return err
}

// messageReader is one NextReader message. An uncompressed message streams
// from src: deflateFrameReader already unmasks payloads, handles interleaved
// controls and rejects anything but a continuation inside the message, and
// messageReader adds the message size limit and UTF-8 validation. A
// compressed message was decoded and validated whole, and replays from
// decoded. Methods other than Read run with readMu held.
type messageReader struct {
	c          *Conn
	src        deflateFrameReader
	decoded    *bytes.Reader // Non-nil for a compressed message.
	text       bool
	utf8       utf8Stream
	read       uint64 // Payload bytes returned so far.
	peerClosed bool
	err        error // Sticky once the message ends or fails.
}

func (r *messageReader) Read(p []byte) (int, error) {
	r.c.readMu.Lock()
	n, err := r.readLocked(p)
	r.c.readMu.Unlock()
	if err == errReaderSuperseded && r.c.shuttingDown.Load() {
		// Shutdown discarded this message; report how it ended.
		return 0, r.c.shutdownReadError()
	}
	return n, err
}

// shutdownReadError waits for the connection to close and returns what
// ReadMessage would have: the peer's Close, or net.ErrClosed. readMu must not
// be held, since Shutdown reads until the peer's Close frame arrives.
func (c *Conn) shutdownReadError() error {
	<-c.doneChan()
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.peerClose != nil {
		return c.peerClose
	}
	return errClosed
}

func (r *messageReader) readLocked(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.c.closed.Load() {
		return 0, r.end(errClosed)
	}
	if r.decoded != nil {
		n, err := r.decoded.Read(p)
		if err != nil {
			return n, r.end(err)
		}
		return n, nil
	}
	if err := r.src.advance(); err != nil {
		return 0, r.stop(err)
	}
	if limit := r.c.maxBytes; limit > 0 && r.src.phase == 0 && r.read+r.src.remaining > uint64(limit) {
		// A frame just began. Its header proves whether the message fits
		// before any of its payload is returned.
		return 0, r.fail(failWith(StatusMessageTooBig, ErrMessageTooLarge))
	}
	n, err := r.src.Read(p)
	if r.text && !r.utf8.check(p[:n]) {
		return 0, r.fail(failWith(StatusInvalidFramePayloadData, errInvalidFrame))
	}
	r.read += uint64(n)
	if err != nil {
		return 0, r.fail(err)
	}
	if r.src.remaining == 0 && r.src.frame.Final {
		return n, r.stop(io.EOF)
	}
	return n, nil
}

// stop ends the message on an error from advancing to the next frame: the
// final frame's end, a peer close inside the message, or a failure.
func (r *messageReader) stop(err error) error {
	switch {
	case r.peerClosed:
		return r.end(r.c.readError(r.c.peerClose))
	case err != io.EOF:
		return r.fail(err)
	case r.text && r.utf8.pending != 0:
		// https://datatracker.ietf.org/doc/html/rfc6455#section-8.1
		return r.fail(failWith(StatusInvalidFramePayloadData, errInvalidFrame))
	}
	return r.end(io.EOF)
}

// end finishes the message without failing the connection.
func (r *messageReader) end(err error) error {
	r.err = err
	if r.c.reader == r {
		r.c.reader = nil
	}
	return err
}

// fail makes the connection terminal, sending a close status when known.
func (r *messageReader) fail(err error) error {
	return r.end(r.c.failRead(err))
}

// discardReader reads and drops the rest of an unfinished NextReader message.
// readMu is held.
func (c *Conn) discardReader() error {
	r := c.reader
	if r == nil {
		return nil
	}
	var buf [4096]byte
	for {
		_, err := r.readLocked(buf[:])
		if err == nil {
			continue
		}
		if err == io.EOF {
			r.err = errReaderSuperseded
			return nil
		}
		return err
	}
}
