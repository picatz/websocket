package websocket

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
)

// A Dialer contains options for connecting to a WebSocket server. Its zero
// value dials directly with TLS defaults, no compression, and the default
// message and response-head limits. A Dialer may be reused and is safe for
// concurrent use; Dial does not modify it or the values it refers to. A nil
// *Dialer behaves as the zero Dialer.
type Dialer struct {
	// NetDialContext opens the TCP transport, either to the WebSocket server
	// or, when Proxy selects one, to the proxy. It receives the Dial context,
	// the network "tcp" and a host:port address. If nil, a zero net.Dialer is
	// used.
	//
	// The returned connection is owned by Dial and, after a successful
	// handshake, by the Conn. Dial still performs TLS for wss URLs and HTTPS
	// proxies itself.
	NetDialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	// Proxy selects an HTTP proxy, with the same signature as
	// [http.Transport.Proxy]. Use [http.ProxyFromEnvironment] to honor
	// HTTP_PROXY, HTTPS_PROXY and NO_PROXY. If nil, or when it returns a nil
	// URL, Dial connects directly.
	//
	// The function receives a GET request whose URL is the WebSocket URL with
	// ws mapped to http and wss to https, so environment-based selection
	// treats wss like HTTPS. Dial tunnels through the proxy with an HTTP/1.1
	// CONNECT request ([RFC 9110, Section 9.3.6]) and then performs the
	// WebSocket handshake, and TLS for wss, end to end inside the tunnel. The
	// proxy never sees the WebSocket request headers of a wss connection.
	//
	// Proxy URLs must use the http or https scheme; https proxies are reached
	// over TLS using a clone of TLSClientConfig with ServerName set to the
	// proxy host, matching [http.Transport]. User info in the proxy URL is sent
	// only to the proxy as Basic Proxy-Authorization. The CONNECT response head
	// counts against MaxResponseHeaderBytes, and any status other than 200
	// fails with an error matching ErrBadHandshake that names the proxy. SOCKS
	// proxies are not supported; supply a SOCKS dialer as NetDialContext
	// instead.
	//
	// [RFC 9110, Section 9.3.6]: https://www.rfc-editor.org/rfc/rfc9110.html#section-9.3.6
	Proxy func(*http.Request) (*url.URL, error)

	// TLSClientConfig configures TLS for wss URLs and HTTPS proxies. Dial uses
	// a clone, setting ServerName from the URL when it is empty. If nil, the
	// zero configuration is used.
	TLSClientConfig *tls.Config

	// Header holds additional request header fields for the handshake. Dial
	// rejects invalid field names and values and fields managed by the
	// handshake: Host, Upgrade, Connection, Sec-WebSocket-Key,
	// Sec-WebSocket-Version and Sec-WebSocket-Extensions. Content-Length and
	// Transfer-Encoding are also forbidden because the handshake has no body.
	// Repeated ordinary fields are preserved; the caller owns their semantics.
	//
	// A raw Sec-WebSocket-Protocol field must hold unique HTTP tokens, and
	// cannot be combined with Subprotocols.
	Header http.Header

	// Subprotocols are the application subprotocols to offer, in order of
	// preference. Each must be a unique, case-sensitive HTTP token. The server
	// selects at most one; Conn.Subprotocol reports it, and Dial rejects a
	// selection that was not offered with an error matching ErrBadHandshake.
	//
	// https://www.rfc-editor.org/rfc/rfc6455#section-4.1
	Subprotocols []string

	// Compression, if non-nil, offers permessage-deflate (RFC 7692) with these
	// settings. Each connection gets its own compression state.
	Compression *Compression

	// MaxMessageSize bounds each incoming frame and reassembled message in
	// bytes, including data decompressed by Compression. Zero means
	// DefaultMaxMessageSize; a negative value means no limit.
	MaxMessageSize int

	// MaxResponseHeaderBytes bounds the HTTP response head: the status line,
	// every raw header line (including duplicates and continuation
	// whitespace), line separators and the final empty line. It counts
	// decrypted bytes, not TLS overhead, bodies or frames, and is not a time
	// limit; use Dial's context for that. Zero means
	// DefaultMaxResponseHeaderBytes; a negative value means no limit. An
	// exceeded limit returns an error matching ErrBadHandshake, with a nil
	// connection and response.
	MaxResponseHeaderBytes int64
}

