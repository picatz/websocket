package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

func dial(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestEcho(t *testing.T) {
	srv := httptest.NewServer(newServer(t.Context()))
	defer srv.Close()
	conn := dial(t, srv)

	for _, msg := range []struct {
		typ  websocket.MessageType
		data string
	}{
		{websocket.TextMessage, "hello"},
		{websocket.BinaryMessage, "\x00\x01\x02"},
		{websocket.TextMessage, ""},
	} {
		if err := conn.WriteMessage(msg.typ, []byte(msg.data)); err != nil {
			t.Fatal(err)
		}
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if typ != msg.typ || string(data) != msg.data {
			t.Fatalf("echo = %v %q, want %v %q", typ, data, msg.typ, msg.data)
		}
	}
}

// TestShutdown checks that a server shutdown reaches each client as a
// going-away close, and that the server waits for its connections.
func TestShutdown(t *testing.T) {
	ctx, shutdown := context.WithCancel(t.Context())
	s := newServer(ctx)
	srv := httptest.NewServer(s)
	defer srv.Close()
	conn := dial(t, srv)

	// A round trip proves the handler is running before shutdown starts.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}

	shutdown()
	_, _, err := conn.ReadMessage()
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusGoingAway {
		t.Fatalf("ReadMessage = %v, want a StatusGoingAway close", err)
	}

	done := make(chan struct{})
	go func() { s.conns.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownWait):
		t.Fatal("server did not finish its connections")
	}
}

func TestHandshakeHTTPResponses(t *testing.T) {
	for _, tc := range []struct {
		name, origin string
		valid        bool
		status       int
	}{
		{"cross origin", "https://other.test", true, http.StatusForbidden},
		{"opaque origin", "null", true, http.StatusForbidden},
		{"malformed origin", "http://example.test/", true, http.StatusForbidden},
		{"bad handshake", "", false, http.StatusBadRequest},
		{"unsupported version", "", true, http.StatusUpgradeRequired},
		{"no hijacker", "", true, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://example.test/ws", nil)
			if tc.valid {
				r.Header.Set("Upgrade", "websocket")
				r.Header.Set("Connection", "Upgrade")
				r.Header.Set("Sec-WebSocket-Version", "13")
				r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			}
			if tc.name == "unsupported version" {
				r.Header.Set("Sec-WebSocket-Version", "12")
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			newServer(t.Context()).ServeHTTP(w, r)
			if tc.status == http.StatusUpgradeRequired && w.Header().Get("Sec-WebSocket-Version") != "13" {
				t.Fatal("missing supported version")
			}
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
		})
	}
}
