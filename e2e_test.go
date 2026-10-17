package websocket_test

// End-to-end tests. Each one runs a real net/http server from httptest over
// real TCP, configured the way a deployed program would be (TLS with HTTP/2,
// server timeouts, middleware, a proxy, graceful shutdown), and drives it
// with this package's own client. They check whole conversations, from the
// opening handshake to the closing one, rather than single functions.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

// echoLoop sends every message on c back with the same type until a read
// fails, and returns that error.
func echoLoop(c *websocket.Conn) error {
	for {
		typ, data, err := c.ReadMessage(context.Background())
		if err != nil {
			return err
		}
		if err := c.WriteMessage(context.Background(), typ, data); err != nil {
			return err
		}
	}
}

// roundTrip writes data as typ on c and reads the echoed reply.
func roundTrip(t *testing.T, c *websocket.Conn, typ websocket.MessageType, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := c.WriteMessage(ctx, typ, data); err != nil {
		t.Fatalf("write %d bytes: %v", len(data), err)
	}
	gotType, got, err := c.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read echo of %d bytes: %v", len(data), err)
	}
	if gotType != typ || !bytes.Equal(got, data) {
		t.Fatalf("echo = %v, %d bytes; want %v, %d bytes", gotType, len(got), typ, len(data))
	}
}

// randomBytes returns n bytes that deflate cannot shrink.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// A WebSocket endpoint can share a TLS server with HTTP/2. Browsers and Go's
// HTTP client negotiate h2 for ordinary requests, while the Dialer must ask
// for HTTP/1.1 even when its TLS config offers h2, since RFC 6455 upgrades
// only HTTP/1.1 connections.
func TestEndToEndTLSWithHTTP2(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		u := websocket.Upgrader{Compression: &websocket.Compression{}}
		c, err := u.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		echoLoop(c)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Proto)
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	proto, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(proto) != "HTTP/2.0" {
		t.Fatalf("plain request used %s, want HTTP/2.0", proto)
	}

	tlsConfig := srv.Client().Transport.(*http.Transport).TLSClientConfig
	if !strings.Contains(strings.Join(tlsConfig.NextProtos, ","), "h2") {
		t.Fatalf("test TLS config offers %q; want h2 among them", tlsConfig.NextProtos)
	}
	d := websocket.Dialer{TLSClientConfig: tlsConfig, Compression: &websocket.Compression{}}
	c, resp, err := d.Dial(t.Context(), wsURL(srv)+"/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	state, ok := c.ConnectionState()
	if !ok {
		t.Fatal("ConnectionState reports no TLS")
	}
	// httptest's listener lists only h2, so ALPN agrees on nothing here;
	// http.Server.ServeTLS would add and select http/1.1.
	if state.NegotiatedProtocol == "h2" {
		t.Fatal("WebSocket connection negotiated h2")
	}
	if ext := resp.Header.Get("Sec-WebSocket-Extensions"); !strings.HasPrefix(ext, "permessage-deflate") {
		t.Fatalf("Sec-WebSocket-Extensions = %q, want permessage-deflate", ext)
	}
	roundTrip(t, c, websocket.TextMessage, bytes.Repeat([]byte("compressible "), 10_000))
	roundTrip(t, c, websocket.BinaryMessage, randomBytes(200_000))
	if err := c.CloseWithStatus(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("CloseWithStatus: %v", err)
	}
}

// http.Server's ReadTimeout, WriteTimeout and IdleTimeout bound HTTP
// requests, not the WebSocket connections that Upgrade takes over. A
// connection must outlive all three, and must still be idle-safe: its own
// deadlines are the program's to set.
func TestEndToEndOutlivesServerTimeouts(t *testing.T) {
	const timeout = 50 * time.Millisecond
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		echoLoop(c)
	}))
	srv.Config.ReadTimeout = timeout
	srv.Config.ReadHeaderTimeout = timeout
	srv.Config.WriteTimeout = timeout
	srv.Config.IdleTimeout = timeout
	srv.Start()
	t.Cleanup(srv.Close)

	c, _, err := websocket.Dial(t.Context(), wsURL(srv))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := range 3 {
		time.Sleep(2 * timeout)
		roundTrip(t, c, websocket.TextMessage, fmt.Appendf(nil, "after %d timeouts", 2*(i+1)))
	}
}