// Dial connects to the WebSocket server at urlStr, a ws or wss URL. ctx covers
// TCP, any proxy tunnel, TLS and the HTTP upgrade handshake, but not later
// message I/O. When the server rejects the upgrade, Dial returns an error
// matching ErrBadHandshake together with the server's response. Its Body
// holds up to the first 4 KiB of the response body, read before the
// connection is closed, so an explanation such as a 401's can still be read.
// An invalid URL, or a NetDialContext that returns neither a connection nor an
// error, fails with ErrInvalidConfig.
func (d *Dialer) Dial(ctx context.Context, urlStr string) (*Conn, *http.Response, error) {
	return d.dial(ctx, urlStr, nil)
}

// dial is Dial with extra extensions negotiated after Compression's, which
// only tests supply.
func (d *Dialer) dial(ctx context.Context, urlStr string, extra []frameExtension) (*Conn, *http.Response, error) {
	if d == nil {
		d = &Dialer{}
	}
	extensions, err := buildExtensions(d.Compression, extra)
	if err != nil {
		return nil, nil, err
	}
	conn, resp, err := dial(ctx, urlStr, &dialOptions{
		extensions:             extensions,
		header:                 d.Header,
		tlsConfig:              d.TLSClientConfig,
		maxBytes:               messageLimit(d.MaxMessageSize),
		maxResponseHeaderBytes: headLimit(d.MaxResponseHeaderBytes),
		netDialContext:         d.NetDialContext,
		proxy:                  d.Proxy,
		subprotocols:           cloneSubprotocols(d.Subprotocols),
	})
	if err != nil && resp != nil && !errors.Is(err, ErrBadHandshake) {
		// The server answered, but not with a valid 101 response.
		err = fmt.Errorf("%w: %w", ErrBadHandshake, err)
	}
	return conn, resp, err
}

// Dial connects to the WebSocket server at urlStr with the zero Dialer. See
// Dialer.Dial: ctx bounds only connecting and the handshake, and a rejected
// handshake returns the server's response with an error matching
// ErrBadHandshake.
func Dial(ctx context.Context, urlStr string) (*Conn, *http.Response, error) {
	return (&Dialer{}).Dial(ctx, urlStr)
}

// An Upgrader contains options for accepting WebSocket connections. Its zero
// value accepts same-origin requests with no subprotocol, no compression and
// the default message limit. One Upgrader may be shared by every handler; it
// is safe for concurrent use, and Upgrade does not modify it. A nil *Upgrader
// behaves as the zero Upgrader.
type Upgrader struct {
	// CheckOrigin replaces the default policy, which allows a missing Origin
	// and otherwise requires a single HTTP or HTTPS Origin matching r.Host
	// and the scheme seen through r.TLS. Forwarded headers and r.URL.Scheme
	// are not trusted. See the package's Limits and security section.
	//
	// The function receives the request and one canonical Origin: a lowercase
	// scheme/ASCII host, normalized numeric port (HTTP/HTTPS defaults
	// omitted), and normalized IPv6; "" means absent and "null" means an
	// opaque origin. Host spelling is otherwise preserved, including trailing
	// dots, and no name is resolved, so "localhost" and "127.0.0.1" are
	// different origins; Unicode hosts must use ASCII/punycode. Malformed,
	// empty, multiple, or list-valued Origin fields are rejected before the
	// function runs.
	//
	// It replaces, rather than supplements, the default: it must decide
	// whether missing and opaque origins are allowed. Use an exact allowlist
	// for public origins behind a TLS-terminating proxy. Origin is not
	// authentication.
	CheckOrigin func(r *http.Request, origin string) bool

	// Subprotocols are the subprotocols the server supports, in order of
	// preference. Upgrade selects the first of these the client offered, or
	// none if the client offered none of them; RFC 6455 allows a server to
	// omit the selection, and the application decides whether to continue
	// without one. Conn.Subprotocol reports the selection. Each must be a
	// unique, case-sensitive HTTP token.
	//
	// https://www.rfc-editor.org/rfc/rfc6455#section-4.2.2
	Subprotocols []string

	// Header holds additional response header fields for the 101 response.
	// Upgrade rejects invalid field names and values and fields managed by
	// the handshake: Upgrade, Connection, Sec-WebSocket-Accept and
	// Sec-WebSocket-Extensions. Content-Length and Transfer-Encoding are also
	// forbidden. Repeated ordinary fields, such as Set-Cookie, are preserved.
	//
	// A raw Sec-WebSocket-Protocol field may select exactly one token the
	// client offered, and cannot be combined with Subprotocols.
	Header http.Header

	// Compression, if non-nil, accepts a client's permessage-deflate (RFC
	// 7692) offer with these settings. Each connection gets its own
	// compression state.
	Compression *Compression

	// MaxMessageSize bounds each incoming frame and reassembled message in
	// bytes, including data decompressed by Compression. Zero means
	// DefaultMaxMessageSize; a negative value means no limit.
	MaxMessageSize int

	// Error, if non-nil, writes the HTTP error response when the handshake
	// fails before the connection is taken over, for example to match an
	// application's error format. Upgrade has already set the header fields
	// the status requires, Allow or Sec-WebSocket-Version, and returns err to
	// its caller after Error returns. The default writes a short plain-text
	// body without the error's details.
	Error func(w http.ResponseWriter, r *http.Request, status int, err error)
}

