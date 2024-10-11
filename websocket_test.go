package websocket_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/picatz/websocket"
)

func ExampleUpgrade() {
	handlerFunc := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r)
		if err != nil {
			http.Error(w, "Could not open WebSocket connection", http.StatusBadRequest)
			return
		}
		defer conn.Close()

		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				// Connection closed by peer, or an error occurred.
				break
			}

			if err := conn.WriteMessage(messageType, message); err != nil {
				break
			}
		}
	})

	// Create an HTTP test server
	server := httptest.NewServer(handlerFunc)
	defer server.Close()

	// Convert the server URL to a WebSocket URL
	wsURL := "ws" + server.URL[len("http"):]

	// Connect to the WebSocket server
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, wsURL)
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		panic(fmt.Errorf("expected status code %d, got %d", http.StatusSwitchingProtocols, resp.StatusCode))
	}

	// Send a message
	testMessage := "Hello, WebSocket!"
	err = conn.WriteMessage(websocket.TextMessage, []byte(testMessage))
	if err != nil {
		panic(err)
	}

	// Read the echoed message
	messageType, messageData, err := conn.ReadMessage()
	if err != nil {
		panic(err)
	}

	if messageType != websocket.TextMessage {
		panic(fmt.Errorf("expected message type %d, got %d", websocket.TextMessage, messageType))
	}

	fmt.Printf("Received message: %q\n", string(messageData))
	// Output:
	// Received message: "Hello, WebSocket!"
}

func ExampleDial() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, "wss://echo.websocket.org")
	if err != nil {
		panic(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		panic(fmt.Errorf("expected status code %d, got %d", http.StatusSwitchingProtocols, resp.StatusCode))
	}
	defer conn.Close()

	testMessage := "Hello, WebSocket!"
	err = conn.WriteMessage(websocket.TextMessage, []byte(testMessage))
	if err != nil {
		panic(err)
	}

	// Read the first message, which in this case is a message that contains
	// which server instance is being used. We discard the message.
	msgType, msgData, err := conn.ReadMessage()
	if err != nil {
		panic(err)
	}

	if msgType != websocket.TextMessage {
		panic(fmt.Errorf("expected message type %d, got %d", websocket.TextMessage, msgType))
	}

	msgType, msgData, err = conn.ReadMessage()
	if err != nil {
		panic(err)
	}

	// Next, read the echoed message from the server, and check if it matches
	// the message we sent, then print it.
	if msgType != websocket.TextMessage {
		panic(fmt.Errorf("expected message type %d, got %d", websocket.TextMessage, msgType))
	}

	if string(msgData) != testMessage {
		panic(fmt.Errorf("expected message '%s', got '%s'", testMessage, string(msgData)))
	}

	fmt.Printf("Received message: %s\n", string(msgData))
	// Output:
	// Received message: Hello, WebSocket!
}

// testEchoHandler is a simple WebSocket handler that echoes received messages back to the client.
func testEchoHandler(t *testing.T, upgradeOptions ...websocket.UpgradeOption) func(w http.ResponseWriter, r *http.Request) {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r, upgradeOptions...)
		if err != nil {
			http.Error(w, "Could not open WebSocket connection", http.StatusBadRequest)
			return
		}
		defer conn.Close()

		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				// Check if the connection is closed
				if errors.Is(err, io.EOF) {
					break
				}
				t.Logf("Error reading message: %v", err)
				break
			}
			// Echo the message back
			err = conn.WriteMessage(messageType, message)
			if err != nil {
				t.Logf("Error writing message: %v", err)
				break
			}
		}
	}
}

func TestWebSocketEcho(t *testing.T) {
	// Create an HTTP test server
	server := httptest.NewServer(http.HandlerFunc(testEchoHandler(t)))
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
	err = conn.WriteMessage(websocket.TextMessage, []byte(testMessage))
	if err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}

	// Read the echoed message
	messageType, message, err := conn.ReadMessage()
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
	server := httptest.NewServer(http.HandlerFunc(testEchoHandler(t)))
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
	err = conn.WriteMessage(websocket.BinaryMessage, testData)
	if err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}

	// Read the echoed binary message
	messageType, message, err := conn.ReadMessage()
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