// Logging, metrics and auth middleware commonly wrap the ResponseWriter.
// Upgrade must reach the connection through any wrapper that exposes Unwrap,
// as http.ResponseController does, and the wrapper must see no status code
// or body: after a successful Upgrade the HTTP response is the 101 the
// package wrote on the hijacked connection.
func TestEndToEndBehindMiddleware(t *testing.T) {
	type record struct {
		status int
		err    error
	}
	records := make(chan record, 2)
	logging := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			records <- record{status: sw.status}
		})
	}
	requireToken := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer secret" {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	ws := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Upgrade(w, r)
		if err != nil {
			records <- record{err: err}
			return
		}
		defer c.Close()
		echoLoop(c)
	})
	srv := httptest.NewServer(logging(requireToken(ws)))
	t.Cleanup(srv.Close)

	// Middleware runs before the handshake, so a rejection is an ordinary
	// HTTP response that Dial returns to the caller.
	_, resp, err := websocket.Dial(t.Context(), wsURL(srv))
	if !errors.Is(err, websocket.ErrBadHandshake) {
		t.Fatalf("Dial without token: err = %v, want ErrBadHandshake", err)
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("Dial without token: response = %+v, want 401 with WWW-Authenticate", resp)
	}
	if rec := <-records; rec.status != http.StatusUnauthorized {
		t.Fatalf("logged status %d, want 401", rec.status)
	}

	d := websocket.Dialer{Header: http.Header{"Authorization": {"Bearer secret"}}}
	c, _, err := d.Dial(t.Context(), wsURL(srv))
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c, websocket.TextMessage, []byte("through two middlewares"))
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if rec := <-records; rec.err != nil || rec.status != 0 {
		t.Fatalf("logged %+v after upgrade, want no status and no error", rec)
	}
}