// Upgrade upgrades the HTTP server connection to a WebSocket connection.
//
// When Upgrade fails before taking over the connection, it writes the HTTP
// error response itself, with Error if set, and returns the error; the
// handler should just return. The error says which response was sent:
//
//   - ErrOriginNotAllowed: 403 Forbidden.
//   - ErrBadHandshake: the client's request was at fault. A non-GET request
//     gets 405 Method Not Allowed with Allow: GET, an
//     unsupported version (ErrUnsupportedVersion) 426 Upgrade Required with
//     Sec-WebSocket-Version: 13 (RFC 6455 section 4.4), and any other
//     malformed request 400 Bad Request.
//   - ErrNotHijacker, or an invalid Upgrader (ErrInvalidConfig):
//     500 Internal Server Error.
//   - ErrBadHandshake from a failed Hijack call or anything after it:
//     nothing, as the transport may already be hijacked.
//
// Upgrade requires an HTTP/1.1 connection with hijacking support. Middleware
// may implement http.Hijacker directly or expose the underlying
// http.ResponseWriter through an Unwrap() http.ResponseWriter method, as used
// by http.ResponseController. A writer's own Hijack method takes precedence
// over Unwrap. HTTP/2 is not supported. After a successful Upgrade the
// connection belongs to the returned Conn, so the handler must not use w.
//
// Origin checks are not authentication: callers still own authentication,
// authorization, and authoritative Host validation.
//
// https://www.rfc-editor.org/rfc/rfc6455#section-4.2.1
func (u *Upgrader) Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if u == nil {
		u = &zeroUpgrader
	}
	return u.upgrade(w, r, nil)
}

// upgrade is Upgrade with extra extensions negotiated after Compression's,
// which only tests supply.
func (u *Upgrader) upgrade(w http.ResponseWriter, r *http.Request, extra []frameExtension) (*Conn, error) {
	// The handshake runs on the handler's goroutine, which keeps whatever
	// stack the handshake grew to for as long as the connection is open, so
	// the options are resolved in a call that returns before upgrade starts
	// rather than in a frame that stays below it.
	opts, err := u.options(extra)
	var conn *Conn
	if err == nil {
		conn, err = upgrade(w, r, &opts)
	}
	if err != nil {
		return nil, writeUpgradeError(w, r, err, u.Error)
	}
	return conn, nil
}

// zeroUpgrader is the Upgrader that Upgrade and a nil *Upgrader use. It is
// never modified.
var zeroUpgrader Upgrader

// options resolves u and extra into the options upgrade takes.
func (u *Upgrader) options(extra []frameExtension) (upgradeOptions, error) {
	extensions, err := buildExtensions(u.Compression, extra)
	if err != nil {
		return upgradeOptions{}, err
	}
	return upgradeOptions{
		extensions:     extensions,
		responseHeader: u.Header,
		maxBytes:       messageLimit(u.MaxMessageSize),
		originCheck:    u.CheckOrigin,
		subprotocols:   cloneSubprotocols(u.Subprotocols),
	}, nil
}

// Upgrade upgrades the HTTP server connection with the zero Upgrader. See
// Upgrader.Upgrade: on failure the HTTP error response has already been
// written, so the handler should just return, and after success the handler
// must not use w.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	return zeroUpgrader.Upgrade(w, r)
}

