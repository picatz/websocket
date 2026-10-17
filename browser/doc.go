// Package browser is a WebSocket client for Go programs compiled to
// WebAssembly and run by a browser or another JavaScript host
// (GOOS=js GOARCH=wasm).
//
// A program running under js/wasm cannot open TCP connections, so package
// websocket's Dial is not usable there. This package instead drives the
// host's WebSocket object, as defined by the [WHATWG WebSockets standard], and
// exposes it with the same message-oriented API as websocket.Conn:
//
//	conn, err := browser.Dial(ctx, "wss://example.com/socket")
//	if err != nil {
//		return err
//	}
//	defer conn.Close()
//	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
//		return err
//	}
//	messageType, data, err := conn.ReadMessage()
//
// The host performs the opening handshake, framing, masking, Ping/Pong and
// the closing handshake, and negotiates any extensions itself, including
// permessage-deflate in browsers. Some things therefore differ from
// websocket.Conn:
//
//   - The host does not report why a connection failed. Dial errors match
//     ErrDialFailed and carry no HTTP status or response.
//   - Custom request headers, TLS settings and proxies are controlled by the
//     host, not the program. Subprotocols can be offered with WithSubprotocols.
//   - The host cannot pause delivery: messages are queued as they arrive
//     until ReadMessage takes them. WithMaxMessageSize bounds each message, and
//     reading promptly bounds the queue.
//   - WriteMessage returns once the host accepts the message for sending, not
//     when it reaches the network. Conn.BufferedAmount reports bytes still
//     queued by the host.
//   - Control frames cannot be sent or observed.
//
// This package builds only for GOOS=js; on other platforms it is empty.
//
// [WHATWG WebSockets standard]: https://websockets.spec.whatwg.org/
package browser
