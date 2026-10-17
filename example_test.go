package websocket_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/picatz/websocket"
)

// wsURL turns an httptest server URL into the matching ws or wss URL.
func wsURL(s *httptest.Server) string {
	return "ws" + strings.TrimPrefix(s.URL, "http")
}

// An echo server and a client that talks to it. The server reads each
// connection from one goroutine until ReadMessage returns an error, which is
// a *websocket.CloseError when the peer closes normally.
func Example() {
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return // Upgrade has already written the HTTP error response.
		}
		defer conn.Close()
		for {
			typ, data, err := conn.ReadMessage(context.Background())
			if err != nil {
				return // io.EOF-matching *CloseError on a normal close
			}
			if err := conn.WriteMessage(context.Background(), typ, data); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(echo)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(context.Background(), websocket.TextMessage, []byte("hello")); err != nil {
		log.Fatal(err)
	}
	typ, data, err := conn.ReadMessage(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(typ, string(data))
	// Output: TextMessage hello
}

// An Upgrader holds the server's handshake settings and is safe to share
// between handlers. When the handshake fails, Upgrade has already answered the
// request with a 4xx or 5xx response, so the handler only returns.
func ExampleUpgrader() {
	upgrader := &websocket.Upgrader{
		Subprotocols:   []string{"chat.v2", "chat.v1"},
		MaxMessageSize: 64 << 10,
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte("welcome, "+conn.Subprotocol()))
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	dialer := &websocket.Dialer{Subprotocols: []string{"chat.v1"}}
	conn, _, err := dialer.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	_, data, err := conn.ReadMessage(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))
	// Output: welcome, chat.v1
}

// A rejected handshake returns an error matching ErrBadHandshake, together
// with the server's response and the start of its body.
func ExampleDial_rejected() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, resp, err := websocket.Dial(context.Background(), wsURL(srv))
	if errors.Is(err, websocket.ErrBadHandshake) && resp != nil {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("%d %s", resp.StatusCode, body)
	}
	// Output: 401 sign in first
}

// CloseWithStatus tells the peer why the connection is ending. The peer's
// ReadMessage returns a *CloseError, and CloseStatus extracts its code from
// any error, returning -1 when the error is not a close.
func ExampleCloseStatus() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		_ = conn.CloseWithStatus(websocket.StatusServiceRestart, "deploying")
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	_, _, err = conn.ReadMessage(context.Background())
	fmt.Println(websocket.CloseStatus(err), errors.Is(err, io.EOF))

	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		fmt.Println(ce.Reason)
	}
	fmt.Println(websocket.CloseStatus(errors.New("not a close")))
	// Output:
	// StatusServiceRestart false
	// deploying
	// StatusCode(-1)
}

// Ping measures a round trip and detects dead peers. The Pong is delivered by
// ReadMessage, so a read loop must be running on the same connection.
func ExampleConn_Ping() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			// ReadMessage answers Pings automatically.
			if _, _, err := conn.ReadMessage(context.Background()); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	go func() {
		for {
			if _, _, err := conn.ReadMessage(context.Background()); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Ping(ctx); err != nil {
		// A timeout leaves the connection open; closing it is the caller's call.
		conn.Close()
		log.Fatal(err)
	}
	fmt.Println("pong")
	// Output: pong
}

// WriteMessage's context bounds the write, including the wait for another writer,
// so a stalled peer cannot hold the caller forever. If the context ends
// mid-write the connection is aborted, because a partial frame cannot be
// retried.
func ExampleConn_WriteMessage() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		_, data, _ := conn.ReadMessage(context.Background())
		fmt.Printf("server got %s\n", data)
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.WriteMessage(ctx, websocket.TextMessage, []byte("tick")); err != nil {
		log.Fatal(err)
	}
	// Wait for the server's reply to the close so its output is printed.
	_, _, _ = conn.ReadMessage(context.Background())
	// Output: server got tick
}

