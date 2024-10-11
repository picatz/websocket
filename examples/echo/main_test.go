package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

func Test_echoHandler(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(echoHandler))
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

	// Send a message
	testMessage := "Hello, WebSocket!"
	err = conn.WriteMessage(websocket.TextMessage, []byte(testMessage))
	if err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}

	// Read the echoed message
	messageType, messageData, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}

	if messageType != websocket.TextMessage {
		t.Fatalf("Expected message type %d, got %d", websocket.TextMessage, messageType)
	}

	t.Logf("Read echoed message: %q", string(messageData))
}
