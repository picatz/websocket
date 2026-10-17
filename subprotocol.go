package websocket

import (
	"fmt"
	"net/http"
	"slices"
)

// WithSubprotocols offers application subprotocols in Dial's handshake, in
// order of preference. Each must be a unique, case-sensitive HTTP token. The
// server selects at most one; Conn.Subprotocol reports it. Dial rejects a
// selection that was not offered with ErrInvalidSubprotocol.
//
// The option replaces a Sec-WebSocket-Protocol field set through WithHeader:
// setting both returns an error matching ErrInvalidHandshakeHeader and
// ErrInvalidSubprotocol before connecting. When repeated, the last option wins;
// no arguments removes the offer.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-4.1
func WithSubprotocols(protocols ...string) DialOption {
	protocols = cloneSubprotocols(protocols)
	return func(opts *dialOptions) {
		opts.subprotocols = protocols
	}
}

// WithUpgradeSubprotocols sets the subprotocols the server supports, in order
// of preference. Upgrade selects the first of these the client offered, or none
// if the client offered none of them; RFC 6455 allows a server to omit the
// selection, and the application decides whether to continue without one.
// Conn.Subprotocol reports the selection.
//
// Each must be a unique, case-sensitive HTTP token. The option replaces a
// Sec-WebSocket-Protocol field set through WithResponseHeader: setting both
// returns an error matching ErrInvalidHandshakeHeader and ErrInvalidSubprotocol
// before hijacking. When repeated, the last option wins; no arguments removes
// the server's list.
//
// https://datatracker.ietf.org/doc/html/rfc6455#section-4.2.2
func WithUpgradeSubprotocols(protocols ...string) UpgradeOption {
	protocols = cloneSubprotocols(protocols)
	return func(opts *upgradeOptions) {
		opts.subprotocols = protocols
	}
}

// Subprotocol returns the subprotocol negotiated during Dial or Upgrade, or ""
// if none was selected. It is fixed after the handshake and safe to call
// concurrently. A Conn from NewConn reports "".
func (c *Conn) Subprotocol() string {
	return c.subprotocol
}

// cloneSubprotocols detaches the caller's slice and normalizes an empty list
// to nil, meaning the option is unset.
func cloneSubprotocols(protocols []string) []string {
	if len(protocols) == 0 {
		return nil
	}
	return slices.Clone(protocols)
}

// validateSubprotocolOption checks a configured list before any I/O. It also
// rejects a raw Sec-WebSocket-Protocol field in header, which the option owns.
func validateSubprotocolOption(protocols []string, header http.Header) error {
	if len(subprotocolValues(header)) != 0 {
		return fmt.Errorf("%w: Sec-WebSocket-Protocol is set by both a header and the subprotocols option", ErrInvalidSubprotocol)
	}
	for i, protocol := range protocols {
		if !isHTTPToken(protocol) {
			return fmt.Errorf("%w: %q is not an HTTP token", ErrInvalidSubprotocol, protocol)
		}
		if slices.Contains(protocols[:i], protocol) {
			return fmt.Errorf("%w: %q is listed twice", ErrInvalidSubprotocol, protocol)
		}
	}
	return nil
}

// selectSubprotocol returns the first supported protocol the client offered.
func selectSubprotocol(supported []string, offered map[string]struct{}) string {
	for _, protocol := range supported {
		if _, ok := offered[protocol]; ok {
			return protocol
		}
	}
	return ""
}
