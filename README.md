# websocket

[![Go Reference](https://pkg.go.dev/badge/github.com/picatz/websocket.svg)](https://pkg.go.dev/github.com/picatz/websocket)
[![Test](https://github.com/picatz/websocket/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/picatz/websocket/actions/workflows/test.yml)
[![Autobahn](https://github.com/picatz/websocket/actions/workflows/conformance.yml/badge.svg?branch=main)](#conformance)

A [WebSocket] client and server for Go 1.26 and later, built only on the
standard library. It implements [RFC 6455] and the permessage-deflate
compression extension of [RFC 7692], and passes every graded case of the
[Autobahn TestSuite](#conformance) as both client and server.

- **Client and server.** `Dial` takes a `ws` or `wss` URL; `Upgrade` takes over
  a `net/http` handler. `Dialer` and `Upgrader` hold the options, and their
  zero values are safe defaults.
- **Messages or streams.** Read and write whole messages, or stream one of any
  size with `NextReader` and `NextWriter`.
- **Contexts throughout.** Dial, read, write, ping and shut down with a
  `context.Context`.
- **A complete closing handshake.** `Close`, `CloseWithStatus`, `CloseWrite`
  for a half-close and `Shutdown` for a graceful drain, with typed status codes.
- **Compression.** permessage-deflate with context takeover, every window
  size, and a bounded memory budget per connection.
- **Safe defaults.** A 1 MiB message limit, a same-origin check for browsers,
  UTF-8 validation, and HTTP error responses written for you.
- **Proxies and tunnels.** HTTP and HTTPS proxies, custom dialers, and
  `NetConn` to carry any stream protocol over a WebSocket.
- **Beyond the core.** [`wsjson`](wsjson) for JSON messages and
  [`browser`](browser) for Go compiled to WebAssembly.

> [!NOTE]
> This package is young. Autobahn is a test baseline, not a certification,
> and [`github.com/coder/websocket`](https://pkg.go.dev/github.com/coder/websocket)
> has a longer production track record.

[WebSocket]: https://en.wikipedia.org/wiki/WebSocket
[RFC 6455]: https://www.rfc-editor.org/rfc/rfc6455.html
[RFC 7692]: https://www.rfc-editor.org/rfc/rfc7692.html

## Install

```console
go get github.com/picatz/websocket
```

## Quick start

A server upgrades a request and reads messages until the connection ends:

```go
http.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Upgrade(w, r)
	if err != nil {
		return // Upgrade has already written the HTTP error response.
	}
	defer conn.Close()
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			return // The peer closed, or the connection failed.
		}
		if err := conn.WriteMessage(typ, data); err != nil {
			return
		}
	}
})
```

A client dials, sends a message and reads the reply:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

conn, _, err := websocket.Dial(ctx, "ws://localhost:8080/echo")
if err != nil {
	log.Fatal(err)
}
defer conn.Close()

if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
	log.Fatal(err)
}
_, data, err := conn.ReadMessage()
if err != nil {
	log.Fatal(err)
}
fmt.Printf("%s\n", data)
```

Options go in a `Dialer` or `Upgrader`, which can be shared:

```go
upgrader := &websocket.Upgrader{
	Subprotocols:   []string{"chat.v2", "chat.v1"},
	Compression:    &websocket.Compression{},
	MaxMessageSize: 64 << 10,
}
conn, err := upgrader.Upgrade(w, r)
```

## Examples

Every example is a tested program; run one with `go run ./examples/<name>`.

| Example | Shows |
| --- | --- |
| [`echo`](examples/echo) | A server with a size limit, bounded writes and a graceful shutdown |
| [`client`](examples/client) | A `wscat`-style command-line client with headers, subprotocols and compression |
| [`reconnect`](examples/reconnect) | A client that resumes a subscription after failures, with jittered backoff and keepalive |
| [`keepalive`](examples/keepalive) | Pinging clients on an interval and dropping those that stop answering |
| [`chat`](examples/chat) | Broadcasting to every client, with a bounded send queue each |
| [`pubsub`](examples/pubsub) | Topic subscriptions over a small JSON protocol |
| [`rpc`](examples/rpc) | A JSON-RPC 2.0 server that runs calls concurrently |
| [`tunnel`](examples/tunnel) | Forwarding TCP connections, such as SSH, over WebSocket with `NetConn` |
| [`browser`](examples/browser) | A Go WebAssembly page that talks to a Go server |

The [package documentation](https://pkg.go.dev/github.com/picatz/websocket#pkg-examples)
has smaller runnable examples for each API.

## Documentation

The [package overview](https://pkg.go.dev/github.com/picatz/websocket) is the
guide. Each topic below links to its section:

- [Reading, writing and concurrency](https://pkg.go.dev/github.com/picatz/websocket#hdr-Reading__writing_and_concurrency):
  one reader per connection, any number of writers, and streaming.
- [Errors](https://pkg.go.dev/github.com/picatz/websocket#hdr-Errors): which
  errors end a connection and how to tell them apart.
- [Contexts and deadlines](https://pkg.go.dev/github.com/picatz/websocket#hdr-Contexts_and_deadlines):
  bounding calls, and keepalives with `Ping`.
- [Closing](https://pkg.go.dev/github.com/picatz/websocket#hdr-Closing): when
  to use `Close`, `CloseWrite` or `Shutdown`.
- [Limits and security](https://pkg.go.dev/github.com/picatz/websocket#hdr-Limits_and_security):
  message limits, the Origin policy, and the HTTP errors `Upgrade` writes.
- [Proxies and custom transports](https://pkg.go.dev/github.com/picatz/websocket#hdr-Proxies_and_custom_transports):
  `Dialer.Proxy`, custom dialers and `NewConn`.
- [Compression](https://pkg.go.dev/github.com/picatz/websocket#hdr-Compression):
  permessage-deflate and its memory and CPU trade-off.

Two subpackages build on the core:

- [`wsjson`](https://pkg.go.dev/github.com/picatz/websocket/wsjson) reads and
  writes one JSON value per text message.
- [`browser`](https://pkg.go.dev/github.com/picatz/websocket/browser) is the
  client for programs compiled with `GOOS=js GOARCH=wasm`. It drives the
  host's own `WebSocket` object with the same message API.

[MIGRATION.md](MIGRATION.md) lists the API changes made on the way to v0.1.0.

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

## Performance

[BENCHMARKS.md](BENCHMARKS.md) explains how to measure the framing, masking
and transport costs, and compares this package with other Go WebSocket
libraries.

## Development

Tests and examples use in-memory connections or local loopback servers, with no
external services or credentials. A few tests use Python 3's `zlib` or Node.js
22 as independent peers, and are skipped when those are not installed.

```console
go test ./...
go test -race ./...
go vet ./...
GOARCH=386 CGO_ENABLED=0 go test ./...
go test . -run '^$' -fuzz '^FuzzReadMessage$' -fuzztime=30s
```

`TestAPI` compares the exported API of every package with
[`api/v0.txt`](api/v0.txt), so any change to it shows up as a reviewed diff.
After an intended change, run `go test -run '^TestAPI$' -update-api .` and
commit the result.
