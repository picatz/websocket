# websocket

[![Go Reference](https://pkg.go.dev/badge/github.com/picatz/websocket.svg)](https://pkg.go.dev/github.com/picatz/websocket)
[![Test](https://github.com/picatz/websocket/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/picatz/websocket/actions/workflows/test.yml)
[![Autobahn](https://github.com/picatz/websocket/actions/workflows/conformance.yml/badge.svg?branch=main)](#conformance)
 
This package provides a small, standard-library-only [WebSocket] client and server implementation for Go 1.26 and later.
It supports text and binary messages, ping/pong/close frames, message fragmentation, and
optional [`permessage-deflate`](#compression) compression with context takeover and every window size.

[WebSocket]: https://en.wikipedia.org/wiki/WebSocket
[RFC 6455]: https://tools.ietf.org/html/rfc6455
[RFC 7692]: https://tools.ietf.org/html/rfc7692

> [!NOTE]
> This package is intentionally small and depends only on the standard library.
> It passes every graded case of the [Autobahn TestSuite](#conformance) as both client
> and server, with and without compression, but that suite is a test baseline,
> not a certification. [`github.com/coder/websocket`] is the more established
> choice if you want a longer production track record.

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
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/picatz/websocket"
)

func echoHandler(w http.ResponseWriter, r *http.Request) {
	// Upgrade the HTTP connection to a WebSocket connection
	conn, err := websocket.Upgrade(w, r)
	if err != nil {
		// Upgrade has already written the HTTP error response.
		log.Printf("Upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	// Handle the WebSocket connection
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				log.Printf("Connection closed by peer (status %v)", websocket.CloseStatus(err))
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
	dialer := &websocket.Dialer{MaxMessageSize: 1 << 20}
	conn, resp, err := dialer.Dial(ctx, "ws://localhost:8080/ws")
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

`Ping` sends a Ping and waits for the matching Pong, for keepalives and
liveness checks. Pongs are processed by `ReadMessage`, so keep a read loop
running; custom pong handlers still see every Pong:

```go
go func() {
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}()

ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
defer cancel()
if err := conn.Ping(ctx); err != nil {
	conn.Close() // the peer did not answer in time
}
```

Each call uses a distinct payload, so concurrent pings are matched
independently. A context that ends while the Ping frame is being written aborts
the connection, like `WriteMessageContext`; one that ends while waiting for the
Pong only returns its error, leaving the decision to close to the caller. A
connection that closes first makes `Ping` return an error matching
`io.ErrClosedPipe`.

### Examples

Each directory under [`examples`](examples) is a small program with tests:

- [`echo`](examples/echo) echoes every message back, with an incoming size
  limit and a graceful shutdown that closes each connection with
  `StatusGoingAway`.
- [`client`](examples/client) is a command-line client in the spirit of
  `wscat`: standard input lines become messages and replies are printed.
- [`keepalive`](examples/keepalive) pings clients on an interval and drops
  those that stop answering.
- [`chat`](examples/chat) broadcasts messages to every connected client, with a
  bounded send queue per client.
- [`pubsub`](examples/pubsub) delivers messages by topic over a small JSON
  protocol.
- [`rpc`](examples/rpc) is a JSON-RPC 2.0 server that runs calls
  concurrently, with a bound per connection.
- [`browser`](examples/browser) serves a page running Go compiled to
  WebAssembly that talks to the server with the `browser` package. Start it
  with `go run ./examples/browser`.

### Browser and WebAssembly clients

Programs compiled with `GOOS=js GOARCH=wasm` cannot open TCP connections, so
`Dial` is not available to them. The [`browser`](browser) subpackage drives the
host's own `WebSocket` object instead. Its methods are a subset of
`*websocket.Conn`'s with identical signatures, and a peer close is a
`*websocket.CloseError`. The intended direction is for `websocket.Dial` itself
to work under `GOOS=js`:

```go
conn, err := browser.Dial(ctx, "wss://example.com/socket", browser.WithSubprotocols("chat"))
if err != nil {
	return err
}
defer conn.Close()
```

The browser handles the handshake, framing, Ping/Pong and compression, so
headers, TLS, proxies and control frames are out of the program's hands, and
dial failures carry no detail. See the package documentation for the full list.
Its tests run in Node.js 22 or later against a native Go server as part of
`go test ./...`, and are skipped when Node is not installed. The
[`browser` example](examples/browser) is a complete page and server.

## Connection details and JSON

`Conn.LocalAddr` and `Conn.RemoteAddr` return the transport's addresses.
`Conn.ConnectionState` returns the TLS state of a `wss` client connection or of
a server connection upgraded from HTTPS, such as the peer's certificates.

The optional `wsjson` package reads and writes one JSON value per text
message, kept separate so the core package carries no encoding helpers:

```go
var event struct{ Kind string }
if err := wsjson.Read(ctx, conn, &event); err != nil {
	return err
}
err := wsjson.Write(ctx, conn, map[string]string{"ack": event.Kind})
```

## Limits and connection lifetime

Connections default to `DefaultMaxMessageSize`, 1 MiB (1,048,576 bytes). Set
`MaxMessageSize` on a `Dialer`, `Upgrader` or `ConnConfig` to choose an
application-appropriate budget. Zero means the default and a negative value
opts into unlimited incoming payloads.

Limits apply to each incoming data-frame wire payload and the reassembled decoded
message. Headers, masking keys and interleaved controls do not consume the message
budget; outgoing messages are not capped. Built-in compression bounds each
encoded frame and the decoded message. Incompressible data can fit the decoded
budget but exceed the encoded budget in a single frame, so allow headroom when
choosing a compression limit. The wire limit is per frame, not aggregate
compressed input.
Custom extensions must bound their own intermediate allocations; their final
results are checked but extension code is trusted.

A payload cap is not a total-memory, CPU, fragment-count, concurrency or lifetime
limit. Applications still need connection/lifetime controls for slow peers and
empty-fragment or control-frame floods. See [migration notes](MIGRATION.md) for
the intentional changes to high-level defaults.

`Dial`'s context covers TCP connection establishment, TLS, any proxy tunnel, and
the HTTP upgrade. It is detached after a successful handshake. It does not cancel
later reads or writes. Bound a single call with `ReadMessageContext` or
`WriteMessageContext`: an already-done context returns its error without touching
the connection, while one that ends mid-call aborts the connection (like a timeout)
and returns an error matching both the context's cause and the I/O failure. Call
`Close` to interrupt connection I/O, or set transport deadlines with
`SetDeadline`, `SetReadDeadline`, and `SetWriteDeadline`. There is no default
message timeout. A zero `time.Time` clears a deadline; otherwise it applies to
future and currently blocked transport I/O in that direction. `SetDeadline`
sets both directions. The setters return the underlying transport error unchanged.

`ReadMessage` may synchronously write an automatic Pong. A read deadline alone
does not bound that write; set an appropriate write deadline too, or use
`SetDeadline` for a shared transport budget. Any read error, including a timeout,
is terminal: clearing the deadline afterward does not make the connection
reusable. A write timeout preserves `ErrWriteFailed` and its underlying cause;
close the connection afterward, because a partial frame cannot be retried safely.

Deadlines are transport controls, not per-call contexts or whole-method timeouts.
They do not preempt callbacks, extension work, lock waits or custom transport
`Close` implementations, and already buffered data may still be consumed.
`NewConn` takes over WebSocket framing and I/O and closes its transport on `Close`
or read failure; do not read or write framed bytes directly on that transport.

Reads and writes are independently serialized. `Close` can run concurrently with
either, as can all three deadline setters. Deadlines are shared transport state;
callers must coordinate competing updates. The library does not mutate the byte
slice passed to `WriteMessage`.
Ping and pong handlers run from `ReadMessage`; a handler must not call
`ReadMessage` recursively. Extension instances are connection-specific and must
not be reused across connections.

`Close` makes a best-effort normal close notification, limited to one second when
no other writer is active, then closes the transport. That notification replaces
any caller write deadline with a one-second deadline, so an earlier deadline is
not a strict bound on graceful `Close`. Read-failure cleanup instead preserves
caller deadlines. `Close` does not wait for the peer's closing handshake.
A second call returns `ErrAlreadyClosed`.

`CloseWithStatus(code, reason)` is `Close` with a chosen status code and a
reason of at most 123 bytes of UTF-8. It rejects codes that may not be sent,
such as 1005, 1006 and 1015, with `ErrInvalidCloseStatus` before any I/O.
When the peer closes, `ReadMessage` returns a `*CloseError` with the peer's
`Code` and `Reason`; `CloseStatus(err)` extracts the code, or returns -1. A
`CloseError` still matches `io.EOF` with `errors.Is`.

```go
conn.CloseWithStatus(websocket.StatusPolicyViolation, "quota exceeded")

_, _, err := conn.ReadMessage()
if websocket.CloseStatus(err) == websocket.StatusGoingAway {
	// The peer is shutting down; reconnect elsewhere.
}
```

`WriteControlFrame(CloseMessage, payload)` starts a closing handshake without
closing the transport. After a successful close frame, data writes and duplicate
close frames return `io.ErrClosedPipe`; reads and ping/pong remain available.
Receiving the peer's close or calling `Close` closes the transport without
sending another close frame. Call `Close` if the peer does not respond; there is
no automatic handshake timeout. Close the connection after any write error.

## Subprotocols

`Dialer.Subprotocols` offers application protocols, in order of preference.
`Upgrader.Subprotocols` lists the protocols a server supports, in its own order
of preference; `Upgrade` selects the first one the client offered,
or none. Both sides read the result with `Conn.Subprotocol()`, which is `""`
when nothing was selected.

```go
// Client
dialer := &websocket.Dialer{Subprotocols: []string{"v2.chat", "v1.chat"}}
conn, _, err := dialer.Dial(ctx, "wss://example.com/socket")

// Server
upgrader := &websocket.Upgrader{Subprotocols: []string{"v2.chat", "v1.chat"}}
conn, err := upgrader.Upgrade(w, r)
if err == nil && conn.Subprotocol() == "" {
	conn.CloseWithStatus(websocket.StatusPolicyViolation, "unsupported subprotocol")
}
```

Protocols are case-sensitive HTTP tokens and must be unique. Setting
`Sec-WebSocket-Protocol` through `Dialer.Header` or `Upgrader.Header` still
works, but combining it with `Subprotocols` is an error.

## Compression

`permessage-deflate` ([RFC 7692]) is opt-in: set `Compression` on a `Dialer` or
`Upgrader`. Each connection gets its own compression state, so one `Dialer` or
`Upgrader` can be shared.

```go
dialer := &websocket.Dialer{Compression: &websocket.Compression{}}
conn, _, err := dialer.Dial(ctx, "wss://example.com/ws")

upgrader := &websocket.Upgrader{Compression: &websocket.Compression{MemoryLimit: 16 << 10}}
conn, err := upgrader.Upgrade(w, r)
```

- **Fragmentation.** Messages are compressed and decompressed as one DEFLATE
  stream across any number of frames ([Section 6]). Large outgoing messages are
  sent as 64 KiB frames, so the encoded message is never buffered whole.
- **Context takeover.** Both directions reuse history between messages
  ([Section 7.1.1]) unless a `*_no_context_takeover` parameter is negotiated, so
  repetitive traffic compresses to a few bytes per message.
- **Window sizes.** Every window from 8 to 15 bits ([Section 7.1.2]) is supported
  for sending and receiving. A client offers a bare `client_max_window_bits` by
  default so the server may choose; `WithClientMaxWindowBits` and
  `WithServerMaxWindowBits` set explicit bounds in either role.
- **Level and threshold.** `WithCompressionLevel` takes a `compress/flate` level.
  `WithCompressionThreshold(n)` sends messages shorter than `n` bytes uncompressed,
  which [Section 6] allows per message.
- **Memory.** The state a connection keeps between messages depends on the mode.
  Buffers used while a message is encoded or decoded come from pools shared by
  all connections and are not part of this.

  | Mode | Kept per idle connection |
  | --- | --- |
  | Default (`WithCompressionMemoryLimit(64<<10)`) | Up to 64 KiB: 32 KiB of send and 32 KiB of receive history |
  | `WithCompressionMemoryLimit(n)` | At most `n` bytes of send and receive history |
  | `WithCompressionMemoryLimit(0)` (unlimited) | A whole compressor (about 0.5–1 MiB, by level) plus up to 32 KiB of receive history |
  | `no_context_takeover` in both directions | Nothing |

  A memory limit is enforced through negotiation: each direction gets half and
  uses the largest window that fits, or no context takeover when none does. The
  default limit fits full 32 KiB windows, so it changes nothing on the wire.
  With a limit, the sender borrows a pooled encoder for each message and
  re-indexes its retained history first. That costs CPU per message: tens of
  microseconds for a small message, and about twice the time of the unlimited
  mode for large ones, in exchange for a much smaller footprint per connection
  and usually a better ratio. `WithCompressionMemoryLimit(0)` selects the
  faster mode for clients or servers with few connections.
- **Other extensions.** Custom frame extensions compose with compression. On
  send, a message is compressed first, then each frame passes through the custom
  extensions in registration order. On receive, each frame passes through them
  first and is then decompressed. Built-in compression owns RSV1 ([Section 5]):
  an extension that changes it fails with `ErrUnsupportedExtensionComposition`,
  as does enabling more than one built-in instance.

Received data is checked as it decodes, but Go's inflater can hold up to 32 KiB of
output before exposing it, so invalid UTF-8 or an exceeded small limit may be
detected only once more compressed input arrives. A size limit does not bound CPU
time; use transport deadlines for untrusted peers.

[Section 5]: https://www.rfc-editor.org/rfc/rfc7692#section-5
[Section 6]: https://www.rfc-editor.org/rfc/rfc7692#section-6
[Section 7.1.1]: https://www.rfc-editor.org/rfc/rfc7692#section-7.1.1
[Section 7.1.2]: https://www.rfc-editor.org/rfc/rfc7692#section-7.1.2

## Tunnelling stream protocols

`NetConn(ctx, conn, websocket.BinaryMessage)` wraps a connection as a
`net.Conn`, so stream protocols such as gRPC, SSH or HTTP/2 can run over a
WebSocket. Each `Write` sends one message and `Read` returns the payloads back
to back. A normal or going-away close reads as `io.EOF`; any other close status
is returned as the peer's `*CloseError`, so `io.Copy` doesn't mistake an
aborted stream for a complete one. Use after `Close` matches
`net.ErrClosed`. Ending `ctx` aborts the connection.

```go
transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
	conn, _, err := websocket.Dial(ctx, "wss://example.com/tunnel")
	if err != nil {
		return nil, err
	}
	return websocket.NetConn(context.Background(), conn, websocket.BinaryMessage), nil
}}
```

Deadlines delegate to the WebSocket, so a read timeout ends the connection,
unlike a TCP connection. Each message is buffered whole and is subject to the
incoming size limit.

## Client proxies and custom dialers

`Dial` connects directly by default. `Dialer.Proxy` has the same function type as
`http.Transport.Proxy`, so `http.ProxyFromEnvironment` honors `HTTP_PROXY`,
`HTTPS_PROXY` and `NO_PROXY` (a `wss` URL is selected like `https`):

```go
dialer := &websocket.Dialer{Proxy: http.ProxyFromEnvironment}
conn, _, err := dialer.Dial(ctx, "wss://example.com/socket")
```

`http` and `https` proxies are supported through an HTTP/1.1 `CONNECT` tunnel; the
WebSocket handshake and `wss` TLS then run end to end inside it. An `https` proxy
is verified with a clone of `Dialer.TLSClientConfig` whose `ServerName` is the proxy host.
Proxy URL credentials are sent only to the proxy as Basic `Proxy-Authorization`
and are never included in errors. The `CONNECT` response head shares the
`Dialer.MaxResponseHeaderBytes` budget; a non-200 status, or data sent before the
tunnel is used, fails with an error matching `ErrBadHandshake` and `ErrProxy`.

`Dialer.NetDialContext` replaces the TCP dialer used to reach the server or proxy,
for example to use a SOCKS dialer, a Unix socket, or an in-memory test transport.

## Browser origins and proxies

`Upgrade` allows a missing Origin for native clients. Otherwise it requires one
valid HTTP/HTTPS Origin matching the request's scheme, host and effective port.
The scheme comes only from the actual `r.TLS` connection; authority comes from
`r.Host`. It ignores `Forwarded`, `X-Forwarded-*` and `r.URL.Scheme`. A default
HTTP/HTTPS port is equivalent to its omission. Host case and IPv6 spelling are
normalized; trailing dots and loopback aliases remain distinct.

Set `Upgrader.CheckOrigin` to a `func(r *http.Request, origin string) bool` to replace
that decision with an exact public-origin allowlist, including behind a
TLS-terminating proxy. The callback receives a canonical origin, `""` for absent,
or `"null"` for an opaque origin. Explicitly decide whether to allow those cases;
`null` does not identify one trusted website. A nil callback keeps the default.
See [migration examples](MIGRATION.md).

Before any callback, empty, multiple, list-valued or malformed fields are denied.
Tuple hosts use ASCII RFC 3986 reg-name characters except percent escapes and
commas, or bracketed IPv6 without a zone. Unicode hosts must use ASCII/punycode.
Userinfo, paths (even `/`), queries, fragments and invalid ports are rejected.
Comma rejection is an intentional single-origin policy, including commas in a
reg-name. There is no DNS resolution or IDNA conversion.

On rejection, `Upgrade` returns `ErrOriginNotAllowed` before extension negotiation
or hijacking, and writes HTTP 403.

When `Upgrade` fails before taking over the connection, it writes the HTTP error
response itself, so the handler only logs and returns: 403 for a rejected
Origin, 405 with `Allow: GET` for another method, 426 with
`Sec-WebSocket-Version: 13` for an unsupported version, 500 when hijacking is
unavailable or the `Upgrader` is invalid, and 400 for any other malformed
handshake. The 400, 405 and 426 errors match `ErrBadHandshake`, so a handler can
tell a bad client from a rejected origin or its own misconfiguration. After
`ErrHandshakeFailed` the transport may have been hijacked, so nothing is written.
Set `Upgrader.Error` to write a different body, for example JSON; the required
`Allow` and `Sec-WebSocket-Version` fields are already set when it runs.

## Conformance

The [Autobahn TestSuite](https://github.com/crossbario/autobahn-testsuite) is
the standard interoperability suite for WebSocket implementations. The
[`Autobahn baseline`](.github/workflows/conformance.yml) workflow runs all 517
of its cases against this package as a server and as a client, on each change
to the protocol code. It uses only the public `Dial`, `Upgrade`, `ReadMessage`,
`WriteMessage` and `Close` API, and excludes no case for being a known failure.

Results for commit
[`f0b52e4`](https://github.com/picatz/websocket/actions/runs/38998965945), the
same for server and client:

| Profile | Autobahn sections | Cases | OK | Informational | Failed |
| --- | --- | ---: | ---: | ---: | ---: |
| core | 1–7, 10 | 247 | 244 | 3 | 0 |
| limits | 9 (large messages) | 54 | 54 | 0 | 0 |
| compression | 12–13 (permessage-deflate) | 216 | 216 | 0 | 0 |

Informational cases have no pass or fail verdict in the suite. The workflow
fails when a run is incomplete or any case is anything other than OK or
informational, so the Autobahn badge above is green only while that holds. Each
run's artifact holds the full Autobahn report for 30 days; see
[`conformance/autobahn`](conformance/autobahn) to reproduce it locally with
Docker.

## Protocol limitations

- Authenticate and authorize requests and validate the authoritative Host in your
  HTTP handler. `Upgrade` enforces the Origin policy described above; Origin is
  not authentication, and non-browser clients can omit or forge it
- Extension negotiation is not comprehensively validated
- Subprotocol offers must contain unique, case-sensitive HTTP tokens. `Dial`
  rejects unsolicited, unoffered, malformed, or multiple server selections.
  `Upgrade` validates offers and any caller-provided selection before hijacking.
  A server may select one offered token or omit selection; the application
  remains responsible for choosing a protocol and implementing it
- Custom request/response headers reject invalid HTTP field names and values
  (including CR/LF, NUL, and other forbidden control bytes) before dialing or
  hijacking. Generated handshake fields and HTTP body-framing fields are reserved
  case-insensitively; see `Dialer.Header` and `Upgrader.Header` for the lists.
  Ordinary repeated fields (including `Set-Cookie`) are preserved. Callers still
  own ordinary field-specific semantics
- Direct built-in `ProcessIncomingFrame` and `ProcessOutgoingFrame` callbacks
  accept complete Text/Binary messages only; nonfinal data and Continuation
  callbacks return `ErrFragmentedCompression`. Controls and disabled callbacks
  remain no-ops. Use `Conn` for fragmented wire messages. Do not use direct
  callbacks concurrently with Conn I/O or share an instance between connections
- Once a compressed message has started, a failure while sending it (a
  transport error, or a custom extension rejecting a frame) poisons later
  writes, because the frame stream and compression context cannot be resumed
- Any `ReadMessage` error terminates the connection. The detecting call preserves
  its original error; later reads and valid writes fail with `io.ErrClosedPipe`.
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
- Neither the tests nor the [Autobahn baseline](#conformance) are a formal
  RFC 6455 or RFC 7692 compliance certification

See [RFC 6455] for framing and [RFC 7692] for compression semantics.

## Error Handling

The package provides detailed error types to help you handle different error scenarios gracefully.

```go
if err != nil {
	if code := websocket.CloseStatus(err); code != -1 {
		// The peer closed; code and the reason are in *websocket.CloseError.
	} else if errors.Is(err, io.EOF) {
		// Transport ended before a message began
	} else if errors.Is(err, websocket.ErrInvalidFrame) {
		// Handle invalid frame
	} else {
		// Other errors
	}
}
```

## Development

Tests and executable examples use in-memory connections or local loopback
servers. They do not depend on public echo services or external credentials.
`TestPMDIndependentZlibWindow` additionally uses Python 3's standard-library
`zlib` module as an independent window oracle, and reports a skip if Python 3
is absent. The Go library has no additional runtime dependency. This check
covers independent unfragmented messages, not complete PMD conformance.

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