// Compression is opt-in on both sides, and the handshake decides whether it is
// used. A Compression value builds a fresh extension for every connection, so
// one Dialer or Upgrader can be shared. Messages shorter than Threshold are
// sent uncompressed.
func ExampleCompression() {
	upgrader := &websocket.Upgrader{Compression: &websocket.Compression{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		typ, data, err := conn.ReadMessage(context.Background())
		if err != nil {
			return
		}
		_ = conn.WriteMessage(context.Background(), typ, data)
	}))
	defer srv.Close()

	dialer := &websocket.Dialer{Compression: &websocket.Compression{Threshold: 256}}
	conn, resp, err := dialer.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	fmt.Println(resp.Header.Get("Sec-WebSocket-Extensions"))

	msg := strings.Repeat("compressible ", 100)
	if err := conn.WriteMessage(context.Background(), websocket.TextMessage, []byte(msg)); err != nil {
		log.Fatal(err)
	}
	_, data, err := conn.ReadMessage(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(data) == len(msg))
	// Output:
	// permessage-deflate; client_max_window_bits=15
	// true
}

// Behind a TLS-terminating proxy the request's Host and scheme are not the
// public origin, so replace the same-origin check with an exact allowlist.
// A rejected origin is answered with 403 Forbidden.
func ExampleUpgrader_checkOrigin() {
	allowed := map[string]bool{"https://app.example.com": true}
	upgrader := &websocket.Upgrader{CheckOrigin: func(r *http.Request, origin string) bool {
		return allowed[origin]
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r)
		if err != nil {
			return
		}
		conn.Close()
	}))
	defer srv.Close()

	for _, origin := range []string{"https://app.example.com", "https://evil.example"} {
		dialer := &websocket.Dialer{Header: http.Header{"Origin": {origin}}}
		conn, resp, err := dialer.Dial(context.Background(), wsURL(srv))
		if err != nil {
			fmt.Println(origin, resp.StatusCode)
			continue
		}
		conn.Close()
		fmt.Println(origin, resp.StatusCode)
	}
	// Output:
	// https://app.example.com 101
	// https://evil.example 403
}

// A Dialer carries client settings and may be shared by many dials. Header
// adds request fields such as credentials, and the server's handshake
// response is returned alongside the connection.
func ExampleDialer() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		upgrader := &websocket.Upgrader{
			Subprotocols: []string{"chat.v1"},
			Header:       http.Header{"X-Server": {"example"}},
		}
		conn, err := upgrader.Upgrade(w, r)
		if err != nil {
			return
		}
		conn.Close()
	}))
	defer srv.Close()

	dialer := &websocket.Dialer{
		Header:         http.Header{"Authorization": {"Bearer secret-token"}},
		Subprotocols:   []string{"chat.v1"},
		MaxMessageSize: 64 << 10,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := dialer.Dial(ctx, wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	fmt.Println(resp.StatusCode, resp.Header.Get("X-Server"), conn.Subprotocol())
	// Output: 101 example chat.v1
}

// NetDialContext replaces the TCP dialer, for service discovery, Unix
// sockets, SOCKS proxies or tests. Here every host name resolves to one local
// server, while the URL's host is still sent in the handshake.
func ExampleDialer_netDialContext() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte("served for "+r.Host))
	}))
	defer srv.Close()

	dialer := &websocket.Dialer{
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Listener.Addr().String())
		},
	}
	conn, _, err := dialer.Dial(context.Background(), "ws://chat.internal/socket")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	_, data, err := conn.ReadMessage(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))
	// Output: served for chat.internal
}

// Error replaces the body Upgrade writes when it rejects a request, for
// example to match an API's JSON errors. The status and any header fields the
// status requires are already decided.
func ExampleUpgrader_error() {
	upgrader := &websocket.Upgrader{
		Error: func(w http.ResponseWriter, r *http.Request, status int, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprintf(w, "{\"error\":%q}\n", http.StatusText(status))
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r)
		if err != nil {
			return // Error has written the response.
		}
		conn.Close()
	}))
	defer srv.Close()

	// A plain HTTP request is not a WebSocket handshake.
	resp, err := http.Get(srv.URL)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(resp.StatusCode, " ", string(body))
	// Output: 400 {"error":"Bad Request"}
}

// ReadMessage's context bounds a single read. When the context ends mid-read
// the connection is aborted, just as after a read timeout, so later calls
// fail with an error matching net.ErrClosed.
func ExampleConn_ReadMessage_timeout() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage(context.Background()) // Never sends; waits for the client to go.
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err = conn.ReadMessage(ctx)
	fmt.Println(errors.Is(err, context.DeadlineExceeded))

	err = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte("too late"))
	fmt.Println(errors.Is(err, net.ErrClosed))
	// Output:
	// true
	// true
}

