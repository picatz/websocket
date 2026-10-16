package websocket_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

// echoTarget is a WebSocket server that echoes one message and records the
// handshake headers it received.
type echoTarget struct {
	*httptest.Server
	headers chan http.Header
}

func newEchoTarget(t *testing.T, useTLS bool) *echoTarget {
	t.Helper()
	target := &echoTarget{headers: make(chan http.Header, 8)}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.headers <- r.Header.Clone()
		c, err := websocket.Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		kind, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		_ = c.WriteMessage(kind, data)
	})
	if useTLS {
		target.Server = httptest.NewTLSServer(handler)
	} else {
		target.Server = httptest.NewServer(handler)
	}
	t.Cleanup(target.Close)
	return target
}

func (e *echoTarget) wsURL() string {
	return "ws" + strings.TrimPrefix(e.URL, "http")
}

func (e *echoTarget) rootsConfig() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(e.Certificate())
	return &tls.Config{RootCAs: pool}
}

// connectProxy is a minimal HTTP CONNECT proxy for tests. Its behavior per
// connection is decided by respond, which may write its own reply and return
// false to stop before tunnelling.
type connectProxy struct {
	ln       net.Listener
	requests chan *http.Request
	accepted atomic.Int32
	respond  func(conn net.Conn, req *http.Request) bool
	wg       sync.WaitGroup
}

func newConnectProxy(t *testing.T, config *tls.Config, respond func(net.Conn, *http.Request) bool) *connectProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if config != nil {
		ln = tls.NewListener(ln, config)
	}
	p := &connectProxy{ln: ln, requests: make(chan *http.Request, 8), respond: respond}
	p.wg.Go(p.serve)
	t.Cleanup(func() {
		ln.Close()
		p.wg.Wait()
	})
	return p
}

func (p *connectProxy) serve() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.accepted.Add(1)
		p.wg.Go(func() { p.handle(conn) })
	}
}

func (p *connectProxy) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	p.requests <- req
	if p.respond != nil && !p.respond(conn, req) {
		return
	}
	upstream, err := net.Dial("tcp", req.Host)
	if err != nil {
		io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer upstream.Close()
	io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, br); done <- struct{}{} }()
	go func() { io.Copy(conn, upstream); done <- struct{}{} }()
	<-done
}

func (p *connectProxy) url(scheme string, user *url.Userinfo) *url.URL {
	return &url.URL{Scheme: scheme, Host: p.ln.Addr().String(), User: user}
}

func fixedProxy(u *url.URL) func(*http.Request) (*url.URL, error) {
	return func(*http.Request) (*url.URL, error) { return u, nil }
}

