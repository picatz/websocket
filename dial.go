package websocket

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// dialTransport opens the TCP connection for target (host:port), or for the
// proxy selected for u. A non-nil proxy URL means the caller must tunnel with
// connectProxy before speaking HTTP to target.
func dialTransport(ctx context.Context, opts *dialOptions, u *url.URL, target string) (net.Conn, *url.URL, error) {
	dial := opts.netDialContext
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}

	var proxyURL *url.URL
	if opts.proxy != nil {
		scheme := "http"
		if u.Scheme == "wss" {
			scheme = "https"
		}
		req := &http.Request{
			Method: http.MethodGet,
			URL:    &url.URL{Scheme: scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery},
			Header: make(http.Header),
			Host:   u.Host,
		}
		req = req.WithContext(ctx)
		selected, err := opts.proxy(req)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %w: select proxy: %w", ErrBadHandshake, errProxy, err)
		}
		if selected != nil {
			if err := validateProxyURL(selected); err != nil {
				return nil, nil, err
			}
			proxyURL = selected
		}
	}

	addr := target
	if proxyURL != nil {
		addr = proxyAddr(proxyURL)
	}
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		if proxyURL != nil {
			return nil, nil, fmt.Errorf("%w: %w: failed to dial proxy: %w", ErrBadHandshake, errProxy, err)
		}
		return nil, nil, fmt.Errorf("%w: failed to dial: %w", ErrBadHandshake, err)
	}
	if conn == nil {
		return nil, nil, fmt.Errorf("%w: dial function returned a nil connection", ErrInvalidConfig)
	}
	return conn, proxyURL, nil
}

func validateProxyURL(u *url.URL) error {
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		// Never format the URL itself: it may carry credentials.
		return fmt.Errorf("%w: %w: proxy URL must use http or https with a host, got scheme %q", ErrBadHandshake, errProxy, u.Scheme)
	}
	return nil
}

func proxyAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// connectProxy establishes a CONNECT tunnel to target over conn, which is
// connected to proxyURL. It returns the connection to use for the tunnel,
// wrapped in TLS for https proxies. The caller closes the raw transport on
// error.
func connectProxy(ctx context.Context, conn net.Conn, proxyURL *url.URL, target string, opts *dialOptions) (net.Conn, error) {
	if proxyURL.Scheme == "https" {
		config := &tls.Config{}
		if opts.tlsConfig != nil {
			config = opts.tlsConfig.Clone()
		}
		config.ServerName = proxyURL.Hostname()
		config.NextProtos = []string{"http/1.1"}
		tlsConn := tls.Client(conn, config)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("%w: %w: TLS handshake with proxy: %w", ErrBadHandshake, errProxy, err)
		}
		conn = tlsConn
	}

	var req strings.Builder
	req.Grow(256)
	req.WriteString("CONNECT " + target + " HTTP/1.1\r\n")
	req.WriteString("Host: " + target + "\r\n")
	if user := proxyURL.User; user != nil {
		password, _ := user.Password()
		credentials := base64.StdEncoding.EncodeToString([]byte(user.Username() + ":" + password))
		req.WriteString("Proxy-Authorization: Basic " + credentials + "\r\n")
	}
	req.WriteString("\r\n")
	if _, err := conn.Write([]byte(req.String())); err != nil {
		return nil, fmt.Errorf("%w: %w: send CONNECT request: %w", ErrBadHandshake, errProxy, err)
	}

	resp, br, err := readHandshakeResponse(conn, opts.maxResponseHeaderBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w: read CONNECT response: %w", ErrBadHandshake, errProxy, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %w: CONNECT returned status %d", ErrBadHandshake, errProxy, resp.StatusCode)
	}
	// A proxy must not send tunnel bytes before the client speaks. Discarding
	// them would corrupt the stream, so treat early data as a protocol error.
	if br.Buffered() > 0 {
		return nil, fmt.Errorf("%w: %w: proxy sent data before the tunnel was used", ErrBadHandshake, errProxy)
	}
	return conn, nil
}

// dialOptions is a Dialer resolved for one Dial: defaults applied, the
// subprotocol list detached and extensions created for this connection.
type dialOptions struct {
	extensions             []frameExtension
	header                 http.Header
	tlsConfig              *tls.Config
	maxBytes               int
	maxResponseHeaderBytes int64
	netDialContext         func(ctx context.Context, network, addr string) (net.Conn, error)
	proxy                  func(*http.Request) (*url.URL, error)
	subprotocols           []string
}

