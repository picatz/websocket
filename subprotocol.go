package websocket

import (
	"fmt"
	"net/http"
	"slices"
)

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