func dialEcho(t *testing.T, target *echoTarget, rawURL string, options ...websocket.DialOption) {
	t.Helper()
	c, _, err := websocket.Dial(t.Context(), rawURL, options...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.WriteMessageContext(t.Context(), websocket.TextMessage, []byte("through")); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.ReadMessageContext(t.Context())
	if err != nil || string(data) != "through" {
		t.Fatalf("echo = %q, %v", data, err)
	}
}

func TestDialThroughHTTPProxyWithCredentials(t *testing.T) {
	target := newEchoTarget(t, false)
	proxy := newConnectProxy(t, nil, nil)
	targetHost := strings.TrimPrefix(target.URL, "http://")

	dialEcho(t, target, target.wsURL()+"/chat?room=1",
		websocket.WithProxy(fixedProxy(proxy.url("http", url.UserPassword("alice", "s3cret")))))

	req := <-proxy.requests
	if req.Method != http.MethodConnect || req.Host != targetHost || req.RequestURI != targetHost {
		t.Fatalf("proxy request = %s %s (Host %s)", req.Method, req.RequestURI, req.Host)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	if got := req.Header.Get("Proxy-Authorization"); got != want {
		t.Fatalf("Proxy-Authorization = %q, want %q", got, want)
	}
	if h := <-target.headers; h.Get("Proxy-Authorization") != "" {
		t.Fatal("proxy credentials reached the WebSocket server")
	}
}

func TestDialWSSThroughHTTPProxy(t *testing.T) {
	target := newEchoTarget(t, true)
	proxy := newConnectProxy(t, nil, nil)
	dialEcho(t, target, target.wsURL(),
		websocket.WithTLSConfig(target.rootsConfig()),
		websocket.WithProxy(fixedProxy(proxy.url("http", nil))))
	if req := <-proxy.requests; req.Header.Get("Proxy-Authorization") != "" {
		t.Fatal("unexpected proxy credentials")
	}
}

func TestDialWSSThroughHTTPSProxy(t *testing.T) {
	target := newEchoTarget(t, true)
	// The httptest certificate is valid for 127.0.0.1, so the proxy can reuse it
	// and the client verifies both hops against the same roots.
	proxyTLS := &tls.Config{Certificates: target.TLS.Certificates}
	proxy := newConnectProxy(t, proxyTLS, nil)
	dialEcho(t, target, target.wsURL(),
		websocket.WithTLSConfig(target.rootsConfig()),
		websocket.WithProxy(fixedProxy(proxy.url("https", nil))))
	if proxy.accepted.Load() != 1 {
		t.Fatalf("proxy accepted %d connections", proxy.accepted.Load())
	}
}

func TestDialHTTPSProxyVerifiesCertificate(t *testing.T) {
	target := newEchoTarget(t, true)
	proxy := newConnectProxy(t, &tls.Config{Certificates: target.TLS.Certificates}, nil)
	// No roots for the self-signed proxy certificate: verification must fail.
	_, _, err := websocket.Dial(t.Context(), target.wsURL(), websocket.WithProxy(fixedProxy(proxy.url("https", nil))))
	if !errors.Is(err, websocket.ErrBadHandshake) || !errors.Is(err, websocket.ErrProxy) {
		t.Fatalf("err = %v, want ErrBadHandshake and ErrProxy", err)
	}
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want certificate verification failure", err)
	}
}

func TestDialProxyRejection(t *testing.T) {
	target := newEchoTarget(t, false)
	proxy := newConnectProxy(t, nil, func(conn net.Conn, _ *http.Request) bool {
		io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
		return false
	})
	c, resp, err := websocket.Dial(t.Context(), target.wsURL(), websocket.WithProxy(fixedProxy(proxy.url("http", nil))))
	if c != nil || resp != nil {
		t.Fatal("rejected tunnel returned a connection or target response")
	}
	if !errors.Is(err, websocket.ErrBadHandshake) || !errors.Is(err, websocket.ErrProxy) || !strings.Contains(err.Error(), "407") {
		t.Fatalf("err = %v", err)
	}
}

func TestDialProxyEarlyDataRejected(t *testing.T) {
	target := newEchoTarget(t, false)
	proxy := newConnectProxy(t, nil, func(conn net.Conn, _ *http.Request) bool {
		io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\nHTTP/1.1 101 Switching Protocols\r\n\r\n")
		return false
	})
	_, _, err := websocket.Dial(t.Context(), target.wsURL(), websocket.WithProxy(fixedProxy(proxy.url("http", nil))))
	if !errors.Is(err, websocket.ErrProxy) {
		t.Fatalf("err = %v, want ErrProxy", err)
	}
}

func TestDialProxyResponseHeadBudget(t *testing.T) {
	target := newEchoTarget(t, false)
	proxy := newConnectProxy(t, nil, func(conn net.Conn, _ *http.Request) bool {
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nX-Padding: "+strings.Repeat("a", 256)+"\r\n\r\n")
		return false
	})
	_, _, err := websocket.Dial(t.Context(), target.wsURL(),
		websocket.WithMaxResponseHeaderBytes(64),
		websocket.WithProxy(fixedProxy(proxy.url("http", nil))))
	if !errors.Is(err, websocket.ErrProxy) || !errors.Is(err, websocket.ErrResponseHeaderTooLarge) {
		t.Fatalf("err = %v, want ErrProxy and ErrResponseHeaderTooLarge", err)
	}
}

func TestDialProxyCancellation(t *testing.T) {
	target := newEchoTarget(t, false)
	stalled := make(chan struct{})
	proxy := newConnectProxy(t, nil, func(net.Conn, *http.Request) bool {
		<-stalled // never answer the CONNECT
		return false
	})
	defer close(stalled)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := websocket.Dial(ctx, target.wsURL(), websocket.WithProxy(fixedProxy(proxy.url("http", nil))))
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, websocket.ErrBadHandshake) {
		t.Fatalf("err = %v, want ErrBadHandshake and DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Dial returned after %v", elapsed)
	}
}

