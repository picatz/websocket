package websocket

import (
	"errors"
	"fmt"
	"net"
)

// Errors returned by Dial and Upgrade and by the configuration they validate.
// They are often wrapped with detail, so test for them with errors.Is.
var (
	// ErrBadHandshake reports an opening handshake that failed because of the
	// other side. From Dial, the request could not be sent, the response could
	// not be read, or the server answered with something other than a valid
	// 101 response; Dial returns the server's response alongside it when one
	// was read. From Upgrade, the client's request was malformed, and Upgrade
	// has already replied with a 4xx status.
	ErrBadHandshake = errors.New("websocket: bad handshake")
	// ErrResponseHeaderTooLarge reports a handshake response head longer than
	// the Dialer.MaxResponseHeaderBytes limit. It is returned together with
	// ErrBadHandshake.
	ErrResponseHeaderTooLarge = errors.New("websocket: response header too large")
	// ErrProxy reports that the proxy chosen by Dialer.Proxy refused or failed
	// the CONNECT tunnel. It is returned together with ErrBadHandshake.
	ErrProxy = errors.New("websocket: proxy tunnel failed")
	// ErrInvalidSecAccept reports a server response whose
	// Sec-WebSocket-Accept does not match the request's key (RFC 6455
	// Section 4.1).
	ErrInvalidSecAccept = errors.New("websocket: invalid Sec-WebSocket-Accept header")
	// ErrFailedToGenerateKey reports that Dial could not read randomness for
	// the Sec-WebSocket-Key.
	ErrFailedToGenerateKey = errors.New("websocket: failed to generate key")

	// ErrOriginNotAllowed reports a request whose Origin the Upgrade origin
	// policy rejected. Upgrade has already replied 403 Forbidden.
	ErrOriginNotAllowed = errors.New("websocket: origin not allowed")
	// ErrInvalidMethod reports an upgrade request whose method is not GET.
	// Upgrade has already replied 405 Method Not Allowed with Allow: GET.
	ErrInvalidMethod = errors.New("websocket: invalid request method")
	// ErrUnsupportedVersion reports an upgrade request whose
	// Sec-WebSocket-Version is not 13. Upgrade has already replied 426
	// Upgrade Required with Sec-WebSocket-Version: 13 (RFC 6455 Section 4.4).
	ErrUnsupportedVersion = errors.New("websocket: unsupported WebSocket version")
	// ErrInvalidUpgradeHeader reports a handshake whose Upgrade field does not
	// name websocket.
	ErrInvalidUpgradeHeader = errors.New("websocket: invalid Upgrade header")
	// ErrInvalidConnectionHeader reports a handshake whose Connection field
	// does not include the upgrade token.
	ErrInvalidConnectionHeader = errors.New("websocket: invalid Connection header")
	// ErrMissingSecKey reports an upgrade request without a
	// Sec-WebSocket-Key.
	ErrMissingSecKey = errors.New("websocket: missing Sec-WebSocket-Key header")
	// ErrInvalidSecKey reports a Sec-WebSocket-Key that is not 16 bytes of
	// base64.
	ErrInvalidSecKey = errors.New("websocket: invalid Sec-WebSocket-Key header")
	// ErrNotHijacker reports a response writer that cannot hand over its
	// connection, such as an HTTP/2 stream. Upgrade has already replied 500
	// Internal Server Error.
	ErrNotHijacker = errors.New("websocket: response does not implement http.Hijacker")
	// ErrHandshakeFailed reports that Upgrade failed after it began taking
	// over the connection, while hijacking or writing the 101 response. It
	// writes no HTTP response, because the connection may no longer belong to
	// the server.
	ErrHandshakeFailed = errors.New("websocket: handshake failed")

	// ErrInvalidConfig reports a misconfigured Dialer, Upgrader or
	// Compression: the caller's mistake, not the peer's. Dial returns it
	// before connecting. Upgrade returns it before hijacking, after replying
	// 500 Internal Server Error.
	ErrInvalidConfig = errors.New("websocket: invalid configuration")
	// ErrInvalidHandshakeHeader reports a Header or Subprotocols field of a
	// Dialer or Upgrader that is malformed, conflicts with the other, or sets
	// a field the handshake manages. It matches ErrInvalidConfig.
	ErrInvalidHandshakeHeader = fmt.Errorf("%w: invalid custom handshake header", ErrInvalidConfig)
	// ErrInvalidExtension reports a malformed or unoffered
	// Sec-WebSocket-Extensions negotiation, or an extension that rejected it.
	ErrInvalidExtension = errors.New("websocket: invalid extension negotiation")
	// ErrInvalidSubprotocol reports a subprotocol list that is not unique HTTP
	// tokens, or a server selection that the client did not offer.
	ErrInvalidSubprotocol = errors.New("websocket: invalid subprotocol negotiation")
)