func TestWebSocketPingPong(t *testing.T) {
	// Create an HTTP test server
	server := httptest.NewServer(http.HandlerFunc(testEchoHandler(t)))
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

	// Set up a pong handler to capture pong messages
	pongReceived := make(chan string, 1)
	conn.SetPongHandler(func(appData string) error {
		pongReceived <- appData
		return nil
	})

	// Start a goroutine to continuously read messages
	go func() {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				// Connection closed or error occurred
				return
			}
		}
	}()

	// Send a ping message
	testData := "ping payload"
	err = conn.WriteControlFrame(websocket.PingMessage, []byte(testData))
	if err != nil {
		t.Fatalf("WriteControlFrame failed: %v", err)
	}

	// Wait for the pong response
	select {
	case appData := <-pongReceived:
		if appData != testData {
			t.Fatalf("Expected pong data '%s', got '%s'", testData, appData)
		}
	case <-time.After(time.Second * 2):
		t.Fatalf("Timed out waiting for pong")
	}
}

func TestWebSocketClose(t *testing.T) {
	// Create an HTTP test server
	server := httptest.NewServer(http.HandlerFunc(testEchoHandler(t)))
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
	err = conn.WriteMessage(websocket.TextMessage, []byte("should fail"))
	if err == nil {
		t.Fatalf("Expected error when writing to closed connection, got nil")
	}
}

func TestWebSocketCompression(t *testing.T) {
	// Create an HTTP test server with permessage-deflate enabled
	server := httptest.NewServer(http.HandlerFunc(testEchoHandler(t, websocket.WithUpgradeExtensions(websocket.NewPerMessageDeflateExtension()))))
	defer server.Close()

	// Convert the server URL to a WebSocket URL
	wsURL := "ws" + server.URL[len("http"):]

	// Create a WebSocket client with permessage-deflate enabled
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, wsURL, websocket.WithExtensions(websocket.NewPerMessageDeflateExtension()))
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer conn.Close()
	defer resp.Body.Close()

	// Send a message
	testMessage := "Hello, compressed WebSocket!"
	err = conn.WriteMessage(websocket.TextMessage, []byte(testMessage))
	if err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}

	// Read the echoed message
	messageType, message, err := conn.ReadMessage()
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
	server := httptest.NewServer(http.HandlerFunc(testEchoHandler(t, websocket.WithUpgradeExtensions(
		websocket.NewPerMessageDeflateExtension(
			websocket.WithClientNoContextTakeover(),
			websocket.WithServerNoContextTakeover(),
		),
	))))
	defer server.Close()

	// Convert the server URL to a WebSocket URL
	wsURL := "ws" + server.URL[len("http"):]

	// Create a WebSocket client with permessage-deflate extension and no context takeover
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, wsURL, websocket.WithExtensions(
		websocket.NewPerMessageDeflateExtension(
			websocket.WithClientNoContextTakeover(),
			websocket.WithServerNoContextTakeover(),
		),
	))
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
		err = conn.WriteMessage(websocket.TextMessage, []byte(testMessage))
		if err != nil {
			t.Fatalf("WriteMessage failed: %v", err)
		}

		messageType, message, err := conn.ReadMessage()
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

func TestWebSockt_Public_Endpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, "wss://echo.websocket.org")
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("Expected status code %d, got %s", http.StatusSwitchingProtocols, resp.Status)
	}
	defer conn.Close()

	testMessage := "Hello, WebSocket!"
	err = conn.WriteMessage(websocket.TextMessage, []byte(testMessage))
	if err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}

	msgType, msgData, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}

	if msgType != websocket.TextMessage {
		t.Fatalf("Expected message type %d, got %d", websocket.TextMessage, msgType)
	}

	t.Logf("Received message: %s", string(msgData))

	msgType, msgData, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}

	if msgType != websocket.TextMessage {
		t.Fatalf("Expected message type %d, got %d", websocket.CloseMessage, msgType)
	}

	if string(msgData) != testMessage {
		t.Fatalf("Expected message '%s', got '%s'", testMessage, string(msgData))
	}

	t.Logf("Received message: %s", string(msgData))
}