// statusWriter records the status a handler writes, as logging middleware
// does, and exposes the writer it wraps.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// A client behind an HTTP proxy reaches the server through a CONNECT tunnel,
// for both ws and wss URLs.
func TestEndToEndThroughProxy(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		echoLoop(c)
	})
	plain := httptest.NewServer(handler)
	t.Cleanup(plain.Close)
	secure := httptest.NewTLSServer(handler)
	t.Cleanup(secure.Close)

	var mu sync.Mutex
	var tunnels []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" { // user:pass
			w.Header().Set("Proxy-Authenticate", "Basic")
			http.Error(w, "proxy auth required", http.StatusProxyAuthRequired)
			return
		}
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		mu.Lock()
		tunnels = append(tunnels, r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		client, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		done := make(chan struct{})
		go func() {
			io.Copy(upstream, buf)
			upstream.(*net.TCPConn).CloseWrite()
			close(done)
		}()
		io.Copy(client, upstream)
		<-done
	}))
	t.Cleanup(proxy.Close)

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL.User = url.UserPassword("user", "pass")

	for _, srv := range []*httptest.Server{plain, secure} {
		var tlsConfig *tls.Config
		if srv.TLS != nil {
			tlsConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
		}
		d := websocket.Dialer{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: tlsConfig}
		c, _, err := d.Dial(t.Context(), wsURL(srv))
		if err != nil {
			t.Fatalf("Dial %s via proxy: %v", srv.URL, err)
		}
		roundTrip(t, c, websocket.BinaryMessage, randomBytes(64<<10))
		c.Close()
	}

	// Without credentials the proxy refuses, and Dial reports its answer.
	proxyURL.User = nil
	d := websocket.Dialer{Proxy: http.ProxyURL(proxyURL)}
	if _, _, err := d.Dial(t.Context(), wsURL(plain)); !errors.Is(err, websocket.ErrBadHandshake) {
		t.Fatalf("Dial with rejected proxy auth: err = %v, want ErrBadHandshake", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{plain.Listener.Addr().String(), secure.Listener.Addr().String()}
	if fmt.Sprint(tunnels) != fmt.Sprint(want) {
		t.Fatalf("proxy tunnelled to %v, want %v", tunnels, want)
	}
}

// A server shutting down tells each client why with StatusGoingAway, the
// way a deploy or a scale-in should: http.Server.Shutdown stops new
// connections, RegisterOnShutdown reaches the hijacked ones it cannot see,
// and each handler finishes the closing handshake with Conn.Shutdown.
func TestEndToEndGracefulServerShutdown(t *testing.T) {
	const clients = 8
	stopping := make(chan struct{})
	var handlers sync.WaitGroup
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		handlers.Add(1)
		defer handlers.Done()
		defer c.Close()

		readErr := make(chan error, 1)
		go func() { readErr <- echoLoop(c) }()
		select {
		case <-stopping:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.Shutdown(ctx, websocket.StatusGoingAway, "server restarting"); err != nil {
				t.Errorf("server Shutdown: %v", err)
			}
			<-readErr
		case <-readErr:
		}
	}))
	srv.Config.RegisterOnShutdown(func() { close(stopping) })
	srv.Start()
	t.Cleanup(srv.Close)

	conns := make([]*websocket.Conn, clients)
	for i := range conns {
		c, _, err := websocket.Dial(t.Context(), wsURL(srv))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		roundTrip(t, c, websocket.TextMessage, fmt.Appendf(nil, "client %d", i))
		conns[i] = c
	}

	// Each client's read loop must see the server's close, then end.
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Go(func() {
			for {
				_, _, err := c.ReadMessage(context.Background())
				if err == nil {
					continue
				}
				var ce *websocket.CloseError
				if !errors.As(err, &ce) || ce.Code != websocket.StatusGoingAway || ce.Reason != "server restarting" {
					t.Errorf("client %d: read error = %v, want StatusGoingAway with the server's reason", i, err)
				}
				if !errors.Is(err, io.EOF) {
					t.Errorf("client %d: going-away close %v does not match io.EOF", i, err)
				}
				return
			}
		})
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := srv.Config.Shutdown(ctx); err != nil {
		t.Fatalf("http.Server.Shutdown: %v", err)
	}
	if _, _, err := websocket.Dial(t.Context(), wsURL(srv)); err == nil {
		t.Fatal("Dial succeeded after Shutdown")
	}
	handlers.Wait()
	wg.Wait()
}

// Each side's closing handshake reaches the other with its code and reason,
// whichever side starts it, and a closed connection reports net.ErrClosed.
func TestEndToEndCloseHandshake(t *testing.T) {
	serverSaw := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		switch r.URL.Path {
		case "/server-closes":
			if _, _, err := c.ReadMessage(context.Background()); err != nil {
				serverSaw <- err
				return
			}
			c.CloseWithStatus(websocket.StatusPolicyViolation, "quota exceeded")
			_, _, err := c.ReadMessage(context.Background())
			serverSaw <- err
		default:
			serverSaw <- echoLoop(c)
		}
	}))
	t.Cleanup(srv.Close)

	t.Run("client closes", func(t *testing.T) {
		c, _, err := websocket.Dial(t.Context(), wsURL(srv))
		if err != nil {
			t.Fatal(err)
		}
		roundTrip(t, c, websocket.TextMessage, []byte("hi"))
		if err := c.CloseWithStatus(websocket.StatusNormalClosure, "done"); err != nil {
			t.Fatalf("CloseWithStatus: %v", err)
		}
		err = <-serverSaw
		var ce *websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != websocket.StatusNormalClosure || ce.Reason != "done" {
			t.Fatalf("server read error = %v, want the client's 1000 \"done\"", err)
		}
		if err := c.WriteMessage(context.Background(), websocket.TextMessage, []byte("late")); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("write after close: err = %v, want net.ErrClosed", err)
		}
		if _, _, err := c.ReadMessage(context.Background()); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("read after close: err = %v, want net.ErrClosed", err)
		}
	})

	t.Run("server closes", func(t *testing.T) {
		c, _, err := websocket.Dial(t.Context(), wsURL(srv)+"/server-closes")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := c.WriteMessage(context.Background(), websocket.TextMessage, []byte("over quota")); err != nil {
			t.Fatal(err)
		}
		_, _, err = c.ReadMessage(context.Background())
		if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("client read error = %v, want StatusPolicyViolation", err)
		}
		if errors.Is(err, io.EOF) {
			t.Fatalf("policy-violation close %v matches io.EOF; only normal closes should", err)
		}
		// The client echoed the Close, so the server's read loop sees the
		// end of the handshake as its own close.
		if err := <-serverSaw; !errors.Is(err, net.ErrClosed) && websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("server read error after its close = %v", err)
		}
	})
}

