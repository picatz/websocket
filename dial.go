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
			return nil, nil, fmt.Errorf("%w: %w: select proxy: %w", ErrBadHandshake, ErrProxy, err)
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
			return nil, nil, fmt.Errorf("%w: %w: failed to dial proxy: %w", ErrBadHandshake, ErrProxy, err)
		}
		return nil, nil, fmt.Errorf("%w: failed to dial: %w", ErrBadHandshake, err)
	}
	if conn == nil {
		return nil, nil, fmt.Errorf("%w: dial function returned a nil connection", ErrBadHandshake)
	}
	return conn, proxyURL, nil
}

func validateProxyURL(u *url.URL) error {
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		// Never format the URL itself: it may carry credentials.
		return fmt.Errorf("%w: %w: proxy URL must use http or https with a host, got scheme %q", ErrBadHandshake, ErrProxy, u.Scheme)
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
			return nil, fmt.Errorf("%w: %w: TLS handshake with proxy: %w", ErrBadHandshake, ErrProxy, err)
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
		return nil, fmt.Errorf("%w: %w: send CONNECT request: %w", ErrBadHandshake, ErrProxy, err)
	}

	resp, br, err := readHandshakeResponse(conn, opts.maxResponseHeaderBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w: read CONNECT response: %w", ErrBadHandshake, ErrProxy, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %w: CONNECT returned status %d", ErrBadHandshake, ErrProxy, resp.StatusCode)
	}
	// A proxy must not send tunnel bytes before the client speaks. Discarding
	// them would corrupt the stream, so treat early data as a protocol error.
	if br.Buffered() > 0 {
		return nil, fmt.Errorf("%w: %w: proxy sent data before the tunnel was used", ErrBadHandshake, ErrProxy)
	}
	return conn, nil
}
