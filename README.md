# websocket
 
This package provides a [WebSocket] client and server implementation, adhering to the [RFC 6455] WebSocket protocol specification. 
It supports all standard WebSocket features, including text and binary messages, control frames (ping, pong, close), and message fragmentation. Additionally, it supports the `permessage-deflate` extension for message compression defined in [RFC 7692].

[WebSocket]: https://en.wikipedia.org/wiki/WebSocket
[RFC 6455]: https://tools.ietf.org/html/rfc6455
[RFC 7692]: https://tools.ietf.org/html/rfc7692

> [!NOTE]
> You probably want to use the [`github.com/coder/websocket`] package instead of this one,
> but this package should be fine for many use cases, and can be extended with additional 
> features, if needed in the future. Pleae feel free to open an issue or a pull request if you
> have any suggestions or improvements.

[`github.com/coder/websocket`]: https://pkg.go.dev/github.com/coder/websocket

## Installation

```console
go get github.com/picatz/websocket
```

## Usage

Can be used as a client or server.

### Server

Here's how you can use the websocket package to create a simple WebSocket echo server:

```go
package main

import (
	"io"
	"log"
	"net/http"

	"github.com/picatz/websocket"
)

func echoHandler(w http.ResponseWriter, r *http.Request) {
	// Upgrade the HTTP connection to a WebSocket connection
	conn, err := websocket.Upgrade(w, r)
	if err != nil {
		log.Printf("Upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	// Handle the WebSocket connection
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if err == io.EOF {
				log.Println("Connection closed by peer")
			} else {
				log.Printf("ReadMessage failed: %v", err)
			}
			break
		}

		log.Printf("Received message: %s", message)

		// Echo the message back to the client
		if err := conn.WriteMessage(messageType, message); err != nil {
			log.Printf("WriteMessage failed: %v", err)
			break
		}
	}
}

func main() {
	http.HandleFunc("/ws", echoHandler)

	log.Println("WebSocket server started on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
```

### Client

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/picatz/websocket"
)

func main() {
	// Dial the WebSocket server
	ctx := context.Background()
	conn, resp, err := websocket.Dial(ctx, "ws://localhost:8080/ws")
	if err != nil {
		log.Fatalf("Dial failed: %v", err)
	}
	defer conn.Close()
	defer resp.Body.Close()

	// Send a message to the server
	message := []byte("Hello, WebSocket!")
	if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
		log.Fatalf("WriteMessage failed: %v", err)
	}

	// Read the server's response
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		log.Fatalf("ReadMessage failed: %v", err)
	}

	fmt.Printf("Received message: %s\n", data)
}
```

### Handling Ping/Pong Frames

```go
// Set a custom ping handler
conn.SetPingHandler(func(appData string) error {
	log.Printf("Received ping: %s", appData)
	// Respond with a pong
	return conn.WriteControlFrame(websocket.PongMessage, []byte(appData))
})

// Set a custom pong handler
conn.SetPongHandler(func(appData string) error {
	log.Printf("Received pong: %s", appData)
	return nil
})
```

## Using Extensions

### `permessage-deflate` Extension

The [`permessage-deflate`] extension compresses WebSocket messages using the [DEFLATE] algorithm, reducing bandwidth usage.

[`permessage-deflate`]: https://datatracker.ietf.org/doc/html/rfc7692#section-7
[DEFLATE]: https://en.wikipedia.org/wiki/DEFLATE

#### Server Side

```go
// In your handler, enable permessage-deflate
conn, err := websocket.Upgrade(w, r, websocket.WithUpgradeExtensions(
	websocket.NewPerMessageDeflate(
		websocket.WithServerNoContextTakeover(),
		websocket.WithClientNoContextTakeover(),
	),
))
```

#### Client Side

```go
// When dialing, enable permessage-deflate
conn, resp, err := websocket.Dial(ctx, "ws://localhost:8080/ws",
	websocket.WithExtensions(
		websocket.NewPerMessageDeflate(
			websocket.WithServerNoContextTakeover(),
			websocket.WithClientNoContextTakeover(),
		),
	),
)
```

#### Context Takeover Options

- `client_no_context_takeover`: The client doesn't use a shared compression object pool between messages.
- `server_no_context_takeover`:  The server doesn't use a shared compression object pool between messages.

Disabling context takeover can reduce memory usage and theoretically mitigate certain security risks,
but may slightly reduce compression efficiency.

```go
// Enable permessage-deflate with context takeover options
pmd := websocket.NewPerMessageDeflate(
	websocket.WithClientNoContextTakeover(),
	websocket.WithServerNoContextTakeover(),
)

// Server side
conn, err := websocket.Upgrade(w, r, websocket.WithUpgradeExtensions(pmd))

// Client side
conn, resp, err := websocket.Dial(ctx, "ws://localhost:8080/ws", websocket.WithExtensions(pmd))
```

## Error Handling

The package provides detailed error types to help you handle different error scenarios gracefully.

```go
if err != nil {
	if errors.Is(err, io.EOF) {
		// Connection closed
	} else if errors.Is(err, websocket.ErrInvalidFrame) {
		// Handle invalid frame
	} else {
		// Other errors
	}
}
```
