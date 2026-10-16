package websocket

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

// StatusCode is a WebSocket close status code. The Status constants are
// untyped, so they can be used as a StatusCode or as any integer type.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-7.4
type StatusCode int

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

// CloseError is the peer's close frame, returned by ReadMessage (and
// ReadMessageContext) when the peer starts or completes the closing handshake.
// A close frame without a body reports StatusNoStatusReceived and an empty
// reason. CloseError matches io.EOF with errors.Is, so code that checks
// errors.Is(err, io.EOF) for an orderly close keeps working.
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

// Is reports whether target is io.EOF, which a peer close has always matched.
func (e *CloseError) Is(target error) bool { return target == io.EOF }

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
// exactly like Close, including the one-second best-effort notification and
// ErrAlreadyClosed on repeated calls.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-7.1.2
func (c *Conn) CloseWithStatus(code StatusCode, reason string) error {
	payload, err := closePayload(code, reason)
	if err != nil {
		return err
	}
	return c.closeWithPayload(payload)
}
