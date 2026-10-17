# Migrating to v0.1.0

v0.1.0 is the first tagged release. Code written against an untagged revision
of `main` from before it needs the changes below, which are listed in the order
they landed. Code new to the package can skip this file; the
[package documentation](https://pkg.go.dev/github.com/picatz/websocket)
describes the v0.1.0 API.

## Incoming payload budgets

Omitting a high-level size option now selects `DefaultMaxMessageSize = 1 << 20`
(1,048,576 bytes). Exactly that many valid application bytes fit; larger incoming
messages fail with `ErrMessageTooLarge` and terminate the connection. Inventory
large snapshots, binary transfers and tunnel protocols, then set an explicit
appropriate budget. Do not split messages just to avoid the cap: message
boundaries belong to the application protocol.

```go
client, _, err := (&websocket.Dialer{MaxMessageSize: 4 << 20}).Dial(ctx, endpoint)
server, err := (&websocket.Upgrader{MaxMessageSize: 64 << 10}).Upgrade(w, r)
```

Positive limits retain their meaning. With the [config structs](#config-structs),
zero selects the default and a negative value means unlimited, for `NewConn`
too. Outgoing writes remain unaffected.

The same budget bounds each encoded data frame and the reassembled decoded
message. For the supported unfragmented built-in compression path, incompressible
data can have decoded size at the boundary but encoded size above it. Select
headroom if using compression. This change does not fix experimental compressed
fragmentation or context takeover, impose an aggregate compressed-wire-message
budget, or sandbox custom extensions' intermediate allocations. A payload cap
is not a total-process memory or connection-lifetime guarantee. Use application
concurrency/lifetime controls and `Close` to interrupt stalled I/O; Dial's context
ends after the handshake.

## Client HTTP response-head budget

`Dial` now independently defaults to `DefaultMaxResponseHeaderBytes = 1 << 20`
(1,048,576 bytes). Previously the standalone HTTP response parser had no
configured head-byte limit. This is an intentional default tightening: inventory
large custom response headers and choose a larger positive budget if needed.

```go
dialer := &websocket.Dialer{MaxResponseHeaderBytes: 2 << 20}
client, response, err := dialer.Dial(ctx, endpoint)
```

The budget includes the status line, every raw header line, duplicate fields,
folding whitespace, line separators, and the final empty line. A complete head
of exactly the budget fits; needing another byte returns an error matching both
`ErrBadHandshake` and `ErrResponseHeaderTooLarge`, closes the transport, and
returns a nil connection and response. Ordinary malformed-input and transport
errors retain their causes; a canceled Dial context retains precedence.

Zero keeps the default. A negative value opts out of the bound. The budget is decrypted HTTP bytes, not
TLS overhead. It does not count HTTP bodies or WebSocket frames, and is separate
from the incoming message limit. Buffered first-frame bytes are preserved.
This is not a total-memory, CPU, concurrency or wall-clock guarantee. Use the
Dial context to bound handshake waiting; there is no new default timeout.

Handshake validation and response ownership are unchanged. A parsed non-101
response is returned with `ErrBadHandshake` after closing its transport; callers
still own `response.Body.Close`. A complete streaming error body is not promised.
The body on a successful 101 response remains `http.NoBody`.

## Browser Origin policy

Previously callers owned every Origin decision. `Upgrade` now allows a missing
Origin, preserving native clients, but otherwise defaults to strict same-origin:
HTTP/HTTPS scheme, host, and effective port must match. It derives scheme from
`r.TLS` and authority from `r.Host`, ignoring all forwarding headers and
`r.URL.Scheme`. Host case, numeric ports and IPv6 spellings are normalized. Mapped
IPv6 uses hexadecimal browser serialization and is not treated as IPv4. Trailing
dots and localhost/127.0.0.1/::1 remain distinct.

A present-empty value, duplicate field (even identical), list or malformed value
is denied before any application callback. The accepted host subset is ASCII
RFC 3986 reg-name without percent escapes or commas, or bracketed IPv6 without a
zone. Unicode domains must be supplied in ASCII/punycode. Userinfo, path/query/
fragment delimiters and invalid ports are rejected. No IDNA conversion or DNS
resolution is performed. Origin lists and comma-containing names are explicitly
unsupported policy inputs, not universally invalid URI syntax.

For a deliberately cross-origin deployment or HTTPS termination onto an HTTP
backend, prefer an exact allowlist of public browser origins:

```go
var allowed = map[string]bool{
    "https://app.example.com": true,
    "https://admin.example.com:8443": true,
}
upgrader := &websocket.Upgrader{CheckOrigin: func(r *http.Request, origin string) bool {
    return origin == "" || allowed[origin]
}}
conn, err := upgrader.Upgrade(w, r)
```

The callback sees one canonical origin, `""` for genuinely absent Origin, or
`"null"` for an opaque origin. It replaces the default, so decide each case
explicitly. The example retains missing-Origin native clients; remove that branch
if your protocol requires a browser Origin. The default rejects `null`; opting
in does not identify a specific trusted site or local file. A nil callback
keeps the default. Malformed inputs cannot
be opted in. Request/header data are not rewritten.

Keep authentication, authorization, routing and authoritative Host validation.
Non-browser clients can forge Origin. For dynamic proxy-aware policies, establish
trusted ingress boundaries, reject direct/untrusted senders, strip supplied
forwarding headers at ingress, and pass only validated metadata through trusted
application context. Merely having a forwarding header or private address does
not establish trust. Do not mutate `r.TLS` to pretend the backend received TLS.

An existing application pre-check is followed by the new library check. Migrate
its deliberately approved policy into the callback, or consciously tighten it.
Do not add an unconditional callback merely to hide denied handshakes. Existing
consumers retain their explicit budgets and need their separate policy
integration reviewed before pinning this change. This library change does not
update downstream dependencies or automatically migrate application policies.

## Peer close errors

`ReadMessage` and `ReadMessageContext` used to return the bare `io.EOF` value
when the peer sent a close frame. They now return a `*CloseError` carrying the
peer's status code and reason. It matches `io.EOF` with `errors.Is`, so code
using `errors.Is(err, io.EOF)` is unaffected, but a direct `err == io.EOF`
comparison no longer matches a peer close. EOF on the transport before a
message begins is still the bare `io.EOF`.

```go
// Before
if err == io.EOF { /* peer closed */ }

// After
if errors.Is(err, io.EOF) { /* peer closed, or transport EOF */ }
if code := websocket.CloseStatus(err); code != -1 { /* peer sent code */ }
```

`Close` still sends `StatusNormalClosure`; use `CloseWithStatus` for another
code or a reason.

## Typed status codes

The `Status*` close-code constants used to be untyped integers. They are now
typed `StatusCode` constants, so they no longer convert implicitly to other
integer types. Convert explicitly where a raw number is needed:

```go
// Before
binary.BigEndian.PutUint16(payload, websocket.StatusGoingAway)

// After
binary.BigEndian.PutUint16(payload, uint16(websocket.StatusGoingAway))
// or, to send it:
conn.CloseWithStatus(websocket.StatusGoingAway, "")
```

`StatusServiceRestart` (1012), `StatusTryAgainLater` (1013) and
`StatusBadGateway` (1014) are new, from the IANA close code registry.
`github.com/picatz/openai` does not use the constants and needs no change.

## HTTP error ownership

`Upgrade` used to return pre-hijack errors without writing anything, and each
handler mapped them to an HTTP status. It now writes the response itself:

| Error | Response |
|---|---|
| `ErrOriginNotAllowed` | 403 |
| `ErrInvalidMethod` | 405, with `Allow: GET` |
| `ErrUnsupportedVersion` | 426, with `Sec-WebSocket-Version: 13` |
| `ErrNotHijacker`, or an invalid `Upgrader` (`ErrInvalidConfig`, which `ErrInvalidHandshakeHeader` also matches) | 500 |
| `ErrHandshakeFailed` | nothing: the transport may already be hijacked |
| any other malformed handshake | 400 |

The 400, 405 and 426 errors also match `ErrBadHandshake`, the same sentinel `Dial`
uses for a failed handshake, so `errors.Is(err, websocket.ErrBadHandshake)`
means the client sent a request Upgrade could not accept.

Delete the handler's own error mapping and just return. A handler that still
calls `http.Error` after `Upgrade` fails now sends a second, ignored status
(`net/http` logs "superfluous response.WriteHeader") and appends its text to
the body.

```go
// Before
conn, err := websocket.Upgrade(w, r)
if err != nil {
	if errors.Is(err, websocket.ErrOriginNotAllowed) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}
	return
}

// After
conn, err := websocket.Upgrade(w, r)
if err != nil {
	log.Printf("upgrade: %v", err)
	return
}
```

`github.com/picatz/openai` returns without writing after a failed `Upgrade` and
needs no change.

## Config structs

Dial, Upgrade and NewConn used to take functional options. Settings now live in
plain structs whose zero values are the defaults: `Dialer` for clients,
`Upgrader` for servers and `ConnConfig` for `NewConn`. One field names each
concept on both sides, a struct can be built once and shared, and the
package-level `Dial(ctx, url)` and `Upgrade(w, r)` use the zero value. In the
`websocket` package, every `DialOption`, `UpgradeOption` and `ConnOption`, and
their `With*` constructors, are removed. The `browser` package keeps its
options until it gets a matching `Dialer`.

| Before | After |
|---|---|
| `WithHeader(h)` | `Dialer.Header` |
| `WithResponseHeader(h)` | `Upgrader.Header` |
| `WithSubprotocols(p...)` | `Dialer.Subprotocols` |
| `WithUpgradeSubprotocols(p...)` | `Upgrader.Subprotocols` |
| `WithMaxMessageSize(n)` | `Dialer.MaxMessageSize` |
| `WithUpgradeMaxMessageSize(n)` | `Upgrader.MaxMessageSize` |
| `WithMaxBytes(n)` on `NewConn` | `ConnConfig.MaxMessageSize` |
| `WithMaxResponseHeaderBytes(n)` | `Dialer.MaxResponseHeaderBytes` |
| `WithTLSConfig(c)` | `Dialer.TLSClientConfig` |
| `WithProxy(f)` | `Dialer.Proxy` |
| `WithNetDialContext(f)` | `Dialer.NetDialContext` |
| `WithUpgradeOriginCheck(f)` | `Upgrader.CheckOrigin` |
| `WithExtensions(NewPerMessageDeflateExtension(...))` | `Dialer.Compression` |
| `WithUpgradeExtensions(NewPerMessageDeflateExtension(...))` | `Upgrader.Compression` |
| `WithExtensions(custom...)`, `WithUpgradeExtensions(custom...)` | removed; see [One compression path](#one-compression-path) |
| `NewConn(c, isServer, exts, WithMaxBytes(n))` | `NewConn(c, ConnConfig{Server: isServer, MaxMessageSize: n})`, which also returns an error |

```go
// Before
conn, _, err := websocket.Dial(ctx, endpoint,
	websocket.WithHeader(header),
	websocket.WithMaxMessageSize(1<<20),
	websocket.WithProxy(http.ProxyFromEnvironment),
	websocket.WithExtensions(websocket.NewPerMessageDeflateExtension()))

// After
dialer := &websocket.Dialer{
	Header:         header,
	MaxMessageSize: 1 << 20,
	Proxy:          http.ProxyFromEnvironment,
	Compression:    &websocket.Compression{},
}
conn, _, err := dialer.Dial(ctx, endpoint)
```

Behavior that changes with the move:

- **Limits.** For `MaxMessageSize` and `MaxResponseHeaderBytes`, zero now means
  the default and a negative value means unlimited, as with `http.Server`. An
  explicit `WithMaxMessageSize(0)` used to mean unlimited; write `-1`.
- **NewConn's default limit.** `NewConn` used to be unlimited unless given
  `WithMaxBytes`. A zero `ConnConfig` now applies `DefaultMaxMessageSize`, like
  Dial and Upgrade; set `MaxMessageSize: -1` to keep the old behavior.
- **Extensions per connection.** An extension holds one connection's state, so
  a shared `Dialer` or `Upgrader` cannot hold one. `Compression` builds a fresh
  permessage-deflate extension for every connection.
- **Compression settings.** `Compression` fields map to the old
  permessage-deflate options: `Level`, `Threshold`, `MemoryLimit` (where -1
  means no limit, the old `WithCompressionMemoryLimit(0)`), the two
  `*NoContextTakeover` flags and the two `*MaxWindowBits`. Invalid values
  return `ErrInvalidConfig`: Dial fails before connecting and Upgrade answers
  500.
- **Error responses.** `Upgrader.Error` replaces the body Upgrade writes when the
  handshake fails, with the status and required header fields already chosen.

`github.com/picatz/openai` moves its one `Dial` call to a `Dialer` in a
companion change.

### Package browser

`browser.Dial` follows the same shape. `WithSubprotocols` and
`WithMaxMessageSize` become `browser.Dialer` fields, and `browser.Dial(ctx, url)`
dials with a zero `Dialer`:

```go
// Before
conn, err := browser.Dial(ctx, url, browser.WithSubprotocols("chat"), browser.WithMaxMessageSize(0))

// After
conn, err := (&browser.Dialer{Subprotocols: []string{"chat"}, MaxMessageSize: -1}).Dial(ctx, url)
```

As in the root package, a zero `MaxMessageSize` now means
`websocket.DefaultMaxMessageSize` and a negative one means unlimited.
`WithMaxMessageSize(0)` used to mean unlimited.

## Compression negotiation

`permessage-deflate` now sends with context takeover, fragments large compressed
messages into 64 KiB frames, and supports every window from 8 to 15 bits. Peers
that follow RFC 7692 need no change; these are the observable differences:

- A client's default offer is `permessage-deflate; client_max_window_bits`,
  declaring that the server may select any client window. Previously the offer
  had no parameters.
- `Compression.ClientMaxWindowBits` (8..14) for `Dial` and
  `Compression.ServerMaxWindowBits` (8..14) for `Upgrade` are honored instead of being rejected before connecting. A server
  accepts an offered `server_max_window_bits` below 15 instead of declining it,
  and includes a configured server window even when the client offered none
  (RFC 7692, Section 7.1.2.1).
- With context takeover, a failure part-way through sending a compressed
  message poisons later writes on that connection.
- Each connection keeps at most `DefaultCompressionMemoryLimit` (64 KiB) of
  compression state between messages by default: its send and receive history,
  not a whole compressor. Negotiated parameters are unchanged. Set
  `Compression.MemoryLimit` to -1 to keep a compressor per connection instead,
  which is faster per message and uses about 0.5–1 MiB.

## Closed-connection errors

Operations on a closed connection report one condition the standard library's
way. Reads, writes and `Ping` after `Close`, after the
peer's close, after an aborted context or after any terminal read error return
an error matching `net.ErrClosed`, the same error a read that `Close`
interrupts already returned from the transport. Repeated `Close` and
`CloseWithStatus` return `ErrAlreadyClosed`, which now matches `net.ErrClosed`
too. The `browser` package's `Conn` follows the same rule.

```go
if errors.Is(err, net.ErrClosed) {
    return // this side or the peer has closed the connection
}
```

They no longer match `io.ErrClosedPipe`. Replace checks for
`io.ErrClosedPipe`, whether `errors.Is` or `==`, with
`errors.Is(err, net.ErrClosed)`. An `io.ErrClosedPipe` returned by a custom
transport is still wrapped unchanged, as is any transport error.

## One compression path

The package used to expose compression twice: as `Compression` on a `Dialer`
or `Upgrader`, and as `NewPerMessageDeflateExtension` with its seven `With*`
options, which a manual `NewConn` handshake needed. It also exported the
`Extension` interface and `Frame` for custom frame extensions. Only
`Compression` remains. Removed:

- `NewPerMessageDeflateExtension`, `PerMessageDeflateOption`,
  `WithClientNoContextTakeover`, `WithServerNoContextTakeover`,
  `WithClientMaxWindowBits`, `WithServerMaxWindowBits`,
  `WithCompressionLevel`, `WithCompressionThreshold` and
  `WithCompressionMemoryLimit`. Set the matching `Compression` field.
- `Extension`, `Frame`, `ErrUnsupportedExtensionComposition` and
  `ErrFragmentedCompression`. permessage-deflate is the only extension the
  package implements; an extension API may return later as an addition.

`NewConn` now returns `(*Conn, error)`. A manual handshake hands over its
agreement as the 101 response's `Sec-WebSocket-Extensions` value, the one the
server sent or the client received, and NewConn takes the negotiated
parameters from it:

```go
// Before
pmd := websocket.NewPerMessageDeflateExtension()
// ...negotiate with pmd.Negotiate and pmd.Offer during the handshake...
conn := websocket.NewConn(c, websocket.ConnConfig{Server: true, Extensions: []websocket.Extension{pmd}})

// After
conn, err := websocket.NewConn(c, websocket.ConnConfig{
	Server:      true,
	Extensions:  "permessage-deflate; client_max_window_bits=15",
	Compression: &websocket.Compression{Threshold: 256},
})
```

The handshake must have followed RFC 7692 section 7: NewConn trusts the
agreement rather than checking it against an offer. It returns
`ErrInvalidExtension` for a malformed value or another extension, and
`ErrInvalidConfig` for an invalid `Compression`. Without `Extensions`, a
`ConnConfig` behaves as before and `NewConn` cannot fail.

`github.com/picatz/openai` uses none of these and needs no change.

A read that `Close` or `CloseWithStatus` interrupts now always reports
`net.ErrClosed`, even when the peer's answer to the Close frame arrives before
the transport closes; it no longer sometimes returns that answer as a
`*CloseError`.

## Half-close with CloseWrite

`CloseWrite(code, reason)` replaces sending a Close frame by hand with
`WriteControlFrame(CloseMessage, payload)`. It validates the status like
`CloseWithStatus`, so there is no payload to encode, and keeps the connection
reading until the peer answers:

```go
// Before
_ = conn.WriteControlFrame(websocket.CloseMessage, binary.BigEndian.AppendUint16(nil, 1000))
// After
_ = conn.CloseWrite(websocket.StatusNormalClosure, "")
```

After a Close frame, from either call, a Ping now fails with an error matching
`net.ErrClosed` instead of going out; Pongs answering the peer's Pings still do.

## Message types and control frames

`MessageType` replaces `Opcode`. It has two values, `TextMessage` and
`BinaryMessage`, which keep their names, so most code only changes where it
spells the type:

```go
// Before
var typ websocket.Opcode = websocket.TextMessage
// After
var typ websocket.MessageType = websocket.TextMessage
```

`ReadMessage`, `ReadMessageContext`, `NextReader`, `WriteMessage`,
`WriteMessageContext`, `NextWriter` and `NetConn` take or return a
`MessageType`. The writers return a plain error for any other value instead of
`ErrInvalidOpcode`, and `MessageType.String` prints `MessageType(n)` for it.

Raw control frames are no longer part of the API. `ContinuationFrame`,
`CloseMessage`, `PingMessage`, `PongMessage`, `WriteControlFrame`,
`SetPingHandler` and `SetPongHandler` are removed:

- To send a Ping and wait for its Pong, call `Ping(ctx)`.
- To send a Close frame, call `CloseWrite`, `CloseWithStatus` or `Shutdown`.
- `ReadMessage` always answers a Ping with a Pong (RFC 6455 Section 5.5.2) and
  ignores unsolicited Pongs (Section 5.5.3). A replacement Ping handler could
  stop those replies by mistake, and a Pong handler only ever saw heartbeats
  once `Ping` matched its own Pongs.

## Protocol errors

The nine sentinels for a peer's protocol violations are replaced by one,
`ErrProtocol`; the violation is still named in the error's text.
`ErrPayloadTooLarge` is renamed `ErrMessageTooLarge` and matches `ErrProtocol`
too, so one check covers every message a connection refused:

```go
// Before
if errors.Is(err, websocket.ErrInvalidFrame) || errors.Is(err, websocket.ErrPayloadTooLarge) { ... }
// After
if errors.Is(err, websocket.ErrProtocol) { ... }
```

| Before | After |
| --- | --- |
| `ErrInvalidFrame`, `ErrUnexpectedFrame`, `ErrUnexpectedContinuation`, `ErrControlFrameFragment`, `ErrUnmaskedFrame`, `ErrMaskedFrame`, `ErrUnsupportedExtensions`, `ErrInvalidOpcode`, `ErrInvalidCompressedPayload` | `ErrProtocol` |
| `ErrPayloadTooLarge` | `ErrMessageTooLarge` (also matches `ErrProtocol`) |

`WriteMessage`, `NextWriter` and `NetConn` writes still reject text that is not
valid UTF-8 before sending anything, but with a plain error rather than
`ErrInvalidFrame`: the mistake is the caller's, not the peer's, so it does not
match `ErrProtocol`. A control frame from the peer with more than 125 bytes of
payload is a protocol error, no longer `ErrPayloadTooLarge`.

## Handshake and write errors

The exported error set is now the errors a caller can act on. The handshake
details are unexported; each still names the problem in its text and matches
the umbrella that says whose fault it was:

- `Dial` wraps every failure after the server answered with `ErrBadHandshake`,
  so one check covers a bad status, a bad `Sec-WebSocket-Accept`, and an
  extension or subprotocol the client did not offer.
- `NewConn` wraps every configuration failure with `ErrInvalidConfig`.
- `ErrNotHijacker` now matches `ErrInvalidConfig` too.

```go
// Before
if errors.Is(err, websocket.ErrProxy) || errors.Is(err, websocket.ErrInvalidSecAccept) { ... }
// After
if errors.Is(err, websocket.ErrBadHandshake) { ... }
```

| Before | After |
| --- | --- |
| `ErrProxy`, `ErrResponseHeaderTooLarge`, `ErrInvalidSecAccept`, `ErrFailedToGenerateKey`, `ErrHandshakeFailed` | `ErrBadHandshake` |
| `ErrInvalidMethod`, `ErrInvalidUpgradeHeader`, `ErrInvalidConnectionHeader`, `ErrMissingSecKey`, `ErrInvalidSecKey` (from `Upgrade`) | `ErrBadHandshake` (Upgrade has already replied 400 or 405) |
| `ErrInvalidExtension`, `ErrInvalidSubprotocol` | `ErrBadHandshake` from `Dial` and `Upgrade`, `ErrInvalidConfig` from `NewConn` |
| `ErrInvalidHandshakeHeader` | `ErrInvalidConfig` |
| `ErrAlreadyClosed` | `net.ErrClosed`, which it already matched |
| `ErrWriteFailed` | the transport's error, which it already wrapped, such as `os.ErrDeadlineExceeded`; any write error is terminal |

## Verification scope

Default-policy tests exercise omitted options separately from configured protocol
tests. The unchanged Autobahn testee explicitly chooses 64 MiB; its results do
not demonstrate default-policy behavior. Compression remains opt-in, and any
conformance failures must remain visible.
