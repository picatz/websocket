// Package websocket provides a small WebSocket client and server implementation.
// It supports text and binary messages, uncompressed fragmentation, and control
// frames. It requires Go 1.27 or later and has no third-party dependencies.
//
// # Connections
//
// Use Dial to connect to a ws or wss URL, and Upgrade to take over an HTTP/1.1
// request on a server. Upgrade allows a missing Origin for native clients;
// otherwise it requires a single valid HTTP/HTTPS Origin matching r.Host and the
// actual connection's TLS scheme. Forwarded headers are not trusted. Use
// WithUpgradeOriginCheck to replace that decision, for example with an exact
// public-origin allowlist behind a TLS-terminating proxy. Malformed/multiple
// fields are rejected before a callback. ErrOriginNotAllowed leaves the response
// writer untouched; the caller may send HTTP 403. Authentication, authorization,
// and authoritative Host validation remain application responsibilities. Origin
// is not authentication and non-browser clients can omit or forge it.
//
// The Dial context covers TCP, TLS, and the HTTP upgrade handshake. Once Dial
// succeeds, canceling that context does not cancel message I/O. Call Conn.Close
// to interrupt reads and writes. Close sends a best-effort notification with a
// bounded write deadline and closes the transport without waiting for the peer.
//
// # Message limits and concurrency
//
// Dial and Upgrade default to DefaultMaxMessageSize (1 MiB). Choose an explicit
// limit with WithMaxMessageSize or WithUpgradeMaxMessageSize; zero/negative means
// unlimited, and the last option wins. NewConn remains unlimited by default;
// WithMaxBytes applies only positive limits and ignores non-positive options.
// Limits bound each incoming data-frame wire payload and reassembled decoded
// message. Headers, masking keys and controls do not consume the message budget.
// Writes are not capped. For supported unfragmented built-in compression, encoded
// and decoded sizes must both fit; incompressible input may need extra headroom.
// The encoded limit is per frame, not an aggregate compressed-message budget.
// Custom extensions must bound their own intermediate allocations. A byte cap
// does not bound total memory, CPU, fragment count, concurrency or lifetime.
//
// Reads and writes are independently serialized. Close may be called concurrently
// with either. WriteMessage does not modify the caller's payload. Ping and pong
// handlers run synchronously from ReadMessage and must not call it recursively.
// An extension instance must not be shared between connections.
//
// # Extension negotiation
//
// Dial and Upgrade validate the complete extension header, including repeated
// fields and quoted parameter tokens. Dial rejects selections that were not
// offered and closes the transport on negotiation failure. Upgrade validates
// extension-generated selections before hijacking. Errors can be checked with
// errors.Is(err, ErrInvalidExtension). Custom extensions receive the complete
// comma-joined peer header and must validate their own parameter and repetition
// semantics. Multiple offers for the same extension remain valid alternatives.
//
// # Limitations
//
// The optional permessage-deflate extension is experimental. Compressed message
// fragmentation, context takeover, and DEFLATE window-size negotiation are not
// fully implemented. Leave it disabled for production interoperability needs.
// Full compression parameter compatibility remains experimental. Subprotocol offers are unique
// HTTP tokens; any selected protocol must be exactly one case-sensitive offered
// token. Omitting selection is allowed. Applications choose and implement their
// subprotocols. Custom headers
// reject invalid HTTP syntax and reserved fields before dialing or hijacking;
// callers still own field-specific semantics. Every ReadMessage error terminates
// the connection while preserving the detecting call's original error. Protocol
// failures notify the peer when the writer and extension encoding are safe, then
// abort transport I/O without changing caller deadlines. The notification effort
// is bounded to one second for default transports. Custom callbacks and transport
// Close implementations, including wrappers hiding TLS, may still block.
// Focused regression tests do not establish full RFC conformance.
//
// See the executable examples for local client/server usage.
//
// [RFC 6455]: https://www.rfc-editor.org/rfc/rfc6455.html
// [RFC 7692]: https://www.rfc-editor.org/rfc/rfc7692.html
package websocket
