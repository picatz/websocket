// Package websocket implements the WebSocket protocol defined in [RFC 6455],
// with the permessage-deflate compression extension of [RFC 7692].
//
// Dial opens a client connection to a ws or wss URL, and Upgrade takes over an
// HTTP/1.1 request on a server. Both return a *Conn, which sends and receives
// whole text and binary messages. A Dialer or Upgrader carries settings such
// as subprotocols, compression and limits; their zero values are the package
// defaults, and Dial and Upgrade use them:
//
//	conn, _, err := websocket.Dial(ctx, "wss://example.com/socket")
//	if err != nil {
//		return err
//	}
//	defer conn.Close()
//	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
//		return err
//	}
//	typ, data, err := conn.ReadMessage()
//
// The package uses only the standard library. Programs compiled for js/wasm
// use [github.com/picatz/websocket/browser], which drives the host's
// WebSocket object with the same message API, and
// [github.com/picatz/websocket/wsjson] reads and writes JSON messages.
// NetConn wraps a Conn as a net.Conn that carries a byte stream in messages
// of one type, for tunnelling protocols such as gRPC or SSH over a WebSocket.
//
// # Reading, writing and concurrency
//
// Read each connection from one goroutine, calling ReadMessage in a loop until
// it returns an error. ReadMessage also handles control frames: it answers
// Pings, delivers Pongs to Conn.Ping, and reports the peer's Close frame as a
// *CloseError. A connection that nobody reads cannot answer a keepalive.
//
// Any number of goroutines may write; writes are serialized. Close and the
// deadline setters may be called from any goroutine and interrupt blocked I/O.
// When several producers feed one connection, as in a chat room, give it a
// bounded queue drained by a single writer, and drop a peer whose queue fills
// instead of blocking every other sender on it.
//
// Conn.NextWriter streams one message of unknown length through an
// io.WriteCloser: other data messages wait for its Close, while control
// frames may go out between its frames. Conn.NextReader is its reading
// counterpart, returning each message's payload as an io.Reader.
//
// Every error from ReadMessage is terminal: return from the read loop. A write
// error that reached the transport is terminal too, because a partially
// written frame cannot be retried.
//
// # Contexts and deadlines
//
// Dial's context bounds connecting, TLS, any proxy tunnel and the handshake,
// and nothing after Dial returns. To bound a single call use
// Conn.ReadMessageContext or Conn.WriteMessageContext; if the context ends
// mid-call the connection is aborted, exactly as after a deadline expires.
// Conn.SetDeadline and its variants delegate to the transport and apply to
// blocked I/O as well.
//
// Conn.Ping sends a Ping and waits for the matching Pong. Call it on a ticker
// with a timeout to detect dead peers; a timeout leaves the connection open,
// so closing it is the caller's decision.
//
// # Closing
//
// Conn.Close makes a best-effort attempt, bounded to one second, to send a
// Close frame with StatusNormalClosure, and then closes the transport without
// waiting for the peer's reply. Conn.CloseWithStatus sends another code and an
// optional reason. Conn.Shutdown performs the full closing handshake: it sends
// the Close frame and waits, bounded by a context, for the peer's, so a peer
// that is still sending is not cut off with a connection reset. When the
// peer closes, ReadMessage returns a *CloseError, which matches io.EOF with
// errors.Is; CloseStatus extracts its code from any error. After either side
// closes, operations return errors matching net.ErrClosed.
//
// # Limits and security
//
// Connections reject incoming messages larger than DefaultMaxMessageSize
// (1 MiB) unless MaxMessageSize sets another limit, where a negative value
// means none; the limit applies to each frame and to the reassembled,
// decompressed message. Dial also bounds the HTTP response head to
// DefaultMaxResponseHeaderBytes.
//
// Upgrade accepts requests without an Origin header, as sent by non-browser
// clients, and otherwise requires the Origin to match the request's host and
// TLS scheme. It never trusts Forwarded or X-Forwarded-* headers, so behind a
// TLS-terminating proxy set Upgrader.CheckOrigin to an exact allowlist.
// Origin checks protect browsers from cross-site use; they are not
// authentication.
//
// When Upgrade rejects a request it writes the HTTP error itself, so the
// handler should just return: 403 for ErrOriginNotAllowed, and 400, 405 or 426
// for a request matching ErrBadHandshake. Upgrader.Error replaces the body.
//
// # Proxies and custom transports
//
// Dial connects directly unless Dialer.Proxy selects an HTTP or HTTPS proxy, for
// example http.ProxyFromEnvironment. It then tunnels with CONNECT and runs the
// handshake, and TLS for wss, end to end inside the tunnel. Redirects are
// never followed. Dialer.NetDialContext replaces the TCP dialer, for SOCKS,
// Unix sockets or tests. NewConn runs the protocol over any net.Conn whose
// handshake the caller has already performed, configured by a ConnConfig.
//
// # Compression
//
// A non-nil Compression on a Dialer or Upgrader opts into permessage-deflate.
// Messages are compressed as one DEFLATE stream across any number of frames,
// with context takeover unless the peers negotiate otherwise and with any
// window from 8 to 15 bits. Each connection gets its own compression state,
// so one Dialer or Upgrader may be shared. By default a connection keeps about 64 KiB of
// compression state between messages (DefaultCompressionMemoryLimit);
// compressors are pooled across connections while idle.
//
// The examples directory holds tested programs for common patterns:
//
//   - [github.com/picatz/websocket/examples/echo]: a server with limits and a
//     graceful shutdown.
//   - [github.com/picatz/websocket/examples/client]: a command-line client.
//   - [github.com/picatz/websocket/examples/keepalive]: dropping peers that
//     stop answering Pings.
//   - [github.com/picatz/websocket/examples/chat]: broadcasting with a bounded
//     queue per client.
//   - [github.com/picatz/websocket/examples/pubsub]: topics over a JSON
//     protocol.
//   - [github.com/picatz/websocket/examples/rpc]: JSON-RPC 2.0 with concurrent
//     calls.
//   - [github.com/picatz/websocket/examples/browser]: a Go WebAssembly page.
//
// [RFC 6455]: https://www.rfc-editor.org/rfc/rfc6455.html
// [RFC 7692]: https://www.rfc-editor.org/rfc/rfc7692.html
package websocket
