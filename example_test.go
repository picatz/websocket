package websocket_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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
			http.Error(w, "websocket upgrade failed", http.StatusBadRequest)
			return
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

// Upgrade takes over an HTTP/1.1 request. Errors returned before the
// connection is hijacked leave the response writer untouched, so the handler
// chooses the HTTP response.
func ExampleUpgrade() {
	handler := func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r,
			websocket.WithUpgradeSubprotocols("chat.v2", "chat.v1"),
			websocket.WithUpgradeMaxMessageSize(64<<10),
		)
		if err != nil {
			if errors.Is(err, websocket.ErrOriginNotAllowed) {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			http.Error(w, "websocket upgrade failed", http.StatusBadRequest)
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte("welcome, "+conn.Subprotocol()))
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	conn, _, err := websocket.Dial(context.Background(), wsURL(srv),
		websocket.WithSubprotocols("chat.v1"))
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
// used. Each connection needs its own extension instance. Messages shorter
// than the threshold are sent uncompressed.
func ExampleNewPerMessageDeflateExtension() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r,
			websocket.WithUpgradeExtensions(websocket.NewPerMessageDeflateExtension()))
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

	pmd := websocket.NewPerMessageDeflateExtension(
		websocket.WithCompressionThreshold(256),
	)
	conn, resp, err := websocket.Dial(context.Background(), wsURL(srv),
		websocket.WithExtensions(pmd))
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
func ExampleWithUpgradeOriginCheck() {
	allowed := map[string]bool{"https://app.example.com": true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r,
			websocket.WithUpgradeOriginCheck(func(r *http.Request, origin string) bool {
				return allowed[origin]
			}))
		if errors.Is(err, websocket.ErrOriginNotAllowed) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if err != nil {
			return
		}
		conn.Close()
	}))
	defer srv.Close()

	for _, origin := range []string{"https://app.example.com", "https://evil.example"} {
		header := http.Header{"Origin": {origin}}
		conn, resp, err := websocket.Dial(context.Background(), wsURL(srv),
			websocket.WithHeader(header))
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
