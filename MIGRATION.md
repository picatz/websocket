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
Do not add an unconditional callback merely to hide denied handshakes. The known
Derpz consumer retains its explicit 64 KiB budget and needs its separate policy
integration reviewed before pinning this change.

## HTTP error ownership

`Upgrade` returns `ErrOriginNotAllowed` without writing, negotiating extensions,
or hijacking. The caller should map it to HTTP 403. Other pre-hijack handshake
errors also leave the response untouched. Use the executable examples for a
minimal 403/400 mapping; after `ErrHandshakeFailed`, a failed hijack or 101 write
may already own the transport, so do not write another HTTP response.

## Verification scope

Default-policy tests exercise omitted options separately from configured protocol
tests. The unchanged Autobahn testee explicitly chooses 64 MiB; its results do
not demonstrate default-policy behavior. Compression remains opt-in and
experimental, and existing conformance failures must remain visible.
