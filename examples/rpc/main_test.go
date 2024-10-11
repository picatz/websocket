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
