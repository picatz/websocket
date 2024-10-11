// Package websocket implements the [WebSocket] protocol as specified in [RFC 6455].
//
// The WebSocket protocol enables two-way communication between a client and a server
// over a single, long-lived connection. This package provides both client and server
// implementations, allowing for the creation of WebSocket connections, sending and
// receiving messages, and handling control frames (ping, pong).
//
// Key Features:
// - Full support for WebSocket protocol as per [RFC 6455].
// - Client and server implementations.
// - Support for text and binary messages.
// - Handling of control frames (ping, pong).
// - Extension support, including permessage-deflate for message compression.
//
// # Getting Started
//
// To create a WebSocket server, use the Upgrade function to upgrade an HTTP connection
// to a WebSocket connection:
//
//	http.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
//	    conn, err := websocket.Upgrade(w, r)
//	    if err != nil {
//	        http.Error(w, "Failed to upgrade to WebSocket", http.StatusInternalServerError)
//	        return
//	    }
//	    defer conn.Close()
//
//	    for {
//	        messageType, message, err := conn.ReadMessage()
//	        if err != nil {
//	            break
//	        }
//	        err = conn.WriteMessage(messageType, message)
//	        if err != nil {
//	            break
//	        }
//	    }
//	})
//
// To create a WebSocket client, use the Dial function:
//
//	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
//	defer cancel()
//
//	conn, resp, err := websocket.Dial(ctx, "ws://localhost:8080/echo")
//	if err != nil {
//	    log.Fatal("Dial failed:", err)
//	}
//	defer conn.Close()
//
//	err = conn.WriteMessage(websocket.TextMessage, []byte("Hello, WebSocket!"))
//	if err != nil {
//	    log.Fatal("WriteMessage failed:", err)
//	}
//
//	messageType, message, err := conn.ReadMessage()
//	if err != nil {
//	    log.Fatal("ReadMessage failed:", err)
//	}
//	fmt.Printf("Received message: %s\n", message)
//
// # Extensions
//
// This package supports WebSocket extensions, including permessage-deflate for
// message compression. Extensions can be enabled during the handshake process:
//
//	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	    conn, err := websocket.Upgrade(w, r, websocket.WithUpgradeExtensions(websocket.NewPerMessageDeflateExtension()))
//	    if err != nil {
//	        http.Error(w, "Failed to upgrade to WebSocket", http.StatusInternalServerError)
//	        return
//	    }
//	    defer conn.Close()
//	    // Handle WebSocket connection
//	}))
//	defer server.Close()
//
//	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
//	defer cancel()
//
//	conn, resp, err := websocket.Dial(ctx, "ws://localhost:8080/echo", websocket.WithExtensions(websocket.NewPerMessageDeflateExtension()))
//	if err != nil {
//	    log.Fatal("Dial failed:", err)
//	}
//	defer conn.Close()
//
// For more details on the WebSocket protocol, refer to the RFC 6455 specification:
// https://tools.ietf.org/html/rfc6455
//
// [WebSocket]: https://en.wikipedia.org/wiki/WebSocket
// [RFC 6455]: https://tools.ietf.org/html/rfc6455
package websocket
