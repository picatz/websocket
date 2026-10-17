package websocket

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"unicode/utf8"
)

// StatusCode is a WebSocket close status code.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-7.4
type StatusCode int

// Close status codes from RFC 6455 and the IANA WebSocket Close Code Number
// Registry. StatusNoStatusReceived, StatusAbnormalClosure and
// StatusTLSHandshake are local-only indicators and must not appear on the wire;
// CloseWithStatus rejects them.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-7.4.1
// https://www.iana.org/assignments/websocket/websocket.xhtml#close-code-number
const (
	StatusNormalClosure           StatusCode = 1000
	StatusGoingAway               StatusCode = 1001
	StatusProtocolError           StatusCode = 1002
	StatusUnsupportedData         StatusCode = 1003
	StatusNoStatusReceived        StatusCode = 1005
	StatusAbnormalClosure         StatusCode = 1006
	StatusInvalidFramePayloadData StatusCode = 1007
	StatusPolicyViolation         StatusCode = 1008
	StatusMessageTooBig           StatusCode = 1009
	StatusMandatoryExtension      StatusCode = 1010
	StatusInternalServerError     StatusCode = 1011
	StatusServiceRestart          StatusCode = 1012 // IANA registry
	StatusTryAgainLater           StatusCode = 1013 // IANA registry
	StatusBadGateway              StatusCode = 1014 // IANA registry
	StatusTLSHandshake            StatusCode = 1015
)

// String returns the RFC 6455 name of a registered code, or its number.
func (s StatusCode) String() string {
	switch s {
	case StatusNormalClosure:
		return "StatusNormalClosure"
	case StatusGoingAway:
		return "StatusGoingAway"
	case StatusProtocolError:
		return "StatusProtocolError"
	case StatusUnsupportedData:
		return "StatusUnsupportedData"
	case StatusNoStatusReceived:
		return "StatusNoStatusReceived"
	case StatusAbnormalClosure:
		return "StatusAbnormalClosure"
	case StatusInvalidFramePayloadData:
		return "StatusInvalidFramePayloadData"
	case StatusPolicyViolation:
		return "StatusPolicyViolation"
	case StatusMessageTooBig:
		return "StatusMessageTooBig"
	case StatusMandatoryExtension:
		return "StatusMandatoryExtension"
	case StatusInternalServerError:
		return "StatusInternalServerError"
	case StatusServiceRestart:
		return "StatusServiceRestart"
	case StatusTryAgainLater:
		return "StatusTryAgainLater"
	case StatusBadGateway:
		return "StatusBadGateway"
	case StatusTLSHandshake:
		return "StatusTLSHandshake"
	default:
		return "StatusCode(" + strconv.Itoa(int(s)) + ")"
	}
}

// maxCloseReason is the largest reason that fits in a control frame after the
// two-byte status code.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-5.5
const maxCloseReason = 125 - 2

// ErrInvalidCloseStatus reports a status code or reason that CloseWithStatus
// cannot send.
var ErrInvalidCloseStatus = errors.New("websocket: invalid close status")

// CloseError is the peer's close frame, returned by ReadMessage,
// ReadMessageContext and NextReader when the peer starts or completes the
// closing handshake.
// A close frame without a body reports StatusNoStatusReceived and an empty
// reason. A normal close (StatusNormalClosure, StatusGoingAway or no status)
// matches io.EOF with errors.Is, so a read loop can treat it as the end of the
// stream; any other status does not. CloseStatus reads the code of either.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-7.1.5
type CloseError struct {
	Code   StatusCode
	Reason string
}

// Error returns a description of the close status and reason.
func (e *CloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("websocket: peer closed with status %d (%v)", int(e.Code), e.Code)
	}
	return fmt.Sprintf("websocket: peer closed with status %d (%v): %q", int(e.Code), e.Code, e.Reason)
}

// Is reports whether target is io.EOF and the peer closed normally, with
// StatusNormalClosure, StatusGoingAway or no status (StatusNoStatusReceived).
// A close with any other status is not an end of file; use CloseStatus or
// errors.As to read its code.
func (e *CloseError) Is(target error) bool {
	if target != io.EOF {
		return false
	}
	switch e.Code {
	case StatusNormalClosure, StatusGoingAway, StatusNoStatusReceived:
		return true
	}
	return false
}

// CloseStatus returns the status code of the first CloseError in err's tree,
// or -1 if there is none.
func CloseStatus(err error) StatusCode {
	if ce, ok := errors.AsType[*CloseError](err); ok {
		return ce.Code
	}
	return -1
}

// newCloseError decodes a close payload that validateIncomingClosePayload has
// already accepted.
func newCloseError(payload []byte) *CloseError {
	if len(payload) < 2 {
		return &CloseError{Code: StatusNoStatusReceived}
	}
	return &CloseError{
		Code:   StatusCode(binary.BigEndian.Uint16(payload)),
		Reason: string(payload[2:]),
	}
}

// closePayload encodes a status code and reason that a local endpoint may send.
// StatusNoStatusReceived, StatusAbnormalClosure and StatusTLSHandshake are
// local-only indicators and are rejected, like other reserved or unassigned
// codes; the reason must be valid UTF-8 of at most 123 bytes.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-7.4.2
func closePayload(code StatusCode, reason string) ([]byte, error) {
	if code < 1000 || code > 4999 {
		return nil, fmt.Errorf("%w: code %d out of range", ErrInvalidCloseStatus, int(code))
	}
	if len(reason) > maxCloseReason {
		return nil, fmt.Errorf("%w: reason is %d bytes, limit is %d", ErrInvalidCloseStatus, len(reason), maxCloseReason)
	}
	if !utf8.ValidString(reason) {
		return nil, fmt.Errorf("%w: reason is not valid UTF-8", ErrInvalidCloseStatus)
	}
	payload := binary.BigEndian.AppendUint16(make([]byte, 0, 2+len(reason)), uint16(code))
	payload = append(payload, reason...)
	if validateIncomingClosePayload(payload) != nil {
		return nil, fmt.Errorf("%w: code %d is reserved", ErrInvalidCloseStatus, int(code))
	}
	return payload, nil
}

