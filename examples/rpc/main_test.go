package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

func Test_rpcHandler(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(rpcHandler))
	defer srv.Close()

	// Convert the server URL to a WebSocket URL
	wsURL := "ws" + srv.URL[len("http"):]

	// Create a WebSocket client
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer conn.Close()
	defer resp.Body.Close()

	err = conn.WriteMessage(websocket.BinaryMessage, []byte(`{"method":"sum","params":{"a":"1","b":"2"}}`))
	if err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}

	// Read the echoed message
	messageType, messageData, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}

	if messageType != websocket.BinaryMessage {
		t.Fatalf("Expected message type %d, got %d", websocket.BinaryMessage, messageType)
	}

	if string(messageData) != `{"result":3}` {
		t.Fatalf("Expected message data %s, got %s", `{"result":3}`, string(messageData))
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://example.test/ws", nil)
			if tc.valid {
				r.Header.Set("Upgrade", "websocket")
				r.Header.Set("Connection", "Upgrade")
				r.Header.Set("Sec-WebSocket-Version", "13")
				r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			rpcHandler(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
		})
	}
}
