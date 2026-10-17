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

// Errors returned by ReadMessage and NextReader when a message cannot be
// accepted. The connection is terminal after either; when it is safe, the
// peer is sent the matching close status first.
var (
	// ErrProtocol reports a peer that violated RFC 6455 or RFC 7692: a
	// malformed, masked or unmasked, fragmented control or reserved frame, an
	// unexpected continuation, text that is not valid UTF-8, or compressed
	// data that does not inflate. The error's text names the violation.
	ErrProtocol = errors.New("websocket: protocol error")
	// ErrMessageTooLarge reports an incoming frame or message larger than the
	// connection's MaxMessageSize, for which the peer is sent
	// StatusMessageTooBig. It matches ErrProtocol, so one check covers every
	// message the connection refused.
	ErrMessageTooLarge error = &protocolError{"websocket: message too large"}
)

// Peer protocol violations, all matching ErrProtocol. Callers cannot act on
// which one occurred, so they stay unexported; their text says which.
var (
	errInvalidFrame           error = &protocolError{"websocket: invalid frame"}
	errUnexpectedFrame        error = &protocolError{"websocket: unexpected frame"}
	errUnexpectedContinuation error = &protocolError{"websocket: unexpected continuation frame"}
	errControlFrameFragment   error = &protocolError{"websocket: control frames cannot be fragmented"}
	errUnmaskedFrame          error = &protocolError{"websocket: received unmasked frame from client"}
	errMaskedFrame            error = &protocolError{"websocket: received masked frame from server"}
	errUnsupportedExtensions  error = &protocolError{"websocket: unsupported extensions (RSV bits are set)"}
	errInvalidOpcode          error = &protocolError{"websocket: invalid opcode"}
)

// protocolError is the type of the errors that match ErrProtocol.
type protocolError struct{ msg string }

func (e *protocolError) Error() string { return e.msg }

// Is reports whether target is ErrProtocol.
func (e *protocolError) Is(target error) bool { return target == ErrProtocol }

// errInvalidUTF8 is returned by WriteMessage and NextWriter, before any I/O,
// for text that is not valid UTF-8. The caller made the mistake, so it does
// not match ErrProtocol.
var errInvalidUTF8 = errors.New("websocket: text message is not valid UTF-8")

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