// A server shutting down ends each connection with Shutdown, which waits for
// the client's reply to its Close frame before closing the transport, so the
// client reads the status instead of a connection reset. Give it a deadline,
// since a peer may never answer.
func ExampleConn_Shutdown_server() {
	shutdown := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		shutdown <- conn.Shutdown(ctx, websocket.StatusGoingAway, "server restarting")
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	// The client's read loop answers the Close frame and ends with the
	// server's status.
	for {
		if _, _, err := conn.ReadMessage(context.Background()); err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				fmt.Println("client:", ce.Code, ce.Reason)
			}
			break
		}
	}
	fmt.Println("server:", <-shutdown)
	// Output:
	// client: StatusGoingAway server restarting
	// server: <nil>
}

// As with net/http, Dial's response carries the TLS state of a wss
// connection, for example to read the server's certificates. A server reads
// the client's from the request's TLS field.
func ExampleDial_tls() {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		conn.Close()
	}))
	defer srv.Close()

	// Trust the test server's certificate.
	dialer := &websocket.Dialer{
		TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig,
	}
	conn, resp, err := dialer.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	state := resp.TLS
	fmt.Println(state != nil, tls.VersionName(state.Version), len(state.PeerCertificates) > 0)
	// Output: true TLS 1.3 true
}

// NetConn turns a connection into a net.Conn, so code written for streams
// can run over a WebSocket. Here a line-based protocol uses bufio on both
// ends; message boundaries are not preserved.
func ExampleNetConn() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		conn := websocket.NetConn(r.Context(), ws, websocket.BinaryMessage)
		defer conn.Close()
		lines := bufio.NewScanner(conn)
		for lines.Scan() {
			fmt.Fprintln(conn, strings.ToUpper(lines.Text()))
		}
	}))
	defer srv.Close()

	ws, _, err := websocket.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	conn := websocket.NetConn(context.Background(), ws, websocket.BinaryMessage)
	defer conn.Close()

	replies := bufio.NewReader(conn)
	for _, line := range []string{"hello", "stream"} {
		fmt.Fprintln(conn, line)
		reply, err := replies.ReadString('\n')
		if err != nil {
			log.Fatal(err)
		}
		fmt.Print(reply)
	}
	// Output:
	// HELLO
	// STREAM
}

// NextWriter streams one message of unknown length, such as a file, without
// holding it in memory. Close sends the final frame.
func ExampleConn_NextWriter() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		_, data, err := conn.ReadMessage(context.Background())
		if err != nil {
			return
		}
		_ = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte(fmt.Sprint(len(data))))
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	w, err := conn.NextWriter(context.Background(), websocket.BinaryMessage)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := io.Copy(w, io.LimitReader(zeros{}, 300<<10)); err != nil {
		log.Fatal(err)
	}
	if err := w.Close(); err != nil {
		log.Fatal(err)
	}
	_, data, err := conn.ReadMessage(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))
	// Output: 307200
}

// A large message can be consumed as it arrives instead of held in memory.
// MaxMessageSize still bounds the whole message.
func ExampleConn_NextReader() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		mw, err := conn.NextWriter(context.Background(), websocket.BinaryMessage)
		if err != nil {
			return
		}
		_, _ = io.Copy(mw, io.LimitReader(zeros{}, 8<<20))
		_ = mw.Close()
		_, _, _ = conn.ReadMessage(context.Background())
	}))
	defer srv.Close()

	d := &websocket.Dialer{MaxMessageSize: 64 << 20}
	conn, _, err := d.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	_, r, err := conn.NextReader(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	n, err := io.Copy(io.Discard, r)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(n)
	// Output: 8388608
}

// zeros is an endless reader of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// CloseWrite is a half-close: it tells the server that no more requests are
// coming but keeps reading, so replies already on their way still arrive.
// The read loop ends with the server's answering Close frame.
func ExampleConn_CloseWrite() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			typ, data, err := conn.ReadMessage(context.Background())
			if err != nil {
				return
			}
			_ = conn.WriteMessage(context.Background(), typ, bytes.ToUpper(data))
		}
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	for _, req := range []string{"one", "two", "three"} {
		if err := conn.WriteMessage(context.Background(), websocket.TextMessage, []byte(req)); err != nil {
			log.Fatal(err)
		}
	}
	if err := conn.CloseWrite(websocket.StatusNormalClosure, "done"); err != nil {
		log.Fatal(err)
	}

	// Bound the drain: CloseWrite does not wait for the server.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, data, err := conn.ReadMessage(ctx)
		if err != nil {
			fmt.Println("closed:", websocket.CloseStatus(err))
			break
		}
		fmt.Println(string(data))
	}
	// Output:
	// ONE
	// TWO
	// THREE
	// closed: StatusNormalClosure
}