func TestDialProxySelection(t *testing.T) {
	target := newEchoTarget(t, true)
	var got *http.Request
	_, _, err := websocket.Dial(t.Context(), target.wsURL()+"/a%2Fb?x=1",
		websocket.WithTLSConfig(target.rootsConfig()),
		websocket.WithProxy(func(r *http.Request) (*url.URL, error) {
			got = r
			return nil, nil // nil selects a direct connection
		}))
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(target.URL, "https://")
	if got == nil || got.Method != http.MethodGet || got.URL.Scheme != "https" || got.URL.Host != host ||
		got.URL.EscapedPath() != "/a%2Fb" || got.URL.RawQuery != "x=1" || got.Host != host || got.Context() == nil {
		t.Fatalf("proxy request = %+v", got)
	}

	ws := newEchoTarget(t, false)
	if _, _, err := websocket.Dial(t.Context(), ws.wsURL(), websocket.WithProxy(func(r *http.Request) (*url.URL, error) {
		if r.URL.Scheme != "http" {
			t.Errorf("ws scheme mapped to %q", r.URL.Scheme)
		}
		return nil, nil
	})); err != nil {
		t.Fatal(err)
	}
}

func TestDialProxyFromEnvironment(t *testing.T) {
	target := newEchoTarget(t, false)
	proxy := newConnectProxy(t, nil, nil)
	// ProxyFromEnvironment caches the environment for the process, and always
	// bypasses loopback targets, so check only that its signature composes and
	// that it leaves this loopback dial direct.
	dialEcho(t, target, target.wsURL(), websocket.WithProxy(http.ProxyFromEnvironment))
	if proxy.accepted.Load() != 0 {
		t.Fatal("loopback dial used a proxy")
	}
}

func TestDialProxyErrors(t *testing.T) {
	target := newEchoTarget(t, false)
	selectErr := errors.New("synthetic selection failure")
	for name, tc := range map[string]struct {
		proxy func(*http.Request) (*url.URL, error)
		want  error
	}{
		"selection error":  {func(*http.Request) (*url.URL, error) { return nil, selectErr }, selectErr},
		"socks scheme":     {fixedProxy(&url.URL{Scheme: "socks5", Host: "127.0.0.1:1080", User: url.UserPassword("u", "hunter2")}), websocket.ErrProxy},
		"missing host":     {fixedProxy(&url.URL{Scheme: "http"}), websocket.ErrProxy},
		"unreachable host": {fixedProxy(&url.URL{Scheme: "http", Host: "127.0.0.1:1"}), websocket.ErrProxy},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := websocket.Dial(t.Context(), target.wsURL(), websocket.WithProxy(tc.proxy))
			if !errors.Is(err, websocket.ErrBadHandshake) || !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want ErrBadHandshake and %v", err, tc.want)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Fatal("error leaked proxy credentials")
			}
		})
	}
}

func TestDialWithNetDialContext(t *testing.T) {
	target := newEchoTarget(t, false)
	targetHost := strings.TrimPrefix(target.URL, "http://")
	var calls []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		calls = append(calls, network+" "+addr)
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	dialEcho(t, target, target.wsURL(), websocket.WithNetDialContext(dial))
	if len(calls) != 1 || calls[0] != "tcp "+targetHost {
		t.Fatalf("calls = %q", calls)
	}

	// With a proxy, the dialer reaches the proxy rather than the target.
	proxy := newConnectProxy(t, nil, nil)
	calls = nil
	dialEcho(t, target, target.wsURL(), websocket.WithNetDialContext(dial), websocket.WithProxy(fixedProxy(proxy.url("http", nil))))
	if len(calls) != 1 || calls[0] != "tcp "+proxy.ln.Addr().String() {
		t.Fatalf("calls = %q", calls)
	}

	// Dialer failures keep their cause; a nil connection is rejected.
	dialErr := errors.New("synthetic dial failure")
	_, _, err := websocket.Dial(t.Context(), target.wsURL(), websocket.WithNetDialContext(func(context.Context, string, string) (net.Conn, error) {
		return nil, dialErr
	}))
	if !errors.Is(err, websocket.ErrBadHandshake) || !errors.Is(err, dialErr) {
		t.Fatalf("err = %v", err)
	}
	_, _, err = websocket.Dial(t.Context(), target.wsURL(), websocket.WithNetDialContext(func(context.Context, string, string) (net.Conn, error) {
		return nil, nil
	}))
	if !errors.Is(err, websocket.ErrBadHandshake) {
		t.Fatalf("err = %v", err)
	}
}