// Both sides keep a quiet connection alive with Conn.Ping, which needs the
// other side's read loop to answer, and a peer that stops answering is
// detected within the Ping's context.
func TestEndToEndKeepalive(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		if r.URL.Path == "/unresponsive" {
			// No read loop: Pings go unanswered.
			<-release
			return
		}
		go echoLoop(c)
		for range 3 {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			err := c.Ping(ctx)
			cancel()
			if err != nil {
				t.Errorf("server Ping: %v", err)
				return
			}
		}
		c.CloseWithStatus(websocket.StatusNormalClosure, "")
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	c, _, err := websocket.Dial(t.Context(), wsURL(srv))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readDone := make(chan error, 1)
	go func() {
		for {
			if _, _, err := c.ReadMessage(context.Background()); err != nil {
				readDone <- err
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("client Ping: %v", err)
	}
	if err := <-readDone; websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("client read loop ended with %v, want the server's normal close", err)
	}

	quiet, _, err := websocket.Dial(t.Context(), wsURL(srv)+"/unresponsive")
	if err != nil {
		t.Fatal(err)
	}
	defer quiet.Close()
	go func() {
		for {
			if _, _, err := quiet.ReadMessage(context.Background()); err != nil {
				return
			}
		}
	}()
	ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := quiet.Ping(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ping of a peer that never reads: err = %v, want context.DeadlineExceeded", err)
	}
}

// A message over the server's MaxMessageSize ends the connection with
// StatusMessageTooBig on both sides, and the server's read reports
// ErrMessageTooLarge, whether the message arrives whole, fragmented by
// NextWriter or compressed.
func TestEndToEndMessageTooLarge(t *testing.T) {
	const limit = 32 << 10
	serverErr := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := websocket.Upgrader{MaxMessageSize: limit, Compression: &websocket.Compression{}}
		c, err := u.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		serverErr <- echoLoop(c)
	}))
	t.Cleanup(srv.Close)

	send := map[string]func(*websocket.Conn) error{
		"whole": func(c *websocket.Conn) error {
			return c.WriteMessage(context.Background(), websocket.BinaryMessage, randomBytes(limit+1))
		},
		"fragmented": func(c *websocket.Conn) error {
			w, err := c.NextWriter(context.Background(), websocket.BinaryMessage)
			if err != nil {
				return err
			}
			for range 5 {
				if _, err := w.Write(randomBytes(limit / 4)); err != nil {
					return err
				}
			}
			return w.Close()
		},
		"compressed": func(c *websocket.Conn) error {
			// Deflate shrinks this far below the limit on the wire; the
			// limit applies to the inflated message.
			return c.WriteMessage(context.Background(), websocket.TextMessage, bytes.Repeat([]byte{'a'}, 4*limit))
		},
	}
	for name, write := range send {
		t.Run(name, func(t *testing.T) {
			d := websocket.Dialer{Compression: &websocket.Compression{}}
			c, _, err := d.Dial(t.Context(), wsURL(srv))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			// A message of exactly the limit passes, even though deflate
			// makes random data a little larger on the wire.
			roundTrip(t, c, websocket.BinaryMessage, randomBytes(limit))

			// The server may close before the client finishes writing, so
			// a write error is expected; the read reports why.
			write(c)
			_, _, err = c.ReadMessage(context.Background())
			if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
				t.Fatalf("client read error = %v, want StatusMessageTooBig", err)
			}
			if err := <-serverErr; !errors.Is(err, websocket.ErrMessageTooLarge) || !errors.Is(err, websocket.ErrProtocol) {
				t.Fatalf("server read error = %v, want ErrMessageTooLarge", err)
			}
		})
	}
}