// CloseWithStatus is Close with an explicit status code and reason. It
// validates both before any I/O: an invalid status returns an error matching
// ErrInvalidCloseStatus and leaves the connection open. Otherwise it behaves
// exactly like Close, including the one-second best-effort notification and the
// net.ErrClosed match on repeated calls.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-7.1.2
func (c *Conn) CloseWithStatus(code StatusCode, reason string) error {
	payload, err := closePayload(code, reason)
	if err != nil {
		return err
	}
	c.localClosed.Store(true)
	return c.closeWithPayload(payload)
}

// CloseWrite starts the closing handshake and keeps reading: it sends a Close
// frame with code and reason and ends the write side, like
// net.TCPConn.CloseWrite. Later writes, Pings and Close frames return an error
// matching net.ErrClosed; Pongs that ReadMessage sends in reply to Pings still
// go out. ReadMessage keeps delivering the messages the peer sent before it
// answered, then returns the peer's *CloseError, and the connection closes.
//
// CloseWrite does not wait for the peer. Bound the drain with SetReadDeadline
// or ReadMessageContext, and call Close if the peer never answers. Use
// Shutdown instead to discard the peer's remaining messages and wait for its
// answer in one call.
//
// An invalid status returns an error matching ErrInvalidCloseStatus before
// any I/O and leaves the connection open. If a Close frame was already sent
// or the connection is closed, CloseWrite returns an error matching
// net.ErrClosed.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-7.1.2
func (c *Conn) CloseWrite(code StatusCode, reason string) error {
	payload, err := closePayload(code, reason)
	if err != nil {
		return err
	}
	return c.writeControlFrame(opClose, payload)
}

// Shutdown closes the connection with the closing handshake: it sends a Close
// frame with code and reason, waits for the peer's Close frame, and then
// closes the transport. It returns nil once the peer has answered.
//
// Unlike Close, Shutdown does not hang up while the peer may still be sending.
// Closing a TCP connection with unread data makes the operating system reset
// it, so the peer can see a connection reset instead of the status code and
// lose frames it had already sent. Shutdown keeps reading until the peer's
// Close frame arrives, which also lets the server be the first to close the
// TCP connection, as RFC 6455 Section 7.1.1 recommends.
//
// Shutdown ends both directions at once. After the Close frame is sent,
// writes fail with an error matching net.ErrClosed, and data messages that
// ReadMessage has not yet returned are read and discarded, whichever
// goroutine reads them. A concurrent read loop therefore receives no more
// messages once Shutdown starts; its ReadMessage returns the peer's
// *CloseError, or an error matching net.ErrClosed if Shutdown read the
// peer's Close frame first.
//
// ctx bounds the whole call, including any wait for a concurrent writer, so
// give it a deadline. Shutdown always leaves the connection closed. If ctx
// ends before the peer answers, or the peer drops the connection without a
// Close frame, Shutdown closes the connection without finishing the handshake
// and returns an error, matching ctx's cause in the first case; a ctx that is
// already done closes it at once. An invalid status returns an error matching
// ErrInvalidCloseStatus before any I/O and leaves the connection open, as for
// CloseWithStatus. On a connection that is already closed, Shutdown returns an
// error matching net.ErrClosed.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-7.1.2
func (c *Conn) Shutdown(ctx context.Context, code StatusCode, reason string) error {
	payload, err := closePayload(code, reason)
	if err != nil {
		return err
	}
	if c.closed.Load() {
		return errAlreadyClosed
	}
	if err := ctx.Err(); err != nil {
		c.cancelIO()
		return fmt.Errorf("websocket: closing handshake not completed: %w", context.Cause(ctx))
	}
	stop := context.AfterFunc(ctx, c.cancelIO)
	defer stop()

	c.shuttingDown.Store(true)
	if err := c.writeControlFrame(opClose, payload); err != nil {
		switch {
		case ctx.Err() != nil, errors.Is(err, errWriteFailed), !errors.Is(err, net.ErrClosed):
			c.cancelIO()
			return c.shutdownError(ctx, err)
		case c.closed.Load():
			return errAlreadyClosed
		}
		// An earlier writeControlFrame already sent a Close frame, so the
		// handshake is under way: wait for the peer's answer.
	}

	// ReadMessage discards data messages while shutting down, so it returns
	// only once the peer's Close frame arrives or the connection fails. A
	// concurrent read loop may get there first; then this call waits for it
	// and returns as soon as the connection is closed.
	_, _, _ = c.ReadMessage()
	<-c.doneChan()
	if c.peerClosed.Load() {
		return nil
	}
	c.cancelIO()
	return c.shutdownError(ctx, io.ErrUnexpectedEOF)
}

// shutdownError reports an unfinished closing handshake, preferring ctx's
// cause when ctx is what ended it.
func (c *Conn) shutdownError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("websocket: closing handshake not completed: %w: %w", context.Cause(ctx), err)
	}
	return fmt.Errorf("websocket: closing handshake not completed: %w", err)
}
