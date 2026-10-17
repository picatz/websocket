# High-level default policy migration

These are intentional compatibility changes to `Dial` and `Upgrade`, not a
behavior-preserving patch. Review callers before adopting the revision. This
note does not assign a release version or announce a release.

## Incoming payload budgets

Omitting a high-level size option now selects `DefaultMaxMessageSize = 1 << 20`
(1,048,576 bytes). Exactly that many valid application bytes fit; larger incoming
messages fail with `ErrPayloadTooLarge` and terminate the connection. Inventory
large snapshots, binary transfers and tunnel protocols, then set an explicit
appropriate budget. Do not split messages just to avoid the cap: message
boundaries belong to the application protocol.

```go
client, _, err := websocket.Dial(ctx, endpoint,
    websocket.WithMaxMessageSize(4 << 20))
server, err := websocket.Upgrade(w, r,
    websocket.WithUpgradeMaxMessageSize(64 << 10))
```

Existing explicit positive limits retain their meaning. Explicit zero or negative
high-level limits remain unlimited; the last option wins. Nil options retain the
default. `NewConn` remains unlimited unless given positive `WithMaxBytes`;
non-positive `WithMaxBytes` options do not erase an earlier positive limit.
Outgoing writes remain unaffected.

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
client, response, err := websocket.Dial(ctx, endpoint,
    websocket.WithMaxResponseHeaderBytes(2 << 20))
```

The budget includes the status line, every raw header line, duplicate fields,
folding whitespace, line separators, and the final empty line. A complete head
of exactly the budget fits; needing another byte returns an error matching both
`ErrBadHandshake` and `ErrResponseHeaderTooLarge`, closes the transport, and
returns a nil connection and response. Ordinary malformed-input and transport
errors retain their causes; a canceled Dial context retains precedence.

Omitted/nil options keep the default. An explicit zero or negative value opts
out of the bound; the last option wins. The budget is decrypted HTTP bytes, not
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
conn, err := websocket.Upgrade(w, r,
    websocket.WithUpgradeOriginCheck(func(r *http.Request, origin string) bool {
        return origin == "" || allowed[origin]
    }))
```

The callback sees one canonical origin, `""` for genuinely absent Origin, or
`"null"` for an opaque origin. It replaces the default, so decide each case
explicitly. The example retains missing-Origin native clients; remove that branch
if your protocol requires a browser Origin. The default rejects `null`; opting
in does not identify a specific trusted site or local file. A nil callback
restores the default and the last callback option wins. Malformed inputs cannot
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

`Upgrade` returns `ErrOriginNotAllowed` without writing, negotiating extensions,
or hijacking. The caller should map it to HTTP 403. Other pre-hijack handshake
errors also leave the response untouched. Use the executable examples for a
minimal 403/426/400 mapping (and 500 for missing hijacking support); after `ErrHandshakeFailed`, a failed hijack or 101 write
may already own the transport, so do not write another HTTP response.

## Compression negotiation and composition

`permessage-deflate` now sends with context takeover, fragments large compressed
messages into 64 KiB frames, and supports every window from 8 to 15 bits. Peers
that follow RFC 7692 need no change; these are the observable differences:

- A client's default offer is `permessage-deflate; client_max_window_bits`,
  declaring that the server may select any client window. Previously the offer
  had no parameters.
- `WithClientMaxWindowBits(8..14)` for `Dial` and `WithServerMaxWindowBits(8..14)`
  for `Upgrade` are honored instead of being rejected before connecting. A server
  accepts an offered `server_max_window_bits` below 15 instead of declining it,
  and includes a configured server window even when the client offered none
  (RFC 7692, Section 7.1.2.1).
- Built-in compression combined with an enabled custom extension is supported
  instead of failing with `ErrUnsupportedExtensionComposition`. That error now
  means more than one enabled built-in instance, or an extension changing RSV1.
- With context takeover, a failure part-way through sending a compressed
  message poisons later writes on that connection.
- Each connection keeps at most `DefaultCompressionMemoryLimit` (64 KiB) of
  compression state between messages by default: its send and receive history,
  not a whole compressor. Negotiated parameters are unchanged. Pass
  `WithCompressionMemoryLimit(0)` to keep a compressor per connection instead,
  which is faster per message and uses about 0.5–1 MiB.

## Closed-connection errors

Operations on a closed connection report one condition the standard library's
way. Reads, writes, `WriteControlFrame` and `Ping` after `Close`, after the
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

An error an outgoing extension returns from `ProcessOutgoingFrame` is now
wrapped with `%w`, like incoming extension errors, so `errors.Is` and
`errors.As` reach it. It still does not match `ErrWriteFailed`.

## Verification scope

Default-policy tests exercise omitted options separately from configured protocol
tests. The unchanged Autobahn testee explicitly chooses 64 MiB; its results do
not demonstrate default-policy behavior. Compression remains opt-in, and any
conformance failures must remain visible.