// NewConn runs the protocol over a connection whose opening handshake the
// caller performed some other way, such as over a custom transport or after
// a proprietary upgrade. Here the two ends of a net.Pipe stand in for it.
func ExampleNewConn() {
	clientSide, serverSide := net.Pipe()

	server, err := websocket.NewConn(serverSide, websocket.ConnConfig{Server: true})
	if err != nil {
		log.Fatal(err)
	}
	client, err := websocket.NewConn(clientSide, websocket.ConnConfig{})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	// The server reads until the client closes, so it is reading when the
	// client's Close frame arrives and answers it at once. A net.Pipe has no
	// buffer: a side that stops reading stalls the other side's writes.
	go func() {
		defer server.Close()
		for {
			typ, data, err := server.ReadMessage(context.Background())
			if err != nil {
				return
			}
			if err := server.WriteMessage(context.Background(), typ, append([]byte("echo: "), data...)); err != nil {
				return
			}
		}
	}()

	if err := client.WriteMessage(context.Background(), websocket.TextMessage, []byte("over a pipe")); err != nil {
		log.Fatal(err)
	}
	_, data, err := client.ReadMessage(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))
	// Output: echo: over a pipe
}

// NetConn can stand in for a TCP connection in an http.Transport, so HTTP
// requests travel through a WebSocket tunnel. The server here answers one
// request read straight from the tunnel.
func ExampleNetConn_httpTransport() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		conn := websocket.NetConn(context.Background(), ws, websocket.BinaryMessage)
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		body := "tunnelled " + req.URL.Path
		resp := &http.Response{
			StatusCode:    http.StatusOK,
			ProtoMajor:    1,
			ProtoMinor:    1,
			ContentLength: int64(len(body)),
			Body:          io.NopCloser(strings.NewReader(body)),
			Close:         true,
		}
		_ = resp.Write(conn)
	}))
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			ws, _, err := websocket.Dial(ctx, wsURL(srv))
			if err != nil {
				return nil, err
			}
			return websocket.NetConn(context.Background(), ws, websocket.BinaryMessage), nil
		},
	}}
	resp, err := client.Get("http://backend.internal/status")
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(resp.StatusCode, string(body))
	// Output: 200 tunnelled /status
}

// Proxy has the signature of http.Transport.Proxy, so
// http.ProxyFromEnvironment honors HTTP_PROXY, HTTPS_PROXY and NO_PROXY,
// selecting for a wss URL as for https. Dial tunnels through the proxy with
// CONNECT and runs TLS end to end inside it.
func ExampleDialer_proxy() {
	dialer := &websocket.Dialer{Proxy: http.ProxyFromEnvironment}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := dialer.Dial(ctx, "wss://example.com/socket")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
}

// A read loop ends at the first error, and the error says why. Here the
// server limits messages to 16 bytes, so the client's second message ends the
// connection: the server's ReadMessage reports ErrMessageTooLarge, and the
// client is sent StatusMessageTooBig.
func ExampleConn_ReadMessage() {
	ended := make(chan string, 1)
	upgrader := &websocket.Upgrader{MaxMessageSize: 16}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage(context.Background())
			switch {
			case err == nil:
				fmt.Printf("server read %q\n", data)
				continue
			case websocket.CloseStatus(err) != -1:
				ended <- "the client closed"
			case errors.Is(err, websocket.ErrMessageTooLarge):
				ended <- "the client sent too large a message"
			case errors.Is(err, websocket.ErrProtocol):
				ended <- "the client broke the protocol"
			case errors.Is(err, net.ErrClosed):
				ended <- "this side closed the connection"
			default:
				ended <- "the transport failed or a deadline passed"
			}
			return
		}
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	_ = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte("short"))
	_ = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte("far longer than sixteen bytes"))

	fmt.Println("server:", <-ended)
	_, _, err = conn.ReadMessage(context.Background())
	fmt.Println("client:", websocket.CloseStatus(err))
	// Output:
	// server read "short"
	// server: the client sent too large a message
	// client: StatusMessageTooBig
}
