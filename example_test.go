package websocket_test

import (
	"bufio"
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
			typ, data, err := conn.ReadMessage()
			if err != nil {
				return // io.EOF-matching *CloseError on a normal close
			}
			if err := conn.WriteMessage(typ, data); err != nil {
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

	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		log.Fatal(err)
	}
	typ, data, err := conn.ReadMessage()
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
		_ = conn.WriteMessage(websocket.TextMessage, []byte("welcome, "+conn.Subprotocol()))
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	dialer := &websocket.Dialer{Subprotocols: []string{"chat.v1"}}
	conn, _, err := dialer.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	_, data, err := conn.ReadMessage()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))
	// Output: welcome, chat.v1
}

// A rejected handshake returns an error matching ErrBadHandshake, together
// with the server's response so the caller can inspect its status.
func ExampleDial_rejected() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, resp, err := websocket.Dial(context.Background(), wsURL(srv))
	fmt.Println(errors.Is(err, websocket.ErrBadHandshake), resp.StatusCode)
	// Output: true 401
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

	_, _, err = conn.ReadMessage()
	fmt.Println(websocket.CloseStatus(err), errors.Is(err, io.EOF))

	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		fmt.Println(ce.Reason)
	}
	fmt.Println(websocket.CloseStatus(errors.New("not a close")))
	// Output:
	// StatusServiceRestart true
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
			if _, _, err := conn.ReadMessage(); err != nil {
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
			if _, _, err := conn.ReadMessage(); err != nil {
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

// WriteMessageContext bounds a write, including the wait for another writer,
// so a stalled peer cannot hold the caller forever. If the context ends
// mid-write the connection is aborted, because a partial frame cannot be
// retried.
func ExampleConn_WriteMessageContext() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		_, data, _ := conn.ReadMessage()
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
	if err := conn.WriteMessageContext(ctx, websocket.TextMessage, []byte("tick")); err != nil {
		log.Fatal(err)
	}
	// Wait for the server's reply to the close so its output is printed.
	_, _, _ = conn.ReadMessage()
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
		typ, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(typ, data)
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
	if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		log.Fatal(err)
	}
	_, data, err := conn.ReadMessage()
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
		_ = conn.WriteMessage(websocket.TextMessage, []byte("served for "+r.Host))
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
	_, data, err := conn.ReadMessage()
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

// ReadMessageContext bounds a single read. When the context ends mid-read
// the connection is aborted, just as after a read timeout, so later calls
// fail with an error matching net.ErrClosed.
func ExampleConn_ReadMessageContext() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage() // Never sends; waits for the client to go.
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err = conn.ReadMessageContext(ctx)
	fmt.Println(errors.Is(err, context.DeadlineExceeded))

	err = conn.WriteMessage(websocket.TextMessage, []byte("too late"))
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
		if _, _, err := conn.ReadMessage(); err != nil {
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

// A Ping handler runs inside ReadMessage before it returns the next message.
// A replacement handler must answer with a Pong itself. Most programs keep
// the default, which already does.
func ExampleConn_SetPingHandler() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteControlFrame(websocket.PingMessage, []byte("heartbeat 1"))
		_ = conn.WriteMessage(websocket.TextMessage, []byte("hello"))
		_, _, _ = conn.ReadMessage()
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	conn.SetPingHandler(func(appData string) error {
		fmt.Println("ping:", appData)
		return conn.WriteControlFrame(websocket.PongMessage, []byte(appData))
	})

	_, data, err := conn.ReadMessage()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("message:", string(data))
	// Output:
	// ping: heartbeat 1
	// message: hello
}

// ConnectionState reports the TLS state of a wss connection, on the client
// and on a server upgraded from HTTPS, for example to read the peer's
// certificates.
func ExampleConn_ConnectionState() {
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
	conn, _, err := dialer.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	state, ok := conn.ConnectionState()
	fmt.Println(ok, tls.VersionName(state.Version), len(state.PeerCertificates) > 0)
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
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprint(len(data))))
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(srv))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	w, err := conn.NextWriter(websocket.BinaryMessage)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := io.Copy(w, io.LimitReader(zeros{}, 300<<10)); err != nil {
		log.Fatal(err)
	}
	if err := w.Close(); err != nil {
		log.Fatal(err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))
	// Output: 307200
}

// zeros is an endless reader of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