// ConnConfig configures NewConn for a connection whose handshake the caller
// performed. Its zero value is a client connection with no extensions and the
// DefaultMaxMessageSize limit.
type ConnConfig struct {
	// Server selects the server role: incoming frames must be masked and
	// outgoing frames are not.
	Server bool

	// Subprotocol is the subprotocol the caller's handshake selected, which
	// Conn.Subprotocol reports.
	Subprotocol string

	// Extensions is the Sec-WebSocket-Extensions value of the 101 response
	// that completed the caller's handshake: the one a server sent or a
	// client received. Empty means no extension was agreed. The only
	// extension NewConn supports is permessage-deflate, whose parameters it
	// takes from this value as agreed; NewConn does not check them against an
	// offer, so the caller's handshake must have followed RFC 7692 section 7.
	//
	// https://www.rfc-editor.org/rfc/rfc7692#section-7
	Extensions string

	// Compression sets Level, Threshold and MemoryLimit for permessage-deflate
	// when Extensions agrees on it. Its window and no_context_takeover fields
	// are ignored, since the agreement decides those, but must still be
	// valid. If nil, the zero Compression is used.
	Compression *Compression

	// MaxMessageSize bounds each incoming frame and reassembled message in
	// bytes, including decompressed data. Zero means DefaultMaxMessageSize; a
	// negative value means no limit.
	MaxMessageSize int
}

// NewConn creates a WebSocket connection over conn, whose opening handshake
// the caller has completed. It takes over WebSocket framing and I/O; Close and
// read-error cleanup close the transport. Callers must not read or write
// framed bytes directly on conn.
//
// NewConn fails with an error matching ErrInvalidConfig when cfg.Extensions
// is malformed or names an extension other than permessage-deflate, or when
// cfg.Compression is invalid.
func NewConn(conn net.Conn, cfg ConnConfig) (*Conn, error) {
	return newConfiguredConn(conn, cfg, nil)
}

// newConfiguredConn is NewConn with extra negotiated extensions after
// cfg.Extensions', which only tests supply.
func newConfiguredConn(conn net.Conn, cfg ConnConfig, extensions []frameExtension) (*Conn, error) {
	if cfg.Extensions != "" {
		pmd, err := agreedCompression(cfg.Extensions, cfg.Compression, cfg.Server)
		if err != nil {
			if !errors.Is(err, ErrInvalidConfig) {
				err = fmt.Errorf("%w: %w", ErrInvalidConfig, err)
			}
			return nil, err
		}
		extensions = append([]frameExtension{pmd}, extensions...)
	}
	ws := newConn(conn, bufio.NewReader(conn), cfg.Server, extensions, messageLimit(cfg.MaxMessageSize))
	ws.subprotocol = cfg.Subprotocol
	return ws, nil
}

// agreedCompression returns an enabled permessage-deflate extension for the
// response value agreement, with settings from c.
func agreedCompression(agreement string, c *Compression, server bool) (*perMessageDeflate, error) {
	offers, err := parseExtensions([]string{agreement})
	if err != nil {
		return nil, err
	}
	if len(offers) != 1 || offers[0].name != "permessage-deflate" {
		return nil, fmt.Errorf("%w: NewConn supports exactly one permessage-deflate agreement, got %q", errInvalidExtension, agreement)
	}
	// A response gives every window parameter a value (RFC 7692 section 7.1.2).
	params, err := parsePMDParameters(offers[0].params, false)
	if err != nil {
		return nil, err
	}
	if c == nil {
		c = &Compression{}
	}
	ext, err := c.extension()
	if err != nil {
		return nil, err
	}
	pmd := ext.(*perMessageDeflate)
	pmd.server = server
	pmd.offered, pmd.offerSet = params, true
	pmd.negotiated, pmd.enabled = params, true
	return pmd, nil
}

// messageLimit maps a configured message size to the internal limit, where 0
// means unlimited.
func messageLimit(n int) int {
	switch {
	case n == 0:
		return DefaultMaxMessageSize
	case n < 0:
		return 0
	}
	return n
}

// headLimit maps a configured response-head limit to the internal one, where
// 0 means unlimited.
func headLimit(n int64) int64 {
	switch {
	case n == 0:
		return DefaultMaxResponseHeaderBytes
	case n < 0:
		return 0
	}
	return n
}

// buildExtensions creates one connection's extensions: permessage-deflate
// from compression, if any, then extra.
func buildExtensions(compression *Compression, extra []frameExtension) ([]frameExtension, error) {
	var extensions []frameExtension
	if compression != nil {
		pmd, err := compression.extension()
		if err != nil {
			return nil, err
		}
		extensions = append(extensions, pmd)
	}
	return append(extensions, extra...), nil
}
