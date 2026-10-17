package websocket

import (
	"errors"
	"fmt"
	"net"
)

// Errors returned by Dial, Upgrade and NewConn. They are wrapped with
// detail, so test for them with errors.Is.
var (
	// ErrBadHandshake reports an opening handshake that failed because of the
	// other side or the network. From Dial, the server could not be reached,
	// the request could not be sent, or the server answered with something
	// other than a valid 101 response; Dial returns the server's response
	// alongside it when one was read. From Upgrade, the client's request was
	// malformed and Upgrade has already replied with a 4xx status, or the
	// connection failed after Upgrade took it over.
	ErrBadHandshake = errors.New("websocket: bad handshake")
	// ErrOriginNotAllowed reports a request whose Origin the Upgrade origin
	// policy rejected. Upgrade has already replied 403 Forbidden.
	ErrOriginNotAllowed = errors.New("websocket: origin not allowed")
	// ErrUnsupportedVersion reports an upgrade request whose
	// Sec-WebSocket-Version is not 13, typically from an outdated client.
	// Upgrade has already replied 426 Upgrade Required with
	// Sec-WebSocket-Version: 13 (RFC 6455 Section 4.4). It is returned
	// together with ErrBadHandshake.
	ErrUnsupportedVersion = errors.New("websocket: unsupported WebSocket version")
	// ErrInvalidConfig reports a misconfigured Dialer, Upgrader, Compression
	// or ConnConfig: the caller's mistake, not the peer's. Dial returns it
	// before connecting, NewConn before taking over the connection, and
	// Upgrade before hijacking, after replying 500 Internal Server Error.
	ErrInvalidConfig = errors.New("websocket: invalid configuration")
	// ErrNotHijacker reports a response writer that cannot hand over its
	// connection: an HTTP/2 stream, or middleware that wraps the
	// ResponseWriter without supporting http.ResponseController. Upgrade has
	// already replied 500 Internal Server Error. It matches ErrInvalidConfig.
	ErrNotHijacker = fmt.Errorf("%w: response does not implement http.Hijacker (HTTP/2 or a wrapped ResponseWriter)", ErrInvalidConfig)
)

// Handshake failure details. Dial, Upgrade and NewConn return them wrapped
// in ErrBadHandshake or ErrInvalidConfig; their text names the problem.
var (
	errResponseHeaderTooLarge  = errors.New("websocket: response header too large")
	errProxy                   = errors.New("websocket: proxy tunnel failed")
	errInvalidSecAccept        = errors.New("websocket: invalid Sec-WebSocket-Accept header")
	errFailedToGenerateKey     = errors.New("websocket: failed to generate key")
	errInvalidMethod           = errors.New("websocket: invalid request method")
	errInvalidUpgradeHeader    = errors.New("websocket: invalid Upgrade header")
	errInvalidConnectionHeader = errors.New("websocket: invalid Connection header")
	errMissingSecKey           = errors.New("websocket: missing Sec-WebSocket-Key header")
	errInvalidSecKey           = errors.New("websocket: invalid Sec-WebSocket-Key header")
	errInvalidExtension        = errors.New("websocket: invalid extension negotiation")
	errInvalidSubprotocol      = errors.New("websocket: invalid subprotocol negotiation")
	// errHandshakeFailed reports that Upgrade failed after it began taking
	// over the connection, so it writes no HTTP response.
	errHandshakeFailed error = fmt.Errorf("%w: connection failed during upgrade", ErrBadHandshake)
	// errInvalidHandshakeHeader reports a Header or Subprotocols field of a
	// Dialer or Upgrader that is malformed, conflicts with the other, or sets
	// a field the handshake manages.
	errInvalidHandshakeHeader = fmt.Errorf("%w: invalid custom handshake header", ErrInvalidConfig)
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

// errWriteFailed reports that writing to the transport failed, and wraps the
// transport's error. The connection cannot be written to again, because a
// partial frame may have been sent.
var errWriteFailed = errors.New("websocket: write failed")

// errAlreadyClosed is returned by Close and CloseWithStatus on a connection
// that is already closed. It matches net.ErrClosed.
var errAlreadyClosed error = &closedError{"websocket: connection already closed"}

// errClosed is returned by operations on a connection that has been closed or
// aborted, or whose closing handshake has begun. It matches net.ErrClosed,
// the standard library's "use of closed connection" error.
var errClosed error = &closedError{"websocket: use of closed connection"}

// closedError is the type of errClosed and errAlreadyClosed.
type closedError struct{ msg string }

func (e *closedError) Error() string { return e.msg }

// Is reports whether target is net.ErrClosed.
func (e *closedError) Is(target error) bool { return target == net.ErrClosed }