// dial performs the client handshake for a resolved Dialer.
func dial(ctx context.Context, urlStr string, opts *dialOptions) (ws *Conn, response *http.Response, err error) {
	if err := validateHandshakeHeaders(opts.header, false); err != nil {
		return nil, nil, err
	}
	if opts.subprotocols != nil {
		if err := validateSubprotocolOption(opts.subprotocols, opts.header); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", errInvalidHandshakeHeader, err)
		}
		// Clone so the caller's header map is never modified.
		opts.header = opts.header.Clone()
		if opts.header == nil {
			opts.header = make(http.Header, 1)
		}
		opts.header.Set("Sec-WebSocket-Protocol", strings.Join(opts.subprotocols, ", "))
	}

	offered, err := parseSubprotocolOffers(opts.header)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errInvalidHandshakeHeader, err)
	}

	extOffer, offeredExtensions, err := extensionOffers(opts.extensions)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errInvalidHandshakeHeader, err)
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid URL: %w", ErrInvalidConfig, err)
	}
	if (u.Scheme != "ws" && u.Scheme != "wss") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, nil, fmt.Errorf("%w: URL must use ws or wss, with a host and no user info or fragment", ErrInvalidConfig)
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	host := net.JoinHostPort(u.Hostname(), port)

	// Establish the network connection, directly or to the selected proxy.
	conn, proxyURL, err := dialTransport(ctx, opts, u, host)
	if err != nil {
		return nil, nil, err
	}
	// Closing the raw transport interrupts blocked HTTP reads/writes as well
	// as TLS. Join the callback before returning so it cannot close a live
	// connection after a successful handshake has been handed to the caller.
	rawConn := conn
	// Deferred first so that it runs last, after the callback is joined.
	defer func() {
		if ctx.Err() != nil {
			rawConn.Close()
			ws = nil
			err = fmt.Errorf("%w: %w", ErrBadHandshake, context.Cause(ctx))
		}
	}()
	if ctx.Done() != nil {
		// A context that can never be canceled needs no callback.
		canceled := make(chan struct{})
		stopCancel := context.AfterFunc(ctx, func() {
			rawConn.Close()
			close(canceled)
		})
		defer func() {
			if !stopCancel() {
				<-canceled
			}
		}()
	}

	// Tunnel through the proxy before any end-to-end TLS. The cancellation
	// callback above closes the raw transport, interrupting this exchange too.
	if proxyURL != nil {
		if conn, err = connectProxy(ctx, conn, proxyURL, host, opts); err != nil {
			rawConn.Close()
			return nil, nil, err
		}
	}

	// If using TLS, perform the handshake
	var tlsState *tls.ConnectionState
	if u.Scheme == "wss" {
		config := &tls.Config{}
		if opts.tlsConfig != nil {
			config = opts.tlsConfig.Clone()
		}
		if config.ServerName == "" {
			config.ServerName = u.Hostname()
		}
		config.NextProtos = []string{"http/1.1"}
		tlsConn := tls.Client(conn, config)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("%w: failed to perform TLS handshake: %w", ErrBadHandshake, err)
		}
		conn = tlsConn
		state := tlsConn.ConnectionState()
		tlsState = &state
	}

	key := generateKey()

	// Build the request. Offers were validated before connecting, including
	// extension-produced values.
	req := make([]byte, 0, 512)
	req = append(req, "GET "...)
	req = append(req, u.RequestURI()...)
	req = append(req, " HTTP/1.1\r\nHost: "...)
	req = append(req, u.Host...)
	req = append(req, "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: "...)
	req = append(req, key[:]...)
	req = append(req, "\r\nSec-WebSocket-Version: 13\r\n"...)
	if extOffer != "" {
		req = append(req, "Sec-WebSocket-Extensions: "...)
		req = append(req, extOffer...)
		req = append(req, "\r\n"...)
	}
	for k, vs := range opts.header {
		for _, v := range vs {
			req = append(req, k...)
			req = append(req, ": "...)
			req = append(req, v...)
			req = append(req, "\r\n"...)
		}
	}
	req = append(req, "\r\n"...)

	// Send the request
	if _, err = conn.Write(req); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("%w: failed to send handshake request: %w", ErrBadHandshake, err)
	}

	// Read the response
	resp, br, err := readHandshakeResponse(conn, opts.maxResponseHeaderBytes)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("%w: failed to read handshake response: %w", ErrBadHandshake, err)
	}
	// As with http.Transport, a response received over TLS carries the
	// connection's TLS state.
	resp.TLS = tlsState
	if resp.StatusCode != http.StatusSwitchingProtocols {
		keepRejectedBody(conn, resp)
		conn.Close()
		return nil, resp, fmt.Errorf("%w: unexpected status code %d", ErrBadHandshake, resp.StatusCode)
	}
	if !headerContains(resp.Header, "Connection", "Upgrade") {
		conn.Close()
		return nil, resp, errInvalidConnectionHeader
	}
	if !headerContains(resp.Header, "Upgrade", "websocket") {
		conn.Close()
		return nil, resp, errInvalidUpgradeHeader
	}
	accept := resp.Header["Sec-Websocket-Accept"]
	if len(accept) != 1 {
		conn.Close()
		return nil, resp, errInvalidSecAccept
	}
	if expected := acceptKey(key[:]); accept[0] != string(expected[:]) {
		conn.Close()
		return nil, resp, errInvalidSecAccept
	}

	subprotocol, err := validateSubprotocolSelection(resp.Header, offered)
	if err != nil {
		conn.Close()
		return nil, resp, err
	}

	// Validate the complete selection before calling any extension. In
	// particular, no configured extensions means no selections are allowed.
	extValues := extensionValues(resp.Header)
	selectedExtensions, err := parseExtensions(extValues)
	if err == nil {
		err = validateExtensionSelection(selectedExtensions, offeredExtensions)
	}
	if err != nil {
		conn.Close()
		return nil, resp, err
	}
	extHeader := strings.Join(extValues, ", ")
	for _, ext := range opts.extensions {
		if err := ext.Negotiate(extHeader); err != nil {
			conn.Close()
			return nil, resp, fmt.Errorf("%w: extension %s: %w", errInvalidExtension, ext.Name(), err)
		}
	}

	if _, err := messageCompression(opts.extensions); err != nil {
		conn.Close()
		return nil, resp, fmt.Errorf("%w: %w", errInvalidExtension, err)
	}

	ws = newConn(conn, br, false, opts.extensions, opts.maxBytes)
	ws.subprotocol = subprotocol
	ws.abortConn = rawConn
	return ws, resp, nil
}
