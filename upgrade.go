package websocket

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// upgradeOptions is an Upgrader resolved for one Upgrade.
type upgradeOptions struct {
	extensions     []frameExtension
	responseHeader http.Header
	maxBytes       int
	originCheck    func(*http.Request, string) bool
	subprotocols   []string
}

// upgrade performs the server handshake for a resolved Upgrader.
func upgrade(w http.ResponseWriter, r *http.Request, opts *upgradeOptions) (*Conn, error) {
	// Each step is its own function so that only one step's locals are on
	// the stack at a time. The handler goroutine keeps whatever stack the
	// handshake grew to for the connection's lifetime.
	acceptKey, subprotocol, err := checkUpgradeRequest(r, opts)
	if err != nil {
		return nil, err
	}
	extensions, err := negotiateUpgradeExtensions(r, opts)
	if err != nil {
		return nil, err
	}

	// Preserve direct Hijacker errors and use the standard controller to reach
	// hijacking support through middleware that exposes Unwrap.
	hj, ok := w.(http.Hijacker)
	if !ok {
		hj = http.NewResponseController(w)
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		if !ok && errors.Is(err, http.ErrNotSupported) {
			return nil, ErrNotHijacker
		}
		return nil, fmt.Errorf("%w: %w", errHandshakeFailed, err)
	}
	if err := writeUpgradeResponse(bufrw.Writer, acceptKey, opts, extensions, subprotocol); err != nil {
		conn.Close()
		return nil, err
	}

	// Keep the hijacked reader, which may already hold the client's first
	// frames. The hijacked writer is released: frames are written through a
	// shared, pooled buffer only while one is being written.
	ws := newConn(conn, bufrw.Reader, true, opts.extensions, opts.maxBytes)
	ws.subprotocol = subprotocol
	return ws, nil
}

// checkUpgradeRequest validates the client's opening handshake and the
// caller's response fields. It returns the Sec-WebSocket-Accept value and the
// selected subprotocol, if any.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-4.2.1
func checkUpgradeRequest(r *http.Request, opts *upgradeOptions) (string, string, error) {
	if err := validateHandshakeHeaders(opts.responseHeader, true); err != nil {
		return "", "", err
	}

	if !headerContains(r.Header, "Connection", "Upgrade") {
		return "", "", errInvalidConnectionHeader
	}
	if !headerContains(r.Header, "Upgrade", "websocket") {
		return "", "", errInvalidUpgradeHeader
	}
	if r.Method != http.MethodGet {
		return "", "", errInvalidMethod
	}
	if !r.ProtoAtLeast(1, 1) {
		return "", "", ErrBadHandshake
	}
	// Header keys are indexed in canonical form directly: Get and Values
	// canonicalize their argument, which allocates for "WebSocket".
	if version := r.Header["Sec-Websocket-Version"]; len(version) != 1 || version[0] != "13" {
		return "", "", ErrUnsupportedVersion
	}
	keys := r.Header["Sec-Websocket-Key"]
	if len(keys) == 0 || keys[0] == "" {
		return "", "", errMissingSecKey
	}
	key := keys[0]
	if len(keys) != 1 || !validChallengeKey(key) {
		return "", "", errInvalidSecKey
	}
	offered, err := parseSubprotocolOffers(r.Header)
	if err != nil {
		return "", "", err
	}
	if opts.subprotocols != nil {
		if err := validateSubprotocolOption(opts.subprotocols, opts.responseHeader); err != nil {
			return "", "", fmt.Errorf("%w: %w", errInvalidHandshakeHeader, err)
		}
	}
	subprotocol, err := validateSubprotocolSelection(opts.responseHeader, offered)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", errInvalidHandshakeHeader, err)
	}
	if subprotocol == "" {
		subprotocol = selectSubprotocol(opts.subprotocols, offered)
	}
	if !upgradeOriginAllowed(r, opts.originCheck) {
		return "", "", ErrOriginNotAllowed
	}
	return computeAcceptKey(key), subprotocol, nil

}

// negotiateUpgradeExtensions negotiates the configured extensions with the
// client's offer and returns the response's Sec-WebSocket-Extensions value,
// or "" when none was accepted.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-9.1
func negotiateUpgradeExtensions(r *http.Request, opts *upgradeOptions) (string, error) {
	extValues := extensionValues(r.Header)
	offeredExtensions, err := parseExtensions(extValues)
	if err != nil {
		return "", err
	}
	extHeader := strings.Join(extValues, ", ")
	var acceptedExtensions []frameExtension
	for _, ext := range opts.extensions {
		if ext == nil {
			return "", fmt.Errorf("%w: nil extension", errInvalidExtension)
		}
		var err error
		if pmd, ok := ext.(*perMessageDeflate); ok {
			err = pmd.negotiate(offeredExtensions, true)
		} else {
			err = ext.Negotiate(extHeader)
		}
		if err != nil {
			return "", fmt.Errorf("%w: extension %s: %w", errInvalidExtension, ext.Name(), err)
		}
		if ext.IsEnabled() {
			acceptedExtensions = append(acceptedExtensions, ext)
		}
	}
	if _, err := messageCompression(opts.extensions); err != nil {
		return "", fmt.Errorf("%w: %w", errInvalidExtension, err)
	}
	selection, selectedExtensions, err := extensionOffers(acceptedExtensions)
	if err == nil {
		err = validateExtensionSelection(selectedExtensions, offeredExtensions)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", errInvalidHandshakeHeader, err)
	}
	return selection, nil
}

// writeUpgradeResponse writes and flushes the 101 Switching Protocols
// response: the caller's header fields, the negotiated extensions and, when
// the Upgrader chose it from its Subprotocols, the subprotocol. Neither of
// those two can also be in the caller's fields, as checkUpgradeRequest
// rejects both before the handshake.
func writeUpgradeResponse(w *bufio.Writer, acceptKey string, opts *upgradeOptions, extensions, subprotocol string) error {
	w.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	w.WriteString("Upgrade: websocket\r\n")
	w.WriteString("Connection: Upgrade\r\n")
	w.WriteString("Sec-WebSocket-Accept: ")
	w.WriteString(acceptKey)
	w.WriteString("\r\n")
	if extensions != "" {
		w.WriteString("Sec-WebSocket-Extensions: ")
		w.WriteString(extensions)
		w.WriteString("\r\n")
	}
	if opts.subprotocols != nil && subprotocol != "" {
		w.WriteString("Sec-WebSocket-Protocol: ")
		w.WriteString(subprotocol)
		w.WriteString("\r\n")
	}
	for k, vs := range opts.responseHeader {
		for _, v := range vs {
			w.WriteString(k)
			w.WriteString(": ")
			w.WriteString(v)
			w.WriteString("\r\n")
		}
	}
	// A bufio.Writer keeps its first error, so checking the last write
	// covers them all.
	if _, err := w.WriteString("\r\n"); err != nil {
		return fmt.Errorf("%w: failed to write handshake response: %w", errHandshakeFailed, err)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("%w: failed to flush handshake response: %w", errHandshakeFailed, err)
	}
	return nil
}