// Errors returned by ReadMessage when the peer violates the protocol. The
// connection is terminal after any of them; when it is safe, the peer is sent
// the matching close status first.
var (
	// ErrInvalidFrame reports a malformed frame or close payload, or text that
	// is not valid UTF-8. WriteMessage also returns it, before any I/O, for a
	// text message that is not valid UTF-8.
	ErrInvalidFrame = errors.New("websocket: invalid frame")
	// ErrUnexpectedFrame reports a new data frame where a continuation of a
	// fragmented message was required.
	ErrUnexpectedFrame = errors.New("websocket: unexpected frame")
	// ErrUnexpectedContinuation reports a continuation frame outside a
	// fragmented message.
	ErrUnexpectedContinuation = errors.New("websocket: unexpected continuation frame")
	// ErrControlFrameFragment reports a fragmented control frame
	// (RFC 6455 Section 5.5).
	ErrControlFrameFragment = errors.New("websocket: control frames cannot be fragmented")
	// ErrPayloadTooLarge reports a data frame or message larger than the
	// connection's incoming limit, for which the peer is sent
	// StatusMessageTooBig, or a control frame with more than 125 bytes of
	// payload.
	ErrPayloadTooLarge = errors.New("websocket: payload too large")
	// ErrUnmaskedFrame reports an unmasked frame from a client
	// (RFC 6455 Section 5.1).
	ErrUnmaskedFrame = errors.New("websocket: received unmasked frame from client")
	// ErrMaskedFrame reports a masked frame from a server
	// (RFC 6455 Section 5.1).
	ErrMaskedFrame = errors.New("websocket: received masked frame from server")
	// ErrUnsupportedExtensions reports reserved bits that no negotiated
	// extension defines.
	ErrUnsupportedExtensions = errors.New("websocket: unsupported extensions (RSV bits are set)")
	// ErrInvalidOpcode reports a frame with a reserved opcode.
	ErrInvalidOpcode = errors.New("websocket: invalid opcode")
)

// errInvalidMessageType is returned by WriteMessage for a MessageType other
// than TextMessage or BinaryMessage.
var errInvalidMessageType = errors.New("websocket: invalid message type")

// Errors returned by writes and Close.
var (
	// ErrWriteFailed reports that writing to the transport failed. The error
	// also wraps the transport's error. The connection cannot be written to
	// again, because a partial frame may have been sent; close it.
	ErrWriteFailed = errors.New("websocket: write failed")
	// ErrAlreadyClosed is returned by Close and CloseWithStatus on a
	// connection that is already closed. It matches net.ErrClosed.
	ErrAlreadyClosed error = &closedError{"websocket: connection already closed"}
)

// errClosed is returned by operations on a connection that has been closed or
// aborted, or whose closing handshake has begun. It matches net.ErrClosed,
// the standard library's "use of closed connection" error.
var errClosed error = &closedError{"websocket: use of closed connection"}

// closedError is the type of errClosed and ErrAlreadyClosed.
type closedError struct{ msg string }

func (e *closedError) Error() string { return e.msg }

// Is reports whether target is net.ErrClosed.
func (e *closedError) Is(target error) bool { return target == net.ErrClosed }
