package websocket_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

// testEchoHandler is a simple WebSocket handler that echoes received messages back to the client.
func testEchoHandler(t *testing.T, u *websocket.Upgrader) func(w http.ResponseWriter, r *http.Request) {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := u.Upgrade(w, r)
		if err != nil {
			http.Error(w, "Could not open WebSocket connection", http.StatusBadRequest)
			return
		}
		defer conn.Close()

		for {
			messageType, message, err := conn.ReadMessage(context.Background())
			if err != nil {
				// Check if the connection is closed
				if errors.Is(err, io.EOF) {
					break
				}
				t.Logf("Error reading message: %v", err)
				break
			}
			// Echo the message back
			err = conn.WriteMessage(context.Background(), messageType, message)
			if err != nil {
				t.Logf("Error writing message: %v", err)
				break
			}
		}
	}
}

func TestWebSocketEcho(t *testing.T) {
	// Create an HTTP test server
	server := httptest.NewServer(http.HandlerFunc(testEchoHandler(t, nil)))
	defer server.Close()

	// Convert the server URL to a WebSocket URL
	wsURL := "ws" + server.URL[len("http"):]

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
	err = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte(testMessage))
	if err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}

	// Read the echoed message
	messageType, message, err := conn.ReadMessage(context.Background())
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("Expected message type %d, got %d", websocket.TextMessage, messageType)
	}
	if string(message) != testMessage {
		t.Fatalf("Expected message '%s', got '%s'", testMessage, string(message))
	}
}

func TestWebSocketBinaryMessage(t *testing.T) {
	// Create an HTTP test server
	server := httptest.NewServer(http.HandlerFunc(testEchoHandler(t, nil)))
	defer server.Close()

	// Convert the server URL to a WebSocket URL
	wsURL := "ws" + server.URL[len("http"):]

	// Create a WebSocket client
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer conn.Close()
	defer resp.Body.Close()

	// Send a binary message
	testData := []byte{0x00, 0xFF, 0x7F, 0x80}
	err = conn.WriteMessage(context.Background(), websocket.BinaryMessage, testData)
	if err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}

	// Read the echoed binary message
	messageType, message, err := conn.ReadMessage(context.Background())
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("Expected message type %d, got %d", websocket.BinaryMessage, messageType)
	}
	if !bytes.Equal(message, testData) {
		t.Fatalf("Expected message %v, got %v", testData, message)
	}
}

func TestWebSocketClose(t *testing.T) {
	// Create an HTTP test server
	server := httptest.NewServer(http.HandlerFunc(testEchoHandler(t, nil)))
	defer server.Close()

	// Convert the server URL to a WebSocket URL
	wsURL := "ws" + server.URL[len("http"):]

	// Create a WebSocket client
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer resp.Body.Close()

	// Close the connection
	err = conn.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Attempt to send a message after closing
	err = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte("should fail"))
	if err == nil {
		t.Fatalf("Expected error when writing to closed connection, got nil")
	}
}

func TestWebSocketCompression(t *testing.T) {
	// Create an HTTP test server with permessage-deflate enabled
	server := httptest.NewServer(http.HandlerFunc(testEchoHandler(t, &websocket.Upgrader{Compression: &websocket.Compression{}})))
	defer server.Close()

	// Convert the server URL to a WebSocket URL
	wsURL := "ws" + server.URL[len("http"):]

	// Create a WebSocket client with permessage-deflate enabled
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	conn, resp, err := (&websocket.Dialer{Compression: &websocket.Compression{}}).Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer conn.Close()
	defer resp.Body.Close()

	// Send a message
	testMessage := "Hello, compressed WebSocket!"
	err = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte(testMessage))
	if err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}

	// Read the echoed message
	messageType, message, err := conn.ReadMessage(context.Background())
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("Expected message type %d, got %d", websocket.TextMessage, messageType)
	}
	if string(message) != testMessage {
		t.Fatalf("Expected message '%s', got '%s'", testMessage, string(message))
	}
}

func TestWebSocketPerMessageDeflateNoContextTakeover(t *testing.T) {
	// Create an HTTP test server with permessage-deflate extension and no context takeover
	server := httptest.NewServer(http.HandlerFunc(testEchoHandler(t, &websocket.Upgrader{Compression: &websocket.Compression{
		ClientNoContextTakeover: true,
		ServerNoContextTakeover: true,
	}})))
	defer server.Close()

	// Convert the server URL to a WebSocket URL
	wsURL := "ws" + server.URL[len("http"):]

	// Create a WebSocket client with permessage-deflate extension and no context takeover
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	conn, resp, err := (&websocket.Dialer{Compression: &websocket.Compression{
		ClientNoContextTakeover: true,
		ServerNoContextTakeover: true,
	}}).Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer conn.Close()
	defer resp.Body.Close()

	// Send multiple messages to test context takeover behavior
	messages := []string{
		"Message 1: Hello, WebSocket!",
		"Message 2: Testing no context takeover",
		"Message 3: Each message compressed independently",
	}

	for _, testMessage := range messages {
		err = conn.WriteMessage(context.Background(), websocket.TextMessage, []byte(testMessage))
		if err != nil {
			t.Fatalf("WriteMessage failed: %v", err)
		}

		messageType, message, err := conn.ReadMessage(context.Background())
		if err != nil {
			t.Fatalf("ReadMessage failed: %v", err)
		}
		if messageType != websocket.TextMessage {
			t.Fatalf("Expected message type %d, got %d", websocket.TextMessage, messageType)
		}
		if string(message) != testMessage {
			t.Fatalf("Expected message '%s', got '%s'", testMessage, string(message))
		}
	}
}