// When the peer's process dies without a closing handshake, the read loop
// ends with an error that is not a CloseError, so programs can tell an
// abnormal closure from a clean one.
func TestEndToEndAbruptDisconnect(t *testing.T) {
	serverErr := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		serverErr <- echoLoop(c)
	}))
	t.Cleanup(srv.Close)

	// A client that vanishes: dial with a raw TCP connection the test can
	// cut, as a crashed process or a dropped NAT mapping would.
	var raw net.Conn
	d := websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		raw = c
		return c, err
	}}
	c, _, err := d.Dial(t.Context(), wsURL(srv))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	roundTrip(t, c, websocket.TextMessage, []byte("alive"))
	raw.(*net.TCPConn).SetLinger(0) // reset, not FIN
	raw.Close()

	select {
	case err := <-serverErr:
		if err == nil || websocket.CloseStatus(err) != -1 {
			t.Fatalf("server read error = %v, want a transport error without a close status", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server read loop did not notice the dead client")
	}
}

// Many clients chat through one server at once, with compression on and
// messages of mixed sizes, while each server connection has several
// concurrent writers. Every client must receive every message intact and in
// each sender's order. Run with -race, this is the package's concurrency
// smoke test over real sockets.
func TestEndToEndConcurrentBroadcast(t *testing.T) {
	const (
		clients  = 8
		messages = 16
	)
	type hub struct {
		mu    sync.Mutex
		conns map[*websocket.Conn]struct{}
	}
	h := &hub{conns: make(map[*websocket.Conn]struct{})}
	var joined sync.WaitGroup
	joined.Add(clients)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := websocket.Upgrader{Compression: &websocket.Compression{Threshold: 64}}
		c, err := u.Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		h.mu.Lock()
		h.conns[c] = struct{}{}
		h.mu.Unlock()
		joined.Done()
		defer func() {
			h.mu.Lock()
			delete(h.conns, c)
			h.mu.Unlock()
		}()
		for {
			typ, data, err := c.ReadMessage(context.Background())
			if err != nil {
				return
			}
			// Fan out from this goroutine, writing to every connection
			// while other handlers do the same: Conn serializes writers.
			h.mu.Lock()
			peers := make([]*websocket.Conn, 0, len(h.conns))
			for p := range h.conns {
				peers = append(peers, p)
			}
			h.mu.Unlock()
			for _, p := range peers {
				p.WriteMessage(context.Background(), typ, data)
			}
		}
	}))
	t.Cleanup(srv.Close)

	conns := make([]*websocket.Conn, clients)
	for i := range conns {
		d := websocket.Dialer{Compression: &websocket.Compression{Threshold: 64}}
		c, _, err := d.Dial(t.Context(), wsURL(srv))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns[i] = c
	}
	joined.Wait()

	// payload is message n from client i: a header the receiver checks for
	// order, padded to a size that varies from a few bytes to tens of KiB.
	payload := func(i, n int) []byte {
		b := fmt.Appendf(nil, "%d:%d:", i, n)
		size := (i*messages + n) * 97 % (40 << 10)
		return append(b, bytes.Repeat([]byte{byte('a' + n%26)}, size)...)
	}

	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Go(func() {
			for n := range messages {
				if err := c.WriteMessage(context.Background(), websocket.TextMessage, payload(i, n)); err != nil {
					t.Errorf("client %d write %d: %v", i, n, err)
					return
				}
			}
		})
		wg.Go(func() {
			next := make([]int, clients)
			for range clients * messages {
				_, data, err := c.ReadMessage(context.Background())
				if err != nil {
					t.Errorf("client %d read: %v", i, err)
					return
				}
				var from, n int
				if _, err := fmt.Sscanf(string(data), "%d:%d:", &from, &n); err != nil {
					t.Errorf("client %d: bad message %.20q", i, data)
					return
				}
				if n != next[from] || !bytes.Equal(data, payload(from, n)) {
					t.Errorf("client %d: got message %d from %d, want %d intact", i, n, from, next[from])
					return
				}
				next[from]++
			}
		})
	}
	wg.Wait()
	for _, c := range conns {
		c.CloseWithStatus(websocket.StatusNormalClosure, "")
	}
}
