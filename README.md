# websocket
 
This package provides a small, standard-library-only [WebSocket] client and server implementation for Go 1.27 and later.
It supports text and binary messages, ping/pong/close frames, and uncompressed message fragmentation.
The optional `permessage-deflate` implementation is experimental; see the limitations below.

[WebSocket]: https://en.wikipedia.org/wiki/WebSocket
[RFC 6455]: https://tools.ietf.org/html/rfc6455
[RFC 7692]: https://tools.ietf.org/html/rfc7692

> [!NOTE]
> You probably want to use the [`github.com/coder/websocket`] package instead of this one,
> especially when you need comprehensive protocol compliance or production compression.
> This package is intentionally small and is not a claim of full RFC conformance.

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
	conn, err := websocket.Upgrade(w, r, websocket.WithUpgradeMaxMessageSize(1<<20))
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
	"time"

	"github.com/picatz/websocket"
)

func main() {
	// Dial the WebSocket server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws://localhost:8080/ws", websocket.WithMaxMessageSize(1<<20))
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
	_, data, err := conn.ReadMessage()
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

## Limits and connection lifetime

Set `WithMaxMessageSize(n)` for a client, `WithUpgradeMaxMessageSize(n)` for a server,
or `WithMaxBytes(n)` with `NewConn`. Limits apply to incoming frame payloads,
reassembled messages, and decoded data from the built-in compression extension.
The default is unlimited; set an application-appropriate positive limit before
accepting untrusted peers. Custom extensions are responsible for bounding their
own intermediate allocations.

`Dial`'s context covers TCP connection establishment, TLS, and the HTTP upgrade.
It is detached after a successful handshake. It does not cancel later reads or
writes. Call `Close` to interrupt connection I/O.

Reads and writes are independently serialized. `Close` can run concurrently with
either. The library does not mutate the byte slice passed to `WriteMessage`.
Ping and pong handlers run from `ReadMessage`; a handler must not call
`ReadMessage` recursively. Extension instances are connection-specific and must
not be reused across connections.

`Close` makes a best-effort normal close notification, limited to one second when
no other writer is active, then closes the transport. It does not wait for the
peer's closing handshake. A second call returns `ErrAlreadyClosed`.

`WriteControlFrame(CloseMessage, payload)` starts a closing handshake without
closing the transport. After a successful close frame, data writes and duplicate
close frames return `io.ErrClosedPipe`; reads and ping/pong remain available.
Receiving the peer's close or calling `Close` closes the transport without
sending another close frame. Call `Close` if the peer does not respond; there is
no automatic handshake timeout. Close the connection after any write error.

## Protocol limitations

- Authenticate requests and validate their `Origin` in your HTTP handler before
  calling `Upgrade`. This package does not enforce an origin policy
- Extension negotiation is not comprehensively validated
- Subprotocol offers must contain unique, case-sensitive HTTP tokens. `Dial`
  rejects unsolicited, unoffered, malformed, or multiple server selections.
  `Upgrade` validates offers and any caller-provided selection before hijacking.
  A server may select one offered token or omit selection; the application
  remains responsible for choosing a protocol and implementing it
- Custom request/response headers reject invalid HTTP field names and values
  (including CR/LF, NUL, and other forbidden control bytes) before dialing or
  hijacking. Generated handshake fields and HTTP body-framing fields are reserved
  case-insensitively; see `WithHeader` and `WithResponseHeader` for the lists.
  Ordinary repeated fields (including `Set-Cookie`) are preserved. Callers still
  own ordinary field-specific semantics
- `permessage-deflate` is experimental. Independent unfragmented messages are
  covered by tests, including a published RFC 7692 vector. Incoming compressed
  fragmentation and context takeover are not implemented correctly, and window
  bit options do not impose DEFLATE window sizes. Leave compression disabled
  when interoperability or untrusted inputs matter
- Any `ReadMessage` error terminates the connection. The detecting call preserves
  its original error; later reads and writes fail with `io.ErrClosedPipe`.
  Known protocol, invalid UTF-8, and configured size violations make a single
  best-effort Close notification with status 1002, 1007, or 1009 when the writer
  is idle and safe. A busy or failed writer, transport/callback error, or enabled
  custom extension instead causes an immediate transport abort. No additional
  peer data is processed after failure
- Automatic failure notification has a one-second transport-abort watchdog and
  preserves existing transport deadlines. For `Dial` and concrete `*tls.Conn`
  inputs to `NewConn`/`Upgrade`, abnormal shutdown closes the raw transport;
  ordinary and valid-peer closure retain TLS shutdown behavior. A custom
  transport or an outer wrapper hiding TLS may block in `Close`; trusted
  extension/handler callbacks can also block. These cannot be bounded by the
  library
- The tests are focused regressions and local round trips, not a full RFC 6455 or
  RFC 7692 compliance certification

See [RFC 6455] for framing and [RFC 7692] for compression semantics.

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

## Development

All tests and executable examples use in-memory connections or local loopback
servers. They do not depend on public echo services or external credentials.

```console
go test ./...
go test -race ./...
go vet ./...
GOARCH=386 CGO_ENABLED=0 go test ./...
go test . -run '^$' -fuzz '^FuzzReadMessage$' -fuzztime=30s
```

The regression suite covers malformed lengths, frame masking and reserved bits,
fragmentation and message limits, text/close payload validation, bounded
DEFLATE output, buffered handshake data, TLS options, handshake cancellation,
and concurrent connection shutdown.
